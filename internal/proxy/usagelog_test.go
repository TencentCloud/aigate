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

package proxy_test

import (
	"context"
	"testing"

	"aigate/internal/model"
	"aigate/internal/proxy"
)

// TestUsageWriter_DroppedCountsOverflow 覆盖有界队列满且无消费者时的丢弃计数：
// buffer=1 时第一条入队，后续两条直接丢弃。
func TestUsageWriter_DroppedCountsOverflow(t *testing.T) {
	store, _ := newTestStore(t)

	w := proxy.NewUsageWriter(store, 1)
	// 故意不调用 Start：没有消费者，队列容量恒为 1。
	for i := 0; i < 3; i++ {
		w.Submit(model.LLMUsageLog{TotalTokens: 1, StatusCode: 200})
	}
	if got := w.Dropped(); got < 2 {
		t.Fatalf("Dropped() = %d, want >= 2", got)
	}
}

// TestUsageWriter_StartStopFlushesQueue 覆盖 Start 消费 + Stop 排空后记录落库：
// 表侧断言 llm_usage_logs 逐条落库、daily_usage_summaries 聚合累计。
func TestUsageWriter_StartStopFlushesQueue(t *testing.T) {
	store, dsn := newTestStore(t)
	read := openReadDB(t, dsn)

	w := proxy.NewUsageWriter(store, 8)
	w.Start()
	for i := 0; i < 3; i++ {
		w.Submit(model.LLMUsageLog{
			APIKeyID:         7,
			UserID:           11,
			GroupID:          3,
			AIModelID:        42,
			Model:            "mock-a",
			Provider:         "mock-provider",
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
			StatusCode:       200,
		})
	}
	w.Stop(context.Background())

	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0 (consumer running)", got)
	}

	if n := countRows(t, read, &model.LLMUsageLog{}); n != 3 {
		t.Fatalf("llm_usage_logs rows = %d, want 3", n)
	}
	if n := countRows(t, read, &model.DailyUsageSummary{}); n != 1 {
		t.Fatalf("daily_usage_summaries rows = %d, want 1 (aggregated by date/user/key/model)", n)
	}

	var summary model.DailyUsageSummary
	if err := read.First(&summary).Error; err != nil {
		t.Fatalf("load daily summary: %v", err)
	}
	if summary.UserID != 11 || summary.APIKeyID != 7 || summary.AIModelID != 42 {
		t.Fatalf("daily summary identity = %+v, want user=11 key=7 model=42", summary)
	}
	if summary.PromptTokens != 30 || summary.CompletionTokens != 15 || summary.TotalTokens != 45 {
		t.Fatalf("daily summary tokens = %+v, want prompt=30 completion=15 total=45", summary)
	}
	if summary.RequestCount != 3 {
		t.Fatalf("daily summary request_count = %d, want 3", summary.RequestCount)
	}
}

// TestUsageWriter_SubmitAfterStopDoesNotPanic 覆盖停机后的 Submit：
// 不得 panic（send on closed channel），并计入丢弃；重复 Stop 也不 panic。
func TestUsageWriter_SubmitAfterStopDoesNotPanic(t *testing.T) {
	store, _ := newTestStore(t)

	w := proxy.NewUsageWriter(store, 4)
	w.Start()
	w.Stop(context.Background())

	before := w.Dropped()
	for i := 0; i < 2; i++ {
		w.Submit(model.LLMUsageLog{TotalTokens: 1})
	}
	if got := w.Dropped() - before; got != 2 {
		t.Fatalf("dropped after stop = %d, want 2", got)
	}
	w.Stop(context.Background()) // 重复 Stop：空操作
}
