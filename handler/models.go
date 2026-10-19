package handler

import (
	"context"
	"errors"
	"slices"

	"github.com/atopos31/llmio/common"
	"github.com/atopos31/llmio/consts"
	"github.com/atopos31/llmio/models"
	"github.com/atopos31/llmio/providers"
	"github.com/atopos31/llmio/service"
	"github.com/gin-gonic/gin"
)

// openAIModel 是 OpenAI 兼容 GET /v1/models 里的一条模型。
//
// 刻意**不**复用 providers.Model：那个结构同时充当"解析上游 /models 响应"的
// DTO（providers/openai.go、providers/openai_res.go），还用于管理端的
// GET /api/providers/models/:id。往它上面挂上下文长度，管理员拉一次上游模型
// 列表就会跟着冒出一串 0——那是对上游的无声断言，而本意只是给客户端补个字段。
type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// ContextLength 与 ContextWindow 是同一个值的两个名字，两个都发是为了
	// 兼容不同的客户端：它们读的字段名不一致（有的认 context_length，有的认
	// context_window）。0 表示这个模型没配过，客户端按未知处理。
	ContextLength int `json:"context_length"`
	ContextWindow int `json:"context_window"`
}

type openAIModelList struct {
	Object string        `json:"object"`
	Data   []openAIModel `json:"data"`
}

func OpenAIModelsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	models, err := service.ModelsByTypes(ctx, consts.StyleOpenAI, consts.StyleOpenAIRes)
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}
	models, err = filterByAuthKey(ctx, models)
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}
	resModels := make([]openAIModel, 0, len(models))
	for _, model := range models {
		resModels = append(resModels, openAIModel{
			ID:            model.Name,
			Object:        "model",
			Created:       model.CreatedAt.Unix(),
			OwnedBy:       "llmio",
			ContextLength: model.ContextLength,
			ContextWindow: model.ContextLength,
		})
	}
	common.SuccessRaw(c, openAIModelList{
		Object: "list",
		Data:   resModels,
	})
}

func AnthropicModelsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	models, err := service.ModelsByTypes(ctx, consts.StyleAnthropic)
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}
	models, err = filterByAuthKey(ctx, models)
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}
	resModels := make([]providers.AnthropicModel, 0)
	for _, model := range models {
		resModels = append(resModels, providers.AnthropicModel{
			ID:          model.Name,
			CreatedAt:   model.CreatedAt,
			DisplayName: model.Name,
			Type:        "model",
		})
	}
	common.SuccessRaw(c, providers.AnthropicModelsResponse{
		Data:    resModels,
		HasMore: false,
	})
}

type GeminiModelsResponse struct {
	Models []GeminiModel `json:"models"`
}

type GeminiModel struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName,omitempty"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods,omitempty"`
}

func GeminiModelsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	models, err := service.ModelsByTypes(ctx, consts.StyleGemini)
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}
	models, err = filterByAuthKey(ctx, models)
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}
	resModels := make([]GeminiModel, 0, len(models))
	for _, model := range models {
		resModels = append(resModels, GeminiModel{
			Name:        "models/" + model.Name,
			DisplayName: model.Name,
			SupportedGenerationMethods: []string{
				"generateContent",
				"streamGenerateContent",
			},
		})
	}
	common.SuccessRaw(c, GeminiModelsResponse{
		Models: resModels,
	})
}

func filterByAuthKey(ctx context.Context, inModels []models.Model) ([]models.Model, error) {
	// 验证是否为允许全部模型
	allowAll, ok := ctx.Value(consts.ContextKeyAllowAllModel).(bool)
	if !ok {
		return nil, errors.New("invalid auth key")
	}
	if allowAll {
		return inModels, nil
	}

	allowedModels, ok := ctx.Value(consts.ContextKeyAllowModels).([]string)
	if !ok {
		return nil, errors.New("invalid auth key")
	}

	return slices.DeleteFunc(inModels, func(m models.Model) bool {
		return !slices.Contains(allowedModels, m.Name)
	}), nil
}
