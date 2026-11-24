package handler

import (
	"errors"
	"strconv"

	"github.com/atopos31/llmio/common"
	"github.com/atopos31/llmio/models"
	"github.com/atopos31/llmio/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// GetModelProviderEfforts 探测某个提供商下的某个模型支持哪些思考档位。
//
// 给关联表单的下拉框填选项用：配「强制思考等级」时不该让用户对着一个写死的
// 清单猜哪个值这家上游认——能问上游就问，问不到（原生 OpenAI / Anthropic /
// Gemini 都不报这个）再退回静态清单。
//
// 永远返回 200：探测失败不是错误，是"我们不知道"。前端据此决定用哪份清单，
// 一条 4xx 只会让它去写一套重复的兜底逻辑。
func GetModelProviderEfforts(c *gin.Context) {
	providerIDStr := c.Query("provider_id")
	providerModel := c.Query("provider_model")
	if providerIDStr == "" || providerModel == "" {
		common.BadRequest(c, "provider_id and provider_model query parameters are required")
		return
	}

	providerID, err := strconv.ParseUint(providerIDStr, 10, 64)
	if err != nil {
		common.BadRequest(c, "Invalid provider_id format")
		return
	}

	ctx := c.Request.Context()
	provider, err := gorm.G[models.Provider](models.DB).Where("id = ?", providerID).First(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.NotFound(c, "Provider not found")
			return
		}
		common.InternalServerError(c, "Failed to retrieve provider: "+err.Error())
		return
	}

	efforts, probed := service.SupportedEfforts(ctx, provider, providerModel)

	common.Success(c, gin.H{
		"efforts": efforts,
		// probed=false 时 efforts 就是兜底清单。把它显式告诉前端，界面才好说清楚
		// "这是默认清单"而不是假装从上游问到了。
		"probed": probed,
	})
}
