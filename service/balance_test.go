package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atopos31/llmio/balancers"
	"github.com/atopos31/llmio/consts"
	"github.com/atopos31/llmio/models"
	"gorm.io/gorm"
)

const balanceTestBody = `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`

// setupBalanceTestDB 起一个临时 SQLite 并写入包级 models.DB。
// BalanceChat 的 retryLog 收集协程会落库（SaveChatLog），DB 为 nil 会 panic。
func setupBalanceTestDB(t *testing.T) {
	t.Helper()
	models.Init(context.Background(), filepath.Join(t.TempDir(), "llmio.db"))
	t.Cleanup(func() {
		if models.DB == nil {
			return
		}
		// Windows 下文件被占用会导致 TempDir 清理失败，必须先关连接
		if sqlDB, err := models.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
}

// newUpstream 起一个固定返回指定状态码的上游，并统计被调用次数。
// 处理器内不要调 t.Errorf：上游协程可能在测试结束后才被调用。
func newUpstream(t *testing.T, status int, body string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testOpenAIProvider(id uint, name, baseURL string) models.Provider {
	return models.Provider{
		Model:  gorm.Model{ID: id},
		Name:   name,
		Type:   consts.StyleOpenAI,
		Config: fmt.Sprintf(`{"base_url":%q,"api_key":"test-key"}`, baseURL),
	}
}

// balanceTestMeta 组装「一个主关联 + 一个备用关联」的候选池。
// 关联的 Backup 标记与 splitWeightItems 的分池结果保持一致（生产路径由同一字段驱动）。
func balanceTestMeta(primaryBaseURL, backupBaseURL string) ProvidersWithMeta {
	return ProvidersWithMeta{
		ModelWithProviderMap: map[uint]models.ModelWithProvider{
			101: {Model: gorm.Model{ID: 101}, ProviderID: 1, ProviderModel: "primary-model", Backup: new(false)},
			102: {Model: gorm.Model{ID: 102}, ProviderID: 2, ProviderModel: "backup-model", Backup: new(true)},
		},
		WeightItems:       map[uint]int{101: 1},
		BackupWeightItems: map[uint]int{102: 1},
		ProviderMap: map[uint]models.Provider{
			1: testOpenAIProvider(1, "primary-provider", primaryBaseURL),
			2: testOpenAIProvider(2, "backup-provider", backupBaseURL),
		},
		MaxRetry: 3,
		TimeOut:  5,
		Strategy: consts.BalancerDefault,
	}
}

func runBalanceChat(t *testing.T, meta ProvidersWithMeta) (*http.Response, *models.ChatLog, error) {
	t.Helper()
	return runBalanceChatWithSession(t, meta, "session-for-test", http.Header{})
}

func runBalanceChatWithSession(t *testing.T, meta ProvidersWithMeta, sessionID string, header http.Header) (*http.Response, *models.ChatLog, error) {
	t.Helper()
	return runBalanceChatWithBody(t, meta, sessionID, header, []byte(balanceTestBody))
}

func runBalanceChatWithBody(t *testing.T, meta ProvidersWithMeta, sessionID string, header http.Header, raw []byte) (*http.Response, *models.ChatLog, error) {
	t.Helper()
	if header == nil {
		header = http.Header{}
	}
	before := Before{
		Model:     "test-model",
		Stream:    false,
		SessionID: sessionID,
		raw:       raw,
	}
	return BalanceChat(context.Background(), time.Now(), consts.StyleOpenAI, before, meta, models.ReqMeta{
		Header:    header,
		RemoteIP:  "127.0.0.1",
		UserAgent: "llmio-test",
	}, NewBridgeNotes())
}

// balanceTestSmartMeta 组装两个主关联的候选池，用于验证智能路由的固定落点。
func balanceTestSmartMeta(firstBaseURL, secondBaseURL string) ProvidersWithMeta {
	return ProvidersWithMeta{
		ModelWithProviderMap: map[uint]models.ModelWithProvider{
			101: {Model: gorm.Model{ID: 101}, ProviderID: 1, ProviderModel: "first-model", Backup: new(false)},
			102: {Model: gorm.Model{ID: 102}, ProviderID: 2, ProviderModel: "second-model", Backup: new(false)},
		},
		WeightItems: map[uint]int{101: 1, 102: 1},
		ProviderMap: map[uint]models.Provider{
			1: testOpenAIProvider(1, "first-provider", firstBaseURL),
			2: testOpenAIProvider(2, "second-provider", secondBaseURL),
		},
		MaxRetry: 3,
		TimeOut:  5,
		Strategy: consts.BalancerSmart,
	}
}

// pickSmartSession 找一个会命中指定关联的会话键，避免测试依赖哈希的偶然结果。
func pickSmartSession(t *testing.T, items map[uint]int, want uint) string {
	t.Helper()
	for i := 0; i < 1000; i++ {
		sessionID := fmt.Sprintf("smart-pick-%d", i)
		_, routingKey, _ := resolveSessionKeys(nil, sessionID, "test-model", 0, nil)
		got, err := balancers.NewSmart(items, routingKey).Pop()
		if err != nil {
			t.Fatalf("NewSmart().Pop() error: %v", err)
		}
		if got == want {
			return sessionID
		}
	}
	t.Fatalf("找不到命中关联 %d 的会话键", want)
	return ""
}

// 日志的会话列写解析后的稳定键：请求体、会话头、对话根哈希都写；
// 随机/项目兜底不写，避免出现每请求一个的一次性值。
func TestBalanceChatLogsStableSessionKey(t *testing.T) {
	setupBalanceTestDB(t)

	first, _ := newUpstream(t, http.StatusOK, `{"ok":true}`)
	second, _ := newUpstream(t, http.StatusOK, `{"ok":true}`)
	meta := balanceTestSmartMeta(first.URL, second.URL)

	tests := []struct {
		name       string
		sessionID  string
		header     http.Header
		raw        []byte
		want       string
		wantPrefix string
	}{
		{
			name:      "请求体会话",
			sessionID: "body-session",
			raw:       []byte(balanceTestBody),
			want:      "body-session",
		},
		{
			name:   "请求头会话",
			header: http.Header{"X-Session-Id": []string{"header-session"}},
			raw:    []byte(balanceTestBody),
			want:   "header-session",
		},
		{
			name:       "对话根哈希",
			raw:        []byte(balanceTestBody),
			wantPrefix: sessionKeyPrefix,
		},
		{
			name: "随机兜底不写",
			raw:  []byte(`{"model":"test-model"}`),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, log, err := runBalanceChatWithBody(t, meta, tt.sessionID, tt.header, tt.raw)
			if err != nil {
				t.Fatalf("BalanceChat() unexpected error: %v", err)
			}
			res.Body.Close()

			if tt.wantPrefix != "" {
				if !strings.HasPrefix(log.SessionID, tt.wantPrefix) {
					t.Fatalf("log.SessionID = %q, want 前缀 %q", log.SessionID, tt.wantPrefix)
				}
				return
			}
			if log.SessionID != tt.want {
				t.Fatalf("log.SessionID = %q, want %q", log.SessionID, tt.want)
			}
		})
	}
}

// 同一会话两次请求必须固定到同一上游，且只调用它。
func TestBalanceChatSmartSticky(t *testing.T) {
	setupBalanceTestDB(t)

	first, firstCalls := newUpstream(t, http.StatusOK, `{"ok":true}`)
	second, secondCalls := newUpstream(t, http.StatusOK, `{"ok":true}`)
	meta := balanceTestSmartMeta(first.URL, second.URL)

	run := func() *models.ChatLog {
		res, log, err := runBalanceChatWithSession(t, meta, "sticky-session", nil)
		if err != nil {
			t.Fatalf("BalanceChat() unexpected error: %v", err)
		}
		res.Body.Close()
		return log
	}

	firstLog := run()
	secondLog := run()
	if firstLog.ProviderName != secondLog.ProviderName {
		t.Fatalf("同一会话命中 %q 与 %q, want 同一上游", firstLog.ProviderName, secondLog.ProviderName)
	}

	a, b := atomic.LoadInt32(firstCalls), atomic.LoadInt32(secondCalls)
	if !(a == 2 && b == 0) && !(a == 0 && b == 2) {
		t.Fatalf("上游调用次数 = %d/%d, want 两次都落在同一家", a, b)
	}
}

// 429 只降权不移出：确定性选路下必须靠本请求内的回避集合换到下一家。
func TestBalanceChatSmartRetriesAfter429(t *testing.T) {
	setupBalanceTestDB(t)

	limited, limitedCalls := newUpstream(t, http.StatusTooManyRequests, `{"error":"rate limit"}`)
	healthy, healthyCalls := newUpstream(t, http.StatusOK, `{"ok":true}`)
	meta := balanceTestSmartMeta(limited.URL, healthy.URL)

	sessionID := pickSmartSession(t, meta.WeightItems, 101)

	res, log, err := runBalanceChatWithSession(t, meta, sessionID, nil)
	if err != nil {
		t.Fatalf("期望 429 后切换上游并成功，实际 error: %v", err)
	}
	defer res.Body.Close()

	if log.ProviderName != "second-provider" {
		t.Fatalf("命中 provider = %q, want second-provider", log.ProviderName)
	}
	if got := atomic.LoadInt32(limitedCalls); got != 1 {
		t.Errorf("429 上游调用次数 = %d, want 1", got)
	}
	if got := atomic.LoadInt32(healthyCalls); got != 1 {
		t.Errorf("健康上游调用次数 = %d, want 1", got)
	}
}

// 备用关联不参与日常轮询：主关联健康时必须只打主关联，备用上游一次都不能被碰。
func TestBalanceChatSkipsBackupWhenPrimaryHealthy(t *testing.T) {
	setupBalanceTestDB(t)

	primary, primaryCalls := newUpstream(t, http.StatusOK, `{"ok":true}`)
	backup, backupCalls := newUpstream(t, http.StatusOK, `{"ok":true}`)

	res, log, err := runBalanceChat(t, balanceTestMeta(primary.URL, backup.URL))
	if err != nil {
		t.Fatalf("BalanceChat() unexpected error: %v", err)
	}
	defer res.Body.Close()

	if log.ProviderName != "primary-provider" {
		t.Fatalf("命中 provider = %q, want primary-provider", log.ProviderName)
	}
	if log.Backup {
		t.Errorf("log.Backup = true, want false（主关联服务时日志不应标记为备用）")
	}
	if got := atomic.LoadInt32(primaryCalls); got != 1 {
		t.Errorf("主上游调用次数 = %d, want 1", got)
	}
	if got := atomic.LoadInt32(backupCalls); got != 0 {
		t.Errorf("备用上游调用次数 = %d, want 0（主关联健康时备用不该被触碰）", got)
	}
}

// 主关联全部失败出局后由备用接管，并正常拿到 200。
func TestBalanceChatFallsBackAfterPrimaryExhausted(t *testing.T) {
	setupBalanceTestDB(t)

	primary, primaryCalls := newUpstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
	backup, backupCalls := newUpstream(t, http.StatusOK, `{"ok":true}`)

	res, log, err := runBalanceChat(t, balanceTestMeta(primary.URL, backup.URL))
	if err != nil {
		t.Fatalf("期望降级到备用后成功，实际 error: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", res.StatusCode)
	}
	if log.ProviderName != "backup-provider" || log.ProviderModel != "backup-model" {
		t.Fatalf("命中 = %s/%s, want backup-provider/backup-model", log.ProviderName, log.ProviderModel)
	}
	if !log.Backup {
		t.Error("log.Backup = false, want true（走备用关联的请求必须在日志里标出来，否则日志页看不出降级）")
	}
	if got := atomic.LoadInt32(primaryCalls); got != 1 {
		t.Errorf("主上游调用次数 = %d, want 1", got)
	}
	if got := atomic.LoadInt32(backupCalls); got != 1 {
		t.Errorf("备用上游调用次数 = %d, want 1", got)
	}
}

// 连备用也挂掉时，错误信息必须标明已经用过备用，
// 否则和「主关联本身就不够用」完全无从区分。
func TestBalanceChatReportsBackupExhausted(t *testing.T) {
	setupBalanceTestDB(t)

	primary, _ := newUpstream(t, http.StatusInternalServerError, `{"error":"primary boom"}`)
	backup, _ := newUpstream(t, http.StatusInternalServerError, `{"error":"backup boom"}`)

	_, _, err := runBalanceChat(t, balanceTestMeta(primary.URL, backup.URL))
	if err == nil {
		t.Fatal("主备全挂时应返回 error")
	}
	if !strings.Contains(err.Error(), "backup providers exhausted") {
		t.Fatalf("error = %q, 期望包含 \"backup providers exhausted\"", err.Error())
	}
	if !strings.Contains(err.Error(), "backup boom") {
		t.Fatalf("error = %q, 期望保留最后一轮的真实原因 \"backup boom\"", err.Error())
	}
}

// 没配备用关联时行为与改动前一致：主关联耗尽即失败，且错误信息不带降级标记。
func TestBalanceChatWithoutBackupKeepsLegacyBehavior(t *testing.T) {
	setupBalanceTestDB(t)

	primary, primaryCalls := newUpstream(t, http.StatusInternalServerError, `{"error":"boom"}`)

	meta := balanceTestMeta(primary.URL, "")
	meta.BackupWeightItems = map[uint]int{}
	delete(meta.ModelWithProviderMap, 102)

	_, _, err := runBalanceChat(t, meta)
	if err == nil {
		t.Fatal("无备用关联且主关联失败时应返回 error")
	}
	if strings.Contains(err.Error(), "backup providers exhausted") {
		t.Fatalf("error = %q, 未配备用时不应出现降级标记", err.Error())
	}
	if got := atomic.LoadInt32(primaryCalls); got != 1 {
		t.Errorf("主上游调用次数 = %d, want 1", got)
	}
}

// 备用标记必须能活到日志页：Backup 只在 BalanceChat 里随 insert 落库，
// 之后 RecordLog 拿 processer 返回的空壳 log 做 struct Updates，
// 全靠 GORM「跳过零值字段」的语义才没被擦成 false。这条测试守的就是整条链路。
func TestRecordLogPreservesBackupFlag(t *testing.T) {
	setupBalanceTestDB(t)
	ctx := context.Background()

	logId, err := SaveChatLog(ctx, models.ChatLog{
		Name:         "test-model",
		ProviderName: "backup-provider",
		Backup:       true,
		Status:       consts.StatusRunning,
	})
	if err != nil {
		t.Fatalf("SaveChatLog() error: %v", err)
	}

	processer := func(_ context.Context, _ io.Reader, _ bool, _ time.Time) (*models.ChatLog, *models.OutputUnion, error) {
		return &models.ChatLog{FirstChunkTime: time.Second, Size: 7}, &models.OutputUnion{}, nil
	}
	RecordLog(ctx, time.Now(), io.NopCloser(strings.NewReader("data")), processer, logId, Before{}, false, NewBridgeNotes())

	got, err := gorm.G[models.ChatLog](models.DB).Where("id = ?", logId).First(ctx)
	if err != nil {
		t.Fatalf("回查日志失败: %v", err)
	}
	if !got.Backup {
		t.Error("RecordLog 之后 Backup 被擦成 false：struct Updates 跳过零值字段的语义已不成立（或已被改成 Save / Select）")
	}
	if got.ProviderName != "backup-provider" {
		t.Errorf("ProviderName = %q, want backup-provider（零值字段不该覆盖已落库的值）", got.ProviderName)
	}
	if got.Status != consts.StatusSuccess {
		t.Errorf("Status = %q, want %q", got.Status, consts.StatusSuccess)
	}
	if got.Size != 7 {
		t.Errorf("Size = %d, want 7（非零字段应被正常更新）", got.Size)
	}
}
