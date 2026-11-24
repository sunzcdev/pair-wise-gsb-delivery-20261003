# 跨库 enqueue 的可恢复交接（Transactional Outbox for Solid Queue）

业务数据写在一个 SQLite 文件里，Solid Queue 接另一个 SQLite 文件。两个库是
**物理独立的数据库**，不存在能同时包住二者的事务。本 demo 用事务性发件箱
（transactional outbox）给这类 enqueue 提供显式、可恢复的交接：

- **业务失败时**：业务记录和 outbox 行在同一个本地事务里一起回滚，队列库里
  永远没有可消费的任务；
- **业务已提交、队列没接到时**：outbox 行是持久化的“意图”，中继进程（relay）
  重启后会重试交接，且**最多只创建一个 Solid Queue job**，可恢复且不重复；
- **一一对应**：每条业务记录对应一行 outbox，每行 outbox 至多对应一个
  Solid Queue job，没有“半个提交”。

## 为什么普通 enqueue 不够

`ActiveJob` 的 solid_queue 适配器会在自己的连接上、自己的事务里写
`solid_queue_jobs` + `solid_queue_ready_executions`。业务事务和它分属两个
数据库，于是只有两种失败窗口：

1. 业务回滚，但 job 已经写进队列库 → 出现没有业务记录支撑的幽灵任务；
2. 业务提交成功，但写队列库前进程崩溃 → 业务发生了，任务却永远丢了。

Outbox 模式把“写队列”从业务事务里挪出去，只在业务事务里写一行与业务记录
同库的 outbox，再由独立中继异步、幂等地把它变成真正的 Solid Queue job。

## 两个数据库

| 文件 | 角色 | 表 |
| --- | --- | --- |
| `demo/tmp/business.sqlite3` | 业务库（`ActiveRecord::Base` 连接） | `orders`、`outbox_messages` |
| `demo/tmp/queue.sqlite3` | 队列库（`SolidQueue::Record` 独立连接池） | 官方 `queue_schema.rb` 的全部 Solid Queue 表 |

队列库额外加了一条 **`solid_queue_jobs.active_job_id` 唯一索引**。官方 schema
在该列上只有非唯一索引；中继每次都用 outbox 行里稳定的 job id 重新 enqueue，
唯一索引把“重试交接”变成硬冲突拒绝，而不是再插一条 job。

## 交接协议（关键不变量）

```
业务进程（业务库事务）
  1. INSERT orders
  2. 生成稳定的 Active Job id
  3. 用 Active Job 自带序列化器生成完整 envelope，INSERT outbox_messages(pending)
  4. commit                              <- 要么 order+outbox 都在，要么都不在

中继进程（可随时重启、可重复运行）
  5. 取 pending 的 outbox 行
  6. 在【队列库事务】里 SolidQueue::Job.enqueue（job + ready execution 同事务）
  7. 队列事务 commit 之后，才在业务库把 outbox 标记为 dispatched
```

崩溃只可能发生在两个事务之间：

- 崩在 6 之前：outbox 仍 pending，队列库没有 job，重启后正常重试；
- 崩在 6 之后、7 之前：**job 已落库，outbox 仍 pending**。重启后再次 enqueue
  撞上唯一索引，relay 据此识别“上次其实交接成功了”，只补做状态登记，
  **不会产生第二个 job**。

job id 在业务事务里就固定下来并随 outbox 持久化，是整条链路幂等的根基。

## 目录结构

```
demo/
  boot.rb                              非 Rails 应用下加载 Solid Queue + Zeitwerk
  database.rb                          两个 SQLite 连接、双方 schema、唯一索引
  support.rb                           审计日志、状态打印、一一对应不变量校验
  run.rb                               CLI：reset/place_order/relay/work/state/scenario ...
  scenario.rb                          端到端场景编排（用真实子进程模拟崩溃）
  app/
    models/order.rb                    业务记录
    models/outbox_message.rb           事务性发件箱
    jobs/fulfill_order_job.rb          真正被 Solid Queue 消费的 Active Job
    services/place_order_with_outbox.rb 业务事务（order + outbox 原子写入）
    services/outbox_relay.rb           幂等、可恢复的跨库交接
    services/demo_worker.rb            最小 Solid Queue 消费者
  test/
    outbox_test.rb                     26 项不变量单进程测试
    crash_recovery_test.rb             跨真实进程的崩溃恢复测试
```

## 运行

```bash
# 端到端演示（含业务回滚、提交后崩溃、冷启动恢复、worker 消费）
ruby demo/run.rb scenario

# 自动化测试
ruby demo/test/outbox_test.rb
ruby demo/test/crash_recovery_test.rb

# 也可以逐步手动执行
ruby demo/run.rb reset
ruby demo/run.rb place_failing_order      # 业务回滚：order/outbox/job 都为 0
ruby demo/run.rb place_order SKU-100 2    # 业务提交：outbox pending，队列库为空
ruby demo/run.rb relay_crash              # 子进程在队列 commit 后 exit!
ruby demo/run.rb relay                    # 冷启动重试：唯一索引去重，恢复一次
ruby demo/run.rb work 10                  # 消费 ready 任务并执行真正的 Active Job
ruby demo/run.rb state                    # 打印双方库内容与不变量校验
```

## 设计说明与边界

- **payload 自包含**：outbox 存的是 `active_job.serialize` 的完整 envelope，
  队列侧不需要连业务库、也不需要能加载 `Order`，位置参数/关键字参数的
  `_aj_ruby2_keywords` 语义随 envelope 一起保留。
- **去重依赖数据库约束**：幂等不是靠“先查再插”，而是靠唯一索引兜底，
  两个 relay 并发跑也不会插出两条 job；业务库侧用 `with_lock` 防止
  dispatched 状态被重复登记。
- **中继只负责交接**：job 进入 Solid Queue 之后的重试、失败、并发控制全部
  仍由 Solid Queue 原生机制处理，demo 的 worker 也复用
  `ReadyExecution → ClaimedExecution → ActiveJob::Base.execute` 的官方数据模型。
- **生产化还需补充**（本 demo 刻意不做）：relay 的轮询/守护进程化、
  outbox 老数据清理、`attempts/last_error` 退避与告警、队列库长期不可用时的
  人工介入流程。
