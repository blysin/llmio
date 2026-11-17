package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/atopos31/llmio/balancers"
	"github.com/atopos31/llmio/models"
)

func availTerms(mode string, slots ...models.AvailabilitySlot) *models.AvailabilityTerms {
	return &models.AvailabilityTerms{Enabled: true, Mode: mode, Slots: slots}
}

// assocWithAvailability 构造带 ID 的关联。ID 来自内嵌的 gorm.Model，
// 不能被复合字面量直接初始化，所以单独赋值。
func assocWithAvailability(id uint, terms *models.AvailabilityTerms) models.ModelWithProvider {
	a := models.ModelWithProvider{Availability: terms}
	a.ID = id
	return a
}

// 2026-11-02 是周一；用配置时区（东八区）构造，避免 UTC 与 Asia/Shanghai 差 8 小时。
func shanghai(t *testing.T) (*time.Location, models.PeakCalendar) {
	t.Helper()
	cal := models.DefaultPeakCalendar()
	loc := cal.Location()
	if loc.String() != "Asia/Shanghai" {
		t.Fatalf("默认日历时区应为 Asia/Shanghai，got %s", loc)
	}
	return loc, cal
}

func TestFilterByAvailabilityKeepsUnconfigured(t *testing.T) {
	loc, cal := shanghai(t)
	at := time.Date(2026, 11, 2, 20, 0, 0, 0, loc) // 周一 20:00

	// 两条必须用不同 ID：gate 以关联 ID 为键，同 ID 会让未配置的那条
	// 误用另一条的规则（真实库里主键唯一，但这个前提值得在测试里显式体现）。
	assocs := []models.ModelWithProvider{
		assocWithAvailability(1, availTerms(models.AvailabilityModeAllow,
			models.AvailabilitySlot{Day: 1, Start: "09:00", End: "18:00"})),
		assocWithAvailability(2, nil), // 没配 → 恒可用
	}

	kept, outside, next := filterByAvailability(assocs, at, cal)
	if len(kept) != 1 || kept[0].ID != 2 {
		t.Fatalf("应只保留未配置的那条(ID=2)，got %d 条", len(kept))
	}
	if outside != 1 {
		t.Errorf("outside = %d, want 1", outside)
	}
	if !next.IsZero() {
		t.Errorf("仍有候选可用时不该给出下次开窗时间，got %v", next)
	}
}

func TestFilterByAvailabilityDenyMode(t *testing.T) {
	loc, cal := shanghai(t)
	// 周一夜里 22:00-24:00 维护
	terms := availTerms(models.AvailabilityModeDeny,
		models.AvailabilitySlot{Day: 1, Start: "22:00", End: "24:00"})

	outsideWindow := time.Date(2026, 11, 2, 23, 0, 0, 0, loc)
	insideWindow := time.Date(2026, 11, 2, 15, 0, 0, 0, loc)

	kept, outside, _ := filterByAvailability([]models.ModelWithProvider{assocWithAvailability(1, terms)}, outsideWindow, cal)
	if len(kept) != 0 || outside != 1 {
		t.Errorf("deny 命中应被剔除，kept=%d outside=%d", len(kept), outside)
	}
	kept, outside, _ = filterByAvailability([]models.ModelWithProvider{assocWithAvailability(1, terms)}, insideWindow, cal)
	if len(kept) != 1 || outside != 0 {
		t.Errorf("deny 未命中应保留，kept=%d outside=%d", len(kept), outside)
	}
}

func TestFilterByAvailabilityAllOutsideReportsNextOpen(t *testing.T) {
	loc, cal := shanghai(t)
	at := time.Date(2026, 11, 2, 20, 0, 0, 0, loc) // 周一 20:00

	assocs := []models.ModelWithProvider{
		assocWithAvailability(1, availTerms(models.AvailabilityModeAllow,
			models.AvailabilitySlot{Day: 1, Start: "09:00", End: "18:00"})),
		assocWithAvailability(2, availTerms(models.AvailabilityModeAllow,
			models.AvailabilitySlot{Day: 1, Start: "10:00", End: "19:00"})),
	}

	kept, outside, next := filterByAvailability(assocs, at, cal)
	if len(kept) != 0 {
		t.Fatalf("全部过窗应被清空，got %d 条", len(kept))
	}
	if outside != 2 {
		t.Errorf("outside = %d, want 2", outside)
	}
	// 周一到周日只有周一配了窗口，所以下次开窗是下周一 09:00。
	if next.Weekday() != time.Monday || next.Hour() != 9 || next.Minute() != 0 {
		t.Errorf("下次开窗应为下周一 09:00，got %v", next)
	}
	if next.Sub(at) != 157*time.Hour {
		t.Errorf("距下次开窗应为 157h，got %v", next.Sub(at))
	}
}

func TestNoAvailabilityErrorMessages(t *testing.T) {
	loc, _ := shanghai(t)
	at := time.Date(2026, 11, 2, 20, 0, 0, 0, loc)

	e := &NoAvailabilityError{
		Model: "gpt-4o", Outside: 3, Total: 3, At: at,
		NextOpen: time.Date(2026, 11, 3, 9, 0, 0, 0, loc),
	}
	msg := e.Error()
	for _, want := range []string{"no provider available at this time", "gpt-4o", "3 of 3", "next window opens", "13h00m"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息缺少 %q: %s", want, msg)
		}
	}
	if e.RetryAfter() != 13*time.Hour {
		t.Errorf("RetryAfter = %v, want 13h", e.RetryAfter())
	}

	// 一周都没有可用槽：不能撒谎说"下次几点"。
	never := &NoAvailabilityError{Model: "m", Outside: 1, Total: 1, At: at}
	if !strings.Contains(never.Error(), "no window opens within the next 7 days") {
		t.Errorf("全关时错误信息应说明一周内无窗口: %s", never.Error())
	}
	if never.RetryAfter() != 0 {
		t.Errorf("全关时 RetryAfter 应为 0，got %v", never.RetryAfter())
	}
}

func TestAvailabilityDisabledTermsIgnored(t *testing.T) {
	loc, cal := shanghai(t)
	at := time.Date(2026, 11, 2, 20, 0, 0, 0, loc)
	terms := availTerms(models.AvailabilityModeAllow,
		models.AvailabilitySlot{Day: 1, Start: "09:00", End: "18:00"})
	terms.Enabled = false

	kept, outside, _ := filterByAvailability([]models.ModelWithProvider{assocWithAvailability(1, terms)}, at, cal)
	if len(kept) != 1 || outside != 0 {
		t.Errorf("条款禁用时应恒可用，kept=%d outside=%d", len(kept), outside)
	}
}

// 时间窗清空主池后，BalanceChat 靠 balancer.Empty() 决定是否降级到备用池。
// 这个契约一旦破了，备用关联永远不会被时间窗"唤醒"，而不会有任何报错。
func TestEmptyPoolAfterAvailabilityTriggersBackupContract(t *testing.T) {
	if !balancers.NewLottery(map[uint]int{}).Empty() {
		t.Fatal("空权重池必须让 Empty() 返回 true，否则主池被时间窗清空后备用不会接管")
	}
	if !balancers.NewRotor(map[uint]int{}).Empty() {
		t.Fatal("Rotor 空池同样必须返回 true")
	}
}

func TestDenyModeWithEmptySlotsIsAlwaysAvailable(t *testing.T) {
	loc, cal := shanghai(t)
	// deny + 空表 = 不禁用任何时段 = 24×7。与"没配"行为一致，
	// 但它是一条真实存在的配置（用户可能先建好用黑名单、还没画格子）。
	terms := availTerms(models.AvailabilityModeDeny)
	at := time.Date(2026, 11, 2, 3, 0, 0, 0, loc)

	kept, outside, _ := filterByAvailability([]models.ModelWithProvider{assocWithAvailability(1, terms)}, at, cal)
	if len(kept) != 1 || outside != 0 {
		t.Errorf("deny 空表应恒可用，kept=%d outside=%d", len(kept), outside)
	}
}

func TestPreviewAvailabilityMergesAdjacentSlots(t *testing.T) {
	loc, cal := shanghai(t)
	terms := availTerms(models.AvailabilityModeAllow,
		models.AvailabilitySlot{Day: 1, Start: "09:00", End: "18:00"})

	start := time.Date(2026, 11, 2, 0, 0, 0, 0, loc)
	spans := PreviewAvailability(terms, cal, start, 1)

	if len(spans) != 3 {
		t.Fatalf("一天应合并成 3 段（关-开-关），got %d 段: %+v", len(spans), spans)
	}
	if spans[0].Available || spans[1].Available != true || spans[2].Available {
		t.Errorf("合并结果顺序应为 false/true/false，got %+v", spans)
	}
	if spans[1].End-spans[1].Start != 9*60*60*1000 {
		t.Errorf("中间段应为 9 小时，got %d ms", spans[1].End-spans[1].Start)
	}
}

func TestAvailabilityNowWithNilTerms(t *testing.T) {
	ok, next := AvailabilityNow(context.Background(), nil)
	if !ok || !next.IsZero() {
		t.Errorf("nil 条款应恒可用且无下次开窗时间，got ok=%v next=%v", ok, next)
	}
}
