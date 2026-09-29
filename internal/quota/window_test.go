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
	"testing"
	"time"

	"aigate/internal/model"
	"aigate/internal/quota"
)

// assertWindow 断言窗口计算结果：from/to 为绝对时间点且位于 UTC。
func assertWindow(t *testing.T, r quota.Rule, now time.Time, wantFrom time.Time, wantTo *time.Time, wantActive bool) {
	t.Helper()
	gotFrom, gotTo, active := r.Window(now)
	if active != wantActive {
		t.Fatalf("Window(mode=%s, refresh=%s) active = %v, want %v", r.Mode, r.Refresh, active, wantActive)
	}
	if !wantActive {
		if !gotFrom.IsZero() || gotTo != nil {
			t.Fatalf("inactive window = (%v, %v), want zero values", gotFrom, gotTo)
		}
		return
	}
	if !gotFrom.Equal(wantFrom) {
		t.Fatalf("Window from = %v, want %v", gotFrom, wantFrom)
	}
	if gotFrom.Location() != time.UTC {
		t.Fatalf("Window from location = %v, want UTC", gotFrom.Location())
	}
	if wantTo == nil {
		if gotTo != nil {
			t.Fatalf("Window to = %v, want nil", *gotTo)
		}
		return
	}
	if gotTo == nil {
		t.Fatalf("Window to = nil, want %v", *wantTo)
	}
	if !gotTo.Equal(*wantTo) {
		t.Fatalf("Window to = %v, want %v", *gotTo, *wantTo)
	}
	if gotTo.Location() != time.UTC {
		t.Fatalf("Window to location = %v, want UTC", gotTo.Location())
	}
}

// 自然日/月/年窗口：按业务时区（Asia/Shanghai）自然边界计算，返回 UTC 时间点。
// 含边界（零点、月末、年末、跨月天数）。
func TestRuleWindow_NaturalWindows(t *testing.T) {
	useShanghai(t)
	cases := []struct {
		name     string
		mode     string
		now      string
		wantFrom time.Time
		wantTo   time.Time
	}{
		{"day/afternoon", quota.ModeDay, "2026-04-03 14:00:00", utc(2026, 4, 2, 16, 0, 0), utc(2026, 4, 3, 16, 0, 0)},
		{
			"day/local midnight",
			quota.ModeDay,
			"2026-04-03 00:00:00",
			utc(2026, 4, 2, 16, 0, 0),
			utc(2026, 4, 3, 16, 0, 0),
		},
		{
			"day/last second of day",
			quota.ModeDay,
			"2026-04-03 23:59:59",
			utc(2026, 4, 2, 16, 0, 0),
			utc(2026, 4, 3, 16, 0, 0),
		},
		{
			"month/start of month",
			quota.ModeMonth,
			"2026-04-01 00:00:00",
			utc(2026, 3, 31, 16, 0, 0),
			utc(2026, 4, 30, 16, 0, 0),
		},
		{
			"month/last second of month",
			quota.ModeMonth,
			"2026-04-30 23:59:59",
			utc(2026, 3, 31, 16, 0, 0),
			utc(2026, 4, 30, 16, 0, 0),
		},
		{
			"month/unequal days across months",
			quota.ModeMonth,
			"2026-01-31 12:00:00",
			utc(2025, 12, 31, 16, 0, 0),
			utc(2026, 1, 31, 16, 0, 0),
		},
		{
			"year/start of year",
			quota.ModeYear,
			"2026-01-01 00:00:00",
			utc(2025, 12, 31, 16, 0, 0),
			utc(2026, 12, 31, 16, 0, 0),
		},
		{
			"year/last second of year",
			quota.ModeYear,
			"2026-12-31 23:59:59",
			utc(2025, 12, 31, 16, 0, 0),
			utc(2026, 12, 31, 16, 0, 0),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := quota.Rule{Mode: tc.mode, Limit: 100}
			assertWindow(t, rule, localTime(t, tc.now), tc.wantFrom, tptr(tc.wantTo), true)
		})
	}
}

// 业务时区为 UTC 时，自然日窗口退化为 UTC 自然日（窗口与时区配置无关地一致）。
func TestRuleWindow_NaturalDayInUTC(t *testing.T) {
	model.SetLocation(time.UTC)

	rule := quota.Rule{Mode: quota.ModeDay, Limit: 100}
	assertWindow(t, rule, utc(2026, 4, 3, 14, 0, 0), utc(2026, 4, 3, 0, 0, 0), tptr(utc(2026, 4, 4, 0, 0, 0)), true)
}

// limit=-1 不影响窗口计算（跳过由检查器负责）。
func TestRuleWindow_IndependentOfLimit(t *testing.T) {
	useShanghai(t)
	rule := quota.Rule{Mode: quota.ModeDay, Limit: -1}
	assertWindow(
		t,
		rule,
		localTime(t, "2026-04-03 14:00:00"),
		utc(2026, 4, 2, 16, 0, 0),
		tptr(utc(2026, 4, 3, 16, 0, 0)),
		true,
	)
}

// custom + refresh=none：固定窗口 [start, end|∞)。
func TestRuleWindow_CustomNone(t *testing.T) {
	useShanghai(t)
	start := localTime(t, "2026-04-01 08:00:00").Unix()
	end := localTime(t, "2026-04-05 08:00:00").Unix()

	t.Run("no end/active", func(t *testing.T) {
		rule := quota.Rule{Mode: quota.ModeCustom, Limit: 100, Start: i64(start), Refresh: quota.RefreshNone}
		assertWindow(t, rule, localTime(t, "2026-04-03 14:00:00"), utc(2026, 4, 1, 0, 0, 0), nil, true)
	})

	t.Run("with end/active", func(t *testing.T) {
		rule := quota.Rule{
			Mode:    quota.ModeCustom,
			Limit:   100,
			Start:   i64(start),
			End:     i64(end),
			Refresh: quota.RefreshNone,
		}
		assertWindow(
			t,
			rule,
			localTime(t, "2026-04-03 14:00:00"),
			utc(2026, 4, 1, 0, 0, 0),
			tptr(utc(2026, 4, 5, 0, 0, 0)),
			true,
		)
	})

	t.Run("refresh defaults to none", func(t *testing.T) {
		rule := quota.Rule{Mode: quota.ModeCustom, Limit: 100, Start: i64(start), End: i64(end)}
		assertWindow(
			t,
			rule,
			localTime(t, "2026-04-03 14:00:00"),
			utc(2026, 4, 1, 0, 0, 0),
			tptr(utc(2026, 4, 5, 0, 0, 0)),
			true,
		)
	})

	t.Run("inactive exactly at end time", func(t *testing.T) {
		rule := quota.Rule{
			Mode:    quota.ModeCustom,
			Limit:   100,
			Start:   i64(start),
			End:     i64(end),
			Refresh: quota.RefreshNone,
		}
		assertWindow(t, rule, localTime(t, "2026-04-05 08:00:00"), time.Time{}, nil, false)
	})

	t.Run("inactive past end time", func(t *testing.T) {
		rule := quota.Rule{
			Mode:    quota.ModeCustom,
			Limit:   100,
			Start:   i64(start),
			End:     i64(end),
			Refresh: quota.RefreshNone,
		}
		assertWindow(t, rule, localTime(t, "2026-04-05 08:00:01"), time.Time{}, nil, false)
	})

	t.Run("inactive before start", func(t *testing.T) {
		rule := quota.Rule{Mode: quota.ModeCustom, Limit: 100, Start: i64(start), Refresh: quota.RefreshNone}
		assertWindow(t, rule, localTime(t, "2026-03-31 23:00:00"), time.Time{}, nil, false)
	})

	t.Run("inactive when start missing", func(t *testing.T) {
		rule := quota.Rule{Mode: quota.ModeCustom, Limit: 100, Refresh: quota.RefreshNone}
		assertWindow(t, rule, localTime(t, "2026-04-03 14:00:00"), time.Time{}, nil, false)
	})
}

// custom + refresh=daily：按锚点 HH:MM:SS 切割的 24h 周期。
func TestRuleWindow_CustomDaily(t *testing.T) {
	useShanghai(t)
	start := localTime(t, "2026-04-01 08:30:00").Unix()

	t.Run("within cycle (before anchor time)", func(t *testing.T) {
		rule := quota.Rule{Mode: quota.ModeCustom, Limit: 100, Start: i64(start), Refresh: quota.RefreshDaily}
		assertWindow(
			t,
			rule,
			localTime(t, "2026-04-03 07:00:00"),
			utc(2026, 4, 2, 0, 30, 0),
			tptr(utc(2026, 4, 3, 0, 30, 0)),
			true,
		)
	})

	t.Run("within cycle (after anchor time)", func(t *testing.T) {
		rule := quota.Rule{Mode: quota.ModeCustom, Limit: 100, Start: i64(start), Refresh: quota.RefreshDaily}
		assertWindow(
			t,
			rule,
			localTime(t, "2026-04-03 09:00:00"),
			utc(2026, 4, 3, 0, 30, 0),
			tptr(utc(2026, 4, 4, 0, 30, 0)),
			true,
		)
	})

	t.Run("exactly at anchor time", func(t *testing.T) {
		rule := quota.Rule{Mode: quota.ModeCustom, Limit: 100, Start: i64(start), Refresh: quota.RefreshDaily}
		assertWindow(
			t,
			rule,
			localTime(t, "2026-04-03 08:30:00"),
			utc(2026, 4, 3, 0, 30, 0),
			tptr(utc(2026, 4, 4, 0, 30, 0)),
			true,
		)
	})

	t.Run("one second before anchor", func(t *testing.T) {
		rule := quota.Rule{Mode: quota.ModeCustom, Limit: 100, Start: i64(start), Refresh: quota.RefreshDaily}
		assertWindow(
			t,
			rule,
			localTime(t, "2026-04-03 08:29:59"),
			utc(2026, 4, 2, 0, 30, 0),
			tptr(utc(2026, 4, 3, 0, 30, 0)),
			true,
		)
	})

	t.Run("hard end narrows cycle", func(t *testing.T) {
		end := localTime(t, "2026-04-03 07:30:00").Unix()
		rule := quota.Rule{
			Mode:    quota.ModeCustom,
			Limit:   100,
			Start:   i64(start),
			End:     i64(end),
			Refresh: quota.RefreshDaily,
		}
		assertWindow(
			t,
			rule,
			localTime(t, "2026-04-03 07:00:00"),
			utc(2026, 4, 2, 0, 30, 0),
			tptr(utc(2026, 4, 2, 23, 30, 0)),
			true,
		)
	})

	t.Run("inactive when hard end reached", func(t *testing.T) {
		end := localTime(t, "2026-04-03 07:30:00").Unix()
		rule := quota.Rule{
			Mode:    quota.ModeCustom,
			Limit:   100,
			Start:   i64(start),
			End:     i64(end),
			Refresh: quota.RefreshDaily,
		}
		assertWindow(t, rule, localTime(t, "2026-04-03 07:30:00"), time.Time{}, nil, false)
	})
}

// custom + refresh=monthly：自锚点起固定 31 天周期滚动（非自然月）。
func TestRuleWindow_CustomMonthly(t *testing.T) {
	useShanghai(t)
	start := localTime(t, "2026-01-01 00:00:00").Unix()
	rule := quota.Rule{Mode: quota.ModeCustom, Limit: 100, Start: i64(start), Refresh: quota.RefreshMonthly}

	t.Run("cycle 0", func(t *testing.T) {
		assertWindow(
			t,
			rule,
			localTime(t, "2026-01-15 00:00:00"),
			utc(2025, 12, 31, 16, 0, 0),
			tptr(utc(2026, 1, 31, 16, 0, 0)),
			true,
		)
	})

	t.Run("cycle end still in cycle 0", func(t *testing.T) {
		assertWindow(
			t,
			rule,
			localTime(t, "2026-01-31 23:59:59"),
			utc(2025, 12, 31, 16, 0, 0),
			tptr(utc(2026, 1, 31, 16, 0, 0)),
			true,
		)
	})

	t.Run("cycle 1 (after 31 days)", func(t *testing.T) {
		assertWindow(
			t,
			rule,
			localTime(t, "2026-02-01 00:00:00"),
			utc(2026, 1, 31, 16, 0, 0),
			tptr(utc(2026, 3, 3, 16, 0, 0)),
			true,
		)
	})
}

// custom + refresh=yearly：自锚点起固定 365 天周期滚动（非自然年）。
func TestRuleWindow_CustomYearly(t *testing.T) {
	useShanghai(t)
	start := localTime(t, "2026-01-01 00:00:00").Unix()
	rule := quota.Rule{Mode: quota.ModeCustom, Limit: 100, Start: i64(start), Refresh: quota.RefreshYearly}

	t.Run("cycle 0", func(t *testing.T) {
		assertWindow(
			t,
			rule,
			localTime(t, "2026-06-01 00:00:00"),
			utc(2025, 12, 31, 16, 0, 0),
			tptr(utc(2026, 12, 31, 16, 0, 0)),
			true,
		)
	})

	t.Run("cycle 1 (after 365 days)", func(t *testing.T) {
		assertWindow(
			t,
			rule,
			localTime(t, "2027-01-01 00:00:00"),
			utc(2026, 12, 31, 16, 0, 0),
			tptr(utc(2027, 12, 31, 16, 0, 0)),
			true,
		)
	})

	t.Run("leap-year anchor: fixed 365-day cycle, not calendar year", func(t *testing.T) {
		leapStart := localTime(t, "2024-01-01 00:00:00").Unix()
		r := quota.Rule{Mode: quota.ModeCustom, Limit: 100, Start: i64(leapStart), Refresh: quota.RefreshYearly}
		// 2024 为闰年：第 0 个周期在 2024-12-31 00:00 结束，而非 2025-01-01。
		assertWindow(
			t,
			r,
			localTime(t, "2024-12-30 12:00:00"),
			utc(2023, 12, 31, 16, 0, 0),
			tptr(utc(2024, 12, 30, 16, 0, 0)),
			true,
		)
		assertWindow(
			t,
			r,
			localTime(t, "2024-12-31 12:00:00"),
			utc(2024, 12, 30, 16, 0, 0),
			tptr(utc(2025, 12, 30, 16, 0, 0)),
			true,
		)
	})
}

// 非法或缺失关键字段的窗口一律不生效（fail-closed 的上游依据）。
func TestRuleWindow_InactiveModes(t *testing.T) {
	useShanghai(t)
	now := localTime(t, "2026-04-03 14:00:00")

	for _, tc := range []struct {
		name string
		rule quota.Rule
	}{
		{"unknown mode", quota.Rule{Mode: "week", Limit: 100}},
		{"empty mode", quota.Rule{Limit: 100}},
		{"custom unknown refresh", quota.Rule{Mode: quota.ModeCustom, Limit: 100, Start: i64(now.Unix()), Refresh: "hourly"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertWindow(t, tc.rule, now, time.Time{}, nil, false)
		})
	}
}
