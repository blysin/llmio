package handler

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/atopos31/llmio/models"
	"github.com/atopos31/llmio/service"
	"gorm.io/gorm"
)

// validateAvailability 校验可用时间条款，顺带把空 mode 归一为 allow。
//
// 为什么校验放在这里而不是 models.AvailabilityTerms.Compile 里报错：
// Compile 在热路径上调用，一条写坏的时段不该让所有请求失败（跳过该段即可）；
// 而写入这一刻是**唯一**能把它拦住、并说清 "第几天的哪一段错了" 的地方。
// 等第一个请求撞上再排查，只剩一句 "no provider available"，谁也看不出根因。
func validateAvailability(a *models.AvailabilityTerms) error {
	if a == nil {
		return nil
	}

	// 空 mode 归一而不是报错：老客户端/手写 JSON 不传这个字段是常态，
	// 按 allow（最保守的"只开列表里的"）处理比拒掉整个请求更合理。
	if a.Mode == "" {
		a.Mode = models.AvailabilityModeAllow
	}
	if a.Mode != models.AvailabilityModeAllow && a.Mode != models.AvailabilityModeDeny {
		return fmt.Errorf("invalid availability mode %q: expected %q or %q",
			a.Mode, models.AvailabilityModeAllow, models.AvailabilityModeDeny)
	}

	// allow 模式下"启用却一格没画" = 整个模型彻底不可用。这几乎必然是
	// "忘了画格子"，拦一次比让它默默生效好。真要全关，应该去关 Status。
	//
	// deny 模式不受这条约束：空表意味着"不禁用任何时段"，是明确且合法的
	// 24×7 语义，跟没配完全等价。把它也拒掉等于替用户发明了一条不存在的规则。
	if a.Enabled && len(a.Slots) == 0 && a.Mode == models.AvailabilityModeAllow {
		return errors.New("availability has no slots: mark at least one allowed slot, or switch to \"unlimited\"")
	}

	type span struct{ start, end int }
	byDay := make(map[int][]span, 7)

	for _, s := range a.Slots {
		if s.Day < 0 || s.Day > 6 {
			return fmt.Errorf("invalid availability day: %d (expected 0=Sunday .. 6=Saturday)", s.Day)
		}
		start, err := service.ParseClock(s.Start)
		if err != nil {
			return fmt.Errorf("invalid availability start %q: %w", s.Start, err)
		}
		end, err := service.ParseClock(s.End)
		if err != nil {
			return fmt.Errorf("invalid availability end %q: %w", s.End, err)
		}
		if start%models.SlotMinutes != 0 || end%models.SlotMinutes != 0 {
			return fmt.Errorf("availability time must align to %d minutes: %s-%s",
				models.SlotMinutes, s.Start, s.End)
		}
		if start >= end {
			return fmt.Errorf("availability slot must not wrap midnight: %s-%s "+
				"(split it into two slots, e.g. 22:00-24:00 and next day 00:00-02:00)", s.Start, s.End)
		}
		byDay[s.Day] = append(byDay[s.Day], span{start: start, end: end})
	}

	// 重叠必须拒绝：网格界面不可能画出重叠，出现了就说明是 API 直调或手工改库。
	// 允许重叠在 allow 模式下无害、在 deny 模式下也无害，但"合法但没人想得到"
	// 的输入越少，将来改动这块的人要操心的情况就越少。
	for day, spans := range byDay {
		slices.SortFunc(spans, func(a, b span) int { return a.start - b.start })
		for i := 1; i < len(spans); i++ {
			if spans[i].start < spans[i-1].end {
				return fmt.Errorf("overlapping availability slots on day %d", day)
			}
		}
	}
	return nil
}

// clearNullableColumn 在结构体 Updates 之后把某一列显式写成 NULL。
//
// 存在的理由：gorm 的结构体更新**跳过零值**，而这些列上 nil 是有业务含义的
// 合法取值（"没配"）。每加一个可空业务列就要记得补一次，忘了的表现是
// "删了配置下次又回来了"——不报错、不打日志、只在用户眼里像个幽灵。
//
// context_length、peak、availability 已经踩过三次，别再复制第四遍。
func clearNullableColumn[T any](ctx context.Context, id uint, column string) error {
	if _, err := gorm.G[T](models.DB).Where("id = ?", id).Update(ctx, column, nil); err != nil {
		return fmt.Errorf("clear %s: %w", column, err)
	}
	return nil
}
