# Feature Flag Config Engine (Go)

一个零第三方依赖、可本地运行的**配置与特性开关系统**。支持：

- 从本地 JSON 文件加载配置，运行时**热更新**（轮询，无需重启进程）；
- 新配置先整体校验，失败即**整体拒绝**，继续服务旧版本，错误可区分为
  语法问题 / 字段缺失 / 取值非法 / 引用错误 / 循环依赖 / 冲突声明错误；
- 灰度放量：按用户的**稳定分桶百分比**、人群（audience）覆盖、
  开关**依赖**与**互斥**；同一用户在同一配置版本下结果稳定可复现；
- 多版本保留、**回滚**，回滚后与之前那一刻判定完全一致；
- 读取路径**无锁**（单次原子指针加载不可变快照），更新不会与判定
  互相阻塞，也不可能读到“更新一半”的中间状态。

## 目录结构

```
internal/config/   配置格式、解析、分类错误、语义校验、文件读取与轮询
internal/bucket/   稳定分桶哈希与条件匹配
internal/engine/   原子快照、版本历史、热更新、回滚、判定引擎
internal/reload/   把文件监听器接到引擎的热更新管理器
cmd/demo/          可运行的命令行 demo（加载/判定/watch/回滚）
configs/example.json  示例配置
```

## 配置格式

JSON 文档，顶层字段：

| 字段        | 必需 | 说明 |
|-------------|------|------|
| `version`   | 是   | 非空版本标签，**全局唯一**；同一标签不得复用于不同内容 |
| `flags`     | 是   | 对象，key 即开关 key；可以为空对象 |
| `audiences` | 否   | 命名人群，供开关按名称引用 |

每个 flag：

| 字段                | 必需 | 说明 |
|---------------------|------|------|
| `enabled`           | 否   | 总开关，缺省 `false`。为 `false` 时一切判定为 OFF |
| `percentage`        | 是   | 整数 `[0,100]`，放量百分比 |
| `include_audiences` | 否   | 命中任一人群 => 强制 ON |
| `exclude_audiences` | 否   | 命中任一人群 => 强制 OFF（优先级最高） |
| `depends_on`        | 否   | 这些开关必须全部 ON，本开关才能 ON |
| `conflicts_with`    | 否   | 互斥开关；必须**对称声明**，否则配置被拒绝 |

每个 audience 含 `conditions`（非空数组，条件之间是 **AND**）：

| 字段        | 说明 |
|-------------|------|
| `attribute` | 用户属性名，非空 |
| `operator`  | `eq` / `neq` / `in` / `not_in` |
| `values`    | 非空字符串数组；`eq`/`neq` 取第一个 |

属性缺失时的语义：正向操作符（`eq`、`in`）不命中；负向操作符
（`neq`、`not_in`）命中。

示例见 [`configs/example.json`](configs/example.json)。

## 判定顺序（Reason 判定依据）

对 `(用户, 开关)`，在一次原子加载的不可变快照上依次执行：

1. `enabled=false` → OFF，`flag_disabled`
2. 命中任一 `exclude_audiences` → OFF，`excluded_audience`
3. 任一 `depends_on` 开关最终为 OFF → OFF，`dependency_off`（fail-closed）
4. 存在“字典序更小且最终为 ON”的互斥开关 → OFF，`conflict_active`
5. 命中任一 `include_audiences` → ON，`included_audience`
6. 否则按百分比分桶：`bucket < percentage` → ON，`in_percentage_rollout`；
   否则 OFF，`out_of_percentage_rollout`
7. 开关不存在 → OFF，`unknown_flag`（绝不静默放行）

**互斥决胜规则**：两个互斥开关同时满足放量条件时，按 key
字典序最小者获胜。该规则完全确定，因此同一用户在同一版本下结果
稳定；同时冲突边只会递归到更小的 key，递归天然无环。依赖的求值也
使用“最终判定”，因此被互斥压制的开关不会再满足依赖。

每次 `Evaluate` 返回的 `Result` 附带证据：`On/Reason/Bucket/
MatchedAudience/RelatedFlag/Version/Fingerprint`，便于日志与排障。

## 灰度分桶规则

```
bucket = (FNV-1a64( flagKey + 0x1f + fingerprint + 0x1f + userID ) mod 1_000_000) / 10_000
```

- 结果是 `[0,100)` 的整数，放量规则为 **`bucket < percentage`**；
- 输入混入 `flagKey`：不同开关对同一用户独立散开；
- 输入混入 `fingerprint`：这是配置**内容**（不含 version 标签）的
  SHA-256 指纹。回滚到相同内容时分桶逐位一致；真正的新版本允许
  重新洗牌（“新配置 = 新的放量随机化”是常见且明确的语义）；
- 先对 1,000,000 取模再整除 10,000，把取模偏差压到百万分之一以下；
- `userID` 为空时退化为常量桶，因此调用方应提供稳定用户标识。

## 热更新与回滚语义

- **原子性 / all-or-nothing**：新文档必须完整解析并通过全部语义
  校验才会被激活；失败时引擎内活跃快照**一行都不变**，继续服务旧版本。
- **无中间状态**：所有配置存在不可变快照里；`Evaluate` 入口只做一次
  `atomic.Pointer.Load()`，此后整次判定都跑在同一个快照上。读到的
  要么是旧世界要么是新世界，不可能是半成品。
- **判定不被更新阻塞**：读取路径无互斥锁；写路径仅在更新小型历史
  数组并交换指针时持一把短临界区的锁。高频更新不会卡住正在进行的判定。
- **版本历史**：最近最多 20 个不同内容指纹的版本被保留；重复激活
  同一内容会移动到队首而非产生重复条目。
- **回滚**：`Rollback(version)` 或 `RollbackToFingerprint(fp)` 原子地
  把活跃指针切回历史快照；因为快照不可变、指纹只取决于内容，回滚后
  同一用户的 `bucket` 与判定结果与当时完全一致。
- **标签保护**：若复用已有版本标签但内容不同，更新以 `conflict`
  错误被拒绝，保证标签永远是可信的回滚目标。
- 文件监听采用**轮询 + 原子 rename** 的写入约定（写临时文件后 rename
  覆盖）；写了一半的文件只会产生一次分类错误事件，下次完整写入即恢复。

## 错误分类

错误统一为 `*config.Error`，含 `Kind`、`Path`、`Message`：

| Kind | 含义 | 典型触发 |
|------|------|----------|
| `syntax`        | 文档不是合法 JSON / 含未知字段 | 括号损坏、类型字面量错误、多余字段 |
| `missing_field` | 必填字段缺失或为空 | 无 `version`、无 `flags`、缺 `percentage`、condition 字段为空 |
| `invalid_value` | 字段存在但取值非法 | `percentage` 越界、未知操作符、自依赖/自互斥 |
| `bad_reference` | 引用了不存在的对象 | 依赖/互斥指向未知开关、人群名不存在 |
| `dependency_cycle` | `depends_on` 图有环 | A→B→C→A |
| `conflict`      | 冲突图不一致 / 版本冲突 | 互斥只声明了一边、版本标签复用为不同内容 |

JSON 里的**类型错误**（如 `"enabled": "yes"`）归为 `invalid_value`，
因为文档本身语法成立但字段取值不合法。

## 本地验证

要求 Go 1.25+（模块声明为 `go 1.25.0`），无外部依赖。

```bash
# 编译与静态检查
go build ./...
go vet ./...

# 全部测试（强烈建议带竞态检测）
go test -race ./...

# 带日志的单测（日志会打印输入配置快照与判定依据）
go test -race -v ./internal/engine/
go test -race -v ./internal/config/
go test -race -v ./internal/reload/
```

测试覆盖：

- **错误可区分**：16 个解析/校验用例分别命中六类错误并断言 `Kind`/`Path`；
- **整体拒绝**：文档中任一开关非法时整份文档被拒，无部分结果；
- **灰度边界**：找出桶为 0/49/50/99 的真实用户，验证
  `bucket < percentage`（49 ON、50 OFF、0 ON、99 OFF）；
- **稳定可复现**：同一 `(用户, 版本)` 多次判定结果（含 bucket）完全相同；
- **回滚一致性**：v1→v2→v3 后回滚 v1，判定与最初逐字段相等；
- **并发热更新**：16 个判定 goroutine + 1 个更新 goroutine（约 2000 次
  更新并夹杂非法更新），在 `-race` 下校验每次判定都自洽于其携带的
  版本指纹，非法更新全部被拒且活跃版本不变；
- **文件热更新**：临时目录中原子改写配置文件，验证自动切换、坏文件被
  拒绝并继续服务旧版本、语法错误不崩溃、随后可回滚。

### Demo 用法

```bash
# 一次性判定
go run ./cmd/demo -config configs/example.json -user u-42 -flag new_checkout
go run ./cmd/demo -config configs/example.json -user u-7 -attr tier=staff -flag new_checkout

# watch 模式：改文件即热更新；stdin 支持 eval / versions / rollback / quit
go run ./cmd/demo -config configs/example.json -user u-42 -watch

# 启动即回滚到某版本再判定
go run ./cmd/demo -config configs/example.json -user u-42 -rollback v1 -flag new_checkout
```

在 watch 模式下用编辑器（或 `mv tmp target`）覆盖配置文件，可看到
`[reload] applied version=...`；写入非法内容会看到
`[reload] REJECTED: ...`，且活跃版本保持不变。

## 编程式用法（速览）

```go
eng, _ := engine.New(rawV1)                 // bootstrap
_ = eng.Update(rawV2)                       // 热更新；非法则返回 *config.Error
_ = eng.Rollback("v1")                      // 按标签回滚
_ = eng.RollbackToFingerprint(fp)           // 或按内容指纹回滚
r := eng.Evaluate(bucket.User{ID: "u-42"}, "new_checkout")
// r.On, r.Reason, r.Bucket, r.Version, r.Fingerprint ...
```
