# 配置与特性开关系统（Go）

一个**本地可跑**的配置与特性开关系统：从本地文件加载配置、按规则做特性开关判定，
并支持**不重启进程**的热更新与多版本回滚。

- 配置来源：本地 JSON 文件，轮询检测变更（也可通过 `store.PublishRaw` 直接投递内容）。
- 发布语义：**先解析 → 再整体校验 → 再编译 → 最后单次原子替换**。校验不通过的版本整体拒绝，
  当前版本保持不变，绝不部分生效。
- 灰度能力：按用户稳定分桶的百分比放量、按人群（segment）与条件树（all/any/attr）覆盖、
  开关之间的依赖（`depends_on`）与互斥（`conflicts_with`）。
- 版本管理：保留最近 50 个已发布版本，可切换/回滚到任意历史修订号；
  回滚后同一用户的判定结果与该版本当时完全一致。
- 并发：读取快照走 `atomic.Pointer`，**完全无锁**，判定不会被频繁更新阻塞；
  并发判定永远观察不到更新到一半的中间状态。

---

## 1. 构建与本地验证

需要 Go 1.25+。

```bash
go build ./...

# 全量测试（强烈建议带竞态检测）
go test ./... -race

# 单测日志会打印“输入配置快照”和“判定依据（reason + trace）”
go test ./internal/engine/ -race -v
go test ./internal/integration/ -race -v
```

命令行演示工具：

```bash
# 加载配置
go run ./cmd/flagdemo load testdata/config.json

# 单次判定（可带任意 k=v 属性；id 为内置属性）
go run ./cmd/flagdemo eval testdata/config.json new-checkout alice
go run ./cmd/flagdemo eval testdata/config.json checkout-coupon dave plan=beta

# 常驻进程：轮询热更新 + HTTP 判定
go run ./cmd/flagdemo serve testdata/config.json --addr :8080 --poll 1s
#   GET /eval?flag=new-checkout&user=alice&plan=beta
#   GET /versions
#   POST /rollback?revision=1
#   SIGHUP  强制重读配置文件
#   SIGUSR1 回滚到上一个历史版本
```

直接修改并保存配置文件即可观察到热更新；写入一份非法配置会被整体拒绝（旧版本继续服务），
把文件修好保存后无需重启即再次生效。

---

## 2. 配置格式

完整示例见 `testdata/config.json`。

```json
{
  "version": "v1",
  "segments": {
    "internal-users": {
      "name": "内部员工",
      "user_ids": ["alice", "bob"]
    },
    "beta-plan": {
      "name": "Beta 套餐用户",
      "rules": [
        { "attr": "plan", "op": "eq", "values": ["beta"] }
      ]
    }
  },
  "flags": [
    {
      "key": "new-checkout",
      "description": "新版结算页",
      "enabled": true,
      "default": false,
      "rollout": { "percentage": 50, "salt": "checkout-salt" },
      "targeting": [
        {
          "name": "内部和 Beta 用户全开",
          "match": {
            "any": [
              { "segment": "internal-users" },
              { "segment": "beta-plan" }
            ]
          },
          "serve": true
        }
      ],
      "depends_on": [],
      "conflicts_with": ["old-checkout"]
    }
  ]
}
```

### 顶层字段

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `version` | 是 | 配置的业务版本标签，缺失报 `missing_field`。 |
| `flags` | 是 | 开关数组，`key` 必填且全局唯一。 |
| `segments` | 否 | 具名人群，供条件以 `{"segment": "..."}` 引用。 |

### flag 字段

| 字段 | 说明 |
| --- | --- |
| `key` | 开关唯一标识，必填。 |
| `enabled` | 总开关。为 `false` 时直接判定关闭（`off:flag_disabled`），其余规则不再参与。 |
| `default` | 没有命中定向与放量时的兜底值。 |
| `rollout` | 百分比放量，见下节。 |
| `targeting` | 定向规则数组，**按顺序匹配，第一条命中即生效**。 |
| `depends_on` | 依赖的开关 key 列表；任一依赖对该用户未打开，本开关关闭。 |
| `conflicts_with` | 互斥开关 key 列表；要求双方对称声明。 |

### 条件树（`targeting[].match` / `segments.*.rules[]`）

- 组合节点：`{"all": [条件...]}`（AND，短路）或 `{"any": [条件...]}`（OR，短路），二者不能同时出现。
- 人群叶子：`{"segment": "internal-users"}`，引用顶层 `segments`。人群定义内部不允许再引用人群。
- 属性叶子：`{"attr": "plan", "op": "eq", "values": ["beta"]}`。
  - `op` 取值：`eq` / `ne`（`values` 必须恰好 1 个）、`in` / `not_in`（1..n 个）。
  - `attr` 为 `id` 时取用户 ID，其余取 `User.Attrs`。
- 空条件、组合节点与叶子混用、未知 `op`、`eq/ne` 多值等都会被校验拒绝。

---

## 3. 判定顺序与灰度分桶规则

对 `(配置版本, flag, user)` 的判定按以下顺序短路：

1. **开关不存在** → 返回 `not_found` 错误（不静默放行）。
2. **`enabled=false`** → `off:flag_disabled`。
3. **依赖**（按拓扑序求值）→ 任一依赖关闭，本开关 `off:dependency:<key>`；
   依赖缺失、循环依赖在**加载期**就被拒绝，运行时另有深度保护。
4. **互斥** → 仅当本开关门控为 on，且互斥方对同一用户的门控也为 on 时，
   返回 `conflict` 错误（`flags_active_together`），不静默放行；互斥方关闭则不冲突。
5. **定向规则** → 第一条命中的规则返回其 `serve`，reason 为 `on/off:targeting:<规则名>`。
6. **百分比放量** → 见下，reason 为 `on/off:rollout`。
7. **兜底** → `default`，reason 为 `on/off:default`。

### 稳定分桶

```
bucket = FNV-1a( salt + "|" + flagKey + "|" + userID ) mod 100      // 取值 [0,99]
on 当且仅当 bucket < percentage                                     // 放量区间 [0, percentage)
```

- **稳定可复现**：哈希输入只含配置内的 `salt`、`flagKey` 与用户 ID，
  不含时间、随机数或进程状态。同一配置版本下，同一用户无论判定多少次、
  无论从哪个 goroutine/进程判定，桶号与结果完全一致（测试覆盖连评 10 轮一致性）。
- **边界语义**：`percentage=0` 时无人放量；`percentage=100` 时全部放量（最大桶号 99 < 100）。
  随着百分比增大，放量集合单调扩张（边界测试覆盖 0 与 100）。
- **开关间独立**：每个 flag 必须提供非空 `salt`，避免不同开关的放量边界完全对齐。
- 注意：分桶稳定性以**配置版本**为前提；`percentage` 或 `salt` 改变属于新版本，
  结果可以变化。回滚到旧版本后，因为 salt/key/uid 都不变，结果与旧版本当时完全一致。

---

## 4. 热更新与回滚语义

### 发布（原子、全有或全无）

`Store.PublishRaw(raw)` 的固定流程：

1. `spec.ParseJSON`：仅做语法解析（拒绝语法错误、类型错误、未知字段、尾部多余数据）。
2. `validate.Validate`：字段缺失、取值非法、引用完整性、依赖环、互斥语义等**一次性全部检查**，
   多个问题用 `errors.Join` 聚合返回，每一条都保留独立分类与配置路径。
3. `compile.Compile`：建立 flag/segment 索引、计算依赖拓扑序，得到**不可变快照**。
4. 分配自增 `revision` 与内容 `SHA-256`，追加到历史，随后用**一次**
   `atomic.Pointer.Store` 替换当前指针。

任何一步失败都在替换指针之前返回，因此旧版本原封不动；不存在“先生效一半再发现错误”的窗口。
内容 SHA 与当前版本完全相同时视为无变化（no-op），不产生新修订号。

### 并发

- 判定路径只调用 `atomic.Pointer.Load()` 拿到不可变快照后在栈上读取，**不加锁、不阻塞**；
  发布/回滚仅在写侧使用互斥锁且临界区极小。
- 快照及其索引在发布后不再被任何代码修改，因此并发判定不可能读到中间状态。
- 测试 `TestConcurrentPublishAndRead` / `TestConcurrentEvalWhileReloading` 在 `-race` 下
  并发发布合法/非法版本并持续判定，校验结构完整性与桶号合法性。

### 回滚

- 保留最近 50 个已发布快照（`store.maxHistory`），`Rollback(revision)` 只是把当前指针
  整体替换为历史快照，**不新建版本、不修改历史**。
- 因为快照不可变且分桶只依赖配置内容，回滚后同一用户的 `on/reason/bucket` 与该版本当时
  逐一一致（集成测试对 200 个用户逐一比对）。
- 不存在的修订号 / 没有任何版本时返回 `state` 类错误。

### 文件监听

- `source.Watcher` 按固定间隔轮询文件内容，内容变化才回调；回调失败（新版本被拒绝）
  不影响监听，文件再次变化时自动重试。
- 同一份坏内容不会被每个轮询周期重复投递；推荐以“临时文件 + rename”方式原子写配置
  （`source.AtomicWrite`），避免读到写一半的截断态；即使读到截断态，也只会被当作一次
  非法更新拒绝，文件写完后自动恢复。

---

## 5. 错误分类

所有错误统一为 `errs.Error{Kind, Code, Path, Msg}`，可用 `errs.IsKind(err, kind)` 判断，
并可穿透 `errors.Join` 的聚合错误。

| Kind | 触发场景 | 典型 code |
| --- | --- | --- |
| `syntax` | JSON 语法错误、空输入、尾部多余数据 | `json_syntax`、`empty_config`、`trailing_data` |
| `missing_field` | 缺 `version`/`key`、规则缺 `serve`、条件缺 `attr/op/values`、空条件 | `flag_key_required`、`serve_required`、`empty_condition` |
| `invalid_value` | 百分比越界、重复 key、非法 op、`eq/ne` 多值、salt 为空、组合/叶子混用等 | `percentage_out_of_range`、`cond_bad_op`、`duplicate_flag` |
| `reference` | 依赖/互斥引用不存在的开关、条件引用不存在的人群 | `missing_dependency`、`missing_conflict_target`、`missing_segment` |
| `dependency` | 自依赖、依赖环、运行时异常深链 | `self_dependency`、`dependency_cycle` |
| `conflict` | 自互斥、互斥声明不对称、既依赖又互斥、运行时两个互斥开关同时打开 | `asymmetric_conflict`、`flags_active_together` |
| `not_found` | 判定一个不存在的 flag | `flag_not_found` |
| `state` | 未加载配置就判定、无版本/修订号可回滚、读取文件失败 | `no_config`、`no_version`、`read_failed` |

一条非法配置可以同时返回多条错误（例如“百分比越界 + 引用缺失 + 依赖环”），
但它们都在发布前报告，**不会有任何一条部分生效**。CLI 的错误输出形如：

```
kind=invalid_value code=percentage_out_of_range path=flags[0].rollout.percentage msg=百分比 150 超出 [0,100]
```

---

## 6. 代码结构

```
internal/errs/      统一错误类型与分类
internal/spec/      配置结构定义 + JSON 语法解析（syntax / 类型错误分类）
internal/validate/  整体语义校验（缺失/非法/引用/依赖环/互斥）
internal/compile/   校验通过后的不可变快照、索引与依赖拓扑序
internal/store/     atomic 当前指针、修订号、SHA-256、历史与回滚
internal/engine/    判定引擎：依赖、互斥、条件/人群匹配、稳定分桶、trace
internal/source/    本地文件来源、轮询监听、原子写
cmd/flagdemo/       load/eval/versions/rollback/serve 命令行与 HTTP 演示
testdata/config.json 示例配置
```
