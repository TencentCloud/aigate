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

package quota

import (
	"time"

	"aigate/internal/model"
)

// custom 滚动周期：monthly/yearly 为固定时长（非自然月/年）。
const (
	customMonthPeriod = 31 * 24 * time.Hour
	customYearPeriod  = 365 * 24 * time.Hour
)

// Window 计算规则在 now 时刻的统计窗口。
//
// 返回 (from, to, active)：
//   - 查询口径为 [from, to)：from/to 是绝对时间点（UTC），to=nil 表示无终止；
//   - active=false 表示规则当前不生效（custom 的 start 未到 / end 已到或已过，或 mode 非法），
//     此时 from/to 为零值，调用方 MUST 按 fail-closed（判定超限）处理。
//
// day/month/year 按业务时区自然日/月/年；custom 语义见 Rule.Start/End/Refresh。
func (r Rule) Window(now time.Time) (from time.Time, to *time.Time, active bool) {
	loc := model.BusinessLocation()
	switch r.Mode {
	case ModeDay:
		y, m, d := now.In(loc).Date()
		start := time.Date(y, m, d, 0, 0, 0, 0, loc)
		end := start.AddDate(0, 0, 1).UTC()
		return start.UTC(), &end, true

	case ModeMonth:
		y, m, _ := now.In(loc).Date()
		start := time.Date(y, m, 1, 0, 0, 0, 0, loc)
		end := start.AddDate(0, 1, 0).UTC()
		return start.UTC(), &end, true

	case ModeYear:
		y, _, _ := now.In(loc).Date()
		start := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
		end := start.AddDate(1, 0, 0).UTC()
		return start.UTC(), &end, true

	case ModeCustom:
		return r.customWindow(now, loc)

	default:
		return time.Time{}, nil, false
	}
}

// customWindow 计算 custom 模式窗口。
//
// start 为必填锚点，now 早于锚点 → 不生效；end（可选硬截止）已到或已过 → 不生效。
//   - refresh=none/""：固定窗口 [start, end|∞)；
//   - refresh=daily：按锚点 HH:MM:SS 切割的 24h 周期；
//   - refresh=monthly / yearly：自锚点起的固定 31 / 365 天周期。
func (r Rule) customWindow(now time.Time, loc *time.Location) (time.Time, *time.Time, bool) {
	if r.Start == nil {
		return time.Time{}, nil, false
	}
	anchor := time.Unix(*r.Start, 0).In(loc)
	if now.Before(anchor) {
		return time.Time{}, nil, false
	}
	hardEnd, expired := r.hardEnd(now, loc)
	if expired {
		return time.Time{}, nil, false
	}

	switch r.Refresh {
	case "", RefreshNone:
		if hardEnd != nil {
			return anchor.UTC(), hardEnd, true
		}
		return anchor.UTC(), nil, true

	case RefreshDaily:
		h, m, s := anchor.Clock()
		local := now.In(loc)
		start := time.Date(local.Year(), local.Month(), local.Day(), h, m, s, 0, loc)
		if local.Before(start) {
			start = start.AddDate(0, 0, -1)
		}
		return start.UTC(), earliest(start.Add(24*time.Hour).UTC(), hardEnd), true

	case RefreshMonthly:
		return rollingWindow(anchor, now, customMonthPeriod, hardEnd)

	case RefreshYearly:
		return rollingWindow(anchor, now, customYearPeriod, hardEnd)

	default:
		return time.Time{}, nil, false
	}
}

// hardEnd 解析可选硬截止：返回截止时刻（UTC）；expired=true 表示 now 已到或已过截止。
// End 为空指针或 <=0 视为未配置。
func (r Rule) hardEnd(now time.Time, loc *time.Location) (*time.Time, bool) {
	if r.End == nil || *r.End <= 0 {
		return nil, false
	}
	end := time.Unix(*r.End, 0).In(loc)
	if !now.Before(end) {
		return nil, true
	}
	utc := end.UTC()
	return &utc, false
}

// rollingWindow 计算自 anchor 起固定 period 长度的滚动周期窗口。
func rollingWindow(anchor, now time.Time, period time.Duration, hardEnd *time.Time) (time.Time, *time.Time, bool) {
	periods := int(now.Sub(anchor) / period)
	start := anchor.Add(time.Duration(periods) * period)
	end := start.Add(period)
	return start.UTC(), earliest(end.UTC(), hardEnd), true
}

// earliest 返回周期结束点与可选硬截止中较早者；hardEnd 为 nil 时取周期结束点。
func earliest(periodEnd time.Time, hardEnd *time.Time) *time.Time {
	if hardEnd != nil && hardEnd.Before(periodEnd) {
		return hardEnd
	}
	e := periodEnd
	return &e
}
