package handler

import (
	"strconv"
	"strings"
	"time"

	"github.com/atopos31/llmio/common"
	"github.com/atopos31/llmio/models"
	"github.com/atopos31/llmio/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// cachedTokensSumExpr 求和 JSON 列 prompt_tokens_details 中的 cached_tokens；NULL 与非法 JSON 均记为 0
const cachedTokensSumExpr = `COALESCE(SUM(CASE WHEN json_valid(prompt_tokens_details) THEN json_extract(prompt_tokens_details, '$.cached_tokens') ELSE 0 END), 0) as cached_tokens`

// metricsSelectExpr 汇总请求数与各 Token 口径
const metricsSelectExpr = `COUNT(id) as reqs,
	COALESCE(SUM(total_tokens), 0) as tokens,
	COALESCE(SUM(prompt_tokens), 0) as prompt_tokens, ` +
	cachedTokensSumExpr

// 折线图时间维度
const (
	rangeToday = "today"
	range24h   = "24h"
	range7d    = "7d"
	range30d   = "30d"
	range90d   = "90d"
)

// dailyRangeDays 按天聚合的时间维度及其跨度
var dailyRangeDays = map[string]int{range7d: 7, range30d: 30, range90d: 90}

// 时间桶标签格式，与下方 SQL 分桶表达式输出的一一对应
const (
	hourBucketLayout = "2006-01-02 15:04" // 对应 strftime('%Y-%m-%d %H:00', ...)
	dayBucketLayout  = "2006-01-02"       // 对应 date(...)
)

// SQL 分桶表达式。必须带 'localtime'：驱动写入的时间戳形如
// "2026-09-29 06:30:00.123+08:00"，SQLite 日期函数会先按偏移折算到 UTC，
// 少了 'localtime' 会把本地凌晨的请求算进前一天（实测 00:00 会整体丢一天）。
const (
	hourBucketExpr = `strftime('%Y-%m-%d %H:00', created_at, 'localtime')`
	dayBucketExpr  = `date(created_at, 'localtime')`
)

// startOfHour 按本地时区取整到整点。
// 不用 time.Truncate：它以 Unix 纪元为基准做整除，对非整小时偏移的时区会错位。
func startOfHour(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, t.Hour(), 0, 0, 0, t.Location())
}

// startOfDay 按本地时区取整到当日 00:00
func startOfDay(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, t.Location())
}

// timelineBuckets 依据时间维度生成升序桶起点、标签格式、SQL 分桶表达式与查询上界（不含）
func timelineBuckets(rangeKey string, now time.Time) (buckets []time.Time, layout, bucketExpr string, end time.Time, ok bool) {
	switch rangeKey {
	case rangeToday:
		// 本地自然日 00:00 起逐小时，含当前所在小时
		for b := startOfDay(now); !b.After(now); b = b.Add(time.Hour) {
			buckets = append(buckets, b)
		}
		return buckets, hourBucketLayout, hourBucketExpr, buckets[len(buckets)-1].Add(time.Hour), true
	case range24h:
		// 滚动 24 小时，含当前所在小时
		last := startOfHour(now)
		for i := 23; i >= 0; i-- {
			buckets = append(buckets, last.Add(-time.Duration(i)*time.Hour))
		}
		return buckets, hourBucketLayout, hourBucketExpr, last.Add(time.Hour), true
	case range7d, range30d, range90d:
		days := dailyRangeDays[rangeKey]
		last := startOfDay(now)
		for i := days - 1; i >= 0; i-- {
			buckets = append(buckets, last.AddDate(0, 0, -i))
		}
		return buckets, dayBucketLayout, dayBucketExpr, last.AddDate(0, 0, 1), true
	default:
		return nil, "", "", time.Time{}, false
	}
}

// rangeWindow 把 range 查询参数解析成统计窗口 [start, end)，与折线图 Timeline 共用同一套分桶边界。
// 返回 limited=false 表示不带时间限制（range 缺省，保留全量口径）；valid=false 表示 range 取值非法。
func rangeWindow(c *gin.Context, now time.Time) (start, end time.Time, limited, valid bool) {
	rangeKey := c.Query("range")
	if rangeKey == "" {
		return time.Time{}, time.Time{}, false, true
	}
	buckets, _, _, bucketEnd, ok := timelineBuckets(rangeKey, now)
	if !ok || len(buckets) == 0 {
		return time.Time{}, time.Time{}, false, false
	}
	return buckets[0], bucketEnd, true, true
}

type MetricsRes struct {
	Reqs         int64 `json:"reqs"`
	Tokens       int64 `json:"tokens"`
	PromptTokens int64 `json:"prompt_tokens"`
	CachedTokens int64 `json:"cached_tokens"`
}

type metricsRow struct {
	Reqs         int64 `gorm:"column:reqs"`
	Tokens       int64 `gorm:"column:tokens"`
	PromptTokens int64 `gorm:"column:prompt_tokens"`
	CachedTokens int64 `gorm:"column:cached_tokens"`
}

func Metrics(c *gin.Context) {
	days, err := strconv.Atoi(c.Param("days"))
	if err != nil {
		common.BadRequest(c, "Invalid days parameter")
		return
	}

	now := time.Now()
	year, month, day := now.Date()
	chain := gorm.G[models.ChatLog](models.DB).Where("created_at >= ?", time.Date(year, month, day, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -days))

	var row metricsRow
	if err := chain.Select(metricsSelectExpr).Scan(c.Request.Context(), &row); err != nil {
		common.InternalServerError(c, "Failed to aggregate metrics: "+err.Error())
		return
	}

	common.Success(c, MetricsRes{
		Reqs:         row.Reqs,
		Tokens:       row.Tokens,
		PromptTokens: row.PromptTokens,
		CachedTokens: row.CachedTokens,
	})
}

type TimelinePoint struct {
	Bucket       string `json:"bucket"`        // 桶标签：按小时 "2006-01-02 15:04"，按天 "2006-01-02"
	Tokens       int64  `json:"tokens"`        // 总 tokens
	CachedTokens int64  `json:"cached_tokens"` // 缓存 tokens
}

// Timeline Tokens 用量趋势：按时间维度分桶聚合 total_tokens 与 cached_tokens，空桶补零。
// model 查询参数缺省或为空时统计全部模型。
func Timeline(c *gin.Context) {
	buckets, layout, bucketExpr, end, ok := timelineBuckets(c.Query("range"), time.Now())
	if !ok {
		common.BadRequest(c, "Invalid range parameter: must be one of today, 24h, 7d, 30d, 90d")
		return
	}

	type timelineRow struct {
		Bucket       string `gorm:"column:bucket"`
		Tokens       int64  `gorm:"column:tokens"`
		CachedTokens int64  `gorm:"column:cached_tokens"`
	}

	chain := models.DB.
		Model(&models.ChatLog{}).
		Select(bucketExpr+" as bucket, COALESCE(SUM(total_tokens), 0) as tokens, "+cachedTokensSumExpr).
		Where("created_at >= ? AND created_at < ?", buckets[0], end)
	if name := strings.TrimSpace(c.Query("model")); name != "" {
		chain = chain.Where("name = ?", name)
	}

	rows := make([]timelineRow, 0, len(buckets))
	if err := chain.
		Group("bucket").
		Order("bucket ASC").
		Scan(&rows).Error; err != nil {
		common.InternalServerError(c, "Failed to aggregate timeline: "+err.Error())
		return
	}

	byBucket := make(map[string]timelineRow, len(rows))
	for _, row := range rows {
		byBucket[row.Bucket] = row
	}

	// 空桶补零，否则折线会出现断点
	points := make([]TimelinePoint, 0, len(buckets))
	for _, bucket := range buckets {
		key := bucket.Format(layout)
		row := byBucket[key]
		points = append(points, TimelinePoint{
			Bucket:       key,
			Tokens:       row.Tokens,
			CachedTokens: row.CachedTokens,
		})
	}

	common.Success(c, points)
}

// TimelineModels 返回日志中出现过的模型名（按调用次数降序），供趋势图的模型筛选下拉使用。
// 刻意不带时间维度过滤：让候选集合保持稳定，不随 range 切换而跳动。
func TimelineModels(c *gin.Context) {
	type modelRow struct {
		Name  string `gorm:"column:name"`
		Total int64  `gorm:"column:total"`
	}

	rows := make([]modelRow, 0)
	if err := models.DB.
		Model(&models.ChatLog{}).
		Select("name, COUNT(*) as total").
		Group("name").
		Order("total DESC").
		Scan(&rows).Error; err != nil {
		common.InternalServerError(c, "Failed to list timeline models: "+err.Error())
		return
	}

	names := make([]string, 0, len(rows))
	for _, row := range rows {
		if name := strings.TrimSpace(row.Name); name != "" {
			names = append(names, name)
		}
	}

	common.Success(c, names)
}

type ProviderModelCount struct {
	Provider      string `json:"provider"`       // 实际供应商名
	ProviderModel string `json:"provider_model"` // 实际上游模型名
	Tokens        int64  `json:"tokens"`         // 总 tokens
	CachedTokens  int64  `json:"cached_tokens"`  // 缓存 tokens
	Others        bool   `json:"others,omitempty"`
}

// parseUsageFilter 解析分析页那套筛选参数（from/to + provider/model/name/ua/status/key_id）。
//
// 只有显式给出 from/to 时才启用：分析页传的是与 /metrics/stats 完全相同的查询串，
// 因此两边的切片定义必然一致；概览页的旧调用只带 range，走下方 range 分支，
// 语义不变。第二个返回值表示"是否采用分析页口径"。
func parseUsageFilter(c *gin.Context) (service.StatsFilter, bool, error) {
	if strings.TrimSpace(c.Query("from")) == "" && strings.TrimSpace(c.Query("to")) == "" {
		return service.StatsFilter{}, false, nil
	}
	f, err := parseStatsFilter(c)
	if err != nil {
		return service.StatsFilter{}, false, err
	}
	return f, true, nil
}

// applyUsageFilter 把筛选条件落到聚合查询上。
//
// WHERE 必须与 service.LoadChatLogs 同形（同一批列、同一套精确匹配语义）：
// 两处口径不一致时，"用量图求和"与"趋势图总数"会给出两个不同的数，而页面
// 上它们看起来是同一件事。
func applyUsageFilter(chain *gorm.DB, f service.StatsFilter) *gorm.DB {
	chain = chain.Where("created_at >= ? AND created_at < ?", f.From, f.To)
	if len(f.Statuses) > 0 {
		chain = chain.Where("status IN ?", f.Statuses)
	}
	if len(f.KeyIDs) > 0 {
		chain = chain.Where("auth_key_id IN ?", f.KeyIDs)
	}
	for _, cond := range []struct {
		col  string
		vals []string
	}{
		{"provider_name", f.Providers},
		{"provider_model", f.Models},
		{"name", f.Names},
		{"user_agent", f.UserAgents},
	} {
		if len(cond.vals) == 0 {
			continue
		}
		chain = chain.Where(cond.col+" IN ?", cond.vals)
	}
	return chain
}

// ProviderModelUsage 按「实际供应商 + 实际上游模型」聚合 total_tokens 与 cached_tokens。
//
// 与 stats 的 `byModelProvider`（请求名 × 提供商）互补：那个维度看的是调用方要的模型，
// 这张图看的是真正被路由到的上游，备用关联接管时会体现在这里。
// 只统计有实际用量的组合（SUM(total_tokens) > 0）；按总 tokens 降序，超出 topN 的合并为一条 others。
//
// 时间窗口三选一：显式 from/to（分析页，连同全部维度筛选）> range（概览页旧口径）> 不限时间。
func ProviderModelUsage(c *gin.Context) {
	chain := models.DB.Model(&models.ChatLog{})
	f, filtered, err := parseUsageFilter(c)
	if err != nil {
		common.BadRequest(c, err.Error())
		return
	}
	if filtered {
		chain = applyUsageFilter(chain, f)
	} else {
		start, end, limited, rangeValid := rangeWindow(c, time.Now())
		if !rangeValid {
			common.BadRequest(c, "Invalid range parameter: must be one of today, 24h, 7d, 30d, 90d")
			return
		}
		if limited {
			chain = chain.Where("created_at >= ? AND created_at < ?", start, end)
		}
	}

	type providerModelRow struct {
		Provider      string `gorm:"column:provider"`
		ProviderModel string `gorm:"column:provider_model"`
		Tokens        int64  `gorm:"column:tokens"`
		CachedTokens  int64  `gorm:"column:cached_tokens"`
	}

	chain = chain.
		Select("provider_name as provider, provider_model, COALESCE(SUM(total_tokens), 0) as tokens, " + cachedTokensSumExpr).
		Group("provider_name, provider_model").
		Having("SUM(total_tokens) > 0").
		Order("tokens DESC")

	rows := make([]providerModelRow, 0)
	if err := chain.Scan(&rows).Error; err != nil {
		common.InternalServerError(c, "Failed to aggregate provider model usage: "+err.Error())
		return
	}

	results := make([]ProviderModelCount, 0, len(rows))
	for _, row := range rows {
		provider := strings.TrimSpace(row.Provider)
		if provider == "" {
			provider = "-"
		}
		providerModel := strings.TrimSpace(row.ProviderModel)
		if providerModel == "" {
			providerModel = "-"
		}
		results = append(results, ProviderModelCount{
			Provider:      provider,
			ProviderModel: providerModel,
			Tokens:        row.Tokens,
			CachedTokens:  row.CachedTokens,
		})
	}

	const topN = 10
	if len(results) > topN {
		var othersTokens, othersCached int64
		for _, item := range results[topN:] {
			othersTokens += item.Tokens
			othersCached += item.CachedTokens
		}
		results = append(results[:topN], ProviderModelCount{
			Tokens:       othersTokens,
			CachedTokens: othersCached,
			Others:       true,
		})
	}

	common.Success(c, results)
}
