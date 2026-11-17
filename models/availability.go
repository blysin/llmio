package models

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 可用时间窗（availability window）的最小粒度：半小时。
//
// 选半小时而不是 15 分钟/1 小时：1 小时太粗，表达不了"午休 12:00-13:30 关掉"；
// 15 分钟让 7×48 的网格变成 7×96，格子在弹窗里小到点不中。
const (
	SlotsPerDay = 48
	SlotMinutes = 30
)

// 可用性模式。
const (
	// AvailabilityModeAllow 白名单：**只有**列表内的槽可用。
	AvailabilityModeAllow = "allow"
	// AvailabilityModeDeny 黑名单：**只有**列表内的槽不可用。
	AvailabilityModeDeny = "deny"
)

// AvailabilitySlot 一个连续可用/禁用区间。
//
// 半小时为最小粒度，Start/End 必须落在 :00 或 :30 上，End 允许 "24:00"。
//
// **不支持跨零点**（Start 必须严格小于 End）。22:00-02:00 请拆成
// 「周一 22:00-24:00」+「周二 00:00-02:00」两段。
//
// 为什么不像 PeakPeriod 那样支持跨零点：跨零点后"这段算周几"没有唯一答案。
// PeakPeriod 用"按起始日归属"糊了过去，代价是周一 22:00-02:00 与
// 周二 22:00-02:00 是两条不同的配置、含义还得靠注释解释。
// 可用性判定直接决定请求被拒绝还是被放行，不该建立在这种隐含约定上。
// 网格界面天然不会产生跨零点，用户感知不到这条限制。
type AvailabilitySlot struct {
	Day   int    `json:"day"`   // 0=周日 … 6=周六（与 time.Weekday、PeakPeriod.Days 一致）
	Start string `json:"start"` // "HH:MM"
	End   string `json:"end"`   // "HH:MM" 或 "24:00"
}

// AvailabilityTerms 某条「模型 × 上游」关联的可用时间条款。
//
// nil（列值为 NULL）表示不限时间、24×7 可用。这条路径**零运行时开销**：
// 热路径判到 nil 直接放行，不编译、不解析（与 PeakTerms 的 nil 一致）。
//
// Enabled 与 nil 的区别：nil 是"没有这份配置"，Enabled=false 是"有配置、暂时关闭"。
// 保留这个区别是为了让用户能"这个月先全开"而不丢掉已经画好的表格。
type AvailabilityTerms struct {
	Enabled bool               `json:"enabled"`
	Mode    string             `json:"mode"`
	Slots   []AvailabilitySlot `json:"slots"`
}

// AvailabilityMask 一周的槽位位图：Mask[day] 的第 i 位表示当天第 i 个半小时槽。
//
// 库里的区间列表是给人看的，位图是给热路径用的。48 位刚好塞进 uint64，
// 一次位测试就能判完一个时刻。
type AvailabilityMask [7]uint64

// AvailableAt 判定某时刻是否落在开启的槽内。
//
// 入参必须是**已转换到配置时区**的本地时间：星期几与小时都按它取。
// 调用方应使用 PeakCalendar.EffectiveTime 或 PeakCalendar.Location 换算，
// 不要在 UTC 上直接调用——那会让整个窗口错位到另一个时区。
//
// 注意：它只回答"位图里这位是不是 1"，模式（allow/deny）的取反由调用方负责。
// 把取反放在这里会让 nil 条款（不限时间）也走进来，而那条路径应该是零开销的。
func (m AvailabilityMask) AvailableAt(local time.Time) bool {
	return m[local.Weekday()]&(1<<uint(SlotOf(local))) != 0
}

// SlotOf 返回某时刻对应的槽位序号（0..47）。
func SlotOf(local time.Time) int {
	return local.Hour()*2 + local.Minute()/SlotMinutes
}

// Compile 把区间列表编译为位图。
//
// 非法条目**跳过**而非报错：写入前的 handler 校验已经拦过一遍，能落到这里的
// 脏数据只可能来自手工改库或旧版本遗留。热路径不该因为一行配置写错就整体停摆
// ——这条与 service.ResolvePeriod 对坏时段的处理保持一致（跳过该段，继续）。
func (t AvailabilityTerms) Compile() AvailabilityMask {
	var m AvailabilityMask
	for _, s := range t.Slots {
		if s.Day < 0 || s.Day > 6 {
			continue
		}
		start, err := slotClockMinutes(s.Start)
		if err != nil {
			continue
		}
		end, err := slotClockMinutes(s.End)
		if err != nil {
			continue
		}
		// 未对齐 30 分钟的条目整条跳过，而不是向下取整：
		// 取整会把 09:15-18:15 静默放大成 09:00-18:00，等于替用户
		// 多开半小时的门，而这半小时的流量是没人批准的。
		if start%SlotMinutes != 0 || end%SlotMinutes != 0 {
			continue
		}
		if start >= end {
			continue // 跨零点或零长度，校验层已拒绝
		}
		for slot := start / SlotMinutes; slot < end/SlotMinutes; slot++ {
			m[s.Day] |= 1 << uint(slot)
		}
	}
	return m
}

// Clone 返回一份深拷贝，供需要修改的调用方使用（缓存/表单回填）。
func (t *AvailabilityTerms) Clone() *AvailabilityTerms {
	if t == nil {
		return nil
	}
	out := *t
	out.Slots = append([]AvailabilitySlot(nil), t.Slots...)
	return &out
}

// slotClockMinutes 解析 "HH:MM" 为一天中的分钟数，接受 "24:00"。
//
// 与 service.ParseClock 逻辑一致，但**不能**直接复用它：models 被 service 依赖，
// 反向导入会形成循环。两处实现都受 TestSlotClockMinutes / TestParseClock 覆盖，
// 任何一边改了另一边的行为都必须同步。
func slotClockMinutes(s string) (int, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid clock %q: expected HH:MM", s)
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, fmt.Errorf("invalid hour in %q", s)
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, fmt.Errorf("invalid minute in %q", s)
	}
	if h < 0 || h > 24 || m < 0 || m > 59 {
		return 0, fmt.Errorf("clock out of range: %q", s)
	}
	if h == 24 && m != 0 {
		return 0, fmt.Errorf("24:%02d is not a valid clock", m)
	}
	return h*60 + m, nil
}
