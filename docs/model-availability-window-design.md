# 模型可用时间窗（Availability Window）设计

> 状态：设计待评审
> 范围：`models.ModelWithProvider` 新增可用时间条款 + 负载均衡候选池过滤 + 前端独立配置弹窗
> 关联既有实现：`models/peak.go`（峰谷计费）、`service/peak.go`（时段解析 + 日历缓存）

---

## 0. 一句话结论

在**关联行**（`ModelWithProvider`）上加一个 JSON 字段 `Availability`，语义为「每天 48 个半小时槽的允许/禁止集合」；后端在**构造候选池**时按请求发起时刻过滤掉窗口外的关联；前端用**独立弹窗 + 7×48 网格**编辑，主表单只保存结果。

同时要指出一件事：**这个功能本身不产生任何额外 SQL**，所以你担心的"频繁查库"并不来自它。真正的查询压力是既有的每请求 3 次查询（`models` / `model_with_providers` / `providers`），要不要一起做缓存是**独立决策**，见 §6。

---

## 1. 三个先于细节的判断

### 1.1 为什么不复用 `PeakTerms`

`PeakTerms` 的结构（`Start`/`End`/`Days`/`Workday`/`Multiplier`）和判定函数（`InClockWindow`）已经解决了一半问题，但它有三处**语义不兼容**：

| 维度 | PeakTerms | 可用时间需要 |
|---|---|---|
| 未命中任何时段 | 回落到乘数 1（放行） | 视模式而定，`allow` 模式下必须**拒绝** |
| 优先级 | 数组顺序首个命中 | 集合语义，无顺序 |
| 跨零点 | `Start > End` 表示跨零点 | 半小时网格下应**禁止**，见 §2.3 |

**结论**：不复用 `PeakTerms` 结构，但**复用它的解析原语**（`service.ParseClock`、`models.PeakCalendar.EffectiveTime`）。判定逻辑另写，因为"未命中"的默认行为相反——这是最容易抄错的一处。

### 1.2 为什么挂在关联行而不是模型行

用户说的是"关联模型的时候"，这与业务事实一致：A 上游有夜间额度、B 上游白天才有配额，同一个模型在同一时刻的可选上游本来就不同。挂模型行只能表达"整个模型几点可用"，那是另一个需求（如果以后要，再叠加一层与运算即可）。

### 1.3 为什么用「模式 + 一个集合」而不是两套时间段

"可用时间"和"禁用时间"是**同一组数据的两种取反解释**，不是两组配置。存两份必然产生"某时刻同时被允许和禁止，谁赢"这种无解语义。

因此只有一份时段集合 `Slots`，外加一个 `Mode`：

- `mode = "allow"`：**列表内的槽可用**，列表外不可用（白名单，适合"只在工作时间开放"）
- `mode = "deny"`：**列表内的槽不可用**，列表外可用（黑名单，适合"夜间维护窗口"）

切换模式 = 一次取反，界面上提供「反转」按钮，不需要用户重画一遍。

---

## 2. 数据模型

### 2.1 Go 结构（新文件 `models/availability.go`）

```go
package models

// AvailabilitySlot 一个连续可用/禁用区间。
//
// 半小时为最小粒度，Start/End 必须落在 :00 或 :30 上，End 允许 "24:00"。
// **不支持跨零点**（Start 必须小于 End）：网格是 7×48 的槽位集合，
// 22:00-02:00 天然表达为「周一 22:00-24:00 + 周二 00:00-02:00」两段。
// 允许跨零点会让「这段算周几」产生歧义，而 PeakTerms 的跨零点实现
// 依赖"按起始日归属"这条隐含约定——可用性不该继承这种含糊。
type AvailabilitySlot struct {
	Day   int    `json:"day"`   // 0=周日 … 6=周六（与 time.Weekday / PeakPeriod.Days 一致）
	Start string `json:"start"` // "HH:MM"
	End   string `json:"end"`   // "HH:MM" 或 "24:00"
}

// AvailabilityTerms 某条「模型 × 上游」关联的可用时间条款。
//
// nil（列值为 NULL）= 不限时间，24×7 可用。这条路径**零运行时开销**：
// 热路径上判到 nil 直接放行，不解析、不查表（与 PeakTerms 的 nil 一致）。
type AvailabilityTerms struct {
	// Enabled 为 false 时条款保留但整体失效（恒可用）。
	// 与 nil 的区别：nil 是"没有这份配置"，false 是"有配置，暂时关闭"。
	// 保留这个区别是为了让用户能"这个月先全开"而不丢已有的表格。
	Enabled bool `json:"enabled"`
	// Mode 取 AvailabilityModeAllow / AvailabilityModeDeny。
	// 空字符串按 allow 处理（宽进严出：误配成空时按"只开列表里的"最保守？
	// 不——见 §5.4，空 mode + 空 slots 会被校验拒绝，不会走到运行时）。
	Mode  string             `json:"mode"`
	Slots []AvailabilitySlot `json:"slots"`
}

const (
	AvailabilityModeAllow = "allow"
	AvailabilityModeDeny  = "deny"
)
```

挂在 `ModelWithProvider` 上（`models/model.go`）：

```go
	// Availability 这一条关联的可用时间窗，nil 表示 24×7 可用。
	//
	// 与 Peak 同处一行是刻意的：两者都是"这条关联自身的条款"，
	// 且都只在选到/选中这一条时才需要 —— 拆表就要在热路径上多一次查询。
	Availability *AvailabilityTerms `gorm:"serializer:json"`
```

### 2.2 为什么 DB 里存区间列表，运行时存位图

| 格式 | 可读性 | 判定成本 | 体积 |
|---|---|---|---|
| 区间列表（存库） | 人工可读、可手改、diff 友好 | 每次判定要遍历 + 解析字符串 | 几十字节～几百字节 |
| `[7]uint64` 位图（运行） | 天书 | 1 次位测试 | 56 字节 |

**存人类格式，跑机器格式**：落库用区间列表，`Compile()` 后得到位图缓存在内存。这样运维能直接 `SELECT` 看懂配置，热路径又是 O(1)。

不存位图字符串的另一个理由：48 位/天用 `uint64` 要移位拼接，存成 base64 之后**没有人**能在排障时看懂它，而这类"为什么这家上游没被选中"的排查本来就够难了。

### 2.3 位图与判定

```go
// models/availability.go（续）

const slotsPerDay = 48 // 24h × 2

// AvailabilityMask 一周的槽位位图：每周 [day] 的 bit i 表示第 i 个半小时槽。
type AvailabilityMask [7]uint64

// Compile 把条款编译为位图。非法输入由 handler 在写入前校验，
// 这里对越界项**跳过**而非报错：热路径不该因为一行脏数据停摆。
func (t AvailabilityTerms) Compile() AvailabilityMask { ... }

// SlotOf 返回某时刻对应的槽位序号（0..47）。
func SlotOf(local time.Time) int { return local.Hour()*2 + local.Minute()/30 }
```

判定：

```go
// service/availability.go
type AvailabilityResolver struct {
	mask    models.AvailabilityMask
	deny    bool
	loc     *time.Location
}

// AvailableAt 判定某时刻是否可用。nil 条款恒可用（由调用方短路）。
func (r AvailabilityResolver) AvailableAt(t time.Time) bool {
	local := t.In(r.loc)
	on := r.mask[local.Weekday()]&(1<<models.SlotOf(local)) != 0
	return on != r.deny // allow: on→可用；deny: on→禁用
}
```

`on != r.deny` 这一行是整个功能的核心：`allow` 模式（deny=false）命中即可用，`deny` 模式（deny=true）命中即禁用。

### 2.4 时区

**复用 `peak_calendar` 的 `Timezone`**，不引入第二份时区配置。理由：两者都是"人的作息"，分属两份配置时，改时区只改一处会让可用窗口和计费窗口整体错位半小时以上，而没人会想到去查第二处。

代价：`service.GetPeakCalendar(ctx)` 已是带 30s TTL 的全局缓存，可直接取用，无额外开销。

### 2.5 节假日/工作日支持

**首版不做**。用户的需求原话是"按星期 + 时间段"，工作日是 `Day ∈ {1..5}` 就能表达的。加上 `DateOverrides` 会让"调休周六要开工作时段"变成必须处理的组合，收益不明确。列为 P2，见 §11。

---

## 3. 与既有开关的关系

一条关联有三种"不可选"的原因，优先级从高到低：

| 层级 | 字段 | 语义 | 生效时机 |
|---|---|---|---|
| 1 | `Status` | 永久启用/停用 | 保存即生效 |
| 2 | `Availability` | 时间窗 | 按当前时刻**每次请求重算** |
| 3 | `Backup` | 备用关联，不参与日常轮询 | 主池耗尽后 |

三者是**与**关系，互不替代。`Status=false` 的关联即使落在可用窗口内也不会进池。

---

## 4. 后端接入点

### 4.1 过滤发生在构造权重池之前

`service/chat.go` 的 `ProvidersWithMetaBymodelsName` 当前顺序是：

```
查 model → 查关联(SQL 条件: status/tool_call/structured_output/image)
        → 查 providers(SQL 条件: type IN ServableTypes)
        → poolFor(直连优先) → splitWeightItems(主/备用) → ProvidersWithMeta
```

新增一步，插在 `splitWeightItems` **之前**：

```go
// 可用时间过滤：放在 splitWeightItems 之前，主池与备用池才会**各自**
// 因为时间窗而变空或变小。若放在之后，主池被时间窗清空时备用池
// 里那些同样在窗口外的关联仍会被选中，等于把窗口当摆设。
available, err := filterByAvailability(modelWithProviders, at, cal)
if err != nil { ... }
```

`at` 由调用方传入**请求发起时刻**（与 `BalanceChat` 的 `start` 同一个变量），不是过滤时的 `time.Now()`。理由：`ResolvePricingFor` 已经用 `start` 计价，如果选上游用另一个时刻，跨时段边界的请求会出现"按谷时计价、按峰时选上游"这种自相矛盾的组合。

### 4.2 主池因时间窗变空 → 自动降级备用（复用既有机制）

`BalanceChat` 里已有：

```go
if hasBackup && !usingBackup && balancer.Empty() {
    balancer = backupBalancer
}
```

`NewLottery(map{})` 的 `Empty()` 返回 `true`，所以**空主池会自然触发备用接管**，不需要新分支。这一条要写进测试，因为它是"时间窗 + 备用"两个功能唯一的耦合点。

### 4.3 全部不可用时的错误语义（关键）

当前若关联全被过滤掉，会走到：

```go
return nil, errors.New("not provider for model " + before.Model)
```

这个信息在时间窗场景下**是误导**：运维会去查"关联是不是被删了"，实际是"现在 20:47，所有上游的窗口 18:00 就关了"。

新增独立错误，并带上下次可用时间：

```
no provider available at this time for model gpt-4o
(all 3 providers outside their availability window; next window opens Mon 09:00, in 12h13m)
```

下次可用时间的计算：从当前槽往后扫 `7 × 48` 个槽（最坏 336 次位测试，可忽略），找到第一个全部主关联都可用的槽。**只在错误路径上算**，不在热路径上算。

同时 `saveFailedLog` 写入的 `Error` 字段用这条消息，日志里能直接看出是时间窗而非配置缺失。

### 4.4 写入校验（handler）

`CreateModelProvider` / `UpdateModelProvider` 共用一个新的 `validateAvailability`：

| 规则 | 拒绝原因字符串 |
|---|---|
| `mode` ∈ {allow, deny}（空按 allow 归一） | `invalid availability mode` |
| `enabled=true` 且 `slots` 为空 | `availability has no slots` |
| `day` ∈ [0,6] | `invalid availability day: %d` |
| `start`/`end` 可解析且对齐 30 分钟 | `availability time must align to 30 minutes: %q` |
| `start < end`（禁止跨零点） | `availability slot must not wrap midnight: %s-%s` |
| 同一天内区间不重叠 | `overlapping availability slots on day %d` |

**区间重叠要不要拒绝**：拒绝。网格 UI 不可能产生重叠，一旦出现就说明是 API 直接调用或手工改库，早报错比晚排查好。

### 4.5 `nil` 清空的坑（必须照抄 Peak 的补丁）

`UpdateModelProvider` 用的是结构体 `Updates`，GORM 会跳过零值（`nil` 指针）。所以：

```go
if req.Availability == nil {
    gorm.G[models.ModelWithProvider](models.DB).Where("id = ?", id).
        Update(c.Request.Context(), "availability", nil)
}
```

不加这段，用户"移除可用时间配置"之后旧表格会一直留在行上，下次打开弹窗又回来了。`Peak` 已经踩过一次，`ContextLength` 也踩过一次（见 `handler/api.go:465`、`handler/api.go:786`），这是本项目第三次遇到同一个坑——**建议顺手抽一个 `clearNullableColumn` 小工具**，而不是继续复制粘贴。

---

## 5. 缓存设计

### 5.1 先把问题说清楚

你的原话是"要做缓存，以免频繁的查询数据库"。但可用时间字段挂在关联行上，而关联行**本来每个请求就要读**（`ProvidersWithMetaBymodelsName` 里那三条 SQL）。所以：

- **可用性功能自身产生的额外 SQL：0 条。**
- **需要缓存的是编译后的位图**，避免每次请求把 `[]AvailabilitySlot` 重新解析成 `[7]uint64`。这个开销其实很小（几个字符串分割），但既然关联池要缓存，位图就该跟着缓存，别让它在热路径上重复算。

### 5.2 真正值得缓存的是关联池（独立优化）

| 方案 | 减少的 SQL | 陈旧窗口 | 复杂度 | 是否解决"频繁查库" |
|---|---|---|---|---|
| A. 只缓存编译位图 | 0 | 0 | 极低 | ❌ |
| B. 缓存关联池（key=model name） | 每请求 3 → 0（命中时） | ≤TTL | 中（失效清单） | ✅ |
| C. SQL 层算时间窗 | 0 | 0 | 高（SQLite 无合适函数、无法走索引） | ❌ |

**推荐 B**，且 B 的价值与可用时间功能无关——不做这个功能也该做。

### 5.3 方案 B 的实现要点

```go
// service/pool_cache.go
type poolSnapshot struct {
	model     models.Model
	assocs    []models.ModelWithProvider
	providers []models.Provider
	loadedAt  time.Time
}

const poolCacheTTL = 30 * time.Second
var poolCache sync.Map // key: string(model name)
```

三个刻意的设计选择：

1. **key 只有 model name，不含 style / 能力位。**
   key 再加上 `style` 与 `toolCall/structuredOutput/image` 会变成 3 × 8 = 24 个组合/模型，命中率随着组合数上升而崩。正确做法是把这三类过滤**从 SQL 挪到内存**：
   - `Where("type IN ?", ServableTypes(style))` → 缓存全量 provider，内存过滤
   - `Where("tool_call = ?", true)` 等 → 缓存全量关联，内存过滤

   这也顺带修掉一个既有小毛病：`ServableTypes(style)` 的三次查询本来就该合并。

2. **缓存的是"原始快照"，不是"过滤后的候选池"。**
   `splitWeightItems` 每次请求新建 map 返回，因为 `balancers.Lottery` 会**直接 delete 传入的 map**（`balancers.go:59`）。如果缓存了候选池 map，一次失败请求就会把缓存里的候选永久删掉。这是必须避开的陷阱，写在这里防止后来者"顺手优化一下"。

3. **`assocs` / `providers` 只读。**
   缓存返回的 slice 不能就地修改，任何需要改的场景都要先 copy。

### 5.4 失效清单（穷尽，漏一个就是 30 秒的诡异行为）

| 触发点 | 文件 |
|---|---|
| `CreateModelProvider` / `UpdateModelProvider` / `DeleteModelProvider` / `UpdateModelProviderStatus` | `handler/api.go` |
| `CreateModel` / `UpdateModel` / `DeleteModel` | `handler/api.go` |
| `CreateProvider` / `UpdateProvider` / `DeleteProvider` | `handler/api.go` |
| 自愈/自动填充写入关联 | `service/selfheal.go`、`handler/modelmeta.go` |
| 启动迁移 | `models/init.go`（缓存本来为空，可不处理） |

统一入口：

```go
func InvalidatePoolCache(modelName string)      // 精确失效
func InvalidatePoolCacheAll()                   // 关联/供应商变更时全清
```

**关联和供应商的变更一律 `InvalidatePoolCacheAll()`**：一次 provider 改动能影响 N 个模型，反查"哪些模型的池里有它"的成本高于全清。全清只是让接下来几个请求各多 3 次 SQL，可接受。

**并发击穿**：缓存未命中时用 `singleflight` 合并同 key 的并发回源。没有它，模型刚生效的瞬间 100 个并发请求会同时打 3 条 SQL。

### 5.5 绝对不能缓存的东西

**不能用缓存保存「这条关联现在是否可用」这个布尔值。**

时间窗是随时间推进而变化的：缓存 30 秒的判定结果，意味着 `18:00` 关窗后最多 30 秒内请求仍会打到它，以及 `09:00` 开窗后最多 30 秒内该上游拿不到流量。前者违反配置意图，后者造成"配了没生效"的错觉。

**缓存位图，每次请求用当前时刻做位测试**——位测试本身是纳秒级，没有任何缓存必要。

---

## 6. 前端设计

### 6.1 入口：关联列表行上的一个图标

`webui/src/routes/model-providers/association-list.tsx` 每行加一个 `Clock` 图标按钮，旁边带状态徽标：

| 状态 | 徽标 | 含义 |
|---|---|---|
| `Availability == null` | 灰 `24×7` | 未配置，随时可用 |
| `enabled=false` | 灰 `已暂停` | 有配置但关闭 |
| 有配置且可算 | 绿 `受限` / 橙 `当前不可用` | 按配置时区算「此刻」 |

徽标只做提示，**不阻止保存**——把它做成报错会让"我就是想先配好明天才生效"变得做不到。

**"当前不可用"的时区问题**：浏览器本地时区可能与 `peak_calendar.Timezone` 不同。徽标旁 tooltip 必须写明 `按 Asia/Shanghai 计算`，否则跨时区办公时这个橙色标记会看起来像 bug。

### 6.2 弹窗结构（`webui/src/routes/model-providers/availability-dialog.tsx`）

```
┌─ 可用时间 ───────────────────────────────────────┐
│  ○ 不限时间（24 × 7）                             │
│  ● 仅下列时段可用          ○ 下列时段禁用          │
│                                                  │
│  [工作时间] [仅工作日] [全天] [清空] [反转]         │
│                                                  │
│  时区 Asia/Shanghai（跟随峰谷日历）                │
│                                                  │
│  周一  ▓▓░░▓▓▓▓▓▓▓▓▓▓░░░░░░░░░░░░░░░░            │
│  周二  ▓▓░░▓▓▓▓▓▓▓▓▓▓░░░░░░░░░░░░░░░░            │
│  周三  ▓▓░░▓▓▓▓▓▓▓▓▓▓░░░░░░░░░░░░░░░░            │
│  周四  ▓▓░░▓▓▓▓▓▓▓▓▓▓░░░░░░░░░░░░░░░░            │
│  周五  ▓▓░░▓▓▓▓▓▓▓▓▓▓░░░░░░░░░░░░░░░░            │
│  周六  ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░            │
│  周日  ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░            │
│        0   2   4   6   8  10  12  14  16  18  20  22 24 │
│                                                  │
│  当前：✅ 可用 · 下次变更 今天 18:00               │
│                        [取消]  [确定]             │
└──────────────────────────────────────────────────┘
```

**尺寸**：48 列 × 12px = 576px，加上左侧 48px 星期标签与内边距，弹窗用 `max-w-2xl`（672px）。不需要更宽。

### 6.3 交互细节（这几条决定了它好不好用）

1. **点击切换单槽；按住拖动连续刷选。**
   拖动方向由第一格决定：第一格从「开」变「关」则整段刷成「关」，反之整段刷成「开」。中途反向拖动不改变模式（避免手抖导致半段开半段关）。

2. **整行/整列快捷操作。**
   点星期标签 → 整行全开/全关；点小时刻度 → 整列全开/全关。这两个是 90% 的实际操作。

3. **预设按钮**（覆盖真实需求的 95%）：
   - 「工作时间」= 周一~周五 09:00-18:00
   - 「仅工作日」= 周一~周五 全天
   - 「全天」= 7×48 全开
   - 「清空」= 全关
   - 「反转」= 逐槽取反（配合「工作时间」一键得到"非工作时间禁用"）

4. **实时"当前状态"行。** 用与后端**完全一致的算法**在前端算（`lib/availability.ts` 复用同一份槽位定义与 `allow/deny` 逻辑），并给出"下次变更时间"。这行是用户唯一能立刻验证"我配对了吗"的反馈，比任何说明文字都有效。

5. **跨零点不提供特殊支持。** 用户想要 22:00-02:00，就在两行各点一段。界面上不做"跨天区间"概念，与后端校验一致。

6. **移动端降级**：网格外层 `overflow-x-auto`，格子最小宽度 8px（48 × 8 = 384px）。不做"按天折叠+区间列表"的第二套 UI——两套 UI 意味着两套校验和两套 bug。

### 6.4 草稿与保存

弹窗内的改动**只写父表单的草稿 state**，点「确定」回写 `useModelProviderForm` 持有的 `availability` state，最终由主表单的「保存」统一提交。

理由：如果弹窗自带保存按钮，就会出现"弹窗已保存、主表单还有未保存的其它字段"这种撕裂状态，用户按下主表单的「取消」后，可用时间的新值却已经落库了。这类问题排查起来极其难受。

（这与 `PeakTermsEditor` 的策略一致——它也是嵌在表单里、跟随主表单保存。）

### 6.5 为什么这一块必须做成独立弹窗

`model-provider-form-dialog.tsx` 已经 598 行，`PeakTermsEditor` 又占了 512 行。再往主表单里塞一个 7×48 网格，主表单会变成 1200 行且无法在一屏内看清——用户在配置权重、价格、extra_body 的时候会被一张大表格打断注意力。

弹窗化的额外收益：网格组件可以独立测试（点、拖、行操作、反转），不需要拉起整个表单。

---

## 7. API 契约

### 7.1 关联的读写（扩展现有端点，不新增）

`GET /api/model-providers` 响应中每条关联新增：

```json
{
  "ID": 12,
  "availability": {
    "enabled": true,
    "mode": "allow",
    "slots": [
      { "day": 1, "start": "09:00", "end": "18:00" },
      { "day": 2, "start": "09:00", "end": "18:00" }
    ]
  }
}
```

`POST /api/model-providers`、`PUT /api/model-providers/:id` 请求体接受同名 `availability` 字段；`null` 或省略 = 清空（后端显式写 NULL，见 §4.5）。

**批量整体提交**：不需要"逐槽"的增量接口。7×48 的完整表格也就几百字节，整表提交最简单，也避免了"两个标签页同时改"的合并问题。

### 7.2 新增一个预览端点（可选但建议）

```
GET /api/model-providers/:id/availability/preview?days=7
```

返回未来 N 天「可用/不可用」的连续区间，形状与 `peak_calendar/preview` 的 `SchedulePoint` 一致。

**建议做**，理由：后端算的"下次开窗时间"和前端徽标算的"当前可用"是两个独立实现，预览端点提供了一个用后端口径校验前端的通道。但这属于自检工具，P1 可做。

---

## 8. 改动清单

### 后端

| 文件 | 改动 |
|---|---|
| `models/availability.go` | **新增**：`AvailabilitySlot` / `AvailabilityTerms` / `AvailabilityMask` / `Compile` / `SlotOf` |
| `models/model.go` | `ModelWithProvider` 加 `Availability` 字段 |
| `service/availability.go` | **新增**：`AvailabilityResolver` / `AvailableAt` / `filterByAvailability` / `nextWindow` |
| `service/chat.go` | `ProvidersWithMetaBymodelsName` 加 `at time.Time` 参数；插入可用性过滤；错误信息区分「时间窗全关」 |
| `service/pool_cache.go` | **新增**（若采纳方案 B）：关联池缓存 + `singleflight` + 失效入口 |
| `handler/api.go` | 读写 `availability`；`validateAvailability`；`nil` 清空补丁；各 CRUD 后调用失效 |
| `handler/models.go` 或 `handler/availability.go` | **新增**：预览端点（可选） |

### 前端

| 文件 | 改动 |
|---|---|
| `webui/src/lib/availability.ts` | **新增**：类型、`slotsToGrid` / `gridToSlots`、预设、校验、`isAvailableAt`、`nextChange` |
| `webui/src/lib/api.ts` | 关联类型加 `availability` |
| `webui/src/routes/model-providers/availability-dialog.tsx` | **新增**：弹窗 + 7×48 网格 |
| `webui/src/routes/model-providers/association-list.tsx` | 行内入口 + 状态徽标 |
| `webui/src/routes/model-providers/use-model-provider-form.ts` | 持有 `availability` 草稿、回填、提交 |
| `webui/src/i18n/locales/*/availability.json` | **新增**：文案（至少 zh-CN / en） |

### 测试

| 类型 | 覆盖点 |
|---|---|
| Go 单测 | `Compile` 边界（0:00 / 23:30 / 24:00 / 非法输入）、`AvailableAt` 的 allow/deny 真值表、跨零点拒绝 |
| Go 单测 | `filterByAvailability` 在"主池清空→备用接管"路径上（§4.2） |
| Go 单测 | 全不可用时错误信息包含下次开窗时间 |
| Go 单测 | `nil` 条款零开销路径（不调 `Compile`） |
| Go 单测 | 缓存失效：改关联后下一次请求拿到新值 |
| 前端单测 | 网格点/拖/行操作/反转 → `slots` 往返一致（`gridToSlots(slotsToGrid(x)) == x`） |
| 前端单测 | 预设按钮产出的 slots 精确值 |

---

## 9. 待裁决的决策点

| # | 问题 | 选项 A | 选项 B | 我的推荐 |
|---|---|---|---|---|
| 1 | 空池时是否"兜底放行" | 报错拒绝请求 | 忽略时间窗照常路由 | **A**。兜底放行会让配置形同虚设，且掩盖"窗口配错了"这类真实故障 |
| 2 | 空池时返回什么 HTTP 状态 | 沿用内部错误 | 503 + `Retry-After` = 距下次开窗秒数 | **B**。`Retry-After` 让客户端能优雅重试，成本几乎为零 |
| 3 | 是否现在做关联池缓存（§5.2 B） | 做（连同可用性一起） | 只缓存编译位图，池缓存另开任务 | **B**。两个改动混在一起时，出问题时无法区分是哪边引起的 |
| 4 | 是否支持节假日/调休 | 不支持（按星期就够） | `skipHolidays` 叠加 `DateOverrides` | **A**（P2）。但要注意"周末全关"在调休周六会导致整个模型不可用 |
| 5 | 跨零点区间 | 拒绝，要求拆两天 | 允许，按起始日归属 | **A**。网格 UI 天然不产生跨零点 |
| 6 | `Mode` 之外是否还要"模型级总开关" | 不要 | 模型行加 `Availability` 与关联行取与 | **A**。现在没有这个需求，YAGNI |
| 7 | 时间窗与 `Status` 是否合并 | 保持独立 | 时间窗全部关闭时自动置 `Status=false` | **A**。自动改用户字段是最糟糕的一类副作用 |

---

## 10. 分期

| 期 | 内容 | 可独立上线 |
|---|---|---|
| P0 | `models/availability.go` + `service/availability.go` + 过滤接入 + handler 读写校验 + 单测 | 是（API 可配，UI 暂缺） |
| P1 | 前端弹窗 + 网格 + 状态徽标 + 提交链路 | 是 |
| P2 | 关联池缓存（§5.2 B）+ 失效清单 | 是（纯性能改动） |
| P3 | 预览端点、`Retry-After`、节假日支持 | 是 |

---

## 11. 一个附带发现

`handler/api.go` 里同一个"GORM 跳过 nil 指针导致清不空列"的补丁已经出现两次（`context_length`、`peak`），本次要加第三次。建议在 P0 里顺手抽成一个函数：

```go
// clearNullableColumn 在结构体 Updates 之后显式写一次 NULL。
//
// 存在的理由：gorm 的结构体更新跳过零值，而"清空这一列"在这些字段上
// 是合法操作（nil 有业务含义）。每加一个可空业务列就要记得补一次，
// 忘了的表现是"删了配置下次又回来了"，且不报错。
func clearNullableColumn[T any](ctx context.Context, id uint, column string) error
```

不抽出的话，这个坑迟早会被踩第四次。

---

## 12. 实现记录（落地时与设计的偏差）

以下是按本文实现时发现设计写错、或与代码现状不符的地方，**以本节为准**。

### 12.1 `deny` 模式允许空时段列表（§4.4 的规则被收紧）

原文写"`enabled=true` 且 `slots` 为空 → 拒绝"。落地时发现这条在 `deny` 模式下是错的：

- `allow` + 空列表 = 任何时刻都不可用，这确实几乎必然是"忘了画格子"，该拦；
- `deny` + 空列表 = **不禁用任何时段**，就是 24×7，是完全合法且明确的语义（用户可能先建好黑名单、还没画格子）。

把它一并拒掉等于替用户发明规则。最终实现只对 `allow` 拒绝（`handler/availability.go`），并有测试固定这个差异。

### 12.2 前端只暴露两态，`enabled` 不进 UI

设计里说的"三态"（未配置 / 配了但暂停 / 生效）在后端字段层成立，但前端不表达中间那个：

- `null` → 不限时间（落库 NULL）
- 有值 → 生效，提交时恒带 `enabled: true`

理由：`enabled=false` 与 `null` 在运行期完全等价，多一个开关只会让用户问"不限时间和暂停有什么区别"。代价是——通过 API 设成 `enabled=false` 的存量配置，在界面打开后再保存会变成 `null`（表格丢了，行为不变）。见 `webui/src/lib/availability.ts` 文件头。

### 12.3 `webui/src/routes/model-providers/association-list.tsx` 是死代码

设计假定关联列表渲染在 `AssociationList` 组件里。实际**该组件没有任何调用点**（`grep AssociationList` 只有定义处），真正渲染表格的是 `model-providers.tsx` 里内联的一份实现（含"主/备用"角色列）。

所以"可用时间"列与徽标加在 `model-providers.tsx` 的内联表格上，`association-list.tsx` 未改动。这两份实现的重复是既有问题，不在本次范围内——但下次改关联列表时**必须两份都改，或者先合并掉**。

### 12.4 缓存（§5.2 方案 B）未做

按决策点 #3 的结论，关联池缓存不与本功能同批。当前状态：

- 可用性判定产生的额外 SQL：**0 条**（字段挂在关联行上，那行本来就每请求必读）；
- 位图在 `newAvailabilityGate` 里每次请求编译一次，成本是几条字符串 split，未缓存；
- `ProvidersWithMetaBymodelsName` 仍然每请求 3 次 SQL，与改动前一致。

### 12.5 测试基线

`webui/src/routes/model-providers.test.tsx` 有 **33 个用例在本次改动之前就是失败的**（`getByRole("button", { name: "编辑" })` 找不到——测试环境下 i18n 未把 aria-label 渲染出来）。已用 `git stash` 验证基线同样失败，本次改动没有新增失败用例。修这个是另一件事。

