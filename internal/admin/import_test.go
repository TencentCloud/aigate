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

package admin_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"aigate/internal/model"
)

// ---------------------------------------------------------------------------
// 响应结构（仅本文件使用）
// ---------------------------------------------------------------------------

type importSummaryResp struct {
	Created int `json:"created"`
	Skipped int `json:"skipped"`
	Errors  int `json:"errors"`
}

type importDetailResp struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Action  string `json:"action"`
	Message string `json:"message"`
}

type importResp struct {
	DryRun  bool               `json:"dry_run"`
	Summary importSummaryResp  `json:"summary"`
	Details []importDetailResp `json:"details"`
}

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// importFixture 返回一份覆盖四类实体的导入载荷：
// 分组 g1（合法 day 规则）+ 用户 alice（属 g1）+ 模型 mock/m1 + 密钥 k1（引用 m1，primary）。
func importFixture(dryRun bool) map[string]any {
	return map[string]any{
		"dry_run": dryRun,
		"user_groups": []map[string]any{
			{"name": "g1", "token_quota_rules": `[{"mode":"day","limit":100}]`},
		},
		"users": []map[string]any{
			{"username": "alice", "role": "user", "group": "g1"},
		},
		"models": []map[string]any{
			{
				"provider": "mock", "model_id": "m1", "model_name": "M1",
				"url": "http://upstream.example/v1", "api_key": "sk-up",
				"model_type": "openai-completions", "quota_day": -1, "enabled": true,
			},
		},
		"keys": []map[string]any{
			{
				"name": "k1", "user": "alice", "group": "g1", "max_tokens": 0,
				"token": "sk-legacy-plain",
				"bindings": []map[string]any{
					{"model": "m1", "role": "primary"},
				},
			},
		},
	}
}

// runImport 提交导入载荷并解析响应。
func runImport(t *testing.T, srv *httptest.Server, adminToken string, payload map[string]any) (importResp, []byte) {
	t.Helper()
	status, body := doRequest(t, srv, http.MethodPost, "/admin/import", adminToken, payload)
	assertStatus(t, status, http.StatusOK, body)
	return mustDecode[importResp](t, body), body
}

// findDetail 按 kind+ref 查找明细；找不到即失败。
func findDetail(t *testing.T, resp importResp, kind, ref string) importDetailResp {
	t.Helper()
	for _, d := range resp.Details {
		if d.Kind == kind && d.Ref == ref {
			return d
		}
	}
	t.Fatalf("kind=%s ref=%s not found in details (details=%+v)", kind, ref, resp.Details)
	return importDetailResp{}
}

// assertCounts 断言明细条数与 summary 自洽。
func assertCounts(t *testing.T, resp importResp) {
	t.Helper()
	if got := len(resp.Details); got != resp.Summary.Created+resp.Summary.Skipped+resp.Summary.Errors {
		t.Fatalf("details count %d inconsistent with summary %+v", got, resp.Summary)
	}
}

// findGroupByName / findModelByModelID / findUserByName 是在切片上的查找辅助。
func findGroupByName(groups []model.Group, name string) *model.Group {
	for i := range groups {
		if groups[i].Name == name {
			return &groups[i]
		}
	}
	return nil
}

func findModelByModelID(models []model.AIModel, modelID string) *model.AIModel {
	for i := range models {
		if models[i].ModelID == modelID {
			return &models[i]
		}
	}
	return nil
}

func findUserByName(users []model.User, name string) *model.User {
	for i := range users {
		if users[i].Username == name {
			return &users[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestImport_DryRunDoesNotPersist 验证 dry_run=true 只返回计划、不写库。
func TestImport_DryRunDoesNotPersist(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ctx := context.Background()

	resp, _ := runImport(t, srv, adminToken, importFixture(true))

	if !resp.DryRun {
		t.Fatalf("dry_run = false, want true")
	}
	if want := (importSummaryResp{Created: 4}); resp.Summary != want {
		t.Fatalf("summary = %+v, want %+v", resp.Summary, want)
	}
	assertCounts(t, resp)
	for _, d := range resp.Details {
		if d.Action != "created" {
			t.Fatalf("dry run entry %+v should be created", d)
		}
	}

	groups, err := store.ListGroups(ctx)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups after dry run = %d, want 0", len(groups))
	}
	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 { // 仅 newAdminServer 预置的 root
		t.Fatalf("users after dry run = %d, want 1 (root only)", len(users))
	}
	models, err := store.ListAIModels(ctx)
	if err != nil {
		t.Fatalf("ListAIModels: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("models after dry run = %d, want 0", len(models))
	}
	keys, err := store.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("keys after dry run = %d, want 0", len(keys))
	}
}

// TestImport_FullImportPersistsAndHashesToken 验证全量导入落库、token 哈希化且明文不回显。
func TestImport_FullImportPersistsAndHashesToken(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ctx := context.Background()
	const plainToken = "sk-legacy-plain"

	resp, raw := runImport(t, srv, adminToken, importFixture(false))

	if want := (importSummaryResp{Created: 4}); resp.Summary != want {
		t.Fatalf("summary = %+v, want %+v", resp.Summary, want)
	}
	assertCounts(t, resp)
	if bytes.Contains(raw, []byte(plainToken)) {
		t.Fatalf("response body should not contain token plaintext: %s", raw)
	}
	if bytes.Contains(raw, []byte("sk-up")) {
		t.Fatalf("response body should not contain upstream api_key plaintext: %s", raw)
	}
	for _, d := range resp.Details {
		if d.Message != "" {
			t.Fatalf("success entry should not carry message: %+v", d)
		}
	}

	// 分组：规则经 quota.NormalizeRules 规范化后落库。
	groups, err := store.ListGroups(ctx)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	g1 := findGroupByName(groups, "g1")
	if g1 == nil {
		t.Fatalf("group g1 not persisted")
	}
	if g1.TokenQuotaRules != `[{"mode":"day","limit":100}]` {
		t.Fatalf("g1.token_quota_rules = %q", g1.TokenQuotaRules)
	}

	// 用户：group_id 解析到 g1，名下的访问 token 哈希非空（生成后明文丢弃）。
	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	alice := findUserByName(users, "alice")
	if alice == nil {
		t.Fatalf("user alice not persisted")
	}
	if alice.GroupID != g1.ID || alice.Role != model.RoleUser {
		t.Fatalf("alice = %+v, want group_id=%d role=user", alice, g1.ID)
	}
	if alice.APITokenHash == "" {
		t.Fatalf("alice.api_token_hash empty, a random token should be generated under the unique index")
	}

	// 模型：(provider, model_id) 落库，api_key 经 model 层策略存取后为明文。
	models, err := store.ListAIModels(ctx)
	if err != nil {
		t.Fatalf("ListAIModels: %v", err)
	}
	m1 := findModelByModelID(models, "m1")
	if m1 == nil {
		t.Fatalf("model m1 not persisted")
	}
	if m1.Provider != "mock" || m1.URL != "http://upstream.example/v1" || m1.ModelType != model.ModelTypeOpenAI {
		t.Fatalf("m1 = %+v", m1)
	}
	if m1.QuotaDay != -1 || !m1.Enabled {
		t.Fatalf("m1 quota_day=%d enabled=%v, want -1/true", m1.QuotaDay, m1.Enabled)
	}

	// 密钥：按 token 哈希可查，前缀与属主/分组正确，绑定指向 m1。
	key, err := store.GetAPIKeyByTokenHash(ctx, model.HashToken(plainToken))
	if err != nil {
		t.Fatalf("query by imported token hash: %v", err)
	}
	if key.ProxyTokenPrefix != model.DisplayTokenPrefix(plainToken) {
		t.Fatalf("proxy_token_prefix = %q, want %q", key.ProxyTokenPrefix, model.DisplayTokenPrefix(plainToken))
	}
	if key.ProxyTokenPrefix == plainToken || key.ProxyTokenHash == plainToken {
		t.Fatalf("plaintext token should not be persisted: %+v", key)
	}
	if key.UserID != alice.ID || key.GroupID != g1.ID {
		t.Fatalf("key user=%d group=%d, want user=%d group=%d", key.UserID, key.GroupID, alice.ID, g1.ID)
	}
	bindings, err := store.ListKeyModels(ctx, key.ID, false)
	if err != nil {
		t.Fatalf("ListKeyModels: %v", err)
	}
	if len(bindings) != 1 || bindings[0].AIModelID != m1.ID || bindings[0].Role != model.ModelRolePrimary {
		t.Fatalf("bindings = %+v, want single primary -> m1(%d)", bindings, m1.ID)
	}
}

// TestImport_ResponseNeverEchoesPlaintext 验证任意长度的 token 都以展示前缀呈现，明文绝不整串回显。
func TestImport_ResponseNeverEchoesPlaintext(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	tokens := []string{"sk-short", "sk-exactly12", "sk-a-very-long-legacy-token-0123456789"}
	keys := make([]map[string]any, 0, len(tokens))
	for i, tok := range tokens {
		keys = append(keys, map[string]any{
			"name":     "k" + strconv.Itoa(i),
			"user":     "ghost", // 属主不存在 → 逐条记错误，token 已参与 ref 计算
			"token":    tok,
			"bindings": []map[string]any{{"model": "m1", "role": "primary"}},
		})
	}
	resp, raw := runImport(t, srv, adminToken, map[string]any{"dry_run": false, "keys": keys})

	if resp.Summary.Errors != len(tokens) {
		t.Fatalf("summary = %+v, want errors=%d", resp.Summary, len(tokens))
	}
	for _, tok := range tokens {
		if bytes.Contains(raw, []byte(tok)) {
			t.Fatalf("response body echoed token plaintext %q: %s", tok, raw)
		}
	}
	for _, d := range resp.Details {
		if d.Ref == "" {
			t.Fatalf("detail should carry display prefix ref: %+v", d)
		}
	}
}

// TestImport_SecondRunAllSkipped 验证重复导入幂等（不产生重复数据、全部 skipped）。
func TestImport_SecondRunAllSkipped(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ctx := context.Background()

	first, _ := runImport(t, srv, adminToken, importFixture(false))
	if first.Summary.Created != 4 {
		t.Fatalf("first import summary = %+v, want created=4", first.Summary)
	}

	second, _ := runImport(t, srv, adminToken, importFixture(false))
	if want := (importSummaryResp{Skipped: 4}); second.Summary != want {
		t.Fatalf("second import summary = %+v, want %+v", second.Summary, want)
	}
	assertCounts(t, second)
	for _, d := range second.Details {
		if d.Action != "skipped" {
			t.Fatalf("second import entry %+v should be skipped", d)
		}
	}

	groups, _ := store.ListGroups(ctx)
	users, _ := store.ListUsers(ctx)
	models, _ := store.ListAIModels(ctx)
	keys, _ := store.ListAPIKeys(ctx)
	if len(groups) != 1 || len(users) != 2 || len(models) != 1 || len(keys) != 1 {
		t.Fatalf(
			"repeated import produced duplicate data: groups=%d users=%d models=%d keys=%d",
			len(groups),
			len(users),
			len(models),
			len(keys),
		)
	}
}

// TestImport_MissingGroupReferenceRecordsError 验证用户引用的分组不存在时记错误且不中断。
func TestImport_MissingGroupReferenceRecordsError(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ctx := context.Background()

	payload := map[string]any{
		"dry_run":     false,
		"user_groups": []map[string]any{{"name": "g1"}},
		"users": []map[string]any{
			{"username": "bob", "role": "user", "group": "nope"},
			{"username": "alice", "role": "user", "group": "g1"},
		},
	}
	resp, _ := runImport(t, srv, adminToken, payload)

	if want := (importSummaryResp{Created: 2, Errors: 1}); resp.Summary != want {
		t.Fatalf("summary = %+v, want %+v", resp.Summary, want)
	}
	bad := findDetail(t, resp, "user", "bob")
	if bad.Action != "error" || bad.Message != "group not found: nope" {
		t.Fatalf("bob detail = %+v", bad)
	}
	if ok := findDetail(t, resp, "user", "alice"); ok.Action != "created" {
		t.Fatalf("alice detail = %+v, want created", ok)
	}
	users, _ := store.ListUsers(ctx)
	if findUserByName(users, "bob") != nil {
		t.Fatalf("user with missing reference should not persist")
	}
	if findUserByName(users, "alice") == nil {
		t.Fatalf("valid user alice should persist")
	}
}

// TestImport_BadKeyReferencesRecordErrorAndContinue 验证密钥的属主/绑定模型缺失时逐条报错且不影响其他条目。
func TestImport_BadKeyReferencesRecordErrorAndContinue(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ctx := context.Background()

	payload := map[string]any{
		"dry_run":     false,
		"user_groups": []map[string]any{{"name": "g1"}},
		"users":       []map[string]any{{"username": "alice", "role": "user", "group": "g1"}},
		"models": []map[string]any{
			{
				"provider":   "mock",
				"model_id":   "m1",
				"url":        "http://upstream.example/v1",
				"model_type": "openai-completions",
			},
		},
		"keys": []map[string]any{
			{"name": "ghost-owner", "user": "ghost", "token": "sk-ghost-owner-0001",
				"bindings": []map[string]any{{"model": "m1", "role": "primary"}}},
			{"name": "bad-model", "user": "alice", "token": "sk-bad-model-0001",
				"bindings": []map[string]any{{"model": "missing", "role": "primary"}}},
			{"name": "no-primary", "user": "alice", "token": "sk-no-primary-0001",
				"bindings": []map[string]any{{"model": "m1", "role": "fallback"}}},
			{"name": "k1", "user": "alice", "token": "sk-legacy-plain",
				"bindings": []map[string]any{{"model": "m1", "role": "primary"}}},
		},
	}
	resp, _ := runImport(t, srv, adminToken, payload)

	if want := (importSummaryResp{Created: 4, Errors: 3}); resp.Summary != want {
		t.Fatalf("summary = %+v, want %+v", resp.Summary, want)
	}
	assertCounts(t, resp)

	if d := findDetail(t, resp, "key", model.DisplayTokenPrefix("sk-ghost-owner-0001")); d.Action != "error" ||
		d.Message != "user not found: ghost" {
		t.Fatalf("ghost-owner detail = %+v", d)
	}
	// ref 为 token 展示前缀（首 8 位 + … + 末 4 位），绝不等于 token 明文。
	for _, tc := range []struct{ token, message string }{
		{"sk-bad-model-0001", "model not found: missing"},
		{"sk-no-primary-0001", "bindings must contain exactly one primary model"},
	} {
		d := findDetail(t, resp, "key", model.DisplayTokenPrefix(tc.token))
		if d.Action != "error" || d.Message != tc.message {
			t.Fatalf("token %s detail = %+v, want error %q", tc.token, d, tc.message)
		}
		if d.Ref == tc.token {
			t.Fatalf("details.ref echoed token plaintext: %q", d.Ref)
		}
	}
	if d := findDetail(t, resp, "key", model.DisplayTokenPrefix("sk-legacy-plain")); d.Action != "created" {
		t.Fatalf("k1 detail = %+v, want created", d)
	}

	// 仅合法密钥落库。
	keys, _ := store.ListAPIKeys(ctx)
	if len(keys) != 1 {
		t.Fatalf("persisted key count = %d, want 1", len(keys))
	}
	if _, err := store.GetAPIKeyByTokenHash(ctx, model.HashToken("sk-legacy-plain")); err != nil {
		t.Fatalf("valid key should be found by hash: %v", err)
	}
}

// TestImport_InvalidQuotaRulesRecordsError 验证分组配额规则非法时记错误且不落库。
func TestImport_InvalidQuotaRulesRecordsError(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	ctx := context.Background()

	payload := map[string]any{
		"dry_run": false,
		"user_groups": []map[string]any{
			{"name": "bad", "token_quota_rules": `[{"mode":"day","limit":-2}]`},
			{"name": "good", "token_quota_rules": `[]`},
		},
	}
	resp, _ := runImport(t, srv, adminToken, payload)

	if want := (importSummaryResp{Created: 1, Errors: 1}); resp.Summary != want {
		t.Fatalf("summary = %+v, want %+v", resp.Summary, want)
	}
	bad := findDetail(t, resp, "group", "bad")
	if bad.Action != "error" || bad.Message == "" {
		t.Fatalf("bad detail = %+v, want error", bad)
	}
	groups, _ := store.ListGroups(ctx)
	if findGroupByName(groups, "bad") != nil {
		t.Fatalf("group with invalid quota rules should not persist")
	}
	good := findGroupByName(groups, "good")
	if good == nil || good.TokenQuotaRules != "[]" {
		t.Fatalf("valid group with explicit unlimited should persist: %+v", good)
	}
}

// TestImport_DryRunValidatesReferences 验证预演同样做引用与绑定校验（与真实导入结论一致）。
func TestImport_DryRunValidatesReferences(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	payload := importFixture(true)
	payload["keys"] = []map[string]any{
		{"name": "k1", "user": "alice", "token": "sk-legacy-plain",
			"bindings": []map[string]any{{"model": "missing", "role": "primary"}}},
	}
	resp, _ := runImport(t, srv, adminToken, payload)

	// 分组/用户/模型可预演创建，密钥因绑定模型缺失记错误。
	if want := (importSummaryResp{Created: 3, Errors: 1}); resp.Summary != want {
		t.Fatalf("summary = %+v, want %+v", resp.Summary, want)
	}
	if d := findDetail(t, resp, "key", model.DisplayTokenPrefix("sk-legacy-plain")); d.Action != "error" {
		t.Fatalf("key detail = %+v, want error", d)
	}
}

// TestImport_RequiresAdmin 验证导入端点在管理面鉴权内。
func TestImport_RequiresAdmin(t *testing.T) {
	srv, store := newTestServer(t)
	_, userToken := seedUser(t, store, "plain", model.RoleUser, true)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/import", "", importFixture(true))
	assertStatus(t, status, http.StatusUnauthorized, body)

	status, body = doRequest(t, srv, http.MethodPost, "/admin/import", userToken, importFixture(true))
	assertStatus(t, status, http.StatusForbidden, body)
}
