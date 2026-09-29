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
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestModelConnectivity_UsesCredentialSecret 覆盖模型引用凭据时的连通性探测：
// 探测必须使用凭据中的 header 密钥（凭据优先），而非模型自身 api_key。
func TestModelConnectivity_UsesCredentialSecret(t *testing.T) {
	allowPrivateUpstreamsForTest(t)
	srv, _, _, adminToken := credServer(t)

	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	cred := createCredential(t, srv, adminToken, "probe-cred", []map[string]any{
		headerSecret("Authorization", "sk-cred-probe"),
	}, nil)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, map[string]any{
		"provider":      "mock",
		"model_id":      "m-cred-probe",
		"model_name":    "m-cred-probe",
		"url":           upstream.URL + "/v1",
		"api_key":       upstreamPlainKey,
		"credential_id": cred.ID,
		"model_type":    "openai-completions",
	})
	assertStatus(t, status, http.StatusCreated, body)
	modelID := mustDecode[modelCredResp](t, body).ID

	status, resp := probeModel(t, srv, adminToken, modelID)
	if status != http.StatusOK || !resp.OK {
		t.Fatalf("probe want ok, got status=%d resp=%+v", status, resp)
	}
	if gotAuth != "Bearer sk-cred-probe" {
		t.Fatalf(
			"upstream Authorization = %q, want credential secret Bearer sk-cred-probe (not model api_key)",
			gotAuth,
		)
	}
}
