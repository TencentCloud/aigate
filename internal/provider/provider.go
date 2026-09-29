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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aigate/internal/protocol"
)

// Usage holds token counts returned by the upstream provider.
type Usage struct {
	PromptTokens           int
	CompletionTokens       int
	TotalTokens            int
	PromptCacheReadTokens  int
	PromptCacheWriteTokens int
}

// CompletionResult holds a non-streaming response body and parsed usage.
type CompletionResult struct {
	Body  []byte
	Usage *Usage
}

// StreamResult 封装流式响应的结果（usage 统计）。
type StreamResult struct {
	Usage *Usage
}

var (
	// ErrNetworkUnreachable means the request never reached the upstream
	// (DNS failure, dial error, TLS handshake failure, timeout, ctx cancel…).
	ErrNetworkUnreachable = errors.New("network unreachable")

	// ErrInvalidAPIKey means the upstream rejected the credentials (HTTP 401).
	ErrInvalidAPIKey = errors.New("invalid api key")

	// ErrForbidden means the upstream returned HTTP 403 (key valid but the
	// account/region/model is not permitted).
	ErrForbidden = errors.New("forbidden")

	// ErrRateLimited means the upstream returned HTTP 429.
	ErrRateLimited = errors.New("rate limited")

	// ErrUpstreamServer means the upstream returned a 5xx status.
	ErrUpstreamServer = errors.New("upstream server error")

	// ErrUpstreamClient means the upstream returned a non-401/403/429 4xx
	// status (bad request, not found, etc).
	ErrUpstreamClient = errors.New("upstream client error")
)

type ConnectivityError struct {
	Kind       error  // one of the Err* sentinels above
	StatusCode int    // HTTP status code; 0 when the request never completed
	Snippet    string // truncated upstream response body, may be empty
	Cause      error  // the original transport error, may be nil
}

// Error renders a human readable message describing the failure.
func (e *ConnectivityError) Error() string {
	switch {
	case e.StatusCode == 0 && e.Cause != nil:
		return fmt.Sprintf("%s: %v", e.Kind.Error(), e.Cause)
	case e.Snippet != "":
		return fmt.Sprintf("%s (status %d): %s", e.Kind.Error(), e.StatusCode, e.Snippet)
	case e.StatusCode != 0:
		return fmt.Sprintf("%s (status %d)", e.Kind.Error(), e.StatusCode)
	default:
		return e.Kind.Error()
	}
}

// Unwrap returns the sentinel kind so errors.Is(err, ErrInvalidAPIKey) works.
func (e *ConnectivityError) Unwrap() error { return e.Kind }

// classifyHTTPStatus maps an HTTP status code (>=400) to the matching sentinel.
// Returns nil for 2xx/3xx, signalling success.
func classifyHTTPStatus(status int) error {
	switch {
	case status >= 200 && status < 400:
		return nil
	case status == http.StatusUnauthorized:
		return ErrInvalidAPIKey
	case status == http.StatusForbidden:
		return ErrForbidden
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status >= 500:
		return ErrUpstreamServer
	default:
		return ErrUpstreamClient
	}
}

// Provider abstracts an LLM provider's chat completion API.
type Provider interface {
	// ChatCompletion sends a non-streaming request and returns the raw
	// OpenAI-compatible JSON response body plus parsed provider metadata.
	// customHeaders are additional HTTP headers to include in the upstream request.
	ChatCompletion(
		ctx context.Context,
		apiKey, apiBase, model string,
		reqBody []byte,
		customHeaders map[string]string,
	) (result *CompletionResult, statusCode int, err error)

	// ChatCompletionStream sends a streaming request and writes OpenAI-compatible
	// SSE chunks to w. It returns the StreamResult (usage) when available.
	// customHeaders are additional HTTP headers to include in the upstream request.
	ChatCompletionStream(
		ctx context.Context,
		apiKey, apiBase, model string,
		reqBody []byte,
		w http.ResponseWriter,
		flusher http.Flusher,
		customHeaders map[string]string,
	) (result *StreamResult, statusCode int, err error)

	// CheckConnectivityWithChat probes the upstream provider by issuing a
	// minimal chat completion request (one user message, max_tokens=1).
	// This is more expensive than CheckConnectivity but works on providers
	// that don't expose a list-models endpoint, and additionally validates
	// that the specified model is actually invocable with the supplied key.
	//
	// On success it returns the round-trip latency and a nil error. On
	// failure it returns a *ConnectivityError wrapping one of the Err*
	// sentinels; callers can use errors.Is to classify the failure.
	CheckConnectivityWithChat(ctx context.Context, apiKey, apiBase, model string) (latency time.Duration, err error)
}

// richErrorf 用英文文案替换上游实现里的 i18n 文案，并通过 %w 把 base 保留在
// 错误链中，保证 errors.Is(err, base) 仍可命中（例如 errSSRFBlocked）。
func richErrorf(base error, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if base == nil {
		return errors.New(msg)
	}
	return fmt.Errorf("%s: %w", msg, base)
}

// applyCustomHeaders 将用户自定义的 HTTP 头设置到请求上。
// 不会覆盖已有的标准头（如 Content-Type、Authorization）。
func applyCustomHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		} else {
			slog.WarnContext(req.Context(), "header already set, custom header skipped", slog.String("header", k))
		}
	}
}

func GetProvider(modelType string) Provider {
	switch modelType {
	case protocol.TypeAnthropicMessages:
		return &AnthropicProvider{}
	default:
		return &OpenAIProvider{}
	}
}

// readBodySnippet reads up to maxBytes from r and returns it as a single-line
// trimmed string, suitable for embedding into a ConnectivityError.Snippet.
// The body is fully drained so the underlying connection can be reused.
func readBodySnippet(r io.Reader, maxBytes int64) string {
	if maxBytes <= 0 {
		maxBytes = 512
	}
	buf, _ := io.ReadAll(io.LimitReader(r, maxBytes))
	// Drain anything that exceeds maxBytes to allow connection reuse.
	_, _ = io.Copy(io.Discard, r)
	s := strings.TrimSpace(string(buf))
	// Collapse newlines so the message stays single-line in logs.
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// ---------------------------------------------------------------------------
// 上游响应体读取上限
// ---------------------------------------------------------------------------

const (
	// maxUpstreamResponseBytes 限制非流式上游响应体大小（防异常/恶意上游导致内存失控）。
	maxUpstreamResponseBytes = 64 << 20
	// maxUpstreamErrorBodyBytes 限制回传给客户端的上游错误体大小。
	maxUpstreamErrorBodyBytes = 1 << 20
)

// ReadUpstreamBody 读取上游非流式响应体（上限 64 MiB）；超限返回错误而非静默截断。
func ReadUpstreamBody(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxUpstreamResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxUpstreamResponseBytes {
		return nil, errors.New("upstream response too large")
	}
	return data, nil
}

// readUpstreamErrorBody 读取上游错误响应体（上限 1 MiB，供原样回传客户端）。
func readUpstreamErrorBody(r io.Reader) []byte {
	data, _ := io.ReadAll(io.LimitReader(r, maxUpstreamErrorBodyBytes))
	return data
}

// ---------------------------------------------------------------------------
// SSRF 漏洞防护
// ---------------------------------------------------------------------------

// sharedClients 按超时缓存 SSRF 安全客户端：复用底层 Transport 连接池，
// 避免每个请求都新建 Transport（重复 TCP/TLS 握手、空闲连接无法回收）。
var sharedClients sync.Map // time.Duration -> *http.Client

// httpClientForTimeout 返回指定超时的 SSRF 安全客户端（进程内按超时共享）。
func httpClientForTimeout(timeout time.Duration) *http.Client {
	if c, ok := sharedClients.Load(timeout); ok {
		return c.(*http.Client)
	}
	c := newSSRFSafeHTTPClient(timeout, true)
	actual, _ := sharedClients.LoadOrStore(timeout, c)
	return actual.(*http.Client)
}

// DoSSRFProtectedRequest sends an outbound HTTP request using the provider
// package's existing URL, DNS, dial, and redirect SSRF protections.
func DoSSRFProtectedRequest(req *http.Request, timeout time.Duration) (*http.Response, error) {
	if err := validateOutboundURL(req.Context(), req.URL.String()); err != nil {
		return nil, err
	}
	return httpClientForTimeout(timeout).Do(req)
}

var errSSRFBlocked = errors.New("target address not allowed")

// allowPrivateUpstreamsFlag 控制是否放行「私网 / 回环」上游地址。
// 零值为 false，即默认严格模式：拒绝一切私网 / 回环目标。
var allowPrivateUpstreamsFlag atomic.Bool

// SetAllowPrivateUpstreams 设置是否放行私网 / 回环上游地址（默认 false = 严格模式）。
//
// 置为 true 后，loopback（127.0.0.0/8、::1）、RFC1918（10/8、172.16/12、192.168/16）、
// CGNAT（100.64.0.0/10）、198.18.0.0/15、ULA（fc00::/7）、2001:db8::/32 会被放行，
// 用于本地 httptest mock 上游，或指向内网自建模型服务（Ollama / vLLM 等）的部署。
//
// 注意：放行私网不会放宽永久拒绝集——云元数据地址、link-local、未指定、多播、广播、
// 0.0.0.0/8、192.0.0.0/24、240.0.0.0/4 与 TEST-NET 网段在任何配置下都会被拒绝。
//
// 并发安全；进程级全局开关，请勿在每请求路径上频繁切换。
func SetAllowPrivateUpstreams(allow bool) {
	allowPrivateUpstreamsFlag.Store(allow)
}

// allowPrivateUpstreams 返回当前是否放行私网 / 回环上游地址。
func allowPrivateUpstreams() bool {
	return allowPrivateUpstreamsFlag.Load()
}

// ipv4/ipv6 元数据 IP 地址
var (
	ipv4MetadataAddrs = map[string]struct{}{
		"169.254.169.254": {}, // AWS / Azure / GCP / Alibaba / Tencent Cloud
		"100.100.100.200": {}, // Alibaba Cloud (legacy)
	}
	ipv6MetadataAddrs = map[string]struct{}{
		"fd00:ec2::254": {}, // AWS IMDSv6
	}
)

// 永久拒绝的保留 IPv4 网段（与上面的元数据地址表配合，见 isPermanentlyDisallowedIP）。
var permanentlyDisallowedV4Nets = mustCIDRs(
	"0.0.0.0/8",       // “本网络”
	"192.0.0.0/24",    // IETF 协议分配
	"192.0.2.0/24",    // TEST-NET-1
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"240.0.0.0/4",     // 保留网段（含广播 255.255.255.255）
)

// mustCIDRs 解析静态 CIDR 表；解析失败属于编程错误，在初始化期直接暴露。
func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, ipNet, err := net.ParseCIDR(c)
		if err != nil {
			panic(fmt.Sprintf("invalid static CIDR %q: %v", c, err))
		}
		out = append(out, ipNet)
	}
	return out
}

// isDisallowedIP 如果返回 true 则代表该 IP 地址不能作为目的地址。
//
// 拒绝策略分两档：
//
//	A. 永久拒绝（与 allowPrivateUpstreams 配置无关，见 isPermanentlyDisallowedIP）：
//	   1. IPv4/IPv6 元数据地址（169.254.169.254 / 100.100.100.200 / fd00:ec2::254）
//	   2. link-local 单播（169.254.0.0/16、fe80::/10）
//	   3. 未指定地址（0.0.0.0、::）
//	   4. link-local 多播、接口本地多播、多播地址
//	   5. 广播地址（255.255.255.255）
//	   6. 0.0.0.0/8
//	   7. 192.0.0.0/24
//	   8. 240.0.0.0/4
//	   9. TEST-NET（192.0.2.0/24、198.51.100.0/24、203.0.113.0/24）
//
//	B. 仅严格模式拒绝（allowPrivateUpstreams()==false 时命中，见 isStrictOnlyDisallowedIP）：
//	   1. 回环地址（127.0.0.0/8、::1）
//	   2. RFC1918 私网（10/8、172.16/12、192.168/16）
//	   3. CGNAT（100.64.0.0/10）
//	   4. 198.18.0.0/15
//	   5. ULA（fc00::/7）
//	   6. 2001:db8::/32
//
// 当 SetAllowPrivateUpstreams(true) 时，B 档地址会被放行。
func isDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if isPermanentlyDisallowedIP(ip) {
		return true
	}
	return !allowPrivateUpstreams() && isStrictOnlyDisallowedIP(ip)
}

// isPermanentlyDisallowedIP 报告该地址是否在任何配置下都不可作为出站目标。
// 这些地址或是云元数据端点，或是协议保留网段，放行它们不存在合法用途。
func isPermanentlyDisallowedIP(ip net.IP) bool {
	if hasLocalScopeOrMulticast(ip) {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return isPermanentlyDisallowedV4(v4)
	}
	if _, ok := ipv6MetadataAddrs[ip.String()]; ok {
		return true
	}
	// IPv4-compatible IPv6（::a.b.c.d，96 位零前缀，如 [::127.0.0.1]）：To4() 不识别
	// 这种废弃写法，需提取内嵌 IPv4 后按同一套规则判定，避免绕过。
	if v4 := ipv4CompatibleTo4(ip); v4 != nil {
		return isPermanentlyDisallowedIP(v4)
	}
	return false
}

// hasLocalScopeOrMulticast 报告地址是否属于 link-local（单播/多播）、接口本地多播、
// 多播或未指定地址——这些地址在任何配置下都不得作为出站目标。
func hasLocalScopeOrMulticast(ip net.IP) bool {
	return ip.IsLinkLocalUnicast() || // 169.254.0.0/16、fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() // 0.0.0.0、::
}

// isPermanentlyDisallowedV4 判定 IPv4 是否属于永久拒绝集：广播地址、保留网段或元数据地址。
func isPermanentlyDisallowedV4(ip net.IP) bool {
	if ip.Equal(net.IPv4bcast) { // 255.255.255.255（240/4 亦覆盖，此处保留显式判断）
		return true
	}
	for _, ipNet := range permanentlyDisallowedV4Nets {
		if ipNet.Contains(ip) {
			return true
		}
	}
	_, isMetadata := ipv4MetadataAddrs[ip.String()]
	return isMetadata
}

// ipv4CompatibleTo4 提取 IPv4-compatible IPv6 地址（::a.b.c.d）内嵌的 IPv4 地址。
// ::（unspecified）与 ::1（IPv6 loopback）不算嵌入式地址，返回 nil 交给通用规则处理。
func ipv4CompatibleTo4(ip net.IP) net.IP {
	if len(ip) != net.IPv6len || ip.To4() != nil {
		return nil
	}
	for i := 0; i < 12; i++ {
		if ip[i] != 0 {
			return nil
		}
	}
	v4 := net.IPv4(ip[12], ip[13], ip[14], ip[15])
	if v4.Equal(net.IPv4zero) || v4.Equal(net.IPv4(0, 0, 0, 1)) {
		return nil
	}
	return v4
}

// isStrictOnlyDisallowedIP 报告该地址是否属于「私网 / 回环」类，仅在严格模式下被拒绝。
// 当 SetAllowPrivateUpstreams(true) 时这类地址会被放行（用于本地 mock 或内网自建模型服务）。
func isStrictOnlyDisallowedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() { // 127.0.0.0/8、::1；RFC1918；ULA fc00::/7
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10（CGNAT）
		if v4[0] == 100 && (v4[1]&0xc0) == 64 {
			return true
		}
		// 198.18.0.0/15
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return true
		}
		return false
	}
	// IPv4-compatible IPv6（如 [::127.0.0.1]、[::10.0.0.1]）：按内嵌 IPv4 判定。
	if v4 := ipv4CompatibleTo4(ip); v4 != nil {
		return isStrictOnlyDisallowedIP(v4)
	}
	if len(ip) == net.IPv6len {
		// 2001:db8::/32
		if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
			return true
		}
	}
	return false
}

// validateOutBoundURL 解析 URL 地址，保证其为合法目标地址
func validateOutboundURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return richErrorf(errSSRFBlocked, "failed to parse target url: %v", err)
	}

	// URL scheme 必须为 HTTP 或者 HTTPS
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return richErrorf(errSSRFBlocked, "url scheme not allowed: %s", u.Scheme)
	}

	// Host 不能为空
	host := u.Hostname()
	if host == "" {
		return richErrorf(errSSRFBlocked, "empty host")
	}

	// 如果 Host 是 IP 字面量则用 isDisallowedIP 检查该 IP 是否可以访问
	if ip := net.ParseIP(host); ip != nil {
		if isDisallowedIP(ip) {
			return richErrorf(errSSRFBlocked, "target address not allowed: %s", ip.String())
		}
		return nil
	}

	// 如果不是 IP 字面量则解析出 IP 然后再用 isDisallowedIP 检查该 IP 是否可以访问
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		// 忽略解析错误 后续 Dial 可以给出更详细错误信息
		return nil
	}
	if len(addrs) == 0 {
		return richErrorf(errSSRFBlocked, "no address found for host %s", host)
	}
	for _, a := range addrs {
		if isDisallowedIP(a.IP) {
			return richErrorf(errSSRFBlocked, "host %s resolves to disallowed address: %s", host, a.IP.String())
		}
	}
	return nil
}

func ssrfSafeDialContext(
	base *net.Dialer,
	enableSSRF bool,
) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !enableSSRF {
			return base.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		if ip := net.ParseIP(host); ip != nil {
			if isDisallowedIP(ip) {
				return nil, richErrorf(errSSRFBlocked, "target address not allowed: %s", ip.String())
			}
			return base.DialContext(ctx, network, addr)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, a := range ips {
			if isDisallowedIP(a.IP) {
				return nil, richErrorf(
					errSSRFBlocked,
					"host %s resolves to disallowed address: %s",
					host,
					a.IP.String(),
				)
			}
			conn, err := base.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, richErrorf(errSSRFBlocked, "cannot resolve host %s", host)
	}
}

func newSSRFSafeHTTPClient(timeout time.Duration, enableSSRF bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// 不使用环境变量代理（HTTP_PROXY/HTTPS_PROXY）：
		// 若走代理，拨号目标是代理地址，拨号层的 SSRF 校验将退化为「只校验代理」，
		// 与「SSRF 全链硬化」冲突；需要代理出口的部署请使用透明代理或在路线图中跟踪。
		Proxy:       nil,
		DialContext: ssrfSafeDialContext(dialer, enableSSRF),
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		// 检查重定向后的目标 URL 是否合法；跨主机跳转时剥离自定义鉴权头
		// （Go 只自动剥离 Authorization/Cookie 等标准头，x-api-key 类需显式处理）。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !enableSSRF {
				return nil
			}
			if err := validateOutboundURL(req.Context(), req.URL.String()); err != nil {
				return err
			}
			if len(via) > 0 && !sameHost(via[0].URL, req.URL) {
				for _, h := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
					req.Header.Del(h)
				}
			}
			return nil
		},
	}
}

// sameHost 判断两个 URL 的 host（含端口）是否一致。
func sameHost(a, b *url.URL) bool {
	return strings.EqualFold(a.Host, b.Host)
}
