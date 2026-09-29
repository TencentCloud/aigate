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

package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

// errStopAfterControl 用于在 socket 建立后、真正 connect 之前中断拨号，
// 使「是否委托给底层 dialer」的断言不依赖真实网络。
var errStopAfterControl = errors.New("stop after control")

// 说明：SSRF 防护始终启用（没有 enableSSRF=false 的生产路径），但「私网 / 回环」
// 是否放行由包级配置 SetAllowPrivateUpstreams 控制，默认严格（拒绝）。
// 因此用例分两档验证：默认严格档直接用默认 context；放行档先用
// withAllowPrivateUpstreams 打开配置，并由 t.Cleanup 还原。涉及包级全局配置，
// 用例间必须串行，禁止使用 t.Parallel。

// withAllowPrivateUpstreams 在用例内放行私网 / 回环地址，并在用例结束时还原为
// 严格模式，保证用例间隔离。
func withAllowPrivateUpstreams(t *testing.T) {
	t.Helper()
	SetAllowPrivateUpstreams(true)
	t.Cleanup(func() { SetAllowPrivateUpstreams(false) })
}

func TestIsDisallowedIP(t *testing.T) {
	cases := []struct {
		ip      string
		want    bool
		comment string
	}{
		{"127.0.0.1", true, "loopback"},
		{"127.0.0.53", true, "loopback range"},
		{"0.0.0.0", true, "unspecified"},
		{"10.0.0.1", true, "rfc1918 10/8"},
		{"172.16.0.1", true, "rfc1918 172.16/12"},
		{"172.31.255.254", true, "rfc1918 172.16/12 upper"},
		{"192.168.1.1", true, "rfc1918 192.168/16"},
		{"169.254.169.254", true, "AWS/Tencent metadata"},
		{"100.100.100.200", true, "Alibaba metadata"},
		{"169.254.0.1", true, "link-local"},
		{"100.64.0.1", true, "CGNAT"},
		{"100.127.255.254", true, "CGNAT upper"},
		{"224.0.0.1", true, "multicast"},
		{"255.255.255.255", true, "broadcast"},
		{"240.0.0.1", true, "reserved"},
		{"::1", true, "ipv6 loopback"},
		{"fc00::1", true, "ipv6 ULA"},
		{"fd12:3456:789a::1", true, "ipv6 ULA"},
		{"fe80::1", true, "ipv6 link-local"},
		{"ff02::1", true, "ipv6 multicast"},
		{"fd00:ec2::254", true, "AWS IMDSv6"},

		// Public addresses should pass.
		{"8.8.8.8", false, "google dns"},
		{"1.1.1.1", false, "cloudflare dns"},
		{"99.83.190.102", false, "public"},
		{"2606:4700:4700::1111", false, "cloudflare ipv6"},
		// CGNAT boundary just below — 100.63 belongs to AT&T public space.
		{"100.63.255.254", false, "below CGNAT"},
		{"100.128.0.1", false, "above CGNAT"},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("invalid test ip %q", c.ip)
		}
		if got := isDisallowedIP(ip); got != c.want {
			t.Errorf("isDisallowedIP(%s) = %v, want %v (%s)", c.ip, got, c.want, c.comment)
		}
	}
}

// TestIsDisallowedIP_Nil ensures a nil IP is treated as disallowed so that
// callers cannot bypass validation by passing a parse failure through.
func TestIsDisallowedIP_Nil(t *testing.T) {
	if !isDisallowedIP(nil) {
		t.Fatalf("isDisallowedIP(nil) = false, want true")
	}
}

// TestIsDisallowedIP_IPv4MappedIPv6 ensures that IPv4 addresses tunnelled
// through IPv6 syntax (::ffff:a.b.c.d) are normalised back to IPv4 and
// rejected if the underlying v4 address is internal. This is a classic
// SSRF bypass technique.
func TestIsDisallowedIP_IPv4MappedIPv6(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"::ffff:192.168.1.1", true},
		{"::ffff:8.8.8.8", false},
		{"::ffff:1.1.1.1", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("invalid test ip %q", c.ip)
		}
		if got := isDisallowedIP(ip); got != c.want {
			t.Errorf("isDisallowedIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// TestIsDisallowedIP_IPv4CompatibleIPv6 覆盖 IPv4-compatible IPv6（::a.b.c.d，废弃写法）：
// To4() 不识别这种形态，必须提取内嵌 IPv4 后按同一套规则判定，避免绕过。
func TestIsDisallowedIP_IPv4CompatibleIPv6(t *testing.T) {
	// 严格模式（默认）。
	strictCases := []struct {
		ip   string
		want bool
	}{
		{"::127.0.0.1", true},       // 内嵌回环
		{"::10.0.0.1", true},        // 内嵌 RFC1918
		{"::192.168.1.1", true},     // 内嵌 RFC1918
		{"::100.64.0.1", true},      // 内嵌 CGNAT
		{"::169.254.169.254", true}, // 内嵌 link-local / 元数据
		{"::100.100.100.200", true}, // 内嵌元数据地址
		{"::8.8.8.8", false},        // 内嵌公网地址
		{"::1.1.1.1", false},        // 内嵌公网地址
	}
	for _, c := range strictCases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("invalid test ip %q", c.ip)
		}
		if got := isDisallowedIP(ip); got != c.want {
			t.Errorf("strict: isDisallowedIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}

	// 放行私网模式：内嵌回环/私网放行，内嵌永久拒绝集仍拒绝。
	withAllowPrivateUpstreams(t)
	relaxedCases := []struct {
		ip   string
		want bool
	}{
		{"::127.0.0.1", false},
		{"::10.0.0.1", false},
		{"::192.168.1.1", false},
		{"::169.254.169.254", true},
		{"::100.100.100.200", true},
	}
	for _, c := range relaxedCases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("invalid test ip %q", c.ip)
		}
		if got := isDisallowedIP(ip); got != c.want {
			t.Errorf("relaxed: isDisallowedIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// TestIsDisallowedIP_RangeBoundaries verifies the edges of every numeric
// range so we catch off-by-one errors in the bitmask arithmetic.
func TestIsDisallowedIP_RangeBoundaries(t *testing.T) {
	cases := []struct {
		ip      string
		want    bool
		comment string
	}{
		// 172.16/12 boundaries — IsPrivate handles this.
		{"172.15.255.255", false, "just below 172.16/12"},
		{"172.16.0.0", true, "172.16/12 lower"},
		{"172.31.255.255", true, "172.16/12 upper"},
		{"172.32.0.0", false, "just above 172.16/12"},

		// 192.168/16 boundaries.
		{"192.167.255.255", false, "just below 192.168/16"},
		{"192.168.0.0", true, "192.168/16 lower"},
		{"192.168.255.255", true, "192.168/16 upper"},
		{"192.169.0.0", false, "just above 192.168/16"},

		// 10/8 boundaries.
		{"9.255.255.255", false, "just below 10/8"},
		{"10.0.0.0", true, "10/8 lower"},
		{"10.255.255.255", true, "10/8 upper"},
		{"11.0.0.0", false, "just above 10/8"},

		// CGNAT 100.64/10 boundaries.
		{"100.63.255.255", false, "just below 100.64/10"},
		{"100.64.0.0", true, "100.64/10 lower"},
		{"100.127.255.255", true, "100.64/10 upper"},
		{"100.128.0.0", false, "just above 100.64/10"},

		// 192.0.0/24 vs sibling 192.0.1/24 (the latter is public).
		{"192.0.0.255", true, "192.0.0/24 upper"},
		{"192.0.1.0", false, "just above 192.0.0/24"},

		// 198.18/15 benchmarking boundaries.
		{"198.17.255.255", false, "just below 198.18/15"},
		{"198.18.0.0", true, "198.18/15 lower"},
		{"198.19.255.255", true, "198.18/15 upper"},
		{"198.20.0.0", false, "just above 198.18/15"},

		// 240/4 reserved boundaries.
		{"239.255.255.255", true, "still multicast 224/4"},
		{"240.0.0.0", true, "240/4 lower"},
		{"254.255.255.255", true, "240/4 upper interior"},

		// Documentation prefixes.
		{"192.0.2.0", true, "TEST-NET-1 lower"},
		{"192.0.2.255", true, "TEST-NET-1 upper"},
		{"198.51.100.128", true, "TEST-NET-2"},
		{"203.0.113.7", true, "TEST-NET-3"},
		{"203.0.114.7", false, "just outside TEST-NET-3"},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("invalid test ip %q", c.ip)
		}
		if got := isDisallowedIP(ip); got != c.want {
			t.Errorf("isDisallowedIP(%s) = %v, want %v (%s)", c.ip, got, c.want, c.comment)
		}
	}
}

// TestIsDisallowedIP_IPv6Boundaries checks specific IPv6 ranges in addition
// to the basic sanity cases already covered by TestIsDisallowedIP.
func TestIsDisallowedIP_IPv6Boundaries(t *testing.T) {
	cases := []struct {
		ip      string
		want    bool
		comment string
	}{
		// ULA fc00::/7 covers fc00:: through fdff::.
		{"fc00::", true, "ULA lower"},
		{"fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true, "ULA upper"},
		// fe00:: is outside ULA but is reserved/unassigned — Go's IsPrivate
		// returns false; we don't currently reject it. Document via false.
		{"fe00::1", false, "outside ULA, currently allowed"},
		// fe80::/10 link-local handled by IsLinkLocalUnicast.
		{"fe80::1", true, "link-local lower"},
		{"febf:ffff:ffff:ffff::1", true, "link-local upper"},
		// 2001:db8::/32 documentation.
		{"2001:db8::", true, "TEST docs lower"},
		{"2001:db8:ffff:ffff:ffff:ffff:ffff:ffff", true, "TEST docs upper"},
		{"2001:db9::", false, "just outside TEST docs"},
		// Multicast ff00::/8.
		{"ff00::1", true, "multicast"},
		{"ff15::1234", true, "site-local multicast"},
		// Public IPv6.
		{"2606:4700::1", false, "cloudflare"},
		{"2400:cb00::", false, "public"},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("invalid test ip %q", c.ip)
		}
		if got := isDisallowedIP(ip); got != c.want {
			t.Errorf("isDisallowedIP(%s) = %v, want %v (%s)", c.ip, got, c.want, c.comment)
		}
	}
}

// ---------------------------------------------------------------------------
// validateOutboundURL cases
// ---------------------------------------------------------------------------

func TestValidateOutboundURL_BlockedSchemes(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"gopher://127.0.0.1:11211/_stats",
		"ftp://example.com/",
		"ldap://internal/",
	} {
		err := validateOutboundURL(context.Background(), raw)
		if err == nil {
			t.Errorf("expected SSRF block for %q, got nil", raw)
			continue
		}
		if !errors.Is(err, errSSRFBlocked) {
			t.Errorf("expected errSSRFBlocked for %q, got %v", raw, err)
		}
	}
}

func TestValidateOutboundURL_BlockedLiteralIPs(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/",
		"https://10.0.0.5:8443/v1/models",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		"http://[fc00::1]/",
		"https://192.168.0.1/",
		"http://100.64.1.2/",
	} {
		err := validateOutboundURL(context.Background(), raw)
		if err == nil {
			t.Errorf("expected SSRF block for %q, got nil", raw)
			continue
		}
		if !errors.Is(err, errSSRFBlocked) {
			t.Errorf("expected errSSRFBlocked for %q, got %v", raw, err)
		}
	}
}

func TestValidateOutboundURL_AllowsPublicLiteralIP(t *testing.T) {
	if err := validateOutboundURL(context.Background(), "https://1.1.1.1/"); err != nil {
		t.Fatalf("expected public IP to be allowed, got %v", err)
	}
}

func TestValidateOutboundURL_EmptyHost(t *testing.T) {
	err := validateOutboundURL(context.Background(), "http:///path")
	if err == nil || !errors.Is(err, errSSRFBlocked) {
		t.Fatalf("expected errSSRFBlocked for empty host, got %v", err)
	}
}

// TestValidateOutboundURL_AlwaysBlocksInternal verifies that validateOutboundURL
// validates unconditionally — there is no opt-out switch any more.
func TestValidateOutboundURL_AlwaysBlocksInternal(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/",
		"http://169.254.169.254/",
		"file:///etc/passwd",
	} {
		if err := validateOutboundURL(context.Background(), raw); err == nil {
			t.Errorf("validateOutboundURL(%q) = nil, want error", raw)
		}
	}
	// Malformed URLs should also be blocked.
	for _, raw := range []string{
		"not a url at all",
		"",
	} {
		if err := validateOutboundURL(context.Background(), raw); err == nil {
			t.Errorf("validateOutboundURL(%q) = nil, want error", raw)
		}
	}
}

// TestValidateOutboundURL_RejectsCustomSchemes covers a mixed bag of
// non-https schemes that have all been used in real-world SSRF exploits.
func TestValidateOutboundURL_RejectsCustomSchemes(t *testing.T) {
	for _, raw := range []string{
		"dict://1.1.1.1/",
		"jar:https://1.1.1.1/",
		"netdoc://1.1.1.1/",
		"sftp://example.com/",
		"data:text/plain,hello",
		"javascript:alert(1)",
	} {
		err := validateOutboundURL(context.Background(), raw)
		if err == nil || !errors.Is(err, errSSRFBlocked) {
			t.Errorf("expected errSSRFBlocked for %q, got %v", raw, err)
		}
	}
}

// TestValidateOutboundURL_RejectsMalformedURL ensures that url.Parse errors
// are surfaced as SSRF blocks rather than swallowed.
func TestValidateOutboundURL_RejectsMalformedURL(t *testing.T) {
	for _, raw := range []string{
		"https://exa mple.com/", // space in host
		"https://[::1",          // unterminated bracket
		"https://%ZZ",           // invalid percent escape
	} {
		err := validateOutboundURL(context.Background(), raw)
		if err == nil || !errors.Is(err, errSSRFBlocked) {
			t.Errorf("expected errSSRFBlocked for malformed %q, got %v", raw, err)
		}
	}
}

// TestValidateOutboundURL_BlocksIPv4MappedIPv6Literal closes the bypass
// where an attacker submits ::ffff:127.0.0.1 to slip past v4-only filters.
func TestValidateOutboundURL_BlocksIPv4MappedIPv6Literal(t *testing.T) {
	for _, raw := range []string{
		"https://[::ffff:127.0.0.1]/",
		"https://[::ffff:10.0.0.1]/",
		"https://[::ffff:169.254.169.254]/",
	} {
		err := validateOutboundURL(context.Background(), raw)
		if err == nil || !errors.Is(err, errSSRFBlocked) {
			t.Errorf("expected errSSRFBlocked for %q, got %v", raw, err)
		}
	}
}

// TestValidateOutboundURL_AllowsPublicWithPortAndPath exercises a non-trivial
// URL shape (path, query, port) to make sure the parser branch only inspects
// the host.
func TestValidateOutboundURL_AllowsPublicWithPortAndPath(t *testing.T) {
	if err := validateOutboundURL(context.Background(), "https://1.1.1.1:8443/v1/models?x=1"); err != nil {
		t.Fatalf("expected public URL to be allowed, got %v", err)
	}
}

// TestValidateOutboundURL_LookupFailureIsTolerated ensures that DNS errors
// are NOT treated as SSRF blocks (the dial layer will surface a clearer
// network error). Using a TLD that cannot resolve.
func TestValidateOutboundURL_LookupFailureIsTolerated(t *testing.T) {
	// The .invalid TLD is reserved by RFC 2606 and must never resolve.
	err := validateOutboundURL(context.Background(), "https://nonexistent.invalid/")
	if err != nil {
		t.Fatalf("DNS failure should be tolerated by validateOutboundURL, got %v", err)
	}
}

// TestValidateOutboundURLError_MessageAndSentinel 校验英文文案与 sentinel 同时保留。
func TestValidateOutboundURLError_MessageAndSentinel(t *testing.T) {
	if got := errSSRFBlocked.Error(); got != "target address not allowed" {
		t.Fatalf("errSSRFBlocked.Error() = %q, want %q", got, "target address not allowed")
	}

	tests := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{name: "scheme", raw: "file:///etc/passwd", wantSub: "url scheme not allowed: file"},
		{name: "empty host", raw: "http:///path", wantSub: "empty host"},
		{name: "literal internal ip", raw: "http://127.0.0.1/", wantSub: "target address not allowed: 127.0.0.1"},
		{name: "unparseable", raw: "https://%ZZ", wantSub: "failed to parse target url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateOutboundURL(context.Background(), tt.raw)
			if err == nil {
				t.Fatalf("expected error for %q", tt.raw)
			}
			if !errors.Is(err, errSSRFBlocked) {
				t.Fatalf("errors.Is(err, errSSRFBlocked) = false, err = %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("err = %q, want it to contain %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// provider 方法层的 SSRF 拦截（防护恒开）
// ---------------------------------------------------------------------------

func TestOpenAIProbe_SSRFBlocked(t *testing.T) {
	p := &OpenAIProvider{}
	for _, base := range []string{
		"http://127.0.0.1:8080",
		"http://10.0.0.1",
		"http://localhost",
		"file:///etc/passwd",
	} {
		_, err := p.CheckConnectivityWithChat(context.Background(), "sk", base, "gpt-4o-mini")
		if err == nil {
			t.Errorf("base=%q: expected SSRF block, got nil", base)
			continue
		}
		if !errors.Is(err, ErrNetworkUnreachable) {
			t.Errorf("base=%q: err = %v, want ErrNetworkUnreachable", base, err)
		}
		var ce *ConnectivityError
		if !errors.As(err, &ce) || ce.Cause == nil || !errors.Is(ce.Cause, errSSRFBlocked) {
			t.Errorf("base=%q: expected ConnectivityError wrapping errSSRFBlocked, got %v", base, err)
		}
	}
}

func TestAnthropicProbe_SSRFBlocked(t *testing.T) {
	p := &AnthropicProvider{}
	for _, base := range []string{
		"http://127.0.0.1:9090",
		"http://192.168.1.1",
		"http://[::1]:8080",
	} {
		_, err := p.CheckConnectivityWithChat(context.Background(), "sk", base, "claude")
		if err == nil {
			t.Errorf("base=%q: expected SSRF block, got nil", base)
			continue
		}
		if !errors.Is(err, ErrNetworkUnreachable) {
			t.Errorf("base=%q: err = %v, want ErrNetworkUnreachable", base, err)
		}
		var ce *ConnectivityError
		if !errors.As(err, &ce) {
			t.Errorf("base=%q: err is not *ConnectivityError: %v", base, err)
			continue
		}
		if ce.Cause == nil || !errors.Is(ce.Cause, errSSRFBlocked) {
			t.Errorf("base=%q: expected SSRF cause, got %v", base, ce.Cause)
		}
	}
}

func TestOpenAIRequestMethods_SSRFBlocked(t *testing.T) {
	p := &OpenAIProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	_, status, err := p.ChatCompletion(context.Background(), "sk", "http://127.0.0.1:8080", "gpt-4o", body, nil)
	if err == nil || !errors.Is(err, errSSRFBlocked) {
		t.Fatalf("ChatCompletion err = %v, want errSSRFBlocked", err)
	}
	if status != 0 {
		t.Fatalf("ChatCompletion status = %d, want 0", status)
	}

	rec := httptest.NewRecorder()
	_, status, err = p.ChatCompletionStream(
		context.Background(),
		"sk",
		"http://127.0.0.1:8080",
		"gpt-4o",
		body,
		rec,
		rec,
		nil,
	)
	if err == nil || !errors.Is(err, errSSRFBlocked) {
		t.Fatalf("ChatCompletionStream err = %v, want errSSRFBlocked", err)
	}
	if status != 0 {
		t.Fatalf("ChatCompletionStream status = %d, want 0", status)
	}
}

func TestAnthropicRequestMethods_SSRFBlocked(t *testing.T) {
	p := &AnthropicProvider{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	_, status, err := p.ChatCompletion(context.Background(), "sk", "http://169.254.169.254", "claude", body, nil)
	if err == nil || !errors.Is(err, errSSRFBlocked) {
		t.Fatalf("ChatCompletion err = %v, want errSSRFBlocked", err)
	}
	if status != 0 {
		t.Fatalf("ChatCompletion status = %d, want 0", status)
	}

	rec := httptest.NewRecorder()
	_, status, err = p.ChatCompletionStream(
		context.Background(),
		"sk",
		"http://169.254.169.254",
		"claude",
		body,
		rec,
		rec,
		nil,
	)
	if err == nil || !errors.Is(err, errSSRFBlocked) {
		t.Fatalf("ChatCompletionStream err = %v, want errSSRFBlocked", err)
	}
	if status != 0 {
		t.Fatalf("ChatCompletionStream status = %d, want 0", status)
	}
}

// ---------------------------------------------------------------------------
// ssrfSafeDialContext direct tests
// ---------------------------------------------------------------------------

// TestSSRFSafeDialContext_RejectsLiteralInternalIP verifies that the dial
// hook short-circuits before the OS-level connect when handed an internal
// literal IP, defeating DNS-rebinding even when validateOutboundURL has
// already passed.
func TestSSRFSafeDialContext_RejectsLiteralInternalIP(t *testing.T) {
	dialer := &net.Dialer{Timeout: time.Second}
	dial := ssrfSafeDialContext(dialer, true)

	for _, addr := range []string{
		"127.0.0.1:80",
		"10.1.2.3:443",
		"169.254.169.254:80",
		"[::1]:80",
		"[fc00::1]:443",
	} {
		conn, err := dial(context.Background(), "tcp", addr)
		if conn != nil {
			conn.Close()
			t.Errorf("expected dial to fail for %q, got a connection", addr)
		}
		if err == nil {
			t.Errorf("expected error for %q, got nil", addr)
			continue
		}
		if !errors.Is(err, errSSRFBlocked) {
			t.Errorf("dial(%q): err = %v, want wrapping errSSRFBlocked", addr, err)
		}
	}
}

// TestSSRFSafeDialContext_BadAddrFormat verifies that a malformed addr is
// reported through the underlying SplitHostPort error, not silently dropped.
func TestSSRFSafeDialContext_BadAddrFormat(t *testing.T) {
	dialer := &net.Dialer{Timeout: time.Second}
	dial := ssrfSafeDialContext(dialer, true)

	conn, err := dial(context.Background(), "tcp", "not-an-address")
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatalf("expected error for malformed addr, got nil")
	}
}

// TestSSRFSafeDialContext_GuardAlwaysActive 覆盖「防护恒开」下的两条路径：
// 回环地址被拒；允许的公网字面量则透传给底层 dialer（错误不来自 SSRF 拦截）。
func TestSSRFSafeDialContext_GuardAlwaysActive(t *testing.T) {
	t.Run("loopback blocked", func(t *testing.T) {
		dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
		dial := ssrfSafeDialContext(dialer, true)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		conn, err := dial(ctx, "tcp", "127.0.0.1:1")
		if conn != nil {
			conn.Close()
		}
		if err == nil {
			t.Fatalf("expected SSRF block for loopback, got nil")
		}
		if !errors.Is(err, errSSRFBlocked) {
			t.Fatalf("err = %v, want errSSRFBlocked", err)
		}
	})

	t.Run("public literal delegates to base dialer", func(t *testing.T) {
		// 1.1.1.1 是通过校验的公网地址：必须交给底层 dialer。
		// 用 Control 钩子在真正 connect 之前中断，既证明委托发生、又不需要真实网络。
		var controlledAddr string
		dialer := &net.Dialer{
			Timeout: 300 * time.Millisecond,
			Control: func(network, address string, c syscall.RawConn) error {
				controlledAddr = address
				return errStopAfterControl
			},
		}
		dial := ssrfSafeDialContext(dialer, true)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		conn, err := dial(ctx, "tcp", "1.1.1.1:9")
		if conn != nil {
			conn.Close()
			t.Fatalf("expected control hook to abort the dial")
		}
		if err == nil {
			t.Fatalf("expected abort from control hook, got nil")
		}
		if errors.Is(err, errSSRFBlocked) {
			t.Fatalf("public address must not be SSRF-blocked, got %v", err)
		}
		if controlledAddr != "1.1.1.1:9" {
			t.Fatalf("control hook address = %q, want 1.1.1.1:9 (dial delegated to base dialer)", controlledAddr)
		}
	})

	t.Run("internal literal never reaches base dialer", func(t *testing.T) {
		called := false
		dialer := &net.Dialer{
			Timeout: 300 * time.Millisecond,
			Control: func(network, address string, c syscall.RawConn) error {
				called = true
				return errStopAfterControl
			},
		}
		dial := ssrfSafeDialContext(dialer, true)

		conn, err := dial(context.Background(), "tcp", "127.0.0.1:9")
		if conn != nil {
			conn.Close()
		}
		if !errors.Is(err, errSSRFBlocked) {
			t.Fatalf("err = %v, want errSSRFBlocked", err)
		}
		if called {
			t.Fatalf("control hook must not run for a blocked address")
		}
	})
}

// ---------------------------------------------------------------------------
// newSSRFSafeHTTPClient tests
// ---------------------------------------------------------------------------

// TestNewSSRFSafeHTTPClient_HasTimeout verifies that the constructor wires
// the timeout through (defence-in-depth: a long-running internal handler
// must not be able to keep the probe goroutine pinned indefinitely).
func TestNewSSRFSafeHTTPClient_HasTimeout(t *testing.T) {
	c := newSSRFSafeHTTPClient(7*time.Second, true)
	if c.Timeout != 7*time.Second {
		t.Fatalf("timeout = %v, want 7s", c.Timeout)
	}
	if c.Transport == nil {
		t.Fatalf("transport should not be nil")
	}
	if c.CheckRedirect == nil {
		t.Fatalf("CheckRedirect should not be nil")
	}
}

// TestNewSSRFSafeHTTPClient_RejectsInternalDestination verifies that an
// end-to-end Do() call against a literal internal IP is blocked at the dial
// layer, not just at validateOutboundURL.
func TestNewSSRFSafeHTTPClient_RejectsInternalDestination(t *testing.T) {
	c := newSSRFSafeHTTPClient(2*time.Second, true)
	req, err := http.NewRequest("GET", "https://10.0.0.1/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := c.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatalf("expected dial-layer SSRF block, got nil")
	}
	if !errors.Is(err, errSSRFBlocked) && !strings.Contains(err.Error(), errSSRFBlocked.Error()) {
		t.Fatalf("expected SSRF error, got %v", err)
	}
}

// TestNewSSRFSafeHTTPClient_CheckRedirect_BlocksInternal directly drives
// the CheckRedirect closure with a synthetic redirect target, isolating
// the redirect-validation behaviour from network I/O.
func TestNewSSRFSafeHTTPClient_CheckRedirect_BlocksInternal(t *testing.T) {
	c := newSSRFSafeHTTPClient(time.Second, true)

	internal, _ := http.NewRequest("GET", "https://127.0.0.1/", nil)
	if err := c.CheckRedirect(internal, nil); err == nil || !errors.Is(err, errSSRFBlocked) {
		t.Fatalf("CheckRedirect to internal: err = %v, want errSSRFBlocked", err)
	}

	metadata, _ := http.NewRequest("GET", "https://169.254.169.254/", nil)
	if err := c.CheckRedirect(metadata, nil); err == nil || !errors.Is(err, errSSRFBlocked) {
		t.Fatalf("CheckRedirect to metadata: err = %v, want errSSRFBlocked", err)
	}

	file, _ := http.NewRequest("GET", "file:///etc/passwd", nil)
	if err := c.CheckRedirect(file, nil); err == nil || !errors.Is(err, errSSRFBlocked) {
		t.Fatalf("CheckRedirect to file scheme: err = %v, want errSSRFBlocked", err)
	}
}

// TestNewSSRFSafeHTTPClient_CheckRedirect_AllowsPublic verifies that a
// public redirect target is permitted (otherwise legitimate providers that
// 30x to a regional endpoint would break).
func TestNewSSRFSafeHTTPClient_CheckRedirect_AllowsPublic(t *testing.T) {
	c := newSSRFSafeHTTPClient(time.Second, true)

	pub, _ := http.NewRequest("GET", "https://1.1.1.1/v1/models", nil)
	if err := c.CheckRedirect(pub, nil); err != nil {
		t.Fatalf("CheckRedirect to public IP: err = %v, want nil", err)
	}
}

// TestRedirectStripsCrossHostAuthHeaders 端到端验证跨主机重定向时的鉴权头剥离：
// 上游密钥（Authorization / x-api-key）不得随 30x 带往另一台主机；
// 同主机（同 host:port）跳转则保留。
func TestRedirectStripsCrossHostAuthHeaders(t *testing.T) {
	withAllowPrivateUpstreams(t)

	var sameHostAPIKey, crossHostAPIKey, crossHostAuth string
	// 目标服务器（跨主机跳转的落点）。
	cross := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		crossHostAPIKey = r.Header.Get("X-Api-Key")
		crossHostAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer cross.Close()

	// 同主机跳转目标与跨主机跳转入口共用一台服务器。
	mux := http.NewServeMux()
	origin := httptest.NewServer(mux)
	defer origin.Close()
	mux.HandleFunc("/same-host", func(w http.ResponseWriter, r *http.Request) {
		sameHostAPIKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/redirect-same", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, origin.URL+"/same-host", http.StatusFound)
	})
	crossURL := strings.Replace(cross.URL, "127.0.0.1", "localhost", 1)
	mux.HandleFunc("/redirect-cross", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, crossURL+"/next", http.StatusFound)
	})

	client := newSSRFSafeHTTPClient(5*time.Second, true)
	do := func(path string) {
		t.Helper()
		req, err := http.NewRequest("GET", origin.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer sk-upstream")
		req.Header.Set("x-api-key", "sk-secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do %s: %v", path, err)
		}
		resp.Body.Close()
	}

	do("/redirect-same")
	if sameHostAPIKey != "sk-secret" {
		t.Fatalf("same-host redirect dropped x-api-key: %q", sameHostAPIKey)
	}

	do("/redirect-cross")
	if crossHostAPIKey != "" {
		t.Fatalf("cross-host redirect leaked x-api-key: %q", crossHostAPIKey)
	}
	if crossHostAuth != "" {
		t.Fatalf("cross-host redirect leaked Authorization: %q", crossHostAuth)
	}
}

// TestNewSSRFSafeHTTPClient_AlwaysEnforcesSSRF documents that the constructor
// wires an always-on guard: there is no opt-out path for internal targets.
func TestNewSSRFSafeHTTPClient_AlwaysEnforcesSSRF(t *testing.T) {
	c := newSSRFSafeHTTPClient(time.Second, true)
	for _, raw := range []string{
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://169.254.169.254/",
		"file:///etc/passwd",
	} {
		req, _ := http.NewRequest("GET", raw, nil)
		if err := c.CheckRedirect(req, nil); err == nil || !errors.Is(err, errSSRFBlocked) {
			t.Fatalf("CheckRedirect(%q): err = %v, want errSSRFBlocked", raw, err)
		}
	}
}

func TestDoSSRFProtectedRequest_BlocksLoopback(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1/", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if _, err := DoSSRFProtectedRequest(req, time.Second); err == nil || !errors.Is(err, errSSRFBlocked) {
		t.Fatalf("DoSSRFProtectedRequest() error = %v, want errSSRFBlocked", err)
	}
}

// TestDoSSRFProtectedRequest_BlocksDisallowedSchemes 覆盖非 http(s) scheme。
func TestDoSSRFProtectedRequest_BlocksDisallowedSchemes(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "gopher://127.0.0.1:11211/_stats"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("create request for %q: %v", raw, err)
		}
		if _, err := DoSSRFProtectedRequest(req, time.Second); err == nil || !errors.Is(err, errSSRFBlocked) {
			t.Fatalf("DoSSRFProtectedRequest(%q) error = %v, want errSSRFBlocked", raw, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 私网 / 回环放行开关
// ---------------------------------------------------------------------------

// TestAllowPrivateUpstreams_DefaultIsStrictAndToggle 校验包级配置的默认值与读写行为。
func TestAllowPrivateUpstreams_DefaultIsStrictAndToggle(t *testing.T) {
	// 先归零，避免受其它用例残留影响；同时验证 atomic.Bool 零值即严格模式。
	SetAllowPrivateUpstreams(false)
	t.Cleanup(func() { SetAllowPrivateUpstreams(false) })
	if allowPrivateUpstreams() {
		t.Fatalf("allowPrivateUpstreams() = true by default, want false (strict mode)")
	}

	SetAllowPrivateUpstreams(true)
	if !allowPrivateUpstreams() {
		t.Fatalf("allowPrivateUpstreams() = false after SetAllowPrivateUpstreams(true)")
	}
	SetAllowPrivateUpstreams(false)
	if allowPrivateUpstreams() {
		t.Fatalf("allowPrivateUpstreams() = true after SetAllowPrivateUpstreams(false)")
	}
}

// TestSSRFPolicy_StrictRejectsPrivate 覆盖默认严格模式：回环 / 私网地址在 URL 层、
// dial 层、redirect 层全部被拒。
func TestSSRFPolicy_StrictRejectsPrivate(t *testing.T) {
	SetAllowPrivateUpstreams(false)
	t.Cleanup(func() { SetAllowPrivateUpstreams(false) })

	urls := []string{
		"http://127.0.0.1/",
		"http://127.0.0.53:8080/",
		"http://[::1]/",
		"http://10.0.0.5/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://100.64.1.2/",
		"http://198.18.0.1/",
		"http://[fc00::1]/",
		"http://[2001:db8::1]/",
	}
	for _, raw := range urls {
		if err := validateOutboundURL(context.Background(), raw); err == nil || !errors.Is(err, errSSRFBlocked) {
			t.Errorf("strict mode: validateOutboundURL(%q) = %v, want errSSRFBlocked", raw, err)
		}
	}

	dial := ssrfSafeDialContext(&net.Dialer{Timeout: time.Second}, true)
	for _, addr := range []string{
		"127.0.0.1:80",
		"10.1.2.3:443",
		"172.16.0.1:80",
		"192.168.1.1:80",
		"100.64.0.1:80",
		"[::1]:80",
		"[fc00::1]:443",
	} {
		conn, err := dial(context.Background(), "tcp", addr)
		if conn != nil {
			conn.Close()
		}
		if err == nil || !errors.Is(err, errSSRFBlocked) {
			t.Errorf("strict mode: dial(%q) = %v, want errSSRFBlocked", addr, err)
		}
	}

	c := newSSRFSafeHTTPClient(time.Second, true)
	for _, raw := range []string{"https://127.0.0.1/", "https://10.0.0.1/", "https://[fc00::1]/"} {
		req, _ := http.NewRequest("GET", raw, nil)
		if err := c.CheckRedirect(req, nil); err == nil || !errors.Is(err, errSSRFBlocked) {
			t.Errorf("strict mode: CheckRedirect(%q) = %v, want errSSRFBlocked", raw, err)
		}
	}
}

// TestSSRFPolicy_AllowPrivatePermitsPrivate 覆盖放行模式：回环 / 私网地址在 URL 层、
// dial 层、redirect 层全部放行，并能端到端访问真实可达的 loopback mock 上游。
func TestSSRFPolicy_AllowPrivatePermitsPrivate(t *testing.T) {
	withAllowPrivateUpstreams(t)

	urls := []string{
		"http://127.0.0.1/",
		"http://127.0.0.53:8080/",
		"http://[::1]/",
		"http://10.0.0.5/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://100.64.1.2/",
		"http://198.18.0.1/",
		"http://[fc00::1]/",
		"http://[2001:db8::1]/",
	}
	for _, raw := range urls {
		if err := validateOutboundURL(context.Background(), raw); err != nil {
			t.Errorf("allow-private mode: validateOutboundURL(%q) = %v, want nil", raw, err)
		}
	}

	// dial 层：loopback 已放行，必须能对可达的 mock 成功建连。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("parse mock addr %q: %v", srv.URL, err)
	}
	dial := ssrfSafeDialContext(&net.Dialer{Timeout: 2 * time.Second}, true)
	conn, err := dial(context.Background(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatalf("allow-private mode: dial to loopback mock failed: %v", err)
	}
	conn.Close()

	// 端到端：newSSRFSafeHTTPClient 能把请求真正送达 loopback mock。
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := newSSRFSafeHTTPClient(2*time.Second, true).Do(req)
	if err != nil {
		t.Fatalf("allow-private mode: Do() to loopback mock failed: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("Do() status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	resp.Body.Close()

	// redirect 层：私网目标不再被拒。
	c := newSSRFSafeHTTPClient(time.Second, true)
	for _, raw := range []string{"https://127.0.0.1/", "https://10.0.0.1/", "https://[fc00::1]/"} {
		req, _ := http.NewRequest("GET", raw, nil)
		if err := c.CheckRedirect(req, nil); err != nil {
			t.Errorf("allow-private mode: CheckRedirect(%q) = %v, want nil", raw, err)
		}
	}
}

// TestSSRFPolicy_AllowPrivateStillRejectsPermanent 覆盖放行模式下的永久拒绝集：
// 元数据、link-local、未指定、多播、广播、保留网段与 TEST-NET 在任何配置下都被拒。
func TestSSRFPolicy_AllowPrivateStillRejectsPermanent(t *testing.T) {
	withAllowPrivateUpstreams(t)

	ips := []string{
		"169.254.169.254", // AWS / Tencent metadata
		"100.100.100.200", // Alibaba metadata
		"fd00:ec2::254",   // AWS IMDSv6
		"169.254.0.1",     // IPv4 link-local
		"fe80::1",         // IPv6 link-local
		"0.0.0.0",         // IPv4 unspecified
		"::",              // IPv6 unspecified
		"224.0.0.1",       // IPv4 multicast
		"ff02::1",         // IPv6 multicast
		"255.255.255.255", // broadcast
		"0.1.2.3",         // 0.0.0.0/8
		"192.0.0.9",       // 192.0.0.0/24
		"240.0.0.1",       // 240.0.0.0/4
		"192.0.2.1",       // TEST-NET-1
		"198.51.100.1",    // TEST-NET-2
		"203.0.113.1",     // TEST-NET-3
	}
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("invalid test ip %q", s)
		}
		if !isDisallowedIP(ip) {
			t.Errorf("allow-private mode: isDisallowedIP(%s) = false, want true (permanently rejected)", s)
		}
	}

	// URL 层同样拒绝。
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://100.100.100.200/",
		"http://[fd00:ec2::254]/",
		"http://[fe80::1]/",
		"http://0.0.0.0/",
		"http://192.0.2.1/",
		"http://255.255.255.255/",
	} {
		if err := validateOutboundURL(context.Background(), raw); err == nil || !errors.Is(err, errSSRFBlocked) {
			t.Errorf("allow-private mode: validateOutboundURL(%q) = %v, want errSSRFBlocked", raw, err)
		}
	}

	// dial 层同样拒绝。
	dial := ssrfSafeDialContext(&net.Dialer{Timeout: time.Second}, true)
	for _, addr := range []string{
		"169.254.169.254:80",
		"100.100.100.200:80",
		"[fd00:ec2::254]:80",
		"[fe80::1]:80",
		"192.0.2.1:80",
	} {
		conn, err := dial(context.Background(), "tcp", addr)
		if conn != nil {
			conn.Close()
		}
		if err == nil || !errors.Is(err, errSSRFBlocked) {
			t.Errorf("allow-private mode: dial(%q) = %v, want errSSRFBlocked", addr, err)
		}
	}
}

// TestSetAllowPrivateUpstreams_IsolationAcrossTests 验证 t.Cleanup 还原后，
// 后续用例看到的仍是严格模式（用例间隔离）。
func TestSetAllowPrivateUpstreams_IsolationAcrossTests(t *testing.T) {
	if allowPrivateUpstreams() {
		t.Fatalf("allowPrivateUpstreams() = true at test start, a previous test failed to restore the config")
	}
	t.Run("inner enables then cleanup restores", func(t *testing.T) {
		withAllowPrivateUpstreams(t)
		if !allowPrivateUpstreams() {
			t.Fatalf("inside subtest: allowPrivateUpstreams() = false, want true")
		}
	})
	// 子用例的 t.Cleanup 已执行，此处必须回到严格模式。
	if allowPrivateUpstreams() {
		t.Fatalf("after subtest: allowPrivateUpstreams() = true, want false")
	}
}
