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

// Package quota 实现网关的多窗口配额引擎：规则解析/校验/规范化、窗口计算、
// 四级检查（全局 → 模型日配额 → 用户 → 密钥）与生效规则解析链
// （用户规则：组策略 → 全局默认；全局规则：组级显式规则 → 站点规则）。
//
// 核心语义：
//   - 窗口按业务时区（model.BusinessLocation）计算自然日/月/年；custom 按锚点滚动；
//   - 规则当前不生效（start 未到 / end 已过）一律 fail-closed（判定超限并拒绝）；
//   - 配置读取故障（DB error）一律 fail-open（记 slog 警告并放行），仅限读取故障。
package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 窗口模式。
const (
	ModeDay    = "day"    // 业务时区自然日 [00:00, 次日 00:00)
	ModeMonth  = "month"  // 业务时区自然月 [1 日 00:00, 次月 1 日 00:00)
	ModeYear   = "year"   // 业务时区自然年 [1 月 1 日 00:00, 次年 1 月 1 日 00:00)
	ModeCustom = "custom" // 自定义锚点窗口（见 Start/End/Refresh）
)

// custom 模式的刷新方式；滚动周期为固定时长（非自然月/年）。
const (
	RefreshDaily   = "daily"   // 按锚点 HH:MM:SS 的 24h 周期滚动
	RefreshMonthly = "monthly" // 自锚点起固定 31 天周期滚动
	RefreshYearly  = "yearly"  // 自锚点起固定 365 天周期滚动
	RefreshNone    = "none"    // 固定窗口 [start, end|∞)
)

// Rule 单条配额规则。
// Limit < 0（即 -1）表示该规则参与窗口计算但不限制用量。
type Rule struct {
	Mode    string `json:"mode"`
	Limit   int    `json:"limit"`
	Start   *int64 `json:"start,omitempty"`   // Unix 秒，custom 锚点
	End     *int64 `json:"end,omitempty"`     // Unix 秒，custom 硬截止（nil 或 <=0 视为未配置）
	Refresh string `json:"refresh,omitempty"` // daily / monthly / yearly / none，仅 custom 有效
}

// ParseRules 解析 DB 中存储的规则 JSON 文本。
//
// 返回值 ok 区分「未设置/非法」与「有值」：
//   - ""：未设置 → nil, false
//   - 非法 JSON：静默容错 → nil, false
//   - "[]"：有值且为显式不限 → 空切片（非 nil）, true
//   - "[{...}]"：有值 → rules, true
func ParseRules(raw string) ([]Rule, bool) {
	if raw == "" {
		return nil, false
	}
	var rules []Rule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, false
	}
	if rules == nil {
		// "null" 等输入：统一归一为非 nil 空切片，保持「有值」语义可区分。
		rules = []Rule{}
	}
	return rules, true
}

// MarshalRules 将规则序列化为 JSON 文本；序列化前自动规范化：
//   - custom 模式的空 refresh 补为 "none"；
//   - 非 custom 模式清除无意义的 start/end/refresh。
//
// 空集合（含 nil）输出 "[]"。不修改入参。
func MarshalRules(rules []Rule) string {
	out := make([]Rule, len(rules))
	copy(out, rules)
	for i := range out {
		if out[i].Mode == ModeCustom {
			if out[i].Refresh == "" {
				out[i].Refresh = RefreshNone
			}
			continue
		}
		out[i].Start = nil
		out[i].End = nil
		out[i].Refresh = ""
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// ValidateRules 校验规则集合；空集合视为显式不限（合法）。
//
// 校验项：同一集合内 mode 不可重复；limit 必须 >= -1；mode 必须合法；
// custom 的 refresh 必须合法；custom 同时给出 start/end 时 end 必须晚于 start。
func ValidateRules(rules []Rule) error {
	if len(rules) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(rules))
	for _, r := range rules {
		if _, dup := seen[r.Mode]; dup {
			return fmt.Errorf("duplicate quota mode %q", r.Mode)
		}
		seen[r.Mode] = struct{}{}

		if r.Limit < -1 {
			return errors.New("limit must be >= -1")
		}

		switch r.Mode {
		case ModeDay, ModeMonth, ModeYear:
			// 仅需要 limit；start/end/refresh 无意义，规范化时清除。
		case ModeCustom:
			switch r.Refresh {
			case "", RefreshDaily, RefreshMonthly, RefreshYearly, RefreshNone:
			default:
				return fmt.Errorf("invalid refresh mode %q", r.Refresh)
			}
			if r.End != nil && *r.End > 0 && r.Start != nil && *r.End <= *r.Start {
				return errors.New("quota end must be after start")
			}
		default:
			return fmt.Errorf("invalid quota mode %q", r.Mode)
		}
	}
	return nil
}

// NormalizeRules 统一处理写入场景的规则文本：解析 → 为 custom 规则补齐锚点
// （start 为空时填充为当前时间）→ 校验 → 规范化序列化。
//
// 必须先补齐锚点再校验：否则「只给了过去的 end、未填 start」的规则会先通过校验、
// 再被填上当前时间，落库成 end < start 的自相矛盾配置——运行期该窗口恒不生效
// （fail-closed），会造成整个作用域永久 429。
func NormalizeRules(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("quota rules must not be empty")
	}
	var rules []Rule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return "", fmt.Errorf("invalid quota rules json: %w", err)
	}
	fillCustomStart(rules)
	if err := ValidateRules(rules); err != nil {
		return "", err
	}
	return MarshalRules(rules), nil
}

// fillCustomStart 为 start 为空的 custom 规则填充锚点（当前时间）。
func fillCustomStart(rules []Rule) {
	sec := time.Now().Unix()
	for i := range rules {
		if rules[i].Mode == ModeCustom && rules[i].Start == nil {
			v := sec
			rules[i].Start = &v
		}
	}
}
