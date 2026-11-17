package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/atopos31/llmio/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 可用时间窗的写入链路：请求体里的 availability 要原样落到
// model_with_providers.availability，删配置时要能真的清成 NULL。

func TestCreateModelProviderTransmitsAvailability(t *testing.T) {
	setupWritePathDB(t)

	w := doJSON(t, CreateModelProvider, http.MethodPost, "/api/model-providers", map[string]any{
		"model_id":      1,
		"provider_id":   1,
		"provider_name": "gpt-4o",
		"weight":        1,
		"availability": map[string]any{
			"enabled": true,
			"mode":    "allow",
			"slots": []map[string]any{
				{"day": 1, "start": "09:00", "end": "18:00"},
				{"day": 1, "start": "20:00", "end": "22:30"},
			},
		},
	}, nil)

	if code := envelopeCode(t, w); code != http.StatusOK {
		t.Fatalf("业务码 = %d，期望 200（原文 %s）", code, w.Body.String())
	}

	row, err := gorm.G[models.ModelWithProvider](models.DB).Where("provider_model = ?", "gpt-4o").First(context.Background())
	if err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.Availability == nil {
		t.Fatal("availability 未落库：调度侧会把它当成 24×7 可用")
	}
	if !row.Availability.Enabled || row.Availability.Mode != models.AvailabilityModeAllow {
		t.Errorf("enabled/mode 不符: %+v", row.Availability)
	}
	if len(row.Availability.Slots) != 2 {
		t.Fatalf("slots 条数 = %d，期望 2", len(row.Availability.Slots))
	}
	if got := row.Availability.Slots[1]; got.Day != 1 || got.Start != "20:00" || got.End != "22:30" {
		t.Errorf("第二段不符: %+v", got)
	}
}

func TestUpdateModelProviderCanClearAvailability(t *testing.T) {
	setupWritePathDB(t)

	existing := models.ModelWithProvider{
		ModelID: 1, ProviderID: 1, ProviderModel: "gpt-4o",
		Availability: &models.AvailabilityTerms{
			Enabled: true, Mode: models.AvailabilityModeAllow,
			Slots: []models.AvailabilitySlot{{Day: 1, Start: "09:00", End: "18:00"}},
		},
	}
	existing.ID = 9
	if err := models.DB.Create(&existing).Error; err != nil {
		t.Fatalf("seed row: %v", err)
	}

	// 不传 availability 键 = 清空。这条路径必须真的写 NULL：
	// 结构体 Updates 会跳过 nil 指针，不清的话旧表格下次打开又回来了。
	w := doJSON(t, UpdateModelProvider, http.MethodPut, "/api/model-providers/9", map[string]any{
		"model_id":      1,
		"provider_id":   1,
		"provider_name": "gpt-4o",
		"weight":        1,
	}, gin.Params{{Key: "id", Value: "9"}})

	if code := envelopeCode(t, w); code != http.StatusOK {
		t.Fatalf("业务码 = %d，期望 200（原文 %s）", code, w.Body.String())
	}

	row, err := gorm.G[models.ModelWithProvider](models.DB).Where("id = ?", 9).First(context.Background())
	if err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.Availability != nil {
		t.Errorf("availability 应为 nil，got %+v", row.Availability)
	}
}

func TestCreateModelProviderRejectsInvalidAvailability(t *testing.T) {
	setupWritePathDB(t)

	w := doJSON(t, CreateModelProvider, http.MethodPost, "/api/model-providers", map[string]any{
		"model_id":      1,
		"provider_id":   1,
		"provider_name": "gpt-4o",
		"weight":        1,
		"availability": map[string]any{
			"enabled": true,
			"mode":    "allow",
			// 跨零点：必须被拒绝并提示拆成两段，而不是静默保存一个
			// 编译期会被跳过的空窗口（那会让这条关联直接不可用）
			"slots": []map[string]any{{"day": 1, "start": "22:00", "end": "02:00"}},
		},
	}, nil)

	if code := envelopeCode(t, w); code != http.StatusBadRequest {
		t.Fatalf("业务码 = %d，期望 400（原文 %s）", code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "wrap midnight") {
		t.Errorf("错误信息应点明跨零点，原文 %s", w.Body.String())
	}
}

func TestValidateAvailability(t *testing.T) {
	slot := func(day int, start, end string) models.AvailabilitySlot {
		return models.AvailabilitySlot{Day: day, Start: start, End: end}
	}

	cases := []struct {
		name    string
		terms   *models.AvailabilityTerms
		wantErr string // 空表示应当通过
	}{
		{"nil 通过", nil, ""},
		{"正常白名单", &models.AvailabilityTerms{Enabled: true, Mode: "allow", Slots: []models.AvailabilitySlot{slot(1, "09:00", "18:00")}}, ""},
		{"正常黑名单", &models.AvailabilityTerms{Enabled: true, Mode: "deny", Slots: []models.AvailabilitySlot{slot(0, "00:00", "06:00")}}, ""},
		{"24:00 合法", &models.AvailabilityTerms{Enabled: true, Mode: "allow", Slots: []models.AvailabilitySlot{slot(2, "00:00", "24:00")}}, ""},
		{"禁用且空表合法", &models.AvailabilityTerms{Enabled: false, Mode: "allow"}, ""},
		{"allow 启用但空表", &models.AvailabilityTerms{Enabled: true, Mode: "allow"}, "has no slots"},
		// deny 模式空表 = 不禁用任何时段 = 24×7，是合法语义，不该跟 allow 一样被拒
		{"deny 启用但空表合法", &models.AvailabilityTerms{Enabled: true, Mode: "deny"}, ""},
		{"非法模式", &models.AvailabilityTerms{Enabled: true, Mode: "only-monday", Slots: []models.AvailabilitySlot{slot(1, "09:00", "18:00")}}, "invalid availability mode"},
		{"day 越界", &models.AvailabilityTerms{Enabled: true, Mode: "allow", Slots: []models.AvailabilitySlot{slot(7, "09:00", "18:00")}}, "invalid availability day"},
		{"day 为负", &models.AvailabilityTerms{Enabled: true, Mode: "allow", Slots: []models.AvailabilitySlot{slot(-1, "09:00", "18:00")}}, "invalid availability day"},
		{"未对齐", &models.AvailabilityTerms{Enabled: true, Mode: "allow", Slots: []models.AvailabilitySlot{slot(1, "09:15", "18:00")}}, "align to 30 minutes"},
		{"跨零点", &models.AvailabilityTerms{Enabled: true, Mode: "allow", Slots: []models.AvailabilitySlot{slot(1, "22:00", "02:00")}}, "wrap midnight"},
		{"零长度", &models.AvailabilityTerms{Enabled: true, Mode: "allow", Slots: []models.AvailabilitySlot{slot(1, "09:00", "09:00")}}, "wrap midnight"},
		{"重叠", &models.AvailabilityTerms{Enabled: true, Mode: "allow", Slots: []models.AvailabilitySlot{
			slot(1, "09:00", "12:00"), slot(1, "11:00", "13:00"),
		}}, "overlapping"},
		{"相邻不算重叠", &models.AvailabilityTerms{Enabled: true, Mode: "allow", Slots: []models.AvailabilitySlot{
			slot(1, "09:00", "12:00"), slot(1, "12:00", "13:00"),
		}}, ""},
		{"不同天同时段合法", &models.AvailabilityTerms{Enabled: true, Mode: "allow", Slots: []models.AvailabilitySlot{
			slot(1, "09:00", "12:00"), slot(2, "09:00", "12:00"),
		}}, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateAvailability(c.terms)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过，got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望报错含 %q，实际通过", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("错误信息应含 %q，got %q", c.wantErr, err.Error())
			}
		})
	}
}

func TestValidateAvailabilityNormalizesEmptyMode(t *testing.T) {
	terms := &models.AvailabilityTerms{
		Enabled: true,
		Slots:   []models.AvailabilitySlot{{Day: 1, Start: "09:00", End: "18:00"}},
	}
	if err := validateAvailability(terms); err != nil {
		t.Fatalf("空 mode 应被归一而不是报错: %v", err)
	}
	if terms.Mode != models.AvailabilityModeAllow {
		t.Errorf("空 mode 应归一为 allow，got %q", terms.Mode)
	}
}
