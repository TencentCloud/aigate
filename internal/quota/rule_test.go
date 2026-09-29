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

package quota_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"aigate/internal/quota"
)

// ParseRules：区分「未设置/非法」与「有值」；"[]" 为有值且长度 0。
func TestParseRules(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantOK  bool
		wantLen int
	}{
		{"empty string means unset", "", false, 0},
		{"empty array means explicitly unlimited", "[]", true, 0},
		{"null normalized to empty set", "null", true, 0},
		{"single rule", `[{"mode":"day","limit":100}]`, true, 1},
		{"object not array", `{"mode":"day"}`, false, 0},
		{"truncated json", `[{"mode":"day"`, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules, ok := quota.ParseRules(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ParseRules(%q) ok = %v, want %v", tc.raw, ok, tc.wantOK)
			}
			if len(rules) != tc.wantLen {
				t.Fatalf("ParseRules(%q) len = %d, want %d", tc.raw, len(rules), tc.wantLen)
			}
			if ok && rules == nil {
				t.Fatalf("ParseRules(%q) rules = nil, want non-nil when ok", tc.raw)
			}
		})
	}
}

// ParseRules：字段完整解析（含指针字段）。
func TestParseRules_Fields(t *testing.T) {
	rules, ok := quota.ParseRules(`[{"mode":"custom","limit":7,"start":100,"end":200,"refresh":"monthly"}]`)
	if !ok || len(rules) != 1 {
		t.Fatalf("ParseRules = (%v, %v), want 1 rule", rules, ok)
	}
	r := rules[0]
	if r.Mode != quota.ModeCustom || r.Limit != 7 {
		t.Fatalf("rule = %+v, want mode=custom limit=7", r)
	}
	if r.Start == nil || *r.Start != 100 || r.End == nil || *r.End != 200 {
		t.Fatalf("rule start/end = %v/%v, want 100/200", r.Start, r.End)
	}
	if r.Refresh != quota.RefreshMonthly {
		t.Fatalf("rule refresh = %q, want %q", r.Refresh, quota.RefreshMonthly)
	}
}

// MarshalRules：序列化前的规范化（custom 补 refresh=none、非 custom 清除多余字段）。
func TestMarshalRules(t *testing.T) {
	t.Run("empty set outputs empty array", func(t *testing.T) {
		if got := quota.MarshalRules(nil); got != "[]" {
			t.Fatalf("MarshalRules(nil) = %q, want []", got)
		}
		if got := quota.MarshalRules([]quota.Rule{}); got != "[]" {
			t.Fatalf("MarshalRules(empty) = %q, want []", got)
		}
	})

	t.Run("non-custom clears start/end/refresh", func(t *testing.T) {
		got := quota.MarshalRules([]quota.Rule{{
			Mode: quota.ModeDay, Limit: 100,
			Start: i64(1710000000), End: i64(1710003600), Refresh: quota.RefreshDaily,
		}})
		if got != `[{"mode":"day","limit":100}]` {
			t.Fatalf("MarshalRules = %q, want %q", got, `[{"mode":"day","limit":100}]`)
		}
	})

	t.Run("custom empty refresh defaults to none", func(t *testing.T) {
		got := quota.MarshalRules([]quota.Rule{{
			Mode: quota.ModeCustom, Limit: 100, Start: i64(1710000000),
		}})
		if got != `[{"mode":"custom","limit":100,"start":1710000000,"refresh":"none"}]` {
			t.Fatalf("MarshalRules = %q", got)
		}
	})

	t.Run("custom keeps explicit refresh", func(t *testing.T) {
		got := quota.MarshalRules([]quota.Rule{{
			Mode: quota.ModeCustom, Limit: 100, Start: i64(1710000000), Refresh: quota.RefreshYearly,
		}})
		if got != `[{"mode":"custom","limit":100,"start":1710000000,"refresh":"yearly"}]` {
			t.Fatalf("MarshalRules = %q", got)
		}
	})

	t.Run("does not mutate input", func(t *testing.T) {
		in := []quota.Rule{{Mode: quota.ModeDay, Limit: 100, Start: i64(1710000000), Refresh: quota.RefreshDaily}}
		_ = quota.MarshalRules(in)
		if in[0].Start == nil || in[0].Refresh != quota.RefreshDaily {
			t.Fatalf("MarshalRules mutated input: %+v", in[0])
		}
	})
}

// ValidateRules：合法集合与各类非法输入的错误文案（恒英文）。
func TestValidateRules(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"empty set valid", "[]", ""},
		{"single day rule valid", `[{"mode":"day","limit":1}]`, ""},
		{
			"day/month/year combination valid",
			`[{"mode":"day","limit":1},{"mode":"month","limit":2},{"mode":"year","limit":3}]`,
			"",
		},
		{"limit=-1 valid", `[{"mode":"day","limit":-1}]`, ""},
		{
			"duplicate mode rejected",
			`[{"mode":"day","limit":1},{"mode":"day","limit":2}]`,
			`duplicate quota mode "day"`,
		},
		{
			"duplicate custom rejected",
			`[{"mode":"custom","limit":1,"start":100},{"mode":"custom","limit":2,"start":200}]`,
			`duplicate quota mode "custom"`,
		},
		{"limit=-2 rejected", `[{"mode":"day","limit":-2}]`, "limit must be >= -1"},
		{"unknown mode rejected", `[{"mode":"week","limit":1}]`, `invalid quota mode "week"`},
		{"custom refresh invalid", `[{"mode":"custom","limit":1,"refresh":"hourly"}]`, `invalid refresh mode "hourly"`},
		{"custom empty refresh valid", `[{"mode":"custom","limit":1,"start":100}]`, ""},
		{"custom refresh none valid", `[{"mode":"custom","limit":1,"start":100,"refresh":"none"}]`, ""},
		{"custom refresh daily valid", `[{"mode":"custom","limit":1,"start":100,"refresh":"daily"}]`, ""},
		{"custom refresh monthly valid", `[{"mode":"custom","limit":1,"start":100,"refresh":"monthly"}]`, ""},
		{"custom refresh yearly valid", `[{"mode":"custom","limit":1,"start":100,"refresh":"yearly"}]`, ""},
		{
			"end equal to start rejected",
			`[{"mode":"custom","limit":1,"start":200,"end":200}]`,
			"quota end must be after start",
		},
		{
			"end before start rejected",
			`[{"mode":"custom","limit":1,"start":300,"end":200}]`,
			"quota end must be after start",
		},
		{"end after start valid", `[{"mode":"custom","limit":1,"start":100,"end":200}]`, ""},
		{"missing start only checks end without error", `[{"mode":"custom","limit":1,"end":200}]`, ""},
		{"non-custom ignores extra fields", `[{"mode":"day","limit":1,"refresh":"hourly","start":1,"end":2}]`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules, _ := quota.ParseRules(tc.raw)
			err := quota.ValidateRules(rules)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateRules(%s) = %v, want nil", tc.raw, err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("ValidateRules(%s) = %v, want %q", tc.raw, err, tc.wantErr)
			}
		})
	}
}

// NormalizeRules：解析 → 校验 → custom 补齐锚点 → 规范化输出。
func TestNormalizeRules(t *testing.T) {
	t.Run("empty string rejected", func(t *testing.T) {
		_, err := quota.NormalizeRules("")
		if err == nil || err.Error() != "quota rules must not be empty" {
			t.Fatalf("NormalizeRules(\"\") = %v, want quota rules must not be empty", err)
		}
	})

	t.Run("invalid json rejected", func(t *testing.T) {
		_, err := quota.NormalizeRules(`{"mode":"day"}`)
		if err == nil || !strings.Contains(err.Error(), "invalid quota rules json") {
			t.Fatalf("NormalizeRules(invalid) = %v, want invalid quota rules json", err)
		}
	})

	t.Run("validation error propagated", func(t *testing.T) {
		_, err := quota.NormalizeRules(`[{"mode":"day","limit":1},{"mode":"day","limit":2}]`)
		if err == nil || err.Error() != `duplicate quota mode "day"` {
			t.Fatalf("NormalizeRules(dup) = %v", err)
		}
		_, err = quota.NormalizeRules(`[{"mode":"week","limit":1}]`)
		if err == nil || err.Error() != `invalid quota mode "week"` {
			t.Fatalf("NormalizeRules(mode) = %v", err)
		}
	})

	t.Run("empty array stays explicitly unlimited", func(t *testing.T) {
		got, err := quota.NormalizeRules("[]")
		if err != nil {
			t.Fatalf("NormalizeRules([]) err = %v", err)
		}
		if got != "[]" {
			t.Fatalf("NormalizeRules([]) = %q, want []", got)
		}
	})

	t.Run("custom auto-fills start", func(t *testing.T) {
		before := time.Now().Unix()
		out, err := quota.NormalizeRules(`[{"mode":"custom","limit":100}]`)
		after := time.Now().Unix()
		if err != nil {
			t.Fatalf("NormalizeRules: %v", err)
		}
		rules, ok := quota.ParseRules(out)
		if !ok || len(rules) != 1 {
			t.Fatalf("normalized = %q, want 1 rule", out)
		}
		if rules[0].Start == nil {
			t.Fatalf("normalized start = nil, want auto-filled")
		}
		if *rules[0].Start < before || *rules[0].Start > after {
			t.Fatalf("normalized start = %d, want within [%d, %d]", *rules[0].Start, before, after)
		}
		if rules[0].Refresh != quota.RefreshNone {
			t.Fatalf("normalized refresh = %q, want none", rules[0].Refresh)
		}
	})

	t.Run("existing start not overwritten", func(t *testing.T) {
		out, err := quota.NormalizeRules(`[{"mode":"custom","limit":100,"start":1710000000,"refresh":"daily"}]`)
		if err != nil {
			t.Fatalf("NormalizeRules: %v", err)
		}
		rules, _ := quota.ParseRules(out)
		if rules[0].Start == nil || *rules[0].Start != 1710000000 || rules[0].Refresh != quota.RefreshDaily {
			t.Fatalf("normalized = %q, want start 1710000000 refresh daily", out)
		}
	})

	t.Run("non-custom clears window fields", func(t *testing.T) {
		out, err := quota.NormalizeRules(
			`[{"mode":"day","limit":100,"start":1710000000,"end":1710003600,"refresh":"daily"}]`,
		)
		if err != nil {
			t.Fatalf("NormalizeRules: %v", err)
		}
		if out != `[{"mode":"day","limit":100}]` {
			t.Fatalf("NormalizeRules = %q, want %q", out, `[{"mode":"day","limit":100}]`)
		}
	})

	t.Run("past end without start rejected", func(t *testing.T) {
		// 若先校验后补 start，这条规则会被落库成 end < start（永久 fail-closed）。
		_, err := quota.NormalizeRules(`[{"mode":"custom","limit":100,"end":1710000000}]`)
		if err == nil || !strings.Contains(err.Error(), "end must be after start") {
			t.Fatalf("NormalizeRules(past end) = %v, want end after start error", err)
		}
	})

	t.Run("future end without start accepted", func(t *testing.T) {
		future := time.Now().Add(24 * time.Hour).Unix()
		out, err := quota.NormalizeRules(fmt.Sprintf(`[{"mode":"custom","limit":100,"end":%d}]`, future))
		if err != nil {
			t.Fatalf("NormalizeRules: %v", err)
		}
		rules, _ := quota.ParseRules(out)
		if rules[0].Start == nil || rules[0].End == nil || *rules[0].End <= *rules[0].Start {
			t.Fatalf("normalized = %q, want start filled before end", out)
		}
	})
}
