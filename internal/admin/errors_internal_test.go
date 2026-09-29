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

package admin

// 白盒单测：管理面错误响应兜底（500 通用文案、不泄露内部细节）
// 与写错误映射（参数校验 → 400 原文；其余 → 500）。这些路径此前零覆盖。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/model"
)

func TestWriteInternalError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeInternalError(rec, errors.New("dial tcp 10.0.0.1:3306: connection refused"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "internal server error") {
		t.Fatalf("body = %q, want the generic message", body)
	}
	if strings.Contains(body, "10.0.0.1") || strings.Contains(body, "connection refused") {
		t.Fatalf("body leaked internal details: %q", body)
	}
}

func TestWriteMutationError(t *testing.T) {
	// 参数校验类（model.ValidationError）→ 400 原文：从 model 层取真实构造的校验错误。
	store, err := model.Open(model.DBTypeSQLite, filepath.Join(t.TempDir(), "admin-errors.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.AutoMigrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	vErr := store.CreateAPIKey(context.Background(),
		&model.APIKey{Name: "k", ProxyTokenHash: "hash-validation", ProxyTokenPrefix: "sk-v"},
		[]model.APIKeyModel{
			{AIModelID: 1, Role: model.ModelRolePrimary},
			{AIModelID: 2, Role: model.ModelRolePrimary},
		})
	if !model.IsValidationError(vErr) {
		t.Fatalf("expected a validation error from CreateAPIKey, got %v", vErr)
	}
	rec := httptest.NewRecorder()
	writeMutationError(rec, vErr)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), vErr.Error()) {
		t.Fatalf("validation mapping = %d %q, want 400 with %q", rec.Code, rec.Body.String(), vErr.Error())
	}

	// 其余（事务/驱动错误）→ 500 通用文案、不回显原文。
	rec = httptest.NewRecorder()
	writeMutationError(rec, errors.New("tx commit failed on 10.0.0.1"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("internal mapping status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.1") {
		t.Fatalf("internal mapping leaked details: %q", rec.Body.String())
	}
}
