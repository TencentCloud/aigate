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

package model_test

import (
	"testing"
	"time"

	"aigate/internal/model"
)

// InsertUsageLog 全字段落库。
func TestInsertUsageLog_PersistsAllFields(t *testing.T) {
	store, dsn := newTestStoreAt(t)
	db := openReadDB(t, dsn)
	ctx := testCtx()

	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	entry := &model.LLMUsageLog{
		APIKeyID:               11,
		UserID:                 22,
		GroupID:                33,
		AIModelID:              44,
		Model:                  "gpt-x",
		Provider:               "openai",
		PromptTokens:           10,
		CompletionTokens:       20,
		TotalTokens:            30,
		PromptCacheReadTokens:  1,
		PromptCacheWriteTokens: 2,
		StatusCode:             200,
		Latency:                123,
		CreatedAt:              created,
	}
	if err := store.InsertUsageLog(ctx, entry); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	if entry.ID == 0 {
		t.Fatal("InsertUsageLog should assign ID")
	}

	var got model.LLMUsageLog
	if err := db.First(&got, entry.ID).Error; err != nil {
		t.Fatalf("read back usage log: %v", err)
	}
	if got.ID != entry.ID {
		t.Fatalf("ID = %d, want %d", got.ID, entry.ID)
	}
	if got.APIKeyID != 11 || got.UserID != 22 || got.GroupID != 33 || got.AIModelID != 44 {
		t.Fatalf("foreign keys = %d/%d/%d/%d, want 11/22/33/44", got.APIKeyID, got.UserID, got.GroupID, got.AIModelID)
	}
	if got.Model != "gpt-x" || got.Provider != "openai" {
		t.Fatalf("model/provider = %q/%q, want gpt-x/openai", got.Model, got.Provider)
	}
	if got.PromptTokens != 10 || got.CompletionTokens != 20 || got.TotalTokens != 30 {
		t.Fatalf("tokens = %d/%d/%d, want 10/20/30", got.PromptTokens, got.CompletionTokens, got.TotalTokens)
	}
	if got.PromptCacheReadTokens != 1 || got.PromptCacheWriteTokens != 2 {
		t.Fatalf("cache tokens = %d/%d, want 1/2", got.PromptCacheReadTokens, got.PromptCacheWriteTokens)
	}
	if got.StatusCode != 200 || got.Latency != 123 {
		t.Fatalf("status/latency = %d/%d, want 200/123", got.StatusCode, got.Latency)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt = %s, want %s", got.CreatedAt, created)
	}
}

// CreatedAt 为空时按当前时间补齐（不落零值时间）。
func TestInsertUsageLog_DefaultsCreatedAt(t *testing.T) {
	store, dsn := newTestStoreAt(t)
	db := openReadDB(t, dsn)
	ctx := testCtx()

	before := time.Now().UTC()
	entry := &model.LLMUsageLog{UserID: 1, TotalTokens: 5}
	if err := store.InsertUsageLog(ctx, entry); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}

	var got model.LLMUsageLog
	if err := db.First(&got, entry.ID).Error; err != nil {
		t.Fatalf("read back usage log: %v", err)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be filled in")
	}
	if got.CreatedAt.Before(before.Add(-time.Minute)) || got.CreatedAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("CreatedAt = %s, want around now (%s)", got.CreatedAt, before)
	}
}

// UpsertDailyUsage：同（日期,用户,密钥,模型）两次 → 单行、token 累加、request_count=2；
// total_tokens<=0 跳过；不同模型分属不同聚合行。
func TestUpsertDailyUsage_AccumulatesGroupsAndSkips(t *testing.T) {
	store, dsn := newTestStoreAt(t)
	db := openReadDB(t, dsn)
	ctx := testCtx()

	first := &model.LLMUsageLog{
		UserID: 1, APIKeyID: 2, AIModelID: 3, GroupID: 4,
		PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30,
		PromptCacheReadTokens: 1, PromptCacheWriteTokens: 2,
	}
	second := &model.LLMUsageLog{
		UserID: 1, APIKeyID: 2, AIModelID: 3, GroupID: 4,
		PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12,
	}
	if err := store.UpsertDailyUsage(ctx, first); err != nil {
		t.Fatalf("UpsertDailyUsage(first): %v", err)
	}
	if err := store.UpsertDailyUsage(ctx, second); err != nil {
		t.Fatalf("UpsertDailyUsage(second): %v", err)
	}

	// 另一模型 → 新聚合行。
	other := &model.LLMUsageLog{UserID: 1, APIKeyID: 2, AIModelID: 99, GroupID: 4, TotalTokens: 100}
	if err := store.UpsertDailyUsage(ctx, other); err != nil {
		t.Fatalf("UpsertDailyUsage(other model): %v", err)
	}

	// total_tokens<=0 跳过（0 与负数均不产生新行、不累加）。
	skipped := []*model.LLMUsageLog{
		{UserID: 7, APIKeyID: 8, AIModelID: 9, TotalTokens: 0},
		{UserID: 7, APIKeyID: 8, AIModelID: 9, TotalTokens: -5},
		{UserID: 1, APIKeyID: 2, AIModelID: 3, PromptTokens: 999, TotalTokens: 0},
	}
	for i, e := range skipped {
		if err := store.UpsertDailyUsage(ctx, e); err != nil {
			t.Fatalf("UpsertDailyUsage(skipped[%d]): %v", i, err)
		}
	}

	var rows []model.DailyUsageSummary
	if err := db.Order("ai_model_id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("read daily summaries: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("daily summary rows = %d, want 2 (%+v)", len(rows), rows)
	}

	agg := rows[0]
	if agg.AIModelID != 3 {
		t.Fatalf("rows[0].AIModelID = %d, want 3", agg.AIModelID)
	}
	if agg.RequestCount != 2 {
		t.Fatalf("RequestCount = %d, want 2", agg.RequestCount)
	}
	if agg.PromptTokens != 15 || agg.CompletionTokens != 27 || agg.TotalTokens != 42 {
		t.Fatalf("aggregated tokens = %d/%d/%d, want 15/27/42", agg.PromptTokens, agg.CompletionTokens, agg.TotalTokens)
	}
	if agg.PromptCacheReadTokens != 1 || agg.PromptCacheWriteTokens != 2 {
		t.Fatalf("aggregated cache tokens = %d/%d, want 1/2", agg.PromptCacheReadTokens, agg.PromptCacheWriteTokens)
	}
	if rows[1].AIModelID != 99 || rows[1].TotalTokens != 100 || rows[1].RequestCount != 1 {
		t.Fatalf("rows[1] = %+v, want model 99 total 100 request_count 1", rows[1])
	}

	if n := countRows(t, db, &model.DailyUsageSummary{}, "user_id = ?", 7); n != 0 {
		t.Fatalf("skipped entries created %d rows, want 0", n)
	}
}

// 日聚合的日期标签取业务时区「当日 00:00 的 UTC 时刻」。
func TestUpsertDailyUsage_DateUsesBusinessTimezone(t *testing.T) {
	store, dsn := newTestStoreAt(t)
	db := openReadDB(t, dsn)
	ctx := testCtx()

	loc := time.FixedZone("UTC+12", 12*60*60)
	model.SetLocation(loc)
	t.Cleanup(func() { model.SetLocation(time.UTC) })

	if err := store.UpsertDailyUsage(
		ctx,
		&model.LLMUsageLog{UserID: 1, APIKeyID: 2, AIModelID: 3, TotalTokens: 5},
	); err != nil {
		t.Fatalf("UpsertDailyUsage: %v", err)
	}

	var got model.DailyUsageSummary
	if err := db.First(&got).Error; err != nil {
		t.Fatalf("read daily summary: %v", err)
	}

	y, m, d := time.Now().In(loc).Date()
	want := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	if !got.Date.Equal(want) {
		t.Fatalf("Date = %s, want business-tz date label %s", got.Date, want)
	}
	if !got.Date.Equal(model.LocalToday()) {
		t.Fatalf("Date = %s, want model.LocalToday() = %s", got.Date, model.LocalToday())
	}
	if _, offset := got.Date.Zone(); offset != 0 {
		t.Fatalf("Date zone offset = %d, want 0 (date-only label stored in UTC)", offset)
	}
}
