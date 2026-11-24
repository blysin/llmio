package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// normalizeEfforts 的两件事：挡住脏值，给出稳定顺序。
func TestNormalizeEfforts(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			// 上游返回的顺序不可靠：实测 OpenRouter 有的模型把 max 排第一位
			name: "按关到最强排序，不跟上游的顺序",
			in:   []string{"max", "high", "medium", "low", "minimal", "none", "xhigh"},
			want: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"},
		},
		{
			name: "去重且大小写归一",
			in:   []string{"HIGH", "high", " High "},
			want: []string{"high"},
		},
		{
			name: "空值丢掉",
			in:   []string{"", "  ", "low"},
			want: []string{"low"},
		},
		{
			// 某家网关自己的档位：认不出就排在认识的值后面，不能顶掉顺序
			name: "陌生档位排在最后，按字典序",
			in:   []string{"zzz", "high", "aaa"},
			want: []string{"high", "aaa", "zzz"},
		},
		{
			name: "空输入不返回 nil 长度以外的意外",
			in:   nil,
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeEfforts(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("normalizeEfforts(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// 探测的核心：从 OpenAI 兼容的 /models 里挖出 reasoning.supported_efforts。
//
// 这里用真实 HTTP 而不是塞一个 json.Unmarshal：这个函数自己发请求、判状态码，
// 只测解码等于把最容易坏的那半段（拼 URL、Authorization、非 200）留在外面。
func TestFetchSupportedEfforts(t *testing.T) {
	const listBody = `{"data":[
		{"id":"gpt-5","reasoning":{"supported_efforts":["high","low","medium"]}},
		{"id":"gpt-4o"},
		{"id":"anthropic/claude-sonnet-4.5","reasoning":{"supported_efforts":["none","xhigh","max"]}}
	]}`

	cases := []struct {
		name    string
		status  int
		body    string
		model   string
		want    []string
		wantErr bool
	}{
		{
			name:   "精确命中，顺序归一",
			status: http.StatusOK,
			body:   listBody,
			model:  "gpt-5",
			want:   []string{"low", "medium", "high"},
		},
		{
			// 用户常只填后半截：OpenRouter 列的是 anthropic/claude-sonnet-4.5
			name:   "后缀唯一命中",
			status: http.StatusOK,
			body:   listBody,
			model:  "claude-sonnet-4.5",
			want:   []string{"none", "xhigh", "max"},
		},
		{
			// 模型在，但没带 capability：这不是错误，是"这家不说"
			name:   "模型不声明档位",
			status: http.StatusOK,
			body:   listBody,
			model:  "gpt-4o",
		},
		{
			name:   "模型不在列表里",
			status: http.StatusOK,
			body:   listBody,
			model:  "no-such-model",
		},
		{
			// 401 之类要能被识别成"真错误"（调用方会记日志再退兜底）
			name:    "非 200 视为错误",
			status:  http.StatusUnauthorized,
			body:    `{"error":"bad key"}`,
			model:   "gpt-5",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/models" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("missing bearer auth: %q", r.Header.Get("Authorization"))
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			got, err := fetchSupportedEfforts(context.Background(), server.URL, "test-key", "", tc.model)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (efforts=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("efforts = %v, want %v", got, tc.want)
			}
		})
	}
}

// body 上限：异常上游回一份超过 8 MiB 的 /models 时，解码必须失败（调用方退
// 兜底清单），而不是把整份 body 读进内存。前缀是合法 JSON、尾部撑爆上限。
func TestFetchSupportedEffortsBodyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"m","reasoning":{"supported_efforts":["low"]}}`))
		// 9 MiB 填充：合法的 JSON 字符串内容，整体超长但不闭合
		chunk := make([]byte, 1<<20)
		for i := range chunk {
			chunk[i] = 'x'
		}
		for i := 0; i < 9; i++ {
			_, _ = w.Write(chunk)
		}
	}))
	defer server.Close()

	_, err := fetchSupportedEfforts(context.Background(), server.URL, "test-key", "", "m")
	if err == nil {
		t.Fatal("超过上限的 body 应以解析错误收场，而不是照单全收")
	}
}
