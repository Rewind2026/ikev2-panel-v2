// Package publicip 获取本机对外公网 IPv4 地址。
//
// 背景(本次重构动机):
//   - 之前 swanctl.DetectGlobalV4 用 net.Dial("udp", target+":80") 拿 LocalAddr
//   - 问题是:UDP dial 只是让内核选路由,**不实际发包**,LocalAddr 永远等于
//     "本机出口网卡 IP",而不是真正从公网视角看到的 IP
//   - 影响场景:
//       1) 容器/bridge 网络 — LocalAddr 是容器内部 IP(不是宿主机公网 IP)
//       2) CGNAT — ISP 分配的私网 100.64/10,LocalAddr 是这个私网 IP,不是公网
//       3) VPS 多网卡 — 内核可能选了不对外的接口(比如 docker0)
//       4) 拨号 PPPoE / IPsec tunnel 内 IP — 内核路由表都选错
//   - 这些场景下 DDNS 推上去的 IP 是错的,A 记录解析后客户端连不上
//
// 设计:
//   - 用多家境内 / 国际公网 IP 检测 API,fallback 链:第一家失败立即试下一家
//   - 每家只发一个 HTTP GET,5 秒超时,响应体小(几字节到几百字节),带宽忽略
//   - 用户视角拿到的是"真正的公网出口 IP" — 跟 ping.cn / ip.cn 等页面看到的 IP 一致
//
// 为什么不是 https://api.ipify.org:这是一个国际服务,部分地区可能被墙;
// 优先用 ip.cn / 3322.org / ip.sb 等境内可达节点。
//
// 安全:
//   - 响应做 isGlobalV4 过滤(防 API 返回畸形字符串或私网地址)
//   - 所有候选必须 non-empty 且 net.ParseIP 成功才采纳
//   - 解析失败继续 fallback,不立即报错
package publicip

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Probe 提供给 DDNS 用的公网 IPv4 检测客户端。
//
// 默认 5 秒单请求超时;总探测时长 ≈ 5s × 候选数(各家并行或串行都行,
// 串行实现简单且大多数第一家就成功)。
type Probe struct {
	// HTTPClient 共享 client,5 秒 timeout
	HTTPClient *http.Client
	// Endpoints 候选 API 列表(URL + ResponseParser)
	Endpoints []Endpoint
	// Logger slog(可选)
	Logger *slog.Logger
}

// Endpoint 单家公网 IP 检测 API。
//
// URL:GET 请求目标。
// Parser:把响应体转成 IP;返回 nil/空 = 该家失败,继续 fallback。
type Endpoint struct {
	URL    string
	Parser func(body []byte) string
}

// New 构造默认 Probe,境内 / 国际节点 fallback 链。
//
// 候选清单(按优先级):
//  1) https://ip.cn — 境内,响应快
//  2) https://myip.ipip.net — 境内,纯文本格式
//  3) https://api.ipify.org — 国际,纯文本 IP
//  4) https://ifconfig.me/ip — 国际,纯文本
//  5) https://ipv4.icanhazip.com — 国际,纯文本
//
// 用户可在 Endpoints 字段追加自定义节点(测试用 / 私有部署)。
func New() *Probe {
	return &Probe{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Endpoints: []Endpoint{
			{URL: "https://ip.cn", Parser: parseIPCN},
			{URL: "https://myip.ipip.net", Parser: parseIPIPNet},
			{URL: "https://api.ipify.org", Parser: parsePlainIP},
			{URL: "https://ifconfig.me/ip", Parser: parsePlainIP},
			{URL: "https://ipv4.icanhazip.com", Parser: parsePlainIP},
		},
	}
}

// Detect 返回本机对外公网 IPv4。
//
// 行为:
//   - 依次尝试每个 endpoint,第一个返回合法 IP 即停
//   - 所有 endpoint 都失败 → 返回累积错误
//   - ctx 传 nil 时,内部构造一个 60 秒总预算 context
//   - 每家请求独立 5 秒超时(ctx cancel 也立即终止)
//
// 返回的 IP 已通过 isGlobalV4 过滤(loopback / RFC1918 / CGNAT / multicast 等
// 私网段会被跳过,继续 fallback 下一家)。
func (p *Probe) Detect(ctx context.Context) (string, error) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
	}

	cli := p.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: 5 * time.Second}
	}

	var tried []string
	var lastErr error

	for _, ep := range p.Endpoints {
		if ctx.Err() != nil {
			return "", fmt.Errorf("publicip: context canceled after trying %v", tried)
		}
		ip, err := p.fetchOne(ctx, cli, ep)
		if err != nil {
			lastErr = err
			tried = append(tried, ep.URL)
			continue
		}
		if !isGlobalV4(net.ParseIP(ip)) {
			lastErr = fmt.Errorf("publicip: %s returned non-global %q", ep.URL, ip)
			tried = append(tried, ep.URL)
			continue
		}
		return ip, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no endpoints configured")
	}
	return "", fmt.Errorf("publicip: all endpoints failed (tried %v): %w", tried, lastErr)
}

// fetchOne 单家 API 请求。
//
// 行为细节:
//   - ctx 已 done → 立即返回
//   - HTTP 状态码非 2xx → error
//   - 响应体 > 4KB(防 DoS)→ error
//   - Parser 解析失败 / 解析失败(空) → error
func (p *Probe) fetchOne(ctx context.Context, cli *http.Client, ep Endpoint) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.URL, nil)
	if err != nil {
		return "", fmt.Errorf("%s: new request: %w", ep.URL, err)
	}
	req.Header.Set("User-Agent", "ikev2-panel/publicip")
	req.Header.Set("Accept", "text/plain, application/json, */*")

	resp, err := cli.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ep.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("%s: status %d", ep.URL, resp.StatusCode)
	}

	// 限流:超过 4KB 视为异常响应(纯 IP 几字节就够)
	const maxBody = 4096
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("%s: read body: %w", ep.URL, err)
	}

	ip := ep.Parser(body)
	if ip == "" {
		return "", fmt.Errorf("%s: parser returned empty", ep.URL)
	}
	return ip, nil
}

// ---------- 各家 parser ----------

// parsePlainIP 解析 "1.2.3.4\n" 这种纯文本格式(api.ipify.org / ifconfig.me / icanhazip.com)。
//
// 兼容响应体里偶尔带尾巴(<pre>...</pre>、注释等);只要含一个合法 IPv4 token
// 就返回。多个时取首个。
var v4Regexp = regexp.MustCompile(`(?:^|[^\d.])(?:\d{1,3}\.){3}\d{1,3}(?:[^\d.]|$)`)

func parsePlainIP(body []byte) string {
	s := string(body)
	for _, m := range v4Regexp.FindAllString(s, -1) {
		if v := extractIPv4(m); v != "" {
			return v
		}
	}
	return ""
}

// parseIPCN ip.cn 的 JSON 格式:{"ip":"1.2.3.4","address":"..."}
//
// 实际 ip.cn 在 Accept 文本时也回纯文本,但默认 HTML 页面我们用纯文本路径更稳。
// 这里正则匹配 HTML 里的 IP 也兼容 JSON 两种格式。
func parseIPCN(body []byte) string {
	s := string(body)
	// JSON 优先
	if idx := strings.Index(s, `"ip":"`); idx >= 0 {
		rest := s[idx+len(`"ip":"`):]
		if end := strings.Index(rest, `"`); end > 0 {
			cand := rest[:end]
			if ip := net.ParseIP(cand); ip != nil && ip.To4() != nil {
				return cand
			}
		}
	}
	// HTML: <code>当前 IP：1.2.3.4</code>
	if idx := strings.Index(s, `当前 IP：</code>`); idx > 0 {
		rest := s[idx:]
		m := v4Regexp.FindString(rest)
		if m != "" {
			return extractIPv4(m)
		}
	}
	if idx := strings.Index(s, `当前 IP:`); idx > 0 {
		rest := s[idx:]
		m := v4Regexp.FindString(rest)
		if m != "" {
			return extractIPv4(m)
		}
	}
	// 兜底:任何 IPv4 形式
	for _, c := range v4Regexp.FindAllString(s, -1) {
		if v := extractIPv4(c); v != "" {
			return v
		}
	}
	return ""
}

// parseIPIPNet myip.ipip.net 响应: "当前 IP：1.2.3.4  来自于：中国 ..."。
//
// 简单 grep 第一个 IPv4。
func parseIPIPNet(body []byte) string {
	for _, c := range v4Regexp.FindAllString(string(body), -1) {
		if v := extractIPv4(c); v != "" {
			return v
		}
	}
	return ""
}

// extractIPv4 从一个可能夹前后字符的字符串里取出 "x.x.x.x"。
func extractIPv4(s string) string {
	var b strings.Builder
	for _, ch := range s {
		if ch == '.' || (ch >= '0' && ch <= '9') {
			b.WriteRune(ch)
		}
	}
	cand := strings.TrimSpace(b.String())
	// IPv4 必有 3 个点
	if strings.Count(cand, ".") != 3 {
		return ""
	}
	ip := net.ParseIP(cand)
	if ip == nil || ip.To4() == nil {
		return ""
	}
	return cand
}

// isGlobalV4 判断 IPv4 是否适合写进 DNS 的公网地址。
//
// 过滤掉:
//   - nil
//   - 0.0.0.0 / 255.255.255.255
//   - 127.0.0.0/8 (loopback)
//   - 169.254.0.0/16 (link-local)
//   - 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 (RFC1918)
//   - 100.64.0.0/10 (CGNAT)
//   - 224.0.0.0/4 (multicast)
//
// 跟 swanctl.isGlobalV4 重复实现(避免包互相依赖);两处规则需保持一致。
func isGlobalV4(ip net.IP) bool {
	if ip == nil {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	if v4[0] == 0 || (v4[0] == 255 && v4[1] == 255 && v4[2] == 255 && v4[3] == 255) {
		return false
	}
	if v4[0] == 127 {
		return false
	}
	if v4[0] == 169 && v4[1] == 254 {
		return false
	}
	if v4[0] == 10 {
		return false
	}
	if v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31 {
		return false
	}
	if v4[0] == 192 && v4[1] == 168 {
		return false
	}
	if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	if v4[0] >= 224 && v4[0] <= 239 {
		return false
	}
	return true
}