package service

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/atopos31/llmio/consts"
	"github.com/atopos31/llmio/models"
)

func setupFailedIOTestDB(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "failed-io.db")
	models.Init(context.Background(), path)
	t.Cleanup(func() {
		if sqlDB, err := models.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
}

func TestSaveFailedLogWritesIOWhenEnabled(t *testing.T) {
	setupFailedIOTestDB(t)

	ctx := context.WithValue(context.Background(), consts.ContextKeyAuthKeyID, uint(7))
	ctx = context.WithValue(ctx, consts.ContextKeyAuthKeyIOLog, true)

	raw := []byte("{\"model\":\"gpt-4o\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
	before := Before{Model: "gpt-4o", raw: raw}
	sessionID := stableSessionKey(nil, before.SessionID, before.Model, 7, raw)
	if sessionID == "" {
		t.Fatal("对话根哈希应得到稳定会话键")
	}

	logId, err := saveFailedLog(ctx, "openai", before, sessionID, "record not found")
	if err != nil {
		t.Fatalf("saveFailedLog: %v", err)
	}
	if logId == 0 {
		t.Fatal("logId should be nonzero")
	}

	var cl models.ChatLog
	if err := models.DB.First(&cl, logId).Error; err != nil {
		t.Fatalf("load chat log: %v", err)
	}
	if cl.Status != consts.StatusError {
		t.Errorf("Status = %q, want error", cl.Status)
	}
	if cl.AuthKeyID != 7 {
		t.Errorf("AuthKeyID = %d, want 7", cl.AuthKeyID)
	}
	if !cl.ChatIO {
		t.Error("ChatIO flag should be true")
	}
	if cl.Error != "record not found" {
		t.Errorf("Error = %q", cl.Error)
	}
	if cl.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", cl.SessionID, sessionID)
	}

	var cio models.ChatIO
	if err := models.DB.Where("log_id = ?", logId).First(&cio).Error; err != nil {
		t.Fatalf("load chat io: %v", err)
	}
	if string(cio.Input) != string(raw) {
		t.Errorf("Input mismatch")
	}
	if cio.OfString != "" {
		t.Errorf("OfString should be empty")
	}
	if len(cio.OfStringArray) != 0 {
		t.Errorf("OfStringArray should be empty")
	}
}

func TestSaveFailedLogNoIOWhenDisabled(t *testing.T) {
	setupFailedIOTestDB(t)

	ctx := context.WithValue(context.Background(), consts.ContextKeyAuthKeyID, uint(3))
	ctx = context.WithValue(ctx, consts.ContextKeyAuthKeyIOLog, false)

	raw := []byte("{\"model\":\"gpt-4o\"}")
	before := Before{Model: "gpt-4o", raw: raw}

	logId, err := saveFailedLog(ctx, "openai", before, "", "no provider")
	if err != nil {
		t.Fatalf("saveFailedLog: %v", err)
	}

	var cl models.ChatLog
	if err := models.DB.First(&cl, logId).Error; err != nil {
		t.Fatalf("load chat log: %v", err)
	}
	if cl.ChatIO {
		t.Error("ChatIO flag should be false")
	}
	if cl.SessionID != "" {
		t.Errorf("SessionID = %q, want 空串（无稳定会话键）", cl.SessionID)
	}

	var count int64
	if err := models.DB.Model(&models.ChatIO{}).Where("log_id = ?", logId).Count(&count).Error; err != nil {
		t.Fatalf("count chat io: %v", err)
	}
	if count != 0 {
		t.Errorf("no ChatIO rows expected when ioLog disabled, got %d", count)
	}
}

// 提前失败（模型不存在/无可用提供商）同样要带上稳定会话键，header 来源也不例外。
func TestProvidersWithMetaLogsHeaderSessionOnMissingModel(t *testing.T) {
	setupFailedIOTestDB(t)

	ctx := context.WithValue(context.Background(), consts.ContextKeyAuthKeyID, uint(7))
	before := Before{Model: "missing-model", raw: []byte(`{"model":"missing-model"}`)}
	header := http.Header{"X-Session-Id": []string{"header-session"}}

	if _, err := ProvidersWithMetaBymodelsName(ctx, header, consts.StyleOpenAI, before); err == nil {
		t.Fatal("模型不存在时应返回 error")
	}

	var cl models.ChatLog
	if err := models.DB.Order("id DESC").First(&cl).Error; err != nil {
		t.Fatalf("load chat log: %v", err)
	}
	if cl.SessionID != "header-session" {
		t.Fatalf("SessionID = %q, want header-session", cl.SessionID)
	}
}

func TestRecordRetryLogWritesIOWhenEnabled(t *testing.T) {
	setupFailedIOTestDB(t)

	ctx := context.Background()
	raw := []byte("{\"model\":\"claude-3\"}")
	log := models.ChatLog{
		Name:      "claude-3",
		Status:    consts.StatusRunning,
		Style:     "anthropic",
		AuthKeyID: 9,
		ChatIO:    true,
	}
	log = log.WithError(fmt.Errorf("upstream 500"))

	ch := make(chan retryLogEntry, 1)
	ch <- retryLogEntry{Log: log, Raw: raw}
	close(ch)

	RecordRetryLog(ctx, ch)

	var cl models.ChatLog
	if err := models.DB.Where("name = ?", "claude-3").First(&cl).Error; err != nil {
		t.Fatalf("load chat log: %v", err)
	}
	if cl.Status != consts.StatusError {
		t.Errorf("Status = %q, want error", cl.Status)
	}

	var cio models.ChatIO
	if err := models.DB.Where("log_id = ?", cl.ID).First(&cio).Error; err != nil {
		t.Fatalf("load chat io: %v", err)
	}
	if string(cio.Input) != string(raw) {
		t.Errorf("Input mismatch")
	}
}
