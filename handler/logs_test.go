package handler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/atopos31/llmio/models"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

/**
 * 请求日志列表的筛选口径。
 *
 * 这一组断言钉的是"多选"这件事的**边界**：哪几个维度可以多选（逗号即或）、
 * 维度之间是什么关系（与）、哪些维度**不能**被逗号拆开（请求标识）、
 * 以及空值不等于筛一个空串。这些判错了，接口不报错，只是返回的行不对——
 * 是最不容易被联调发现的那类错。
 */

func setupLogsHandlerDB(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logs-handler.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	// GetRequestLogs 查完日志还会按 auth_key_id 取一次密钥名，因此这张表也得建；
	// ChatIO 是删除单条时要一并清掉的表（见 TestDeleteLog）
	if err := db.AutoMigrate(&models.ChatLog{}, &models.AuthKey{}, &models.ChatIO{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	seed := []models.ChatLog{
		{ID: 1, Name: "alpha", ProviderName: "p1", ProviderModel: "gpt-4o", Status: "success", Style: "openai", AuthKeyID: 1, TraceID: "t-1"},
		// 与上一行只差一个后缀：钉住 provider_model 是精确匹配而不是子串
		{ID: 2, Name: "beta", ProviderName: "p2", ProviderModel: "gpt-4o-mini", Status: "error", Style: "openai", AuthKeyID: 2, TraceID: "t-2"},
		{ID: 3, Name: "alpha", ProviderName: "p1", ProviderModel: "gpt-4o", Status: "error", Style: "anthropic", AuthKeyID: 1, TraceID: "t-3"},
		// 空的 provider_model：候选端点必须把它排除在外
		{ID: 4, Name: "gamma", ProviderName: "p3", ProviderModel: "", Status: "success", Style: "gemini", AuthKeyID: 0, TraceID: "t-4"},
		// 假的 TraceID 里带逗号：用来验证请求标识不会被当成多值拆开
		{ID: 5, Name: "delta", ProviderName: "p4", ProviderModel: "claude-3.5", Status: "success", Style: "openai", AuthKeyID: 0, TraceID: "a,b"},
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}
	prev := models.DB
	models.DB = db
	t.Cleanup(func() {
		models.DB = prev
		// 先关连接再让 t.TempDir 清理（Windows 上文件被占用会导致清理失败）
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
}

// queryLogIDs 跑一次列表接口，返回命中的日志 ID（升序）与业务码。
func queryLogIDs(t *testing.T, rawQuery string) ([]uint, int) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/logs?"+rawQuery, nil)

	GetRequestLogs(c)

	var env struct {
		Code int `json:"code"`
		Data struct {
			Data []struct {
				ID uint `json:"ID"`
			} `json:"data"`
		} `json:"data"`
	}
	decodeEnvelope(t, w, &env)

	ids := make([]uint, 0, len(env.Data.Data))
	for _, row := range env.Data.Data {
		ids = append(ids, row.ID)
	}
	slices.Sort(ids)
	return ids, env.Code
}

func TestGetRequestLogsFilters(t *testing.T) {
	setupLogsHandlerDB(t)

	tests := []struct {
		name  string
		query string
		want  []uint
	}{
		{name: "不带筛选时全给", query: "", want: []uint{1, 2, 3, 4, 5}},

		// 单值仍然按精确匹配走：旧调用方（与旧链接）的语义不该变
		{name: "单值精确命中", query: "provider_name=p1", want: []uint{1, 3}},
		{name: "单值查不到就是空", query: "provider_name=nope", want: []uint{}},

		// 多值：逗号即"任一命中"
		{name: "多值供应商取并集", query: "provider_name=p1,p2", want: []uint{1, 2, 3}},
		{name: "多值模型名取并集", query: "name=alpha,gamma", want: []uint{1, 3, 4}},
		{name: "单值状态精确命中", query: "status=error", want: []uint{2, 3}},
		{name: "多值状态取并集", query: "status=error,running", want: []uint{2, 3}},
		{name: "多值协议取并集", query: "style=anthropic,gemini", want: []uint{3, 4}},
		{name: "多值密钥取并集", query: "auth_key_id=1,2", want: []uint{1, 2, 3}},

		// provider_model 是上游模型名，与 name（请求名）是两列。这一组的
		// 期望值刻意排除了 ID 2（gpt-4o-mini），钉住精确匹配：若退回子串，
		// 筛 gpt-4o 会把 gpt-4o-mini 一并带出来。
		{name: "单值上游模型名精确命中", query: "provider_model=gpt-4o", want: []uint{1, 3}},
		{name: "上游模型名多值取并集", query: "provider_model=gpt-4o,claude-3.5", want: []uint{1, 3, 5}},
		{name: "空串等于没筛", query: "provider_model=", want: []uint{1, 2, 3, 4, 5}},

		// 逗号两侧的空白与多余的分隔符不算取值
		{name: "取值两侧空白被忽略", query: "provider_name=p1,+p2", want: []uint{1, 2, 3}},
		{name: "只有分隔符等于没筛", query: "provider_name=,,+", want: []uint{1, 2, 3, 4, 5}},

		// 维度之间是"与"：既是 p1 又要是 error
		{name: "维度之间取交集", query: "provider_name=p1,p2&status=error", want: []uint{2, 3}},
		{name: "上游模型与供应商取交集", query: "provider_model=gpt-4o&provider_name=p2", want: []uint{}},

		// 请求标识不拆：这一串里真有一条 TraceID 就叫 "a,b"
		{name: "trace_id 整串匹配不被拆开", query: "trace_id=a%2Cb", want: []uint{5}},
		{name: "id 仍按单值匹配", query: "id=4", want: []uint{4}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, code := queryLogIDs(t, tt.query)
			if code != 200 {
				t.Fatalf("业务码应为 200，实得 %d", code)
			}
			if !slices.Equal(ids, tt.want) {
				t.Fatalf("命中的 ID 应为 %v，实得 %v", tt.want, ids)
			}
		})
	}
}

func TestGetRequestLogsInvalidAuthKeyID(t *testing.T) {
	setupLogsHandlerDB(t)

	// 密钥 id 是整型列：把 "abc" 当成字符串塞进 IN 会静默变成"查不到"，
	// 用户看到空列表而不知道自己的筛选值不合法。
	_, code := queryLogIDs(t, "auth_key_id=abc")
	if code != http.StatusBadRequest {
		t.Fatalf("非法 auth_key_id 应返回业务码 400，实得 %d", code)
	}
}

func TestGetRequestLogsKeyName(t *testing.T) {
	setupLogsHandlerDB(t)

	if err := models.DB.Create(&models.AuthKey{Model: gorm.Model{ID: 1}, Name: "生产"}).Error; err != nil {
		t.Fatalf("seed auth key: %v", err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/logs?id=1", nil)
	GetRequestLogs(c)

	var env struct {
		Data struct {
			Data []struct {
				KeyName string `json:"key_name"`
			} `json:"data"`
		} `json:"data"`
	}
	decodeEnvelope(t, w, &env)

	if len(env.Data.Data) != 1 || env.Data.Data[0].KeyName != "生产" {
		t.Fatalf("应带出密钥名，实得 %+v", env.Data.Data)
	}
}

// deleteLogByID 跑一次单条删除接口，返回业务码。
func deleteLogByID(t *testing.T, id string) int {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/logs/"+id, nil)
	withPathParam(c, "id", id)

	DeleteLog(c)

	var env struct {
		Code int `json:"code"`
	}
	decodeEnvelope(t, w, &env)
	return env.Code
}

// countRow 连软删过的行一起数。DeleteLog 走的是 Unscoped，用默认 Count
// 会把软删的行漏掉——那样子软删的实现也能"通过"硬删的断言。
func countRow(t *testing.T, model any, where string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := models.DB.Unscoped().Model(model).Where(where, args...).Count(&n).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

/**
 * 单条删除是本文件里唯一**动数据**的接口。要钉住的是三件事：
 * 行真的没了（而且是硬删，不是打个删除标记）、它的 IO 记录一起清掉、
 * 别人的行不受影响。少其中任何一条，用户的观感都是"点了删除，空间没回来"。
 */
func TestDeleteLog(t *testing.T) {
	setupLogsHandlerDB(t)

	// 给被删的那条挂上 IO：请求体会被压成帧存进表里，软删根本释放不了它
	if err := models.DB.Create(&models.ChatIO{LogId: 2, OutputUnion: models.OutputUnion{OfString: "resp"}}).Error; err != nil {
		t.Fatalf("seed chat io: %v", err)
	}

	if code := deleteLogByID(t, "2"); code != 200 {
		t.Fatalf("删除应返回业务码 200，实得 %d", code)
	}

	if n := countRow(t, &models.ChatLog{}, "id = ?", 2); n != 0 {
		t.Fatalf("日志行应被硬删除（含软删行也算），实得 %d 行", n)
	}
	if n := countRow(t, &models.ChatIO{}, "log_id = ?", 2); n != 0 {
		t.Fatalf("IO 记录应一并删除，实得 %d 行", n)
	}

	// 删一条就是删一条，不能牵连别的行
	ids, _ := queryLogIDs(t, "")
	if !slices.Equal(ids, []uint{1, 3, 4, 5}) {
		t.Fatalf("删除只应影响目标行，实得 %v", ids)
	}
}

func TestDeleteLogErrors(t *testing.T) {
	setupLogsHandlerDB(t)

	// id 不是数字：400。放进 Where 让 SQLite 隐式转换只会静默删掉 0 行
	if code := deleteLogByID(t, "abc"); code != http.StatusBadRequest {
		t.Fatalf("非法 id 应返回业务码 400，实得 %d", code)
	}

	// 不存在的 id：404。报成功的话界面会弹"已删除"，而这个 id 本来就不在
	if code := deleteLogByID(t, "999"); code != http.StatusNotFound {
		t.Fatalf("不存在的日志应返回业务码 404，实得 %d", code)
	}

	ids, _ := queryLogIDs(t, "")
	if !slices.Equal(ids, []uint{1, 2, 3, 4, 5}) {
		t.Fatalf("失败的删除不该动到任何行，实得 %v", ids)
	}
}

// 上游模型名候选：只取日志里观测到的、非空去重值，按字母序返回。
//
// 它是日志页"模型"筛选的下拉来源。空值必须排除——否则下拉里会多出一个
// 点一下查不到任何东西的空白项；去重保证同名多次出现只列一次。
func TestGetProviderModelList(t *testing.T) {
	setupLogsHandlerDB(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/provider-models", nil)

	GetProviderModelList(c)

	var env struct {
		Code int      `json:"code"`
		Data []string `json:"data"`
	}
	decodeEnvelope(t, w, &env)
	if env.Code != http.StatusOK {
		t.Fatalf("业务码应为 200，实得 %d", env.Code)
	}
	want := []string{"claude-3.5", "gpt-4o", "gpt-4o-mini"}
	if !slices.Equal(env.Data, want) {
		t.Fatalf("候选应为 %v（去重、排除空值、字母序），实得 %v", want, env.Data)
	}
}
