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
	"encoding/json"
	"net/http"
	"testing"
)

// TestGetSettings_Defaults 覆盖首次读取返回默认值并落库单例。
func TestGetSettings_Defaults(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodGet, "/admin/settings", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	got := mustDecode[settingsResp](t, body)
	if got.DefaultTokenQuotaRules != "[]" {
		t.Errorf("default_token_quota_rules default want [], got %q", got.DefaultTokenQuotaRules)
	}
	if got.GlobalTokenQuotaRules != "[]" {
		t.Errorf("global_token_quota_rules default want [], got %q", got.GlobalTokenQuotaRules)
	}
	if got.GlobalQuotaScope != "site" {
		t.Errorf("global_quota_scope default want site, got %q", got.GlobalQuotaScope)
	}
}

// TestUpdateSettings_Partial 覆盖局部更新：只改传入字段，其余保持。
func TestUpdateSettings_Partial(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)
	const rules = `[{"mode":"day","limit":2000000}]`

	status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
		"default_token_quota_rules": rules,
	})
	assertStatus(t, status, http.StatusOK, body)
	got := mustDecode[settingsResp](t, body)
	if got.DefaultTokenQuotaRules != rules {
		t.Errorf("default_token_quota_rules want %q, got %q", rules, got.DefaultTokenQuotaRules)
	}
	if got.GlobalTokenQuotaRules != "[]" || got.GlobalQuotaScope != "site" {
		t.Errorf("unset fields should not change, got %+v", got)
	}

	t.Run("switch scope", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
			"global_quota_scope": "group",
		})
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[settingsResp](t, body); got.GlobalQuotaScope != "group" {
			t.Errorf("global_quota_scope want group, got %q", got.GlobalQuotaScope)
		}
	})

	t.Run("read back persisted value", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/settings", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[settingsResp](t, body)
		if got.DefaultTokenQuotaRules != rules {
			t.Errorf("read-back default_token_quota_rules want %q, got %q", rules, got.DefaultTokenQuotaRules)
		}
		if got.GlobalQuotaScope != "group" {
			t.Errorf("read-back global_quota_scope want group, got %q", got.GlobalQuotaScope)
		}
	})
}

// TestUpdateSettings_Validation 覆盖配额规则格式轻校验与作用域枚举校验。
func TestUpdateSettings_Validation(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	invalidRules := []string{"not-json", "{}", "null", `""`, ""}
	for _, raw := range invalidRules {
		t.Run("invalid rule "+raw, func(t *testing.T) {
			status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
				"default_token_quota_rules": raw,
			})
			assertStatus(t, status, http.StatusBadRequest, body)
		})
	}

	t.Run("invalid scope", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
			"global_quota_scope": "global",
		})
		assertStatus(t, status, http.StatusBadRequest, body)
	})

	t.Run("empty object does not error", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{})
		assertStatus(t, status, http.StatusOK, body)
	})
}

// TestUpdateSettings_RuleValidation 覆盖配额规则深度校验与规范化：
// 重复 mode / limit<-1 / 非法 mode / 非法 refresh → 400；custom 缺 start 自动填充锚点后落库。
func TestUpdateSettings_RuleValidation(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)

	invalid := []string{
		`[{"mode":"day","limit":1},{"mode":"day","limit":2}]`,
		`[{"mode":"day","limit":-2}]`,
		`[{"mode":"week","limit":1}]`,
		`[{"mode":"custom","limit":1,"refresh":"hourly"}]`,
		`[{"mode":"custom","limit":1,"start":200,"end":100}]`,
	}
	for _, raw := range invalid {
		t.Run("invalid rule "+raw, func(t *testing.T) {
			status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
				"default_token_quota_rules": raw,
			})
			assertStatus(t, status, http.StatusBadRequest, body)
		})
	}

	t.Run("custom anchor auto-filled and normalized", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
			"default_token_quota_rules": `[{"mode":"custom","limit":100}]`,
		})
		assertStatus(t, status, http.StatusOK, body)

		var rules []struct {
			Mode    string `json:"mode"`
			Limit   int    `json:"limit"`
			Start   *int64 `json:"start"`
			Refresh string `json:"refresh"`
		}
		got := mustDecode[settingsResp](t, body)
		if err := json.Unmarshal([]byte(got.DefaultTokenQuotaRules), &rules); err != nil {
			t.Fatalf("decode persisted rules: %v, rules: %s", err, got.DefaultTokenQuotaRules)
		}
		if len(rules) != 1 || rules[0].Mode != "custom" || rules[0].Limit != 100 ||
			rules[0].Start == nil || rules[0].Refresh != "none" {
			t.Errorf("custom rule should fill in start and refresh=none, got %s", got.DefaultTokenQuotaRules)
		}
	})

	t.Run("global rules empty array means explicitly unlimited", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
			"global_token_quota_rules": "[]",
		})
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[settingsResp](t, body); got.GlobalTokenQuotaRules != "[]" {
			t.Errorf("global_token_quota_rules want [], got %q", got.GlobalTokenQuotaRules)
		}
	})

	t.Run("empty string explicitly rejected", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken, map[string]any{
			"global_token_quota_rules": "",
		})
		assertStatus(t, status, http.StatusBadRequest, body)
	})
}
