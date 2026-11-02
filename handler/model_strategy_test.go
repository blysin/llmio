package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/atopos31/llmio/consts"
	"github.com/atopos31/llmio/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestCreateModelStrategyValidation(t *testing.T) {
	setupWritePathDB(t)

	t.Run("接受 session_affinity", func(t *testing.T) {
		w := doJSON(t, CreateModel, http.MethodPost, "/api/models", map[string]any{
			"name":     "session-affinity-model",
			"strategy": consts.BalancerSessionAffinity,
		}, nil)
		if code := envelopeCode(t, w); code != http.StatusOK {
			t.Fatalf("业务码 = %d, want 200（原文 %s）", code, w.Body.String())
		}

		got, err := gorm.G[models.Model](models.DB).Where("name = ?", "session-affinity-model").First(context.Background())
		if err != nil {
			t.Fatalf("回查模型失败: %v", err)
		}
		if got.Strategy != consts.BalancerSessionAffinity {
			t.Fatalf("strategy = %q, want %q", got.Strategy, consts.BalancerSessionAffinity)
		}
	})

	t.Run("空值回填默认策略", func(t *testing.T) {
		w := doJSON(t, CreateModel, http.MethodPost, "/api/models", map[string]any{
			"name": "default-model",
		}, nil)
		if code := envelopeCode(t, w); code != http.StatusOK {
			t.Fatalf("业务码 = %d, want 200（原文 %s）", code, w.Body.String())
		}

		got, err := gorm.G[models.Model](models.DB).Where("name = ?", "default-model").First(context.Background())
		if err != nil {
			t.Fatalf("回查模型失败: %v", err)
		}
		if got.Strategy != consts.BalancerDefault {
			t.Fatalf("strategy = %q, want 默认值 %q", got.Strategy, consts.BalancerDefault)
		}
	})

	t.Run("非法值被拒绝", func(t *testing.T) {
		w := doJSON(t, CreateModel, http.MethodPost, "/api/models", map[string]any{
			"name":     "bad-strategy-model",
			"strategy": "random",
		}, nil)
		if code := envelopeCode(t, w); code != http.StatusBadRequest {
			t.Fatalf("业务码 = %d, want 400（原文 %s）", code, w.Body.String())
		}

		count, err := gorm.G[models.Model](models.DB).Where("name = ?", "bad-strategy-model").Count(context.Background(), "id")
		if err != nil {
			t.Fatalf("回查模型失败: %v", err)
		}
		if count != 0 {
			t.Fatalf("非法策略不应落库，实际写入 %d 条", count)
		}
	})
}

func TestUpdateModelStrategyValidation(t *testing.T) {
	setupWritePathDB(t)
	params := gin.Params{{Key: "id", Value: "1"}}

	w := doJSON(t, UpdateModel, http.MethodPut, "/api/models/1", map[string]any{
		"name":     "gpt-4o",
		"strategy": consts.BalancerSessionAffinity,
	}, params)
	if code := envelopeCode(t, w); code != http.StatusOK {
		t.Fatalf("更新会话亲和业务码 = %d, want 200（原文 %s）", code, w.Body.String())
	}

	got, err := gorm.G[models.Model](models.DB).Where("id = ?", 1).First(context.Background())
	if err != nil {
		t.Fatalf("回查模型失败: %v", err)
	}
	if got.Strategy != consts.BalancerSessionAffinity {
		t.Fatalf("strategy = %q, want %q", got.Strategy, consts.BalancerSessionAffinity)
	}

	w = doJSON(t, UpdateModel, http.MethodPut, "/api/models/1", map[string]any{
		"name":     "gpt-4o",
		"strategy": "random",
	}, params)
	if code := envelopeCode(t, w); code != http.StatusBadRequest {
		t.Fatalf("更新非法策略业务码 = %d, want 400（原文 %s）", code, w.Body.String())
	}

	got, err = gorm.G[models.Model](models.DB).Where("id = ?", 1).First(context.Background())
	if err != nil {
		t.Fatalf("回查模型失败: %v", err)
	}
	if got.Strategy != consts.BalancerSessionAffinity {
		t.Fatalf("非法更新后 strategy = %q, want 保持 %q", got.Strategy, consts.BalancerSessionAffinity)
	}

	w = doJSON(t, UpdateModel, http.MethodPut, "/api/models/1", map[string]any{
		"name": "gpt-4o",
	}, params)
	if code := envelopeCode(t, w); code != http.StatusOK {
		t.Fatalf("更新空策略业务码 = %d, want 200（原文 %s）", code, w.Body.String())
	}

	got, err = gorm.G[models.Model](models.DB).Where("id = ?", 1).First(context.Background())
	if err != nil {
		t.Fatalf("回查模型失败: %v", err)
	}
	if got.Strategy != consts.BalancerDefault {
		t.Fatalf("空策略更新后 strategy = %q, want 默认值 %q", got.Strategy, consts.BalancerDefault)
	}
}

func TestGetModelsStrategyFilter(t *testing.T) {
	setupWritePathDB(t)

	for _, tc := range []struct {
		name     string
		strategy string
	}{
		{name: "lottery-model", strategy: consts.BalancerLottery},
		{name: "session-affinity-model", strategy: consts.BalancerSessionAffinity},
	} {
		w := doJSON(t, CreateModel, http.MethodPost, "/api/models", map[string]any{
			"name":     tc.name,
			"strategy": tc.strategy,
		}, nil)
		if code := envelopeCode(t, w); code != http.StatusOK {
			t.Fatalf("创建 %s 业务码 = %d（原文 %s）", tc.name, code, w.Body.String())
		}
	}

	w := doJSON(t, GetModels, http.MethodGet, "/api/models?strategy=session_affinity", nil, nil)
	if code := envelopeCode(t, w); code != http.StatusOK {
		t.Fatalf("筛选 session_affinity 业务码 = %d, want 200（原文 %s）", code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "session-affinity-model") {
		t.Fatalf("筛选 session_affinity 结果缺少 session-affinity-model：%s", body)
	}
	if strings.Contains(body, "lottery-model") {
		t.Fatalf("筛选 session_affinity 结果混入 lottery-model：%s", body)
	}

	w = doJSON(t, GetModels, http.MethodGet, "/api/models?strategy=random", nil, nil)
	if code := envelopeCode(t, w); code != http.StatusBadRequest {
		t.Fatalf("非法筛选业务码 = %d, want 400（原文 %s）", code, w.Body.String())
	}
}
