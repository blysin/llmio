package models

import (
	"testing"
	"time"
)

// atWeekday 构造「某个星期几的 HH:MM」，基准日 2026-11-01 是周日。
func atWeekday(t *testing.T, wd time.Weekday, hour, min int) time.Time {
	t.Helper()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if base.Weekday() != time.Sunday {
		t.Fatalf("基准日不再是周日，测试前提被破坏: %v", base.Weekday())
	}
	return time.Date(2026, 11, 1+int(wd), hour, min, 0, 0, time.UTC)
}

func TestSlotOf(t *testing.T) {
	cases := []struct {
		hour, min int
		want      int
	}{
		{0, 0, 0},
		{0, 29, 0}, // 同一槽内
		{0, 30, 1},
		{9, 0, 18},
		{9, 30, 19},
		{23, 30, 47},
		{23, 59, 47},
	}
	for _, c := range cases {
		got := SlotOf(time.Date(2026, 11, 2, c.hour, c.min, 0, 0, time.UTC))
		if got != c.want {
			t.Errorf("SlotOf(%02d:%02d) = %d, want %d", c.hour, c.min, got, c.want)
		}
	}
}

func TestAvailabilityCompileAndQuery(t *testing.T) {
	terms := AvailabilityTerms{
		Enabled: true,
		Mode:    AvailabilityModeAllow,
		Slots: []AvailabilitySlot{
			{Day: 1, Start: "09:00", End: "18:00"},
		},
	}
	mask := terms.Compile()

	cases := []struct {
		wd   time.Weekday
		hour int
		min  int
		want bool
	}{
		{time.Monday, 8, 59, false}, // 左边界外
		{time.Monday, 9, 0, true},   // 左闭
		{time.Monday, 9, 30, true},
		{time.Monday, 17, 30, true}, // 最后一个槽
		{time.Monday, 18, 0, false}, // 右开
		{time.Sunday, 12, 0, false}, // 没配周日
		{time.Tuesday, 12, 0, false},
	}
	for _, c := range cases {
		at := atWeekday(t, c.wd, c.hour, c.min)
		if got := mask.AvailableAt(at); got != c.want {
			t.Errorf("%v %02d:%02d 命中 = %v, want %v", c.wd, c.hour, c.min, got, c.want)
		}
	}
}

func TestAvailabilityCompileFullDay(t *testing.T) {
	// "00:00"-"24:00" 必须覆盖全部 48 个槽，包括 23:30 这一格。
	terms := AvailabilityTerms{Slots: []AvailabilitySlot{{Day: 3, Start: "00:00", End: "24:00"}}}
	mask := terms.Compile()
	// 只有 48 位有效（一天 48 个槽），高 16 位必须保持 0。
	if want := (uint64(1) << SlotsPerDay) - 1; mask[3] != want {
		t.Errorf("全天应为低 48 位全 1，got %b", mask[3])
	}
	if !mask.AvailableAt(atWeekday(t, time.Wednesday, 23, 30)) {
		t.Error("23:30 应命中全天窗口")
	}
}

func TestAvailabilityCompileSkipsDirtyRows(t *testing.T) {
	// 脏数据整条跳过，而不是向下取整把窗口偷偷放大或缩小。
	cases := map[string]AvailabilitySlot{
		"未对齐 30 分钟": {Day: 1, Start: "09:15", End: "18:00"},
		"跨零点":       {Day: 1, Start: "22:00", End: "02:00"},
		"零长度":       {Day: 1, Start: "09:00", End: "09:00"},
		"day 越界":    {Day: 7, Start: "09:00", End: "10:00"},
		"时间写坏":      {Day: 1, Start: "9am", End: "18:00"},
	}
	for name, slot := range cases {
		mask := AvailabilityTerms{Slots: []AvailabilitySlot{slot}}.Compile()
		var total uint64
		for _, w := range mask {
			total |= w
		}
		if total != 0 {
			t.Errorf("%s: 应被整条跳过，却产生了位图 %b", name, total)
		}
	}
}

func TestAvailabilityCompileMultipleSlotsSameDay(t *testing.T) {
	terms := AvailabilityTerms{Slots: []AvailabilitySlot{
		{Day: 1, Start: "09:00", End: "12:00"},
		{Day: 1, Start: "14:00", End: "18:00"},
	}}
	mask := terms.Compile()
	if mask.AvailableAt(atWeekday(t, time.Monday, 13, 0)) {
		t.Error("午休 12:00-14:00 不应命中")
	}
	if mask.AvailableAt(atWeekday(t, time.Monday, 12, 0)) {
		t.Error("12:00 是左段的右开边界，不应命中")
	}
	if !mask.AvailableAt(atWeekday(t, time.Monday, 11, 30)) {
		t.Error("11:30 应命中左段最后一个槽")
	}
}

func TestAvailabilityTermsCloneIsDeep(t *testing.T) {
	src := &AvailabilityTerms{Enabled: true, Slots: []AvailabilitySlot{{Day: 1, Start: "09:00", End: "18:00"}}}
	dst := src.Clone()
	dst.Slots[0].Start = "10:00"
	if src.Slots[0].Start != "09:00" {
		t.Error("Clone 必须是深拷贝，改副本不应影响原件")
	}
	if (*AvailabilityTerms)(nil).Clone() != nil {
		t.Error("nil 的 Clone 应返回 nil")
	}
}
