package quota

import (
	"fmt"
	"strings"
	"time"
)

// opencode.ai（opencode zen / go 套餐）余量 —— API Key 直连版。
//
// 接口：GET {base}/usage   Authorization: Bearer <API Key>
//
// 与 quota/opencode.go 的分工是「同一个套餐、两条鉴权路」：
//   - 那边读控制台会话（Cookie __Host-console_session + x-org-id），
//     拿到的是 access.meters 里的 microCents 金额，要手工换算成 USD；
//   - 这边读 API Key，拿到的是每个窗口已经用掉的百分比，无需换算。
//
// 少一道「登录控制台再复制 Cookie」的手续，代价是只有百分比：
// 金额明细仍然只在登录型那边有。
//
// 响应结构（与 providers/usage.go 的 fetchOpenCodeUsage 同一份，
// 该形状已在供应商管理页对真上游跑通）：
//
//	{ usage: {
//	    rolling: { status, percent, resetsAt },
//	    weekly:  { status, percent, resetsAt },
//	    monthly: { status, percent, resetsAt } } }
//
// percent 是**已用**百分比，0-100；status 实测只有 ok / exceeded 两种。
//
// base 是 OpenAI 兼容入口的根（**含** /zen/go/v1 这一段），用量路径由它直接拼出：
// https://opencode.ai/zen/go/v1/usage。只填到 https://opencode.ai 会 404。
//
// 为什么这个源值得做成内置（它不像 scnet 那样非 Go 不可）：一次 fetch 加三个
// 窗口的摊平，脚本类型完全写得下。做成内置是为了省掉那份样板——用户只要贴
// Key，不必再配 itemsPath 与逐字段映射；也让它在下拉里与登录型并排，
// 「同一个套餐、两种鉴权」这件事在界面上就是一眼能看见的。

const (
	opencodeKeyDefaultPath    = "/usage"
	opencodeKeyDefaultTimeout = 20 * time.Second
)

// opencodeKeyWindowMeta 三档窗口的展示元信息。顺序固定 5h → 周 → 月。
//
// 键名沿用上游的 rolling / weekly / monthly。rolling 就是 5 小时滚动窗口
// （契约的 windowAliases 里也按这个口径认它），与登录型适配器的 fiveHour
// 同一个窗口，因此标签保持一致，两种数据源摊在同一张卡片上读起来不别扭。
var opencodeKeyWindowMeta = []struct {
	Key    string
	Label  string
	Window Window
}{
	{"rolling", "5 小时限额", Window5h},
	{"weekly", "每周限额", WindowWeek},
	{"monthly", "每月限额", WindowMonth},
}

// runOpencodeKey 执行 opencode（API Key）取数。
func runOpencodeKey(cfg BuiltinConfig, base string) (*HTTPAdapterResult, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("缺少 API Key：opencode 的用量接口用 Bearer 鉴权，" +
			"请把该渠道的 API Key 填在这个数据源的 apiKey（与供应商管理里那条 opencode 渠道同一个 key）")
	}

	timeout := opencodeKeyDefaultTimeout
	if cfg.TimeoutMs > 0 {
		timeout = time.Duration(cfg.TimeoutMs) * time.Millisecond
	}
	if timeout < time.Second {
		timeout = time.Second
	}

	path := cfg.Path
	if path == "" {
		path = opencodeKeyDefaultPath
	}

	data, err := fetchJSON(joinURL(base, path, cfg.Query), apiKey, cfg.Headers, timeout, nil)
	if err != nil {
		return nil, fmt.Errorf("取用量失败: %w%s", err, opencodeKeyStatusHint(err.Error()))
	}

	items, err := opencodeKeyBuildItems(data)
	if err != nil {
		return nil, err
	}
	return &HTTPAdapterResult{Items: items, Raw: data}, nil
}

// opencodeKeyBuildItems 把 usage 的三个窗口转成契约行。
//
// 只认 percent：它归一进契约后会被补成 used/total=100/remaining，
// 于是这一条与「上游给了金额」的行同构，进度条、阈值告警、最差摘要都照常工作。
// 缺 percent 的窗口直接跳过——上游对没用到的窗口给 null，那是「没有这一档」，
// 不是「用量为 0」，硬凑一条 0% 会把「套餐只有周限额」显示成「三档全空着」。
func opencodeKeyBuildItems(data any) ([]map[string]any, error) {
	root, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("opencode 用量响应不是对象，接口结构可能变了")
	}
	usage, _ := root["usage"].(map[string]any)
	if usage == nil {
		return nil, fmt.Errorf("响应里没有 usage（API Key 对应的套餐可能未开通，" +
			"或 base_url 少了 /zen/go/v1 这一段 → 用「试跑」看原始返回）")
	}

	out := []map[string]any{}
	for _, meta := range opencodeKeyWindowMeta {
		w, ok := usage[meta.Key].(map[string]any)
		if !ok {
			continue
		}
		percent := toFloat(w["percent"])
		if percent == nil {
			continue
		}
		row := map[string]any{
			"id":      meta.Key,
			"label":   meta.Label,
			"unit":    string(UnitPercent),
			"window":  string(meta.Window),
			"resetAt": w["resetsAt"],
			// 必须写**解引用后的 float64**：契约的 toFloat 只认数值本体，
			// 放 *float64 进去会被当成不可识别的类型，整条被丢弃。
			"percent": *percent,
		}
		if s := opencodeKeyStatus(toStr(w["status"])); s != "" {
			row["status"] = s
		}
		out = append(out, row)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("usage 里没有可识别的用量：rolling / weekly / monthly 都没有 percent")
	}
	return out, nil
}

// opencodeKeyStatus 把上游的窗口状态翻成契约状态。
//
// 只认「用尽」与「告警」，**故意不认 ok**：契约优先采用上游显式状态，
// 照搬 ok 会让一个已用 95% 的窗口恒显「正常」，把阈值告警吃掉。
func opencodeKeyStatus(raw string) string {
	if raw == "" {
		return ""
	}
	if s := NormalizeStatus(raw); s == StatusExhausted || s == StatusWarning {
		return string(s)
	}
	// 上游用 exceeded 表示「已超额」，NormalizeStatus 的词表里没有它，
	// 而这个判断比按百分比猜准（超额的口径只有上游自己清楚）
	if strings.EqualFold(raw, "exceeded") {
		return string(StatusExhausted)
	}
	return ""
}

// opencodeKeyStatusHint 按状态码给出可操作的自查方向。
//
// fetchJSON 的报错是纯文本（"HTTP %d: %s"），这里按前缀做一次分类；
// 命中不了就原样返回，不硬造提示。
func opencodeKeyStatusHint(msg string) string {
	switch {
	case strings.Contains(msg, "HTTP 401"), strings.Contains(msg, "HTTP 403"):
		return "（API Key 无效或已撤销 → 从供应商管理里那条 opencode 渠道复制 api_key 重填）"
	case strings.Contains(msg, "HTTP 404"):
		return "（路径不存在 → base_url 要含 /zen/go/v1，用量地址 = base_url + " +
			opencodeKeyDefaultPath + "；上游改路径时用该数据源的 path 覆盖）"
	case strings.Contains(msg, "HTTP 429"):
		return "（请求过于频繁 → 调大刷新间隔后重试）"
	default:
		return ""
	}
}
