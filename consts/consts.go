package consts

type Style = string

const (
	StyleOpenAI    Style = "openai"
	StyleOpenAIRes Style = "openai-res"
	StyleAnthropic Style = "anthropic"
	StyleGemini    Style = "gemini"
)

// ProviderCategory 是供应商分类，仅用于标记渠道归属（当前用于决定是否展示用量）。
// 与 Style（上游协议）相互独立：分类不参与请求构建，改分类不会影响接口协议与路由。
type ProviderCategory = string

const (
	ProviderCategoryOther       ProviderCategory = "other"
	ProviderCategoryOpenCode    ProviderCategory = "opencode"
	ProviderCategoryCommandCode ProviderCategory = "commandcode"
)

const (
	// 按权重概率抽取，类似抽签。
	BalancerLottery = "lottery"
	// 按顺序循环轮转，每次降低权重后移到队尾
	BalancerRotor = "rotor"
	// 默认策略
	BalancerDefault = BalancerLottery
)

const (
	KeyPrefix = "sk-llmio-"
	KeyLength = 32
)
