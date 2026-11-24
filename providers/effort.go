package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// EffortProber 是上游可选实现的能力：报出这个模型支持哪些思考档位。
//
// 刻意不挂进 Provider 接口。挂进去等于让 Anthropic / Gemini 也实现一个只会返回
// "我不知道"的方法——那是把"不支持"伪装成"实现了"，调用方还得再用返回值区分。
// 用类型断言表达"这家会不会说"，语义更诚实：
//
//	if p, ok := provider.(providers.EffortProber); ok { ... }
//
// 目前只有 OpenAI 系的两家实现（它们共用 /models 的响应形状）。能不能读到取决于
// 具体网关：OpenRouter 一族会在 reasoning.supported_efforts 里明说，原生 OpenAI
// 一个字段都不给——后者返回空切片、不报错，"这家不说"不是失败。
type EffortProber interface {
	SupportedEfforts(ctx context.Context, model string) ([]string, error)
}

// effortCanonicalRank 是档位的展示顺序：从"关掉思考"到"想得最多"。
//
// 上游返回的顺序不可靠（实测同一份清单里 max 排第一），而界面上选项顺序乱跳
// 比缺一档更让人迷惑。不在表里的值排最后并按字典序，免得某个网关的新档位
// 混进中间。
var effortCanonicalRank = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

// normalizeEfforts 去空、去重、转小写并按档位强弱排序。
func normalizeEfforts(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, iok := effortCanonicalRank[out[i]]
		rj, jok := effortCanonicalRank[out[j]]
		switch {
		case iok && jok:
			return ri < rj
		case iok:
			return true // 认识的值排在陌生值前面
		case jok:
			return false
		default:
			return out[i] < out[j]
		}
	})
	return out
}

// effortModelList 只解 /models 里与思考档位相关的那两层。
//
// 不复用 Models() 的 Model 结构：那个结构是 /models 端点的对外契约
// （只有 id/object/created/owned_by），往里加字段会让所有适配器的返回一起变胖，
// 而这里只是顺路看一眼能力，不该反过来定义公共结构。
type effortModelList struct {
	Data []struct {
		ID        string `json:"id"`
		Reasoning *struct {
			SupportedEfforts []string `json:"supported_efforts"`
		} `json:"reasoning"`
	} `json:"data"`
}

// fetchSupportedEfforts 拉一次 OpenAI 兼容的 /models，取出指定模型的档位清单。
//
// 找不到这个模型、或它没带 reasoning.supported_efforts，都返回空切片 + nil：
// 调用方据此退化成静态清单。只有网络/状态码/解析问题才算真错误（同样由调用方
// 退化成静态清单，但要留日志）。
func fetchSupportedEfforts(ctx context.Context, baseURL, apiKey, proxy, model string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/models", strings.TrimRight(baseURL, "/")), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	res, err := GetClient(30*time.Second, proxy).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code: %d", res.StatusCode)
	}

	// body 上限：/models 常态几百 KB（OpenRouter 实测 910 KB），8 MiB 已留足
	// 余量。GetClient 的 Timeout 是 0、ctx 只兜慢流不兜大小，异常上游回一份
	// 几百 MB 的 body 不该把管理端点的内存吃穿。截断后 JSON 解不完整会走
	// 解析错误 → 调用方退兜底清单，语义正确。
	var list effortModelList
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&list); err != nil {
		return nil, err
	}

	for _, m := range list.Data {
		if m.ID == model && m.Reasoning != nil {
			return normalizeEfforts(m.Reasoning.SupportedEfforts), nil
		}
	}

	// 精确匹配没中，再试一次"网关加了前缀"的形态：OpenRouter 列的是
	// anthropic/claude-sonnet-4.5，而用户填 provider_model 时常常只写后半截。
	// 只接受**唯一**的那个后缀匹配——两条都对上就放弃，猜错了比不猜更糟。
	var (
		hits int
		vals []string
	)
	for _, m := range list.Data {
		if m.Reasoning == nil || !strings.HasSuffix(m.ID, "/"+model) {
			continue
		}
		hits++
		vals = append(vals, m.Reasoning.SupportedEfforts...)
	}
	if hits == 1 {
		return normalizeEfforts(vals), nil
	}
	return nil, nil
}

// SupportedEfforts 实现 EffortProber（chat completions 与 Responses API 共用
// 同一个 /models 响应形状，因此两家的实现完全一样）。
func (o *OpenAI) SupportedEfforts(ctx context.Context, model string) ([]string, error) {
	return fetchSupportedEfforts(ctx, o.BaseURL, o.APIKey, o.Proxy, model)
}

// SupportedEfforts 见 OpenAI 版本。
func (o *OpenAIRes) SupportedEfforts(ctx context.Context, model string) ([]string, error) {
	return fetchSupportedEfforts(ctx, o.BaseURL, o.APIKey, o.Proxy, model)
}
