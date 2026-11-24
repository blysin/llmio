package service

import (
	"testing"

	"github.com/atopos31/llmio/consts"
	"github.com/tidwall/gjson"
)

// applyReasoningEffort 的边界：谁被改、谁不被改、改成什么。
//
// 这里刻意不碰 BalanceChat：那是整条重试与路由链路，测它要起 balancer、假上游、
// 数据库。覆盖/不覆盖这个判断只活在这一个纯函数里，就只测它。
func TestApplyReasoningEffort(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		style    string
		effort   string
		wantPath string // 空表示这个键不该出现在结果里
		want     string
	}{
		{
			name:     "chat completions 覆盖客户端传的旧档位",
			body:     `{"model":"m","reasoning_effort":"low"}`,
			style:    consts.StyleOpenAI,
			effort:   "high",
			wantPath: "reasoning_effort",
			want:     "high",
		},
		{
			name:     "chat completions 客户端没传也补上",
			body:     `{"model":"m"}`,
			style:    consts.StyleOpenAI,
			effort:   "medium",
			wantPath: "reasoning_effort",
			want:     "medium",
		},
		{
			name:     "Responses API 写进 reasoning.effort 而不是顶层键",
			body:     `{"model":"m"}`,
			style:    consts.StyleOpenAIRes,
			effort:   "low",
			wantPath: "reasoning.effort",
			want:     "low",
		},
		{
			// 从 chat 请求改造过来的 body 常带着顶层旧键。两个键并存时宽松
			// 网关取哪个说不准（强制静默失效）、严格网关 400，必须删干净
			name:     "Responses API 清理残留的顶层 reasoning_effort",
			body:     `{"model":"m","reasoning_effort":"low"}`,
			style:    consts.StyleOpenAIRes,
			effort:   "high",
			wantPath: "reasoning.effort",
			want:     "high",
		},
		{
			// Anthropic 要的是 token 预算而不是等级，硬写一个自己的键只会换 400
			name:   "Anthropic 上游一律不碰",
			body:   `{"model":"m","thinking":{"type":"enabled","budget_tokens":2048}}`,
			style:  consts.StyleAnthropic,
			effort: "high",
		},
		{
			name:   "Gemini 上游一律不碰",
			body:   `{"model":"m"}`,
			style:  consts.StyleGemini,
			effort: "high",
		},
		{
			// 存量模型的常态：没配就不干预，连字节都不该动
			name:   "没配等级时原样返回",
			body:   `{"model":"m","reasoning_effort":"low"}`,
			style:  consts.StyleOpenAI,
			effort: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyReasoningEffort([]byte(tc.body), tc.style, tc.effort)

			if tc.wantPath == "" {
				// 不该被改：原样返回同一份字节，客户端传的档位保持不动
				if string(got) != tc.body {
					t.Fatalf("body changed unexpectedly: %s -> %s", tc.body, got)
				}
				return
			}

			if v := gjson.GetBytes(got, tc.wantPath).String(); v != tc.want {
				t.Fatalf("%s = %q, want %q (body: %s)", tc.wantPath, v, tc.want, got)
			}
			// Responses 路径注入后顶层旧键必须不存在（双写清理）
			if tc.style == consts.StyleOpenAIRes && gjson.GetBytes(got, "reasoning_effort").Exists() {
				t.Fatalf("top-level reasoning_effort should be removed: %s", got)
			}
			// 模型名等其它字段不能被顺手改掉
			if v := gjson.GetBytes(got, "model").String(); v != "m" {
				t.Fatalf("model field lost: %s", got)
			}
		})
	}
}

// 覆盖必须是覆盖：客户端传 low、模型配 high，发出去的就只能是 high。
// 单拎出来是因为它是这个功能存在的理由——若退化成"只在缺失时补"，
// 上面那条用例靠"客户端没传"也能过，看不出语义已经错了。
func TestApplyReasoningEffortOverridesClientValue(t *testing.T) {
	got := applyReasoningEffort([]byte(`{"reasoning_effort":"low"}`), consts.StyleOpenAI, "high")
	if v := gjson.GetBytes(got, "reasoning_effort").String(); v != "high" {
		t.Fatalf("reasoning_effort = %q, want high (override)", v)
	}
}

// 专属降级的判据：上游点名思考等级键（参数或值）才降级，别的错误一律不碰。
// 值错误（Unsupported value）是通用自愈短语表不认的形态，正是这条规则存在的理由。
func TestDetectEffortComplaint(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "OpenAI 值错误：模型不支持该档位",
			status: 400,
			body:   `{"error":{"message":"Unsupported value: 'reasoning_effort' does not support 'max' with this model."}}`,
			want:   true,
		},
		{
			name:   "参数名被点名拒收",
			status: 400,
			body:   `{"error":{"message":"Unknown parameter: 'reasoning_effort'."}}`,
			want:   true,
		},
		{
			name:   "Responses 的嵌套键被点名",
			status: 422,
			body:   `{"error":{"message":"Unrecognized request argument supplied: reasoning.effort"}}`,
			want:   true,
		},
		{
			name:   "别的参数报错不降级",
			status: 400,
			body:   `{"error":{"message":"Unknown parameter: 'top_k'."}}`,
		},
		{
			// 429/5xx 与参数存在性无关，措辞命中也不碰
			name:   "429 不进降级",
			status: 429,
			body:   `{"error":{"message":"Unsupported value: 'reasoning_effort'"}}`,
		},
		{
			name:   "非 JSON 报文按原文匹配",
			status: 400,
			body:   `bad request: 'reasoning_effort' is invalid`,
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectEffortComplaint(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("DetectEffortComplaint(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// 降级删键：两处都删干净、消息本体保留；没键可删时不空转。
func TestDropReasoningEffort(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantDropped bool
	}{
		{
			name:        "chat 顶层键",
			body:        `{"model":"m","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`,
			wantDropped: true,
		},
		{
			name:        "Responses 嵌套键与顶层残留一起删",
			body:        `{"model":"m","reasoning":{"effort":"high"},"reasoning_effort":"low"}`,
			wantDropped: true,
		},
		{
			name: "没键可删原样返回",
			body: `{"model":"m"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := DropReasoningEffort([]byte(tc.body))
			if dropped != tc.wantDropped {
				t.Fatalf("dropped = %v, want %v", dropped, tc.wantDropped)
			}
			if !dropped {
				if string(got) != tc.body {
					t.Fatalf("body changed unexpectedly: %s", got)
				}
				return
			}
			if gjson.GetBytes(got, "reasoning_effort").Exists() || gjson.GetBytes(got, "reasoning.effort").Exists() {
				t.Fatalf("effort keys still present: %s", got)
			}
			if gjson.GetBytes(got, "model").String() != "m" {
				t.Fatalf("model field lost: %s", got)
			}
		})
	}
}
