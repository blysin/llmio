package service

import (
	"context"
	"fmt"
	"time"

	"github.com/atopos31/llmio/models"
)

// ---------------------------------------------------------------------------
// 可用时间判定
// ---------------------------------------------------------------------------
//
// 设计取舍见 docs/model-availability-window-design.md。
//
// 三条不能违反的规则，改代码前先读一遍：
//
//  1. **判定结果不能缓存。** 缓存的只能是编译后的位图。缓存"此刻是否可用"
//     会让关窗后最多一个 TTL 内请求继续打到该上游，也会让开窗后一段时间内
//     该上游拿不到流量——前者违反配置意图，后者制造"配了没生效"的错觉。
//  2. **过滤要发生在主/备用拆分之前。** 否则主池被时间窗清空时，备用池里
//     同样在窗口外的关联仍会被选中，时间窗等于摆设。
//  3. **全不可用必须报错，不许兜底放行。** 静默忽略时间窗会让配置形同虚设，
//     并且掩盖"窗口配错了"这类真实故障。

// availabilityRule 是一条关联的条款编译结果。
//
// mask 与 deny 预编译一次，热路径上只剩一次位测试。
type availabilityRule struct {
	mask models.AvailabilityMask
	deny bool
}

// availabilityGate 承载一次请求内的可用性判定。
//
// 只装**真正配了且启用**的关联：没配的 ID 不在 rules 里，查不到即视为
// 恒可用，从而让"完全没用这个功能"的部署走零开销路径。
type availabilityGate struct {
	rules map[uint]availabilityRule
	loc   *time.Location
}

// newAvailabilityGate 从关联列表与全局日历构造判定器。
func newAvailabilityGate(assocs []models.ModelWithProvider, cal models.PeakCalendar) *availabilityGate {
	rules := make(map[uint]availabilityRule, len(assocs))
	for _, a := range assocs {
		t := a.Availability
		if t == nil || !t.Enabled {
			continue
		}
		rules[a.ID] = availabilityRule{
			mask: t.Compile(),
			// 空 mode 按 allow 处理：写入前 handler 会归一，这里的兜底
			// 只针对手工改库或旧数据，取"最保守的白名单"而非"全放行"。
			deny: t.Mode == models.AvailabilityModeDeny,
		}
	}
	return &availabilityGate{rules: rules, loc: cal.Location()}
}

// available 判定某条关联在某时刻是否可选。
func (g *availabilityGate) available(id uint, t time.Time) bool {
	rule, ok := g.rules[id]
	if !ok {
		return true
	}
	on := rule.mask.AvailableAt(t.In(g.loc))
	// allow：命中即可用；deny：命中即禁用。整个功能的核心就是这一行取反。
	return on != rule.deny
}

// firstOpen 从 at 起找首个「至少有一条关联可选」的半小时槽起点。
//
// 只扫 7×48 = 336 个槽：一周还没有可用槽，说明配置是全关的（allow 模式空表
// 或 deny 模式铺满），再往后扫也没有意义。
//
// 只在错误路径上调用，不在热路径上算。
func (g *availabilityGate) firstOpen(assocs []models.ModelWithProvider, at time.Time) (time.Time, bool) {
	local := at.In(g.loc)
	base := time.Date(local.Year(), local.Month(), local.Day(),
		local.Hour(), local.Minute()/models.SlotMinutes*models.SlotMinutes, 0, 0, g.loc)

	step := time.Duration(models.SlotMinutes) * time.Minute
	for i := 0; i < 7*models.SlotsPerDay; i++ {
		t := base.Add(time.Duration(i) * step)
		for _, a := range assocs {
			if g.available(a.ID, t) {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// filterByAvailability 剔除此刻不在可用窗口内的关联。
//
// 返回保留的关联、被剔除的条数，以及首个有候选可用的时刻（没有则为零值）。
// 被剔除的条数大于 0 时调用方才需要关心第三个返回值——它只用于构造错误信息。
func filterByAvailability(assocs []models.ModelWithProvider, at time.Time, cal models.PeakCalendar) ([]models.ModelWithProvider, int, time.Time) {
	gate := newAvailabilityGate(assocs, cal)
	if len(gate.rules) == 0 {
		// 这个模型下没有任何一条关联配置了时间窗：原样返回，不分配新切片。
		return assocs, 0, time.Time{}
	}

	kept := make([]models.ModelWithProvider, 0, len(assocs))
	for _, a := range assocs {
		if gate.available(a.ID, at) {
			kept = append(kept, a)
		}
	}
	if len(kept) == len(assocs) {
		return kept, 0, time.Time{}
	}

	outside := len(assocs) - len(kept)
	// 只有全被剔除时才值得算下次开窗：它唯一的用途是构造 NoAvailabilityError，
	// 而那个错误只在 kept 为空时产生。还有候选可用时白扫 336 个槽没有意义。
	if len(kept) == 0 {
		next, _ := gate.firstOpen(assocs, at)
		return kept, outside, next
	}
	return kept, outside, time.Time{}
}

// ---------------------------------------------------------------------------
// 错误语义
// ---------------------------------------------------------------------------

// NoAvailabilityError 报告某模型此刻没有任何上游处于可用窗口内。
//
// 单独成型而不是拼一句字串，是因为 handler 需要据此返回 503 + Retry-After：
// 这是**可预期的、会自行恢复的**状态，跟"关联被删光了"这种配置错误不是一类。
// 前者让客户端等一会儿重试，后者应该惊动运维。
type NoAvailabilityError struct {
	Model string
	// Outside 窗口外的关联条数，Total 为参与判定的关联总数。
	Outside int
	Total   int
	// At 判定所用的请求发起时刻。
	At time.Time
	// NextOpen 首个有候选可用的槽起点。零值表示一周内都没有可用槽。
	NextOpen time.Time
}

// RetryAfter 返回距下次开窗的时长，供 HTTP Retry-After 使用。
// 没有下次开窗时返回 0，调用方据此决定是否发送该响应头。
func (e *NoAvailabilityError) RetryAfter() time.Duration {
	if e.NextOpen.IsZero() {
		return 0
	}
	d := e.NextOpen.Sub(e.At)
	if d < 0 {
		return 0
	}
	return d
}

func (e *NoAvailabilityError) Error() string {
	if e.NextOpen.IsZero() {
		return fmt.Sprintf(
			"no provider available at this time for model %s: all %d providers are outside their availability window, and no window opens within the next 7 days",
			e.Model, e.Total)
	}
	return fmt.Sprintf(
		"no provider available at this time for model %s: %d of %d providers are outside their availability window; next window opens %s (in %s)",
		e.Model, e.Outside, e.Total,
		e.NextOpen.Format("Mon 15:04"), formatShortDuration(e.RetryAfter()))
}

// formatShortDuration 把时长格式化成 "12h13m" / "47m" / "<1m"。
//
// 不用 Duration.String()：它对整分钟会输出 "12h13m0s"，多出来的秒位在
// 一条错误提示里只是噪音。
func formatShortDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int(d%time.Hour) / int(time.Minute)
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// ---------------------------------------------------------------------------
// 预览
// ---------------------------------------------------------------------------

// AvailabilitySpan 时间轴上的一段可用/不可用区间。
type AvailabilitySpan struct {
	Start     int64 `json:"start"` // Unix 毫秒
	End       int64 `json:"end"`   // Unix 毫秒
	Available bool  `json:"available"`
}

// PreviewAvailability 从 start 起按半小时步进采样 days 天，把相邻同结果的
// 槽合并为连续区间。
//
// 合并而非逐槽返回：7 天是 336 条，前端画不出来也读不懂；合并后通常只剩
// 十几段，跨零点拆成两行造成的错位一眼可见。
func PreviewAvailability(terms *models.AvailabilityTerms, cal models.PeakCalendar, start time.Time, days int) []AvailabilitySpan {
	if days < 1 {
		days = 1
	}
	gate := newAvailabilityGate([]models.ModelWithProvider{{Availability: terms}}, cal)
	loc := cal.Location()
	local := start.In(loc)
	base := time.Date(local.Year(), local.Month(), local.Day(),
		local.Hour(), local.Minute()/models.SlotMinutes*models.SlotMinutes, 0, 0, loc)

	step := time.Duration(models.SlotMinutes) * time.Minute
	total := days * models.SlotsPerDay

	out := make([]AvailabilitySpan, 0, 16)
	var cur *AvailabilitySpan
	for i := 0; i < total; i++ {
		t := base.Add(time.Duration(i) * step)
		ok := gate.available(0, t)
		ms := t.UnixMilli()
		if cur != nil && cur.Available == ok {
			cur.End = ms + step.Milliseconds()
			continue
		}
		if cur != nil {
			out = append(out, *cur)
		}
		cur = &AvailabilitySpan{Start: ms, End: ms + step.Milliseconds(), Available: ok}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

// AvailabilityNow 报告某条条款此刻是否可用，供管理界面的状态徽标使用。
//
// 与热路径共用同一个判定，避免界面显示"可用"、请求却被拒这种撕裂。
func AvailabilityNow(ctx context.Context, terms *models.AvailabilityTerms) (bool, time.Time) {
	if terms == nil || !terms.Enabled {
		return true, time.Time{}
	}
	cal := GetPeakCalendar(ctx)
	gate := newAvailabilityGate([]models.ModelWithProvider{{Availability: terms}}, cal)
	now := time.Now()
	if gate.available(0, now) {
		return true, time.Time{}
	}
	next, _ := gate.firstOpen([]models.ModelWithProvider{{Availability: terms}}, now)
	return false, next
}
