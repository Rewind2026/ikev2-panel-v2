// IPv4 公网地址探测(v2-84 新增)。
//
// 设计动机:
//   - DDNS 之前只同步 AAAA(v2-82),IPv4-only 部署完全没 DDNS
//   - 需要一个简单可靠的"获取本机出口 IPv4"的方法,供 DDNS 用
//
// 实现选择:
//   - 用 net.Dial("udp", target+":80") 拿 LocalAddr()
//   - target 默认 "8.8.8.8",可由调用方覆盖(中国大陆用户改 223.5.5.5)
//   - 3 秒 timeout(防御性编程:UDP dial 在黑洞网络会阻塞到 OS TCP 重传超时)
//
// 安全过滤(isGlobalV4):
//   - 0.0.0.0 / 255.255.255.255:无效
//   - 127.0.0.0/8:loopback
//   - 169.254.0.0/16:link-local(IPv6 已有 fe80::/10 过滤,对称)
//   - 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16:RFC1918 私网
//   - 100.64.0.0/10:CGNAT
//   - 224.0.0.0/4:multicast
//   - 命中过滤 → return error,绝不返回给 DDNS 上层
//
// iface 校验:
//   - 非空时,先 net.InterfaceByName(iface) 拿该接口地址列表
//   - dial 出的 IP 必须 ∈ 该列表,否则 return error
//   - 防止默认路由选了别的接口(比如 docker0 / br-xxx)拿到内网 IP
//
// 为什么不读 /proc/net/fib_trie:
//   - bridge 网络下 /proc 是宿主视角,容器拨号可能命中容器默认路由
//   - 格式非稳定 API
//   - host 网络(当前默认)下跟 net.Dial 行为一致
package swanctl

import (
	"fmt"
	"net"
	"time"
)

// DefaultProbeTarget IPv4 探测默认目标。
//
// 选 8.8.8.8 的原因:
//   - 全球可达(Google DNS),大多数 VPS 都能 dial 通
//   - 用 UDP "连接"不实际发包,不会泄露信息给对方(内核只选路由)
//   - 不需要响应(本地只拿 LocalAddr)
//
// 中国大陆部署可能因网络环境不稳,可通过 env IKEV2_DDNS_PROBE_TARGET
// 覆盖为 223.5.5.5(阿里 DNS)。
const DefaultProbeTarget = "8.8.8.8"

// probeDialTimeout UDP dial 的总超时。
//
// 短到足够防御黑洞网络(UDP 无 SYN,黑洞也不会 RST,可能阻塞到 OS 重传),
// 长到足够让正常网络完成。
const probeDialTimeout = 3 * time.Second

// isGlobalV4 判断 IPv4 是否为"适合写进 DNS 的公网地址"。
//
// 返回 false 的情况:
//   - nil
//   - 非 IPv4(IPv6 不在这里处理,留给 DetectGlobalV6)
//   - 命中过滤列表任一段
func isGlobalV4(ip net.IP) bool {
	if ip == nil {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}

	// 0.0.0.0 和 255.255.255.255
	if v4[0] == 0 || (v4[0] == 255 && v4[1] == 255 && v4[2] == 255 && v4[3] == 255) {
		return false
	}

	// 127.0.0.0/8 (loopback)
	if v4[0] == 127 {
		return false
	}

	// 169.254.0.0/16 (link-local)
	if v4[0] == 169 && v4[1] == 254 {
		return false
	}

	// 10.0.0.0/8 (RFC1918)
	if v4[0] == 10 {
		return false
	}

	// 172.16.0.0/12 (RFC1918)
	if v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31 {
		return false
	}

	// 192.168.0.0/16 (RFC1918)
	if v4[0] == 192 && v4[1] == 168 {
		return false
	}

	// 100.64.0.0/10 (CGNAT)
	if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}

	// 224.0.0.0/4 (multicast): 224-239 开头
	if v4[0] >= 224 && v4[0] <= 239 {
		return false
	}

	return true
}

// DetectGlobalV4 探测本机对外出口的 IPv4 地址。
//
// 参数:
//   - iface: 非空时,dial 出的 IP 必须属于该接口;为空时不限制
//   - target: 探测目标(默认用 DefaultProbeTarget);格式可以是 "1.2.3.4" 或 "host:port"
//
// 返回:
//   - string: 公网 IPv4 地址(点分十进制)
//   - error: 探测失败 / 命中过滤 / iface 不匹配
//
// 注入测试:dialer 字段,可由 ipv4watch_test.go 替换。
func DetectGlobalV4(iface, target string) (string, error) {
	if target == "" {
		target = DefaultProbeTarget
	}

	// 补上端口(如果没有)
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "80")
	}

	// dial UDP,3 秒超时
	d := net.Dialer{Timeout: probeDialTimeout}
	conn, err := d.Dial("udp", target)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", target, err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().String()
	// localAddr 格式 "1.2.3.4:port" 或 "[::1]:port"
	host, _, err := net.SplitHostPort(localAddr)
	if err != nil {
		return "", fmt.Errorf("parse local addr %q: %w", localAddr, err)
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("parse local IP %q: nil", host)
	}

	// 安全过滤:loopback / RFC1918 / link-local / multicast / 无效
	if !isGlobalV4(ip) {
		return "", fmt.Errorf("unsafe IPv4 %s (loopback/private/link-local/multicast)", host)
	}

	// iface 校验:如果用户指定了接口,IP 必须属于该接口
	if iface != "" {
		ifaceObj, err := net.InterfaceByName(iface)
		if err != nil {
			return "", fmt.Errorf("lookup iface %s: %w", iface, err)
		}
		addrs, err := ifaceObj.Addrs()
		if err != nil {
			return "", fmt.Errorf("list iface %s addrs: %w", iface, err)
		}
		found := false
		for _, a := range addrs {
			ifnIP, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ifnIP.IP.Equal(ip) {
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("dial-out IP %s not on iface %s (check IKEV2_OUT_IF or use iface=\"\")", host, iface)
		}
	}

	return host, nil
}
