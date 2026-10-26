package handler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/atopos31/llmio/models"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

/**
 * 用量聚合接口的筛选口径。
 *
 * 这套接口原来只认 `range`（today / 24h / 7d ...），迁到分析页之后要接受与
 * `/metrics/stats` 完全相同的查询串（from/to + 各维度）。这一组断言钉的是
 * "同一个筛选在两处命中同一批行"：只要 WHERE 的形状与 service.LoadChatLogs
 * 不一致，页面上就会出现"趋势图说 100 次、用量图说 98 次"这种假矛盾，
 * 而接口本身不会报错——正是最难被联调发现的那类错。
 */

func setupUsageDB(t *testing.T) time.Time {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage-handler.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.AutoMigrate(&models.ChatLog{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}

	// 窗口基准取本地 10:00，避免跨时区时 created_at 落到相邻日
	base := time.Date(2026, 10, 19, 10, 0, 0, 0, time.Local)
	seed := []models.ChatLog{
		{ID: 1, Name: "alpha", ProviderName: "p1", ProviderModel: "gpt-4o", Status: "success", Usage: models.Usage{TotalTokens: 100}, CreatedAt: base},
		{ID: 2, Name: "alpha", ProviderName: "p1", ProviderModel: "gpt-4o", Status: "error", Usage: models.Usage{TotalTokens: 50}, CreatedAt: base.Add(time.Hour)},
		// 落在窗口内的另一家供应商：用来验证 provider 筛选确实收窄了结果
		{ID: 3, Name: "beta", ProviderName: "p2", ProviderModel: "gpt-4o-mini", Status: "success", Usage: models.Usage{TotalTokens: 999}, CreatedAt: base.Add(30 * time.Minute)},
		// 位于窗口起点之前：窗口生效时它不该出现在任何一条里
		{ID: 4, Name: "alpha", ProviderName: "p1", ProviderModel: "gpt-4o", Status: "success", Usage: models.Usage{TotalTokens: 777}, CreatedAt: base.Add(-time.Hour)},
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}

	prev := models.DB
	models.DB = db
	t.Cleanup(func() {
		models.DB = prev
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return base
}

// queryProviderModelUsage 跑一次接口，返回按「供应商 / 模型」索引的 tokens。
func queryProviderModelUsage(t *testing.T, rawQuery string) (map[string]int64, int) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/metrics/provider-models?"+rawQuery, nil)

	ProviderModelUsage(c)

	var env struct {
		Code int                  `json:"code"`
		Data []ProviderModelCount `json:"data"`
	}
	decodeEnvelope(t, w, &env)

	out := make(map[string]int64, len(env.Data))
	for _, row := range env.Data {
		out[row.Provider+"/"+row.ProviderModel] = row.Tokens
	}
	return out, env.Code
}

func windowQuery(base time.Time, extra string) string {
	from := base.Add(-time.Minute).Unix()
	to := base.Add(90 * time.Minute).Unix()
	q := "from=" + strconv.FormatInt(from, 10) + "&to=" + strconv.FormatInt(to, 10)
	if extra != "" {
		q += "&" + extra
	}
	return q
}

func TestProviderModelUsageFilterWindow(t *testing.T) {
	base := setupUsageDB(t)

	got, code := queryProviderModelUsage(t, windowQuery(base, ""))
	if code != http.StatusOK {
		t.Fatalf("业务码应为 %d，实得 %d", http.StatusOK, code)
	}
	if got["p1/gpt-4o"] != 150 {
		t.Errorf("窗口内 p1/gpt-4o 应为 150，实得 %d（窗口起点之前的 777 不该计入）", got["p1/gpt-4o"])
	}
	if got["p2/gpt-4o-mini"] != 999 {
		t.Errorf("窗口内 p2/gpt-4o-mini 应为 999，实得 %d", got["p2/gpt-4o-mini"])
	}
}

func TestProviderModelUsageFilterDimensions(t *testing.T) {
	base := setupUsageDB(t)

	// provider：只剩 p2
	got, code := queryProviderModelUsage(t, windowQuery(base, "provider=p2"))
	if code != http.StatusOK {
		t.Fatalf("业务码应为 %d，实得 %d", http.StatusOK, code)
	}
	if len(got) != 1 || got["p2/gpt-4o-mini"] != 999 {
		t.Errorf("provider=p2 应只剩 p2/gpt-4o-mini=999，实得 %v", got)
	}

	// status：排除那一条 error 行（150 → 100）
	got, _ = queryProviderModelUsage(t, windowQuery(base, "status=success"))
	if got["p1/gpt-4o"] != 100 {
		t.Errorf("status=success 时 p1/gpt-4o 应为 100，实得 %d", got["p1/gpt-4o"])
	}

	// model 精确匹配 provider_model，不是子串：gpt-4o 不该命中 gpt-4o-mini
	got, _ = queryProviderModelUsage(t, windowQuery(base, "model=gpt-4o"))
	if _, ok := got["p2/gpt-4o-mini"]; ok {
		t.Errorf("model=gpt-4o 不该命中 gpt-4o-mini，实得 %v", got)
	}
}

// 非法窗口必须当场拒绝：静默忽略会让调用方以为拿到的是筛选后的数字。
func TestProviderModelUsageRejectsBadWindow(t *testing.T) {
	base := setupUsageDB(t)

	from := base.Unix()
	if _, code := queryProviderModelUsage(t, "from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(from, 10)); code == http.StatusOK {
		t.Fatalf("from 不早于 to 时应拒绝，实得 %d", code)
	}
}
