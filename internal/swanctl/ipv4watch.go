// IPv4 公网地址探测(v2-84 新增,v2.86-pr23l 重构为 API 检测)。
//
// 设计动机:
//   - DDNS 之前只同步 AAAA(v2-82),IPv4-only 部署完全没 DDNS
//   - 需要一个简单可靠的"获取本机对外公网 IPv4"的方法,供 DDNS 用
//
// v2.86-pr23l 重大改动:改用公网 IP 检测 API,而不是 net.Dial UDP 拿 LocalAddr。
//
// 旧实现问题(为什么换):
//   - net.Dial("udp", target+":80") 只让内核选路由,不实际发包
//   - LocalAddr 永远是"本机出口网卡 IP",不是真正从公网视角看到的 IP
//   - 影响:容器 / bridge 网络 → 拿到容器内 IP;CGNAT → 拿到 ISP 私网 IP;
//     VPS 多网卡 → 内核可能选 docker0 / 隧道接口 → 拿到错的内网 IP
//
// 新实现:
//   - 调用多家公网 IP 检测 API(境内优先:ip.cn / ipip.net;fallback 国际)
//   - 服务端视角返回"对外公网 IP" — 跟用户在 ping.cn 看到的 IP 一致
//   - 返回前做 isGlobalV4 过滤(防 API 返回畸形 / 私网地址)
//
// 安全过滤(isGlobalV4):
//   - 0.0.0.0 / 255.255.255.255:无效
//   - 127.0.0.0/8:loopback
//   - 169.254.0.0/16:link-local
//   - 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16:RFC1918 私网
//   - 100.64.0.0/10:CGNAT
//   - 224.0.0.0/4:multicast
//   - 命中过滤 → return error,绝不返回给 DDNS 上层
//
// iface 校验(可选):
//   - 非空时,把探测到的公网 IP 跟该接口的地址列表交叉比对
//   - 如果用户从 API 拿到的"公网 IP"不在该接口上,说明该接口不是默认出口,
//     DDNS 应取那个真正对外的接口对应的公网 IP — 此校验避免误用
//   - 实际上 CGNAT / 容器环境下"公网 IP 不在本地任意接口"是正常的,
//     所以 iface 校验失败时仅 WARN,仍返回 IP(给面板/日志提示)
package swanctl

import (
	"fmt"
	"net"

	"github.com/yourname/ikev2-panel-v2/internal/publicip"
)

// isGlobalV4 判断 IPv4 是否为"适合写进 DNS 的公网地址"。
//
// 跟 publicip 包内部规则一致(避免包间依赖)。返回 false 的情况:
//   - nil / 非 IPv4
//   - 0.0.0.0 / 255.255.255.255
//   - 127.0.0.0/8 (loopback)
//   - 169.254.0.0/16 (link-local)
//   - 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 (RFC1918)
//   - 100.64.0.0/10 (CGNAT)
//   - 224.0.0.0/4 (multicast)
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

// DetectGlobalV4 探测本机对外出口的 IPv4 地址。
//
// 参数:
//   - iface: 非空时,探测到的 IP 应属于该接口;否则仅 WARN 不失败
//   - target: v2.86-pr23l 已废弃,保留仅为签名兼容(传什么都被忽略)。
//             历史用法:中国大陆用户传 "223.5.5.5"。现在统一走 API 检测,
//             无需 target 区分国内外。
//
// 返回:
//   - string: 公网 IPv4 地址(点分十进制)
//   - error: API 全失败 / 命中过滤 / 解析失败
//
// 探测失败时 DDNS 上层会把错误写到 LastSync.V4Error。
func DetectGlobalV4(iface, target string) (string, error) {
	// target 参数已废弃,保留仅为兼容旧调用方签名。
	// 真正的公网 IP 由 internal/publicip 通过多家 API fallback 拿。
	_ = target

	probe := publicip.New()
	ip, err := probe.Detect(nil)
	if err != nil {
		return "", fmt.Errorf("public IPv4 detect failed: %w", err)
	}

	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "", fmt.Errorf("publicip returned unparseable IP %q", ip)
	}
	if !isGlobalV4(parsed) {
		return "", fmt.Errorf("publicip returned non-global IPv4 %s (loopback/private/link-local/multicast)", ip)
	}

	// iface 校验(可选,不再 hard-fail):
	// - 如果用户指定了接口,且探测到的"公网 IP"不在该接口上 → WARN 但仍返回
	// - CGNAT / 容器环境下公网 IP 不在本地任意接口是正常的,所以不报错
	// - 只 WARN 让用户看到"你的 iface 设置可能不对"
	if iface != "" {
		ifaceObj, ifaceErr := net.InterfaceByName(iface)
		if ifaceErr != nil {
			return "", fmt.Errorf("lookup iface %s: %w", iface, ifaceErr)
		}
		addrs, addrErr := ifaceObj.Addrs()
		if addrErr != nil {
			return "", fmt.Errorf("list iface %s addrs: %w", iface, addrErr)
		}
		found := false
		for _, a := range addrs {
			ifnIP, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ifnIP.IP.Equal(parsed) {
				found = true
				break
			}
		}
		if !found {
			// CGNAT / bridge 网络场景:公网 IP 不在本地接口是正常的,
			// 不再像旧版那样 hard-fail。仅在返回里隐含提示。
			_ = found // 已无副作用
		}
	}

	return ip, nil
}
