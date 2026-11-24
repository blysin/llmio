package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/atopos31/llmio/models"
	"github.com/atopos31/llmio/providers"
)

// FallbackEfforts 是探测不出档位时的兜底清单。
//
// 值域照抄 OpenRouter 的 supported_efforts（实测 239/469 个模型会带这个字段，
// 全集就是 none/minimal/low/medium/high/xhigh/max）。这里去掉 minimal 是有意的：
// 支持它的上游是少数（43/469），放进兜底清单等于把一个大多数模型会拒收的档位
// 摆在最显眼的位置——探到了它会自己出现在真实清单里，没探到就不该假装有。
var FallbackEfforts = []string{"none", "low", "medium", "high", "xhigh", "max"}

const (
	// effortProbeTTL 探测结果的缓存时长。上游的 /models 可能很大（OpenRouter
	// 一份 910 KB / 469 个模型），而用户改一个关联会反复打开这个对话框——
	// 不缓存就是每次开框拉一次近 1 MB 的 JSON。
	effortProbeTTL = 10 * time.Minute
	// effortProbeTimeout 单次探测的超时。这个调用是给界面填下拉框用的，
	// 卡住 30 秒（providers 里 GetClient 的默认值）等于对话框开不出来。
	effortProbeTimeout = 8 * time.Second
	// effortCacheLimit 缓存条数上限。模型名是自由输入的，逐字符探测会留下
	// 一堆中间态键；超了整表清空，代价只是下一次重新探测。
	effortCacheLimit = 256
)

type effortEntry struct {
	efforts []string
	probed  bool
	expiry  time.Time
}

var (
	effortMu    sync.Mutex
	effortCache = map[string]effortEntry{}
)

// SupportedEfforts 返回「这个提供商下的这个模型」支持哪些思考档位。
//
// probed=false 表示**没探到**——上游不支持这个能力（Anthropic / Gemini 的
// /models 里没有这个概念）、上游报错、或者模型没带 supported_efforts。
// 这三种在界面上是同一件事：退回 FallbackEfforts。区分它们只对日志有意义。
//
// 缓存键带上 provider.UpdatedAt：改了 base_url 或换了 key 之后，上一家上游的
// 能力清单不该继续生效，而那正是用户最可能来重开这个对话框的时刻。
func SupportedEfforts(ctx context.Context, provider models.Provider, providerModel string) ([]string, bool) {
	if providerModel == "" {
		return FallbackEfforts, false
	}

	key := fmt.Sprintf("%d|%d|%s", provider.ID, provider.UpdatedAt.Unix(), providerModel)
	if got, ok := effortCacheGet(key); ok {
		return got.efforts, got.probed
	}

	prober, ok := newEffortProber(provider)
	if !ok {
		// 这家协议没有这个能力，连网络都不用打
		effortCachePut(key, effortEntry{efforts: FallbackEfforts, probed: false})
		return FallbackEfforts, false
	}

	probeCtx, cancel := context.WithTimeout(ctx, effortProbeTimeout)
	defer cancel()

	efforts, err := prober.SupportedEfforts(probeCtx, providerModel)
	if err != nil {
		// 探测失败不影响配置：界面照常用兜底清单，用户该配什么配什么
		slog.Warn("probe reasoning efforts failed",
			"provider", provider.Name, "model", providerModel, "error", err)
		effortCachePut(key, effortEntry{efforts: FallbackEfforts, probed: false})
		return FallbackEfforts, false
	}
	if len(efforts) == 0 {
		// "这家不说"也是稳定结论，一并缓存——否则原生 OpenAI 的每个模型每次
		// 开框都要白拉一次 /models
		effortCachePut(key, effortEntry{efforts: FallbackEfforts, probed: false})
		return FallbackEfforts, false
	}

	effortCachePut(key, effortEntry{efforts: efforts, probed: true})
	return efforts, true
}

// newEffortProber 按 provider 的协议配置造出一个探测者。
//
// 造不出来（配置损坏、协议不支持）返回 false，由调用方走兜底——这里不该
// 把错误往上抛：下拉框填什么值，不值得让整个对话框失败。
func newEffortProber(provider models.Provider) (providers.EffortProber, bool) {
	instance, err := providers.New(provider.Type, provider.Config, provider.Proxy)
	if err != nil {
		return nil, false
	}
	prober, ok := instance.(providers.EffortProber)
	return prober, ok
}

func effortCacheGet(key string) (effortEntry, bool) {
	effortMu.Lock()
	defer effortMu.Unlock()
	entry, ok := effortCache[key]
	if !ok {
		return effortEntry{}, false
	}
	if time.Now().After(entry.expiry) {
		delete(effortCache, key)
		return effortEntry{}, false
	}
	return entry, true
}

func effortCachePut(key string, entry effortEntry) {
	effortMu.Lock()
	defer effortMu.Unlock()
	if len(effortCache) >= effortCacheLimit {
		effortCache = map[string]effortEntry{}
	}
	entry.expiry = time.Now().Add(effortProbeTTL)
	effortCache[key] = entry
}
