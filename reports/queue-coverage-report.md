# Solid Queue 领取覆盖对照报告

生成时间：2026-11-24T22:45:19.936Z  
模式：`deterministic_fixture`  
SQLite：`F:/GSB/workspaces/midnight-110-20261125-append-60/986/tmp/queue-coverage.sqlite3`  
复跑命令：

```powershell
$env:TARGET_DB='sqlite'
bundle exec ruby bin/queue_coverage_report.rb --write reports/queue-coverage-report.md --json reports/queue-coverage-report.json
```

报告扫描在单个 `SolidQueue::Record.uncached` + 数据库事务中完成，并对该连接设置 `PRAGMA query_only=ON`；没有调用 `ReadyExecution.claim`，也没有启动 worker。

## 1. 结论摘要（候选配置）

- 能领取：`mail-transactional`（前缀(prefix) mail-transaction* (#2)）
- 已配置但暂停：`billing`（精确名(exact) billing (#1)）
- 配置缺席：`legacy-etl`（无命中）
- 相对旧配置丢失的可领取队列：`legacy-etl`
- 场景核对：`legacy-etl` 由旧前缀 `legacy-*` 命中，候选删除该声明，因此属于配置缺席，不是暂停。
- 暂停排除：`billing` 虽被精确名 `billing` 命中，但 `solid_queue_pauses` 有暂停行，真实 `QueueSelector#queue_names` 会减去它。
- 顺序去重：`mail-transactional` 同时命中两条前缀；只保留一个领取队列，首选声明由配置顺序决定。

## 2. 两套配置共用的 ReadyExecution / Pause 视图

| 队列 | ReadyExecution 行数 | 暂停 | 参与本次对照 |
| --- | ---: | :---: | :---: |
| `billing` | 1 | 是 | 是 |
| `legacy-etl` | 1 | 否 | 是 |
| `mail-transactional` | 1 | 否 | 是 |

## 3. 候选配置（准备发布）

配置顺序：#1 `billing` → #2 `mail-transaction*` → #3 `mail-*` → #4 `ops-*`

真实选择后的覆盖顺序：`billing`, `mail-transactional`  
减去暂停后的领取顺序：`mail-transactional`

| 队列 | 状态 | 真实匹配（精确名 / 前缀） | 覆盖顺序位置 | 实际领取位置 |
| --- | --- | --- | :---: | :---: |
| `billing` | `CONFIGURED_BUT_PAUSED`<br>已配置但暂停 | #1 精确名(exact) `billing` | 1 | — |
| `legacy-etl` | `CONFIGURATION_ABSENT`<br>配置缺席 |  | — | — |
| `mail-transactional` | `CLAIMABLE`<br>能领取 | #2 前缀(prefix) `mail-transaction*`<br>#3 前缀(prefix) `mail-*` | 2 | 1 |

## 4. 正在运行的旧配置

配置顺序：#1 `billing` → #2 `mail-*` → #3 `mail-transaction*` → #4 `legacy-*` → #5 `ops-*`

真实选择后的覆盖顺序：`billing`, `mail-transactional`, `legacy-etl`  
减去暂停后的领取顺序：`mail-transactional`, `legacy-etl`

| 队列 | 状态 | 真实匹配（精确名 / 前缀） | 覆盖顺序位置 | 实际领取位置 |
| --- | --- | --- | :---: | :---: |
| `billing` | `CONFIGURED_BUT_PAUSED`<br>已配置但暂停 | #1 精确名(exact) `billing` | 1 | — |
| `legacy-etl` | `CLAIMABLE`<br>能领取 | #4 前缀(prefix) `legacy-*` | 3 | 2 |
| `mail-transactional` | `CLAIMABLE`<br>能领取 | #2 前缀(prefix) `mail-*`<br>#3 前缀(prefix) `mail-transaction*` | 2 | 1 |

## 5. 旧配置 vs 候选配置（同一快照）

| 队列 | 旧状态 | 候选状态 | 覆盖差异 | 领取差异 | 判定来源 |
| --- | --- | --- | --- | --- | --- |
| `billing` | `CONFIGURED_BUT_PAUSED`<br>已配置但暂停 | `CONFIGURED_BUT_PAUSED`<br>已配置但暂停 | 不变 | 不变 | 两版都有精确名 `billing`；不能领取的唯一原因是 Pause 行，不是配置顺序或配置缺失。 |
| `legacy-etl` | `CLAIMABLE`<br>能领取 | `CONFIGURATION_ABSENT`<br>配置缺席 | 覆盖丢失 | 可领取丢失 | 旧配置的 `legacy-*` 是真实前缀匹配；候选列表删除该声明，因此属于**配置缺席**，不是暂停。 |
| `mail-transactional` | `CLAIMABLE`<br>能领取 | `CLAIMABLE`<br>能领取 | 不变 | 不变 | 两版都实际覆盖且可领取；顺序首选从 `前缀(prefix) mail-* (#2)` 变为 `前缀(prefix) mail-transaction* (#2)`，重复前缀只影响该队列的归属/顺序，不产生重复领取。 |

## 6. 配置顺序与暂停排除是两件事

### 顺序决定重复匹配归属于哪条声明

- `mail-transactional`
  - 旧配置首选：**前缀(prefix) mail-* (#2)**；其余命中：#3 前缀(prefix) `mail-transaction*`
  - 候选首选：**前缀(prefix) mail-transaction* (#2)**；其余命中：#3 前缀(prefix) `mail-*`
- `SolidQueue::QueueSelector` 会按原始配置顺序重排并去重；同一队列不会因为命中多条声明而被领取多次。

### 暂停决定已覆盖队列是否退出领取列表

快照 Pause：`billing`

- `billing`：被候选配置命中（精确名(exact) billing (#1)）；候选状态 `CONFIGURED_BUT_PAUSED`。
- 对已覆盖且暂停的队列，仍可在 `eligible_order` 中审计到，但真实 `queue_names` / `scoped_relations` 会将其排除。

## 7. 候选扫描期间的新任务（并发对照）

- 新任务队列：`ops-late`
- 第二连接提交后的实时 ReadyExecution 行数：1
- 是否进入本次共享快照：`false`
- 快照结束后实时匹配：候选 #4 前缀(prefix) `ops-*`；旧配置 #5 前缀(prefix) `ops-*`
- `solid_queue_claimed_executions` 行数：0（报告不领取任务）。
- 结论：新到达只作为快照后的新鲜度证据；候选/旧版差异仍只来自同一快照中的两套配置。重跑报告会在新的快照中一致地评估它。

| # | 事件 | UTC 时间 | 细节 |
| ---: | --- | --- | --- |
| 1 | `shared_snapshot_begin` | `2026-11-24T22:45:20.651Z` | `{mode: "DEFERRED", read_only: true}` |
| 2 | `shared_view_read` | `2026-11-24T22:45:20.652Z` | `{ready_queues: ["billing", "legacy-etl", "mail-transactional"], paused_queues: ["billing"]}` |
| 3 | `selector_core_read_complete` | `2026-11-24T22:45:20.654Z` | `{scan: "candidate", eligible_order: ["billing", "mail-transactional"], claim_order: ["mail-transactional"]}` |
| 4 | `candidate_scan_midpoint` | `2026-11-24T22:45:20.654Z` | `{action: "insert ReadyExecution through a second SQLite connection", queue: "ops-late"}` |
| 5 | `late_arrival_committed_on_second_connection` | `2026-11-24T22:45:20.685Z` | `{job_id: 4, ready_execution_count: 1, queue: "ops-late"}` |
| 6 | `selector_match_attribution_complete` | `2026-11-24T22:45:20.686Z` | `{scan: "candidate", entry_count: 4}` |
| 7 | `selector_core_read_complete` | `2026-11-24T22:45:20.686Z` | `{scan: "running", eligible_order: ["billing", "mail-transactional", "legacy-etl"], claim_order: ["mail-transactional", "legacy-etl"]}` |
| 8 | `selector_match_attribution_complete` | `2026-11-24T22:45:20.687Z` | `{scan: "running", entry_count: 5}` |
| 9 | `both_configs_scanned_in_shared_view` | `2026-11-24T22:45:20.687Z` | `{}` |
| 10 | `shared_snapshot_released` | `2026-11-24T22:45:20.687Z` | `{}` |

## 8. 复跑自检

| 结果 | 断言 |
| --- | --- |
| PASS | snapshot contains exactly three fixture queues |
| PASS | billing is exact-matched in candidate |
| PASS | billing is configured but paused |
| PASS | mail-transactional matches two candidate prefixes |
| PASS | mail-transactional is claimable in candidate |
| PASS | legacy-etl is absent from candidate |
| PASS | legacy-etl is claimable under running config |
| PASS | late arrival is excluded from shared snapshot |
| PASS | late ops-* match is invisible inside snapshot |
| PASS | late arrival is visible after snapshot |
| PASS | report did not claim executions |

## 9. 人工复跑

```powershell
# 在仓库根目录；当前 GSB Windows 环境已通过 BUNDLE_GEMFILE 指向 .gsb-env/Gemfile
$env:TARGET_DB='sqlite'
bundle exec ruby bin/queue_coverage_report.rb `
  --write reports/queue-coverage-report.md `
  --json reports/queue-coverage-report.json
```

检查点：

1. SQLite 初始只有 `billing`、`mail-transactional`、`legacy-etl` 三条 ReadyExecution 队列。
2. `billing` 有对应 Pause 行。
3. `mail-transactional` 同时命中 `mail-*` 与 `mail-transaction*`。
4. `legacy-etl` 只被旧配置的 `legacy-*` 命中。
5. 候选扫描中点，第二连接向 `ops-late` 插入 Job + ReadyExecution。
6. 报告事务内两套配置看到相同的三张初始队列；事务后的实时读取才能看到 `ops-late`。

