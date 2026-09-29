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
	"testing"
	"time"

	"aigate/internal/model"
	"aigate/internal/quota"
)

// CheckGlobal：全站口径与按组隔离口径互不污染；非法 scope 归一为全站。
func TestCheckGlobal_SiteAndGroupIsolation(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	u := mustCreateUser(t, store, "u-global")
	gA := mustCreateGroup(t, store, "A", "")
	gB := mustCreateGroup(t, store, "B", "")
	rules := mustParseRules(t, `[{"mode":"day","limit":100}]`)

	mustLogUsage(t, store, u.ID, gA.ID, 0, 60, now)
	mustLogUsage(t, store, u.ID, gB.ID, 0, 50, now)

	if st := quota.CheckGlobal(testCtx(), store, "group", gA.ID, rules, now); st.Exceeded {
		t.Fatalf("group A = %+v, want not exceeded", st)
	}
	if st := quota.CheckGlobal(testCtx(), store, "group", gB.ID, rules, now); st.Exceeded {
		t.Fatalf("group B = %+v, want not exceeded", st)
	}

	// 组 A 再加 60 → 120 ≥ 100 超限；组 B 不受影响。
	mustLogUsage(t, store, u.ID, gA.ID, 0, 60, now)
	assertStatus(t, quota.CheckGlobal(testCtx(), store, "group", gA.ID, rules, now), quota.Status{
		Exceeded: true, Kind: kindGlobal, Used: 120, Limit: 100, RetryAfter: 10 * time.Hour,
	})
	if st := quota.CheckGlobal(testCtx(), store, "group", gB.ID, rules, now); st.Exceeded {
		t.Fatalf("group B after A exceeded = %+v, want not exceeded", st)
	}

	// 全站口径：60 + 50 + 60 = 170 ≥ 100。
	assertStatus(t, quota.CheckGlobal(testCtx(), store, "site", 0, rules, now), quota.Status{
		Exceeded: true, Kind: kindGlobal, Used: 170, Limit: 100, RetryAfter: 10 * time.Hour,
	})
	// 非法 scope 视为全站口径。
	assertStatus(t, quota.CheckGlobal(testCtx(), store, "unknown", gA.ID, rules, now), quota.Status{
		Exceeded: true, Kind: kindGlobal, Used: 170, Limit: 100, RetryAfter: 10 * time.Hour,
	})
	// 空规则不限制。
	if st := quota.CheckGlobal(testCtx(), store, "site", 0, nil, now); st != (quota.Status{}) {
		t.Fatalf("empty rules = %+v, want zero Status", st)
	}
}

// CheckGlobal：不生效窗口 fail-closed；limit=-1 跳过；空集合不限制。
func TestCheckGlobal_FailClosedAndUnlimited(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	past := localTime(t, "2026-03-01 00:00:00").Unix()
	expiredEnd := localTime(t, "2026-03-15 00:00:00").Unix()
	future := localTime(t, "2026-05-01 00:00:00").Unix()

	t.Run("expired custom window", func(t *testing.T) {
		rules := mustParseRules(
			t,
			fmt.Sprintf(`[{"mode":"custom","limit":10,"start":%d,"end":%d,"refresh":"none"}]`, past, expiredEnd),
		)
		assertStatus(t, quota.CheckGlobal(testCtx(), store, "site", 0, rules, now), quota.Status{
			Exceeded: true, Kind: kindGlobal, Limit: 10,
		})
	})

	t.Run("window not started yet", func(t *testing.T) {
		rules := mustParseRules(t, fmt.Sprintf(`[{"mode":"custom","limit":10,"start":%d,"refresh":"none"}]`, future))
		assertStatus(t, quota.CheckGlobal(testCtx(), store, "site", 0, rules, now), quota.Status{
			Exceeded: true, Kind: kindGlobal, Limit: 10,
		})
	})

	t.Run("limit=-1 skips the rule", func(t *testing.T) {
		rules := mustParseRules(
			t,
			fmt.Sprintf(`[{"mode":"custom","limit":-1,"start":%d,"end":%d,"refresh":"none"}]`, past, expiredEnd),
		)
		if st := quota.CheckGlobal(testCtx(), store, "site", 0, rules, now); st != (quota.Status{}) {
			t.Fatalf("limit=-1 = %+v, want zero Status", st)
		}
	})

	t.Run("empty rule set is unlimited", func(t *testing.T) {
		if st := quota.CheckGlobal(testCtx(), store, "site", 0, []quota.Rule{}, now); st != (quota.Status{}) {
			t.Fatalf("empty rules = %+v, want zero Status", st)
		}
	})
}

// CheckGlobal：RetryAfter 来源（窗口结束时间；无窗口结束信息则为 0）。
func TestCheckGlobal_RetryAfterSource(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-15 12:00:00")
	u := mustCreateUser(t, store, "u-retry")
	mustLogUsage(t, store, u.ID, 0, 0, 100, now)

	// 自然月窗口结束于次月 1 日 00:00（本地）→ 15 天 12 小时。
	monthRules := mustParseRules(t, `[{"mode":"month","limit":100}]`)
	assertStatus(t, quota.CheckGlobal(testCtx(), store, "site", 0, monthRules, now), quota.Status{
		Exceeded: true, Kind: kindGlobal, Used: 100, Limit: 100,
		RetryAfter: 15*24*time.Hour + 12*time.Hour,
	})

	// custom 固定窗口无 end → 无法推算 → 0。
	start := localTime(t, "2026-04-01 00:00:00").Unix()
	customRules := mustParseRules(t, fmt.Sprintf(`[{"mode":"custom","limit":10,"start":%d,"refresh":"none"}]`, start))
	assertStatus(t, quota.CheckGlobal(testCtx(), store, "site", 0, customRules, now), quota.Status{
		Exceeded: true, Kind: kindGlobal, Used: 100, Limit: 10,
	})
}

// CheckUser：groupID 过滤口径（按组只统计该组；0 统计全部；他人用量不计入）。
func TestCheckUser_GroupFilter(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	u := mustCreateUser(t, store, "u-filter")
	other := mustCreateUser(t, store, "u-other")
	gA := mustCreateGroup(t, store, "A", "")
	gB := mustCreateGroup(t, store, "B", "")
	rules := mustParseRules(t, `[{"mode":"day","limit":100}]`)

	mustLogUsage(t, store, u.ID, gA.ID, 0, 40, now)
	mustLogUsage(t, store, u.ID, gB.ID, 0, 200, now)
	mustLogUsage(t, store, other.ID, gA.ID, 0, 1000, now)

	// 按组 A：只计 40。
	if st := quota.CheckUser(testCtx(), store, u.ID, gA.ID, rules, now); st.Exceeded {
		t.Fatalf("group-scoped user check = %+v, want not exceeded", st)
	}
	// 不分组：40 + 200 = 240 ≥ 100。
	assertStatus(t, quota.CheckUser(testCtx(), store, u.ID, 0, rules, now), quota.Status{
		Exceeded: true, Kind: kindUser, Used: 240, Limit: 100, RetryAfter: 10 * time.Hour,
	})
}

// CheckUser：不生效窗口 fail-closed；limit=-1 跳过；空集合不限制；窗口内用量判定。
func TestCheckUser_FailClosedAndLimits(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	u := mustCreateUser(t, store, "u-failclosed")
	past := localTime(t, "2026-03-01 00:00:00").Unix()
	expiredEnd := localTime(t, "2026-03-15 00:00:00").Unix()

	expired := mustParseRules(
		t,
		fmt.Sprintf(`[{"mode":"custom","limit":5,"start":%d,"end":%d,"refresh":"none"}]`, past, expiredEnd),
	)
	assertStatus(t, quota.CheckUser(testCtx(), store, u.ID, 0, expired, now), quota.Status{
		Exceeded: true, Kind: kindUser, Limit: 5,
	})

	skipped := mustParseRules(
		t,
		fmt.Sprintf(`[{"mode":"custom","limit":-1,"start":%d,"end":%d,"refresh":"none"}]`, past, expiredEnd),
	)
	if st := quota.CheckUser(testCtx(), store, u.ID, 0, skipped, now); st != (quota.Status{}) {
		t.Fatalf("limit=-1 = %+v, want zero Status", st)
	}

	mustLogUsage(t, store, u.ID, 0, 0, 5, now)
	dayRules := mustParseRules(t, `[{"mode":"day","limit":5}]`)
	assertStatus(t, quota.CheckUser(testCtx(), store, u.ID, 0, dayRules, now), quota.Status{
		Exceeded: true, Kind: kindUser, Used: 5, Limit: 5, RetryAfter: 10 * time.Hour,
	})

	if st := quota.CheckUser(testCtx(), store, u.ID, 0, nil, now); st != (quota.Status{}) {
		t.Fatalf("empty rules = %+v, want zero Status", st)
	}
}

// CheckModelDaily：日聚合用量口径、边界与 quotaDay<0 跳过。
func TestCheckModelDaily(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	u := mustCreateUser(t, store, "u-model")
	m1 := mustCreateModel(t, store, "m1", 500000)
	m2 := mustCreateModel(t, store, "m2", 100)
	m3 := mustCreateModel(t, store, "m3", 1)
	mustSumDailyUsage(t, store, u.ID, m1.ID, 500000)
	mustSumDailyUsage(t, store, u.ID, m2.ID, 99)

	// 已达上限 → 超限，RetryAfter 为距下一个业务时区零点。
	assertStatus(t, quota.CheckModelDaily(testCtx(), store, m1.ID, 500000, now), quota.Status{
		Exceeded: true, Kind: kindModel, Used: 500000, Limit: 500000, RetryAfter: 10 * time.Hour,
	})

	// 未达上限。
	if st := quota.CheckModelDaily(testCtx(), store, m2.ID, 100, now); st.Exceeded {
		t.Fatalf("m2 under limit = %+v, want not exceeded", st)
	}
	// 恰好达到上限。
	mustSumDailyUsage(t, store, u.ID, m2.ID, 1)
	assertStatus(t, quota.CheckModelDaily(testCtx(), store, m2.ID, 100, now), quota.Status{
		Exceeded: true, Kind: kindModel, Used: 100, Limit: 100, RetryAfter: 10 * time.Hour,
	})
	// 其他模型的用量不计入。
	if st := quota.CheckModelDaily(testCtx(), store, m3.ID, 1, now); st.Exceeded {
		t.Fatalf("m3 unused = %+v, want not exceeded", st)
	}
	// quotaDay=-1 → 不限制。
	if st := quota.CheckModelDaily(testCtx(), store, m1.ID, -1, now); st != (quota.Status{}) {
		t.Fatalf("quotaDay=-1 = %+v, want zero Status", st)
	}
}

// ResolveUserRules：组策略优先、五种分支与回退全局默认。
func TestResolveUserRules_Branches(t *testing.T) {
	store := newTestStore(t)
	defaultRules := `[{"mode":"day","limit":1000}]`
	mustSetSettings(t, store, defaultRules, "[]", "site")

	gEmpty := mustCreateGroup(t, store, "empty", "")
	gNone := mustCreateGroup(t, store, "none", "[]")
	gValid := mustCreateGroup(t, store, "valid", `[{"mode":"month","limit":2000}]`)
	gBad := mustCreateGroup(t, store, "bad", `{"mode":"day"}`)

	t.Run("no group falls back to default", func(t *testing.T) {
		rules, err := quota.ResolveUserRules(testCtx(), store, 0)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertRules(t, rules, defaultRules)
	})

	t.Run("missing group falls back to default", func(t *testing.T) {
		rules, err := quota.ResolveUserRules(testCtx(), store, 999999)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertRules(t, rules, defaultRules)
	})

	t.Run("unconfigured group falls back to default", func(t *testing.T) {
		rules, err := quota.ResolveUserRules(testCtx(), store, gEmpty.ID)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertRules(t, rules, defaultRules)
	})

	t.Run("invalid group rules json falls back to default", func(t *testing.T) {
		rules, err := quota.ResolveUserRules(testCtx(), store, gBad.ID)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertRules(t, rules, defaultRules)
	})

	t.Run("empty group rules mean explicitly unlimited", func(t *testing.T) {
		rules, err := quota.ResolveUserRules(testCtx(), store, gNone.ID)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if rules == nil {
			t.Fatalf("rules = nil, want empty non-nil slice")
		}
		if len(rules) != 0 {
			t.Fatalf("rules len = %d, want 0", len(rules))
		}
	})

	t.Run("group rules take precedence over global default", func(t *testing.T) {
		rules, err := quota.ResolveUserRules(testCtx(), store, gValid.ID)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertRules(t, rules, `[{"mode":"month","limit":2000}]`)
	})
}

// ResolveGlobalScope：作用域返回与非法值归一。
func TestResolveGlobalScope(t *testing.T) {
	store := newTestStore(t)
	globalRules := `[{"mode":"day","limit":7}]`

	cases := []struct {
		name      string
		scope     string
		wantScope string
	}{
		{"group scoped", "group", "group"},
		{"site wide", "site", "site"},
		{"invalid scope normalized to site", "bogus", "site"},
		{"empty scope normalized to site", "", "site"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustSetSettings(t, store, "[]", globalRules, tc.scope)
			rules, scope, err := quota.ResolveGlobalScope(testCtx(), store, 0)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if scope != tc.wantScope {
				t.Fatalf("scope = %q, want %q", scope, tc.wantScope)
			}
			assertRules(t, rules, globalRules)
		})
	}

	t.Run("invalid global rules json treated as unlimited", func(t *testing.T) {
		mustSetSettings(t, store, "[]", "not-json", "site")
		rules, scope, err := quota.ResolveGlobalScope(testCtx(), store, 0)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if scope != "site" || len(rules) != 0 {
			t.Fatalf("ResolveGlobalScope = (%v, %q), want empty rules + site", rules, scope)
		}
	})

	// 组级显式规则覆盖（照搬 hatchery「组策略优先 → 站点默认」链）。
	t.Run("group explicit rules override site rules with forced group scope", func(t *testing.T) {
		mustSetSettings(t, store, "[]", globalRules, "site")
		g := mustCreateGroupWithGlobal(t, store, "g-override", "", `[{"mode":"day","limit":5}]`)
		rules, scope, err := quota.ResolveGlobalScope(testCtx(), store, g.ID)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if scope != "group" {
			t.Fatalf("scope = %q, want group (forced by group override)", scope)
		}
		assertRules(t, rules, `[{"mode":"day","limit":5}]`)
	})

	t.Run("group explicit empty array means unlimited (no fallback)", func(t *testing.T) {
		mustSetSettings(t, store, "[]", globalRules, "site")
		g := mustCreateGroupWithGlobal(t, store, "g-unlimited", "", `[]`)
		rules, scope, err := quota.ResolveGlobalScope(testCtx(), store, g.ID)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if scope != "group" || len(rules) != 0 {
			t.Fatalf("ResolveGlobalScope = (%v, %q), want empty rules + group", rules, scope)
		}
	})

	t.Run("group without explicit rules falls back to site rules and scope", func(t *testing.T) {
		mustSetSettings(t, store, "[]", globalRules, "site")
		g := mustCreateGroupWithGlobal(t, store, "g-fallback", "", "")
		rules, scope, err := quota.ResolveGlobalScope(testCtx(), store, g.ID)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if scope != "site" {
			t.Fatalf("scope = %q, want site (fallback)", scope)
		}
		assertRules(t, rules, globalRules)
	})

	t.Run("missing group falls back to site rules", func(t *testing.T) {
		mustSetSettings(t, store, "[]", globalRules, "group")
		rules, scope, err := quota.ResolveGlobalScope(testCtx(), store, 99999)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if scope != "group" {
			t.Fatalf("scope = %q, want group", scope)
		}
		assertRules(t, rules, globalRules)
	})
}

// UserRuleUsages：生效规则的窗口与用量、不生效规则的 active/周期字段。
func TestUserRuleUsages(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	u := mustCreateUser(t, store, "u-view")

	startDaily := localTime(t, "2026-04-01 08:00:00").Unix()
	startFuture := localTime(t, "2026-04-05 00:00:00").Unix()
	startPast := localTime(t, "2026-03-01 00:00:00").Unix()
	endPast := localTime(t, "2026-03-15 00:00:00").Unix()

	rules := mustParseRules(t, fmt.Sprintf(`[
		{"mode":"day","limit":100},
		{"mode":"custom","limit":50,"start":%d,"refresh":"daily"},
		{"mode":"custom","limit":10,"start":%d,"refresh":"none"},
		{"mode":"custom","limit":20,"start":%d,"end":%d,"refresh":"none"},
		{"mode":"week","limit":30}
	]`, startDaily, startFuture, startPast, endPast))

	// 02:00 的 5 tokens 只落在自然日窗口；14:00 的 30 tokens 两个窗口都命中。
	mustLogUsage(t, store, u.ID, 0, 0, 5, localTime(t, "2026-04-03 02:00:00"))
	mustLogUsage(t, store, u.ID, 0, 0, 30, now)

	usages, err := quota.UserRuleUsages(testCtx(), store, u.ID, 0, rules, now)
	if err != nil {
		t.Fatalf("UserRuleUsages err = %v", err)
	}
	if len(usages) != 5 {
		t.Fatalf("usages len = %d, want 5", len(usages))
	}

	dayStart := localTime(t, "2026-04-03 00:00:00").Unix()
	dayEnd := localTime(t, "2026-04-04 00:00:00").Unix()
	dailyStart := localTime(t, "2026-04-03 08:00:00").Unix()
	dailyEnd := localTime(t, "2026-04-04 08:00:00").Unix()

	want := []quota.RuleUsage{
		{
			Index:       0,
			Mode:        quota.ModeDay,
			Limit:       100,
			Used:        35,
			Active:      true,
			PeriodStart: i64(dayStart),
			PeriodEnd:   i64(dayEnd),
		},
		{
			Index:       1,
			Mode:        quota.ModeCustom,
			Limit:       50,
			Used:        30,
			Active:      true,
			PeriodStart: i64(dailyStart),
			PeriodEnd:   i64(dailyEnd),
		},
		{Index: 2, Mode: quota.ModeCustom, Limit: 10, Active: false, PeriodStart: i64(startFuture)},
		{
			Index:       3,
			Mode:        quota.ModeCustom,
			Limit:       20,
			Active:      false,
			PeriodStart: i64(startPast),
			PeriodEnd:   i64(endPast),
		},
		{Index: 4, Mode: "week", Limit: 30, Active: false},
	}
	for i := range want {
		got := usages[i]
		if got.Index != want[i].Index || got.Mode != want[i].Mode || got.Limit != want[i].Limit ||
			got.Used != want[i].Used || got.Active != want[i].Active {
			t.Fatalf("usage[%d] = %+v, want %+v", i, got, want[i])
		}
		if !sameInt64(got.PeriodStart, want[i].PeriodStart) || !sameInt64(got.PeriodEnd, want[i].PeriodEnd) {
			t.Fatalf(
				"usage[%d] period = %v/%v, want %v/%v",
				i,
				got.PeriodStart,
				got.PeriodEnd,
				want[i].PeriodStart,
				want[i].PeriodEnd,
			)
		}
	}
}

// CheckRequest：检查顺序（全局 → 模型 → 用户）与各 Kind 的 RetryAfter。
func TestCheckRequest_Priority(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	u := mustCreateUser(t, store, "u-priority")
	m := mustCreateModel(t, store, "m-priority", 500000)
	key := mustCreateKey(t, store, u.ID, 0, m.ID)

	mustLogUsage(t, store, u.ID, 0, m.ID, 150000, now)
	mustSumDailyUsage(t, store, u.ID, m.ID, 500000)

	t.Run("global takes precedence over model and user", func(t *testing.T) {
		mustSetSettings(t, store, `[{"mode":"day","limit":100000}]`, `[{"mode":"day","limit":100000}]`, "site")
		assertStatus(t, quota.CheckRequest(testCtx(), store, key, ref(m, 500000), now), quota.Status{
			Exceeded: true, Kind: kindGlobal, Used: 150000, Limit: 100000, RetryAfter: 10 * time.Hour,
		})
	})

	t.Run("model takes precedence over user after global passes", func(t *testing.T) {
		mustSetSettings(t, store, `[{"mode":"day","limit":100000}]`, `[{"mode":"day","limit":200000}]`, "site")
		assertStatus(t, quota.CheckRequest(testCtx(), store, key, ref(m, 500000), now), quota.Status{
			Exceeded: true, Kind: kindModel, Used: 500000, Limit: 500000, RetryAfter: 10 * time.Hour,
		})
	})

	t.Run("user exceeds after model check skipped", func(t *testing.T) {
		assertStatus(t, quota.CheckRequest(testCtx(), store, key, ref(m, -1), now), quota.Status{
			Exceeded: true, Kind: kindUser, Used: 150000, Limit: 100000, RetryAfter: 10 * time.Hour,
		})
	})

	t.Run("all three levels pass", func(t *testing.T) {
		mustSetSettings(t, store, `[]`, `[{"mode":"day","limit":200000}]`, "site")
		assertStatus(t, quota.CheckRequest(testCtx(), store, key, ref(m, -1), now), quota.Status{})
	})
}

// CheckRequest：quotaDay<0 或密钥无可解析模型时跳过模型日配额（不误拒绝）。
func TestCheckRequest_SkipsModelCheck(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	u := mustCreateUser(t, store, "u-skip")
	m := mustCreateModel(t, store, "m-skip", 500000)
	mustSumDailyUsage(t, store, u.ID, m.ID, 500000)
	mustSetSettings(t, store, "[]", "[]", "site")

	// 无解析结果（resolved=nil）→ 跳过模型检查。
	unbound := mustCreateKey(t, store, u.ID, 0, 0)
	assertStatus(t, quota.CheckRequest(testCtx(), store, unbound, nil, now), quota.Status{})

	// quotaDay<0 → 跳过模型检查（即使模型已超额）。
	bound := mustCreateKey(t, store, u.ID, 0, m.ID)
	assertStatus(t, quota.CheckRequest(testCtx(), store, bound, ref(m, -1), now), quota.Status{})
}

// CheckRequest：全局配额按组隔离（A 组用量不影响 B 组请求）。
func TestCheckRequest_GroupScopedGlobal(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	gA := mustCreateGroup(t, store, "A", "")
	gB := mustCreateGroup(t, store, "B", "")
	uA := mustCreateUser(t, store, "u-a")
	uB := mustCreateUser(t, store, "u-b")
	keyA := mustCreateKey(t, store, uA.ID, gA.ID, 0)
	keyB := mustCreateKey(t, store, uB.ID, gB.ID, 0)

	mustSetSettings(t, store, "[]", `[{"mode":"day","limit":100}]`, "group")
	mustLogUsage(t, store, uA.ID, gA.ID, 0, 200, now)

	assertStatus(t, quota.CheckRequest(testCtx(), store, keyA, nil, now), quota.Status{
		Exceeded: true, Kind: kindGlobal, Used: 200, Limit: 100, RetryAfter: 10 * time.Hour,
	})
	assertStatus(t, quota.CheckRequest(testCtx(), store, keyB, nil, now), quota.Status{})
}

// ref 构造一次模型解析结果（测试用）。
func ref(m *model.AIModel, quotaDay int) *model.Resolved {
	return &model.Resolved{AIModelID: m.ID, QuotaDay: quotaDay, ModelID: m.ModelID}
}

// CheckRequest：模型日配额按传入的 resolved 模型计量（而非密钥 primary）。
func TestCheckRequest_UsesResolvedModel(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	u := mustCreateUser(t, store, "u-resolved")
	primary := mustCreateModel(t, store, "m-primary", -1)
	alt := mustCreateModel(t, store, "m-alt", 100)
	key := mustCreateKey(t, store, u.ID, 0, primary.ID)
	mustSumDailyUsage(t, store, u.ID, alt.ID, 150)
	mustSetSettings(t, store, "[]", "[]", "site")

	// 实际路由到 alt（日配额 100、已用 150）→ 命中模型检查。
	assertStatus(t, quota.CheckRequest(testCtx(), store, key, ref(alt, alt.QuotaDay), now), quota.Status{
		Exceeded: true, Kind: kindModel, Used: 150, Limit: 100, RetryAfter: 10 * time.Hour,
	})
	// 实际路由到 primary（quota_day=-1）→ 跳过模型检查，通过。
	assertStatus(t, quota.CheckRequest(testCtx(), store, key, ref(primary, primary.QuotaDay), now), quota.Status{})
	// resolved 为 nil → 跳过模型检查，通过。
	assertStatus(t, quota.CheckRequest(testCtx(), store, key, nil, now), quota.Status{})
}

// CheckKey：密钥级配额的窗口判定、显式不限与用量隔离。
func TestCheckKey(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	u := mustCreateUser(t, store, "u-key")
	kA := mustCreateKey(t, store, u.ID, 0, 0)
	kB := mustCreateKey(t, store, u.ID, 0, 0)
	rules := mustParseRules(t, `[{"mode":"day","limit":100}]`)

	mustLogKeyUsage(t, store, kA.ID, u.ID, 0, 60, now)

	if st := quota.CheckKey(testCtx(), store, kA.ID, rules, now); st.Exceeded {
		t.Fatalf("key A = %+v, want not exceeded", st)
	}
	// 再 +40 → 100 ≥ 100 超限（day 窗口 10h 后重置）。
	mustLogKeyUsage(t, store, kA.ID, u.ID, 0, 40, now)
	assertStatus(t, quota.CheckKey(testCtx(), store, kA.ID, rules, now), quota.Status{
		Exceeded: true, Kind: kindKey, Used: 100, Limit: 100, RetryAfter: 10 * time.Hour,
	})
	// 密钥 B 用量独立（0 < 100）。
	if st := quota.CheckKey(testCtx(), store, kB.ID, rules, now); st.Exceeded {
		t.Fatalf("key B = %+v, want not exceeded (per-key isolation)", st)
	}

	t.Run("empty rules unlimited", func(t *testing.T) {
		if st := quota.CheckKey(testCtx(), store, kA.ID, nil, now); st.Exceeded {
			t.Fatalf("empty rules = %+v, want not exceeded", st)
		}
	})

	t.Run("limit=-1 skipped", func(t *testing.T) {
		neg := mustParseRules(t, `[{"mode":"day","limit":-1}]`)
		if st := quota.CheckKey(testCtx(), store, kA.ID, neg, now); st.Exceeded {
			t.Fatalf("limit=-1 = %+v, want not exceeded", st)
		}
	})

	t.Run("inactive custom window fails closed", func(t *testing.T) {
		startFuture := localTime(t, "2026-04-05 00:00:00").Unix()
		inactive := mustParseRules(
			t,
			fmt.Sprintf(`[{"mode":"custom","limit":10,"start":%d,"refresh":"none"}]`, startFuture),
		)
		st := quota.CheckKey(testCtx(), store, kA.ID, inactive, now)
		assertStatus(t, st, quota.Status{Exceeded: true, Kind: kindKey, Limit: 10})
	})
}

// CheckRequest：四级顺序 —— 密钥级超限在用户级通过后仍会命中；事件 kind=key。
func TestCheckRequest_KeyLevel(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	u := mustCreateUser(t, store, "u-four")
	m := mustCreateModel(t, store, "m-four", -1)
	key := mustCreateKey(t, store, u.ID, 0, m.ID)

	// 配置密钥级 day=50；写入 50 tokens → 命中第四级。
	if _, err := store.UpdateAPIKey(testCtx(), key.ID, map[string]any{
		"token_quota_rules": `[{"mode":"day","limit":50}]`,
	}); err != nil {
		t.Fatalf("update key rules: %v", err)
	}
	key, err := store.GetAPIKeyByID(testCtx(), key.ID)
	if err != nil {
		t.Fatalf("reload key: %v", err)
	}
	mustLogKeyUsage(t, store, key.ID, u.ID, 0, 50, now)

	resolved := &model.Resolved{AIModelID: m.ID, ModelID: m.ModelID, QuotaDay: -1}
	st := quota.CheckRequest(testCtx(), store, key, resolved, now)
	assertStatus(t, st, quota.Status{Exceeded: true, Kind: kindKey, Used: 50, Limit: 50, RetryAfter: 10 * time.Hour})

	// 清除密钥级规则后恢复放行（其余三级均不限制）。
	if _, err := store.UpdateAPIKey(testCtx(), key.ID, map[string]any{"token_quota_rules": ""}); err != nil {
		t.Fatalf("clear key rules: %v", err)
	}
	key, _ = store.GetAPIKeyByID(testCtx(), key.ID)
	if st := quota.CheckRequest(testCtx(), store, key, resolved, now); st.Exceeded {
		t.Fatalf("after clearing key rules = %+v, want not exceeded", st)
	}

	// malformed JSON 视为不限制（照搬 hatchery：空/非法 = 每日无限制）。
	if _, err := store.UpdateAPIKey(testCtx(), key.ID, map[string]any{"token_quota_rules": "not-json"}); err != nil {
		t.Fatalf("set malformed rules: %v", err)
	}
	key, _ = store.GetAPIKeyByID(testCtx(), key.ID)
	if st := quota.CheckRequest(testCtx(), store, key, resolved, now); st.Exceeded {
		t.Fatalf("malformed rules = %+v, want not exceeded", st)
	}
}

// 组级全局覆盖的端到端判定：组 A 独立池，组 B 回退站点规则不受影响。
func TestCheckRequest_GroupGlobalOverride(t *testing.T) {
	store := newTestStore(t)
	now := localTime(t, "2026-04-03 14:00:00")
	u := mustCreateUser(t, store, "u-goverride")
	m := mustCreateModel(t, store, "m-goverride", -1)

	gA := mustCreateGroupWithGlobal(t, store, "A", "", `[{"mode":"day","limit":50}]`)
	gB := mustCreateGroup(t, store, "B", "")
	mustSetSettings(t, store, "[]", `[{"mode":"day","limit":1000000}]`, "site")

	keyA := mustCreateKey(t, store, u.ID, gA.ID, m.ID)
	keyB := mustCreateKey(t, store, u.ID, gB.ID, m.ID)
	resolved := &model.Resolved{AIModelID: m.ID, QuotaDay: -1}

	// 组 A 用量 50 → 命中组级规则被拒；组 B 无用量 → 放行。
	mustLogKeyUsage(t, store, keyA.ID, u.ID, gA.ID, 50, now)
	keyA, _ = store.GetAPIKeyByID(testCtx(), keyA.ID)
	st := quota.CheckRequest(testCtx(), store, keyA, resolved, now)
	assertStatus(t, st, quota.Status{Exceeded: true, Kind: kindGlobal, Used: 50, Limit: 50, RetryAfter: 10 * time.Hour})

	keyB, _ = store.GetAPIKeyByID(testCtx(), keyB.ID)
	if st := quota.CheckRequest(testCtx(), store, keyB, resolved, now); st.Exceeded {
		t.Fatalf("group B = %+v, want not exceeded", st)
	}
}
