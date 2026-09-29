//
//
// Tencent is pleased to support the open source community by making Tencent Cloud AIGate available.
//
// Copyright (C) 2026 Tencent.
// All rights reserved.
//
// If you have downloaded a copy of the Tencent Cloud AIGate source code from Tencent,
// please note that Tencent Cloud AIGate source code is licensed under the Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
//
//

// Package proxy_test 以黑盒方式测试数据面转发管道（internal/proxy）。
//
// 夹具说明：
//   - 每个用例使用独立的临时 SQLite 文件（见 newTestStore），t.Cleanup 关闭连接；
//   - 上游一律用 httptest 起在 127.0.0.1 的 mock 服务，因此夹具统一调用
//     provider.SetAllowPrivateUpstreams(true)，并在 t.Cleanup 还原为严格模式
//     （SSRF 防护默认严格，会拦截回环 mock 上游）；
//   - 被测网关用 httptest.NewServer + http.ServeMux 起真实 HTTP 服务，请求经真实
//     网络栈往返，避免绕过 http.ResponseWriter 的 Flusher 能力（流式用例依赖它）。
//
// 由于 SetAllowPrivateUpstreams 是包级全局开关，用例之间必须串行，禁止使用 t.Parallel。
package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"aigate/internal/model"
	"aigate/internal/provider"
	"aigate/internal/proxy"
)

// ctx 返回测试用上下文。
func ctx() context.Context { return context.Background() }

// allowPrivateUpstreams 放行私网 / 回环上游（mock 落在 127.0.0.1），用例结束时还原为
// 默认严格模式，保证用例间隔离。
func allowPrivateUpstreams(t *testing.T) {
	t.Helper()
	provider.SetAllowPrivateUpstreams(true)
	t.Cleanup(func() { provider.SetAllowPrivateUpstreams(false) })
}

// newTestStore 创建隔离的 SQLite Store（已建表），返回 store 与 DSN。
// DSN 供 openReadDB 另开一条连接做表侧断言。
func newTestStore(t *testing.T) (*model.Store, string) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "proxy-test.db")
	store, err := model.Open(model.DBTypeSQLite, dsn)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	if err := store.AutoMigrate(); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dsn
}

// openReadDB 打开指向同一 SQLite 文件的第二条连接，用于直读表数据断言。
func openReadDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open read db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("read db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// countRows 统计 dest 对应表的行数。
func countRows(t *testing.T, db *gorm.DB, dest any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(dest).Count(&n).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// 实体构造
// ---------------------------------------------------------------------------

// mustCreateUser 入库一个默认角色（user）的用户。
func mustCreateUser(t *testing.T, store *model.Store, name string) *model.User {
	t.Helper()
	_, hash, prefix, err := model.GenerateToken(model.TokenPrefixAccess)
	if err != nil {
		t.Fatalf("generate user token: %v", err)
	}
	u := &model.User{Username: name, APITokenHash: hash, APITokenPrefix: prefix}
	if err := store.CreateUser(ctx(), u); err != nil {
		t.Fatalf("create user %q: %v", name, err)
	}
	return u
}

// newAIModel 构造一个字段齐全、默认启用的 OpenAI 兼容模型配置（未入库）。
func newAIModel(modelID, upstreamURL string) *model.AIModel {
	return &model.AIModel{
		Provider:          "mock-provider",
		ModelID:           modelID,
		ModelName:         "name-" + modelID,
		URL:               upstreamURL,
		APIKey:            "upstream-secret",
		ModelType:         model.ModelTypeOpenAI,
		CustomHTTPHeaders: "",
		QuotaDay:          -1,
		Enabled:           true,
	}
}

// mustCreateAIModel 入库模型并返回实体。
func mustCreateAIModel(t *testing.T, store *model.Store, m *model.AIModel) *model.AIModel {
	t.Helper()
	if err := store.CreateAIModel(ctx(), m); err != nil {
		t.Fatalf("create ai model %q: %v", m.ModelID, err)
	}
	return m
}

// mustCreateKey 入库一个数据面密钥（含绑定），返回实体与 token 明文。
func mustCreateKey(
	t *testing.T,
	store *model.Store,
	ownerID uint,
	maxTokens int,
	bindings ...model.APIKeyModel,
) (*model.APIKey, string) {
	t.Helper()
	plain, hash, prefix, err := model.GenerateToken(model.TokenPrefixKey)
	if err != nil {
		t.Fatalf("generate api key token: %v", err)
	}
	k := &model.APIKey{
		Name:             "test-key",
		UserID:           ownerID,
		ProxyTokenHash:   hash,
		ProxyTokenPrefix: prefix,
		MaxTokens:        maxTokens,
		Enabled:          true,
	}
	if err := store.CreateAPIKey(ctx(), k, bindings); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return k, plain
}

// primary 构造一条 primary 绑定。
func primary(aiModelID uint, order int) model.APIKeyModel {
	return model.APIKeyModel{AIModelID: aiModelID, Role: model.ModelRolePrimary, SortOrder: order}
}

// fallback 构造一条 fallback 绑定。
func fallback(aiModelID uint, order int) model.APIKeyModel {
	return model.APIKeyModel{AIModelID: aiModelID, Role: model.ModelRoleFallback, SortOrder: order}
}

// ---------------------------------------------------------------------------
// mock 上游
// ---------------------------------------------------------------------------

// upstreamRequest 记录 mock 上游收到的请求（body 已解析为 map）。
type upstreamRequest struct {
	mu      sync.Mutex
	calls   int
	method  string
	path    string
	headers http.Header
	rawBody []byte
	body    map[string]any
}

func (u *upstreamRequest) record(r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	u.method = r.Method
	u.path = r.URL.Path
	u.headers = r.Header.Clone()
	u.rawBody = raw
	u.body = nil
	if len(raw) > 0 {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			u.body = m
		}
	}
}

// Body 返回上游最近一次收到的请求体（JSON 解析结果）。
func (u *upstreamRequest) Body() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.body
}

// Header 返回上游最近一次收到的指定请求头。
func (u *upstreamRequest) Header(key string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.headers.Get(key)
}

// Calls 返回上游累计收到的请求数。
func (u *upstreamRequest) Calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// startUpstream 启动一个记录请求并执行 respond 的 mock 上游（127.0.0.1）。
func startUpstream(
	t *testing.T,
	respond func(w http.ResponseWriter, r *http.Request),
) (*httptest.Server, *upstreamRequest) {
	t.Helper()
	rec := &upstreamRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// respondConstantJSON 返回一个固定状态码与响应体的 mock 上游处理器。
func respondConstantJSON(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// ---------------------------------------------------------------------------
// 被测环境
// ---------------------------------------------------------------------------

// env 是数据面用例的公共夹具：隔离 store + mock 上游 + 经真实 HTTP 暴露的被测网关。
type env struct {
	store *model.Store
	dsn   string
	usage *proxy.UsageWriter
	user  *model.User

	upstream *httptest.Server
	rec      *upstreamRequest

	server *httptest.Server
	client *http.Client

	stopOnce sync.Once
}

// newEnv 组装一套完整夹具（含用量写入器并在用例结束排空）。
func newEnv(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *env {
	t.Helper()
	allowPrivateUpstreams(t)

	store, dsn := newTestStore(t)
	upstream, rec := startUpstream(t, respond)

	usage := proxy.NewUsageWriter(store, 0)
	usage.Start()

	mux := http.NewServeMux()
	proxy.NewHandler(store, usage, 429).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	e := &env{
		store:    store,
		dsn:      dsn,
		usage:    usage,
		user:     mustCreateUser(t, store, "tester"),
		upstream: upstream,
		rec:      rec,
		server:   server,
		client:   server.Client(),
	}
	t.Cleanup(e.stopUsage)
	return e
}

// stopUsage 停止用量写入器并把队列排空（幂等）。断言落库前必须先调用。
func (e *env) stopUsage() {
	e.stopOnce.Do(func() { e.usage.Stop(context.Background()) })
}

// postChat 向被测网关发出一次 chat/completions 请求。
func (e *env) postChat(t *testing.T, token, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build chat request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do chat request: %v", err)
	}
	return resp
}

// getModels 向被测网关发出一次 /v1/models 请求。
func (e *env) getModels(t *testing.T, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.server.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("build models request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do models request: %v", err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// 断言与解析辅助
// ---------------------------------------------------------------------------

// readBody 读取并关闭响应体，返回字符串。
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(b)
}

// assertStatus 断言状态码；不匹配时把响应体一并打印出来便于定位。
func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d (body=%s)", resp.StatusCode, want, readBody(t, resp))
	}
}

// bodyNumber 从上游请求体里取数字字段（JSON 解析后为 float64）。
func bodyNumber(t *testing.T, body map[string]any, key string) (float64, bool) {
	t.Helper()
	v, ok := body[key]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("upstream body[%q] = %#v, want number", key, v)
	}
	return f, true
}

// reqWithModel 构造一个最小 chat 请求体（显式指定 model）。
func reqWithModel(modelID string) string {
	return `{"model":"` + modelID + `","messages":[{"role":"user","content":"hi"}]}`
}
