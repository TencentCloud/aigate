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

package queryparams_test

// 黑盒单测：列表类接口的查询参数解析口径（管理面与用户面共用）。

import (
	"net/http/httptest"
	"testing"

	"aigate/internal/queryparams"
)

// TestParsePagination 覆盖默认值、上限与非法输入（含 page 溢出的上界保护）。
func TestParsePagination(t *testing.T) {
	cases := []struct {
		name         string
		query        string
		wantPage     int
		wantPageSize int
		wantErr      bool
	}{
		{name: "defaults", query: "", wantPage: 1, wantPageSize: 50},
		{name: "explicit", query: "?page=3&page_size=20", wantPage: 3, wantPageSize: 20},
		{name: "page_size capped", query: "?page_size=100000", wantPage: 1, wantPageSize: 500},
		{name: "page at max allowed", query: "?page=1000000", wantPage: 1000000, wantPageSize: 50},
		{name: "page over max rejected", query: "?page=1000001", wantErr: true},
		{name: "huge page rejected before overflow", query: "?page=18446744073709551", wantErr: true},
		{name: "invalid page", query: "?page=0", wantErr: true},
		{name: "invalid page_size", query: "?page_size=0", wantErr: true},
		{name: "non numeric", query: "?page=abc", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/x"+tc.query, nil)
			page, pageSize, err := queryparams.ParsePagination(r)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got page=%d page_size=%d", page, pageSize)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if page != tc.wantPage || pageSize != tc.wantPageSize {
				t.Fatalf("got page=%d page_size=%d, want %d/%d", page, pageSize, tc.wantPage, tc.wantPageSize)
			}
		})
	}
}

// TestValidGroupBy 覆盖白名单取值。
func TestValidGroupBy(t *testing.T) {
	for _, ok := range []string{"date", "model", "key", "user", "group"} {
		if !queryparams.ValidGroupBy(ok) {
			t.Errorf("ValidGroupBy(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "DATE", "keys", "models"} {
		if queryparams.ValidGroupBy(bad) {
			t.Errorf("ValidGroupBy(%q) = true, want false", bad)
		}
	}
}

// TestParseUsageFilter 覆盖日期解析、过滤字段与行级隔离（forcedUserID 忽略请求中的 user_id）。
func TestParseUsageFilter(t *testing.T) {
	t.Run("invalid dates", func(t *testing.T) {
		for _, q := range []string{"?start=2026/01/01", "?end=01-01-2026"} {
			r := httptest.NewRequest("GET", "/x"+q, nil)
			if _, err := queryparams.ParseUsageFilter(r, 0); err == nil {
				t.Fatalf("%s: want error", q)
			}
		}
	})

	t.Run("forced user id wins", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?user_id=42&group_id=7", nil)
		f, err := queryparams.ParseUsageFilter(r, 99)
		if err != nil {
			t.Fatalf("ParseUsageFilter: %v", err)
		}
		if f.UserID != 99 {
			t.Fatalf("UserID = %d, want forced 99", f.UserID)
		}
		if f.GroupID != 7 {
			t.Fatalf("GroupID = %d, want 7", f.GroupID)
		}
	})

	t.Run("explicit user id and empty values", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?user_id=5", nil)
		f, err := queryparams.ParseUsageFilter(r, 0)
		if err != nil {
			t.Fatalf("ParseUsageFilter: %v", err)
		}
		if f.UserID != 5 || f.GroupID != 0 || f.APIKeyID != 0 || f.AIModelID != 0 {
			t.Fatalf("filter = %+v, want user_id=5 and zeros elsewhere", f)
		}
	})
}
