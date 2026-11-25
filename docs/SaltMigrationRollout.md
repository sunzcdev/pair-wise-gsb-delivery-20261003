# 更换 actor 百分比 rollout 的分组盐：无退组过渡指南

## 背景：Flipper 里"盐"是什么

Flipper 的按 actor 百分比分桶没有独立的盐字段，**盐就是 feature 名字本身**。
两条求值路径用的是同一个公式（源码中互相注明 "Keep in sync ... Do not change"）：

- `lib/flipper/gates/percentage_of_actors.rb`
- `lib/flipper/expressions/percentage_of_actors.rb`

```ruby
Zlib.crc32("#{feature_name}#{actor_id}") % 100_000 < percentage * 1_000
```

因此：

- 换 feature 名（或换哈希前缀）= 换盐 = **几乎所有 actor 被重新分桶**；
- 分桶结果是 `(盐, actor_id, 阈值)` 的纯函数，**不需要、也不会写任何 actor 级状态**；
- 同一个 feature 内各 gate 之间是 OR，跨 feature 的 OR 可用 `FeatureEnabled` 表达式实现。

直接在 10% 上换盐的后果可离线量化（5,000 个 actor 的固定样本）：旧盐命中 519 人，
新盐命中 509 人，重叠仅 52 人，**467 人（9.34%）会意外退组**。

## 过渡原则（为什么不会意外退组）

每个阶段的有效命中集合是

```
S = S_old(旧盐, old_pct) ∪ S_new(新盐, new_pct)
```

三条不变式：

1. **旧盐冻结**：兼容窗口内 `old_pct` 固定为切换前的 10%，永不下调；
2. **新盐单调**：`new_pct` 从 0 开始，只升不降（0 → 2 → 5 → 10 → 25 → 50 → 100）；
3. **取并集**：任一约定命中即命中。

两个操作数各自单调不减，其并集必然单调不减——所以任何 actor 一旦命中就不会退出。
新规则按声明的阈值逐步接替，旧约定在新盐到达 100% 后才退役（退役那一刻集合不变，
因为新盐已覆盖全体）。

## 配置方式

### 阶段表（声明式，只有三个整数/表达式在变，无 actor 状态）

| 阶段 | 旧盐 `search` | 新盐 `search_v2` | 动作 |
|---|---:|---:|---|
| 0 切换前 | 10% | — | 现状 |
| 1 首次切换 | **冻结 10%** | 声明 0% | 新规则就位，集合不变 |
| 2 扩大阈值 | 10% | 2% | 仅新增 |
| 3 | 10% | 5% | 仅新增 |
| 4 到达平价 | 10% | 10% | 仅新增 |
| 5 | 10% | 25% | 仅新增 |
| 6 | 10% | 50% | 仅新增 |
| 7 | 10% | 100% | 新盐覆盖全体 |
| 8 结束过渡 | 关闭/删除 | 100% | 退役旧盐，集合不变 |

### 方式 A：两个 feature，调用点取 OR（最简单，零表达式）

```ruby
# 配置（每个阶段只调这一个百分比）
flipper.enable_percentage_of_actors :search, 10        # 旧盐，窗口内冻结
flipper.enable_percentage_of_actors :search_v2, 2      # 新盐，单调爬坡

# 调用点
flipper.enabled?(:search, actor) || flipper.enabled?(:search_v2, actor)
```

### 方式 B：一个稳定的包装 feature（调用点只认一个名字，推荐）

```ruby
# 一次性声明包装关系，之后只改两个底层百分比
flipper.enable :search_next, Flipper.any(
  Flipper.feature_enabled(:search),
  Flipper.feature_enabled(:search_v2),
)

# 调用点始终不变
flipper.enabled?(:search_next, actor)
```

`FeatureEnabled` 带环检测（`A → B → A` 返回 false），不会递归。

### 方式 B′：单 feature 内联两条盐的表达式

表达式里的 `PercentageOfActors` 以**当前求值 feature 名**为盐，因此旧约定用
`FeatureEnabled` 引入，新约定直接内联，两者写在同一份声明里：

```ruby
flipper.enable_percentage_of_actors :search, 10

flipper.enable :search_next, Flipper.any(
  Flipper.feature_enabled(:search),
  Flipper.property(:flipper_id).percentage_of_actors(5), # 盐 = "search_next"
)
```

## 离线复算与验证

```bash
# 纯 stdlib，逐字复刻 Flipper 的 crc32 公式，打印每阶段命中/新增/退出
ruby script/salt-migration/offline_recompute.rb 5000

# 真实 Flipper 实例（内存 adapter）上的 minitest，15k+ 断言
ruby -Ilib -Itest test/salt-migration/salt_migration_test.rb
```

脚本输出（5,000 actor 样本）关键列：每阶段 `exited` 恒为 0，`hits` 单调上升，
阶段 7 达到 5,000/5,000，阶段 8 退役旧盐后仍为 5,000。`old-only / both / new-only / off`
四类计数可以直接看出切换期间每个 actor 由哪条约定兜底，从而解释为何没有意外退组。

## 注意事项

- 旧盐在窗口内**只能冻结或关闭，不能下调**；下调会立刻破坏单调性。
- 结束顺序：先把新盐推到 100%，再关闭旧 feature；顺序反了就退化成"直接换盐"。
- 支持小数百分比（内部 `SCALING_FACTOR = 1_000`，最多三位小数），爬坡步长可按需细化。
- 不要修改 crc32 公式本身：源码注释明确说明任何改动都会重新分桶并静默翻转命中集合。
- 百分比 gate 只存一个整数，包装 feature 只存一份表达式；全程无 actor 级写入，
  可在 CI 中随时用同一批 actor id 离线复算复核。
