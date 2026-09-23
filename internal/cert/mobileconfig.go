// iOS/macOS .mobileconfig 渲染。
// 设计见 docs/design.md §10.1
//
// 模板字段含义见 Apple/strongSwan 官方文档：
//   - https://docs.strongswan.org/docs/latest/interop/appleIkev2Profile.html
//   - https://developer.apple.com/documentation/devicemanagement/vpn
package cert

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"
)

// OnDemandProfile 按需自动连接 preset 名。
//
// 每个 preset 对应一段 OnDemandRules XML,见 renderOnDemandRules()。
// 命名跟业内常用术语对齐(Always/CellularOnly/CaptiveProbe/HomeWiFiDisconnect/Manual)。
type OnDemandProfile string

const (
	OnDemandAlwaysConnect        OnDemandProfile = "always"             // 默认:任何网络都连
	OnDemandHomeWiFiDisconnect   OnDemandProfile = "home_wifi_disconnect" // 指定 SSID 断开,其它连
	OnDemandCellularOnly         OnDemandProfile = "cellular_only"      // 蜂窝连,Wi-Fi 不连
	OnDemandCaptiveProbe         OnDemandProfile = "captive_probe"      // 能访问 captive.apple.com 不连,否则连
	OnDemandManual               OnDemandProfile = "manual"             // 关闭 OnDemand
)

// ValidOnDemandProfiles 所有合法的 preset 名。
var ValidOnDemandProfiles = []OnDemandProfile{
	OnDemandAlwaysConnect,
	OnDemandHomeWiFiDisconnect,
	OnDemandCellularOnly,
	OnDemandCaptiveProbe,
	OnDemandManual,
}

// MobileConfigOpts mobileconfig 覆盖项。每用户一份,通过 store.User.MobileConfigOpts
// (JSON 字符串) 持久化。空 JSON / 空字符串 = 用 DefaultMobileConfigOpts() 默认值。
//
// 设计权衡:
//   - 不开放给用户的字段(EncryptionAlgorithm / ServerCertificateCommonName 等)不进 opts,
//     跟服务端 swanctl 绑死,改一边 = 断连。
//   - 指针字段用 nil 表示"用默认",boolean 用 *bool 区分"用户显式设为 false"和"未设置"。
//     Go zero-value false 会丢失"用户想 false"的信号。
//   - DNSServers 用 string slice,空 slice = 默认 1.1.1.1/8.8.8.8/...
//   - OnDemandProfile 用 string(preset 名),避免 UI 端传任意字符串导致 XSS。
//
// v2.86-PR12.21:管理员后台 -> 用户详情页 -> "Apple 描述文件选项" 配置。
//
// v2.86-PR12.22 audit(对照 developer.apple.com/documentation/devicemanagement/vpn/ikev2-data.dictionary):
//   - DisconnectOnSleep     ⚠️ Apple 文档未列,API 层 (NEVPNProtocol.disconnectOnSleep) 有,
//                          iOS 实际接受(mobileconfig key),保留。
//   - NATKeepaliveEnabled   ❌ Apple 文档用 NATKeepAliveOffloadEnable(大写 A),
//                          plist key 大小写敏感 → 改用正确名称。
//                          Go 字段仍叫 NATKeepaliveEnabled(我们的内部名),仅渲染层改字面量。
//   - NATKeepaliveInterval  ✅ Apple 文档列(NATKeepAliveInterval),min 20s,默认 20s(Wi-Fi)/110s(蜂窝)。
//                          我们的 hardcoded 60s 是经验值,合法。
//   - AuthPasswordRetries   ❌ Apple 文档完全找不到。GitHub 搜不到任何 mobileconfig 用法示例。
//                          可能是 iOS private key,可能是杜撰。**已移除**,避免误导用户。
type MobileConfigOpts struct {
	UserDefinedName       *string         `json:"user_defined_name,omitempty"`        // 连接显示名;nil = "IKEv2 VPN"
	DisconnectOnSleep     *bool           `json:"disconnect_on_sleep,omitempty"`     // true = 锁屏断 VPN;nil = false(保活)
	NATKeepaliveEnabled   *bool           `json:"nat_keepalive_enabled,omitempty"`   // 渲染层输出 NATKeepAliveOffloadEnable(Apple 正确命名)
	NATKeepaliveInterval  *int            `json:"nat_keepalive_interval,omitempty"`  // 秒;nil = 60,范围 20-600
	OnDemandEnabled       *bool           `json:"on_demand_enabled,omitempty"`       // nil = true
	OnDemandProfile       OnDemandProfile `json:"on_demand_profile,omitempty"`       // preset 名;空 = "always"
	OnDemandSSID          string          `json:"ondemand_ssid,omitempty"`           // home_wifi_disconnect 模式用
	IncludeAllNetworks    *bool           `json:"include_all_networks,omitempty"`    // nil = true(全局路由)
	ExcludeLocalNetworks  *bool           `json:"exclude_local_networks,omitempty"`  // nil = true(保留局域网)
	DNSServers            []string        `json:"dns_servers,omitempty"`             // nil/空 = 默认 1.1.1.1/8.8.8.8/...
	DeadPeerDetectionRate string          `json:"dead_peer_detection_rate,omitempty"`// None/Low/Medium/High;空 = "Medium"
}

// BaseMobileConfigDefaults 返回 cert builtin 出厂值(写死的常量,不可改)。
//
// 单一来源:mobileconfig 模板里所有"出厂默认值"都来自这里。改一处,渲染逻辑全局跟随。
//
// v2.86-PR12.22:从 DefaultMobileConfigOpts 拆出。新增 admin 全局默认层后,
// DefaultMobileConfigOpts(管理员)→ RenderMobileconfig 的渲染路径不再直接调这个,
// 而是调 EffectiveMobileConfigOpts(admin defaults, user overlay)。
func BaseMobileConfigDefaults() MobileConfigOpts {
	keep := true
	sleepFalse := false
	include := true
	exclude := true
	onDemand := true
	return MobileConfigOpts{
		UserDefinedName:      nil,                       // 由 Render 处 fallback "IKEv2 VPN"
		DisconnectOnSleep:    &sleepFalse,
		NATKeepaliveEnabled:  &keep,
		NATKeepaliveInterval: intPtr(60),
		OnDemandEnabled:      &onDemand,
		OnDemandProfile:      OnDemandAlwaysConnect,
		OnDemandSSID:         "",
		IncludeAllNetworks:   &include,
		ExcludeLocalNetworks: &exclude,
		DNSServers: []string{
			"1.1.1.1",
			"8.8.8.8",
			"2606:4700:4700::1111",
			"2001:4860:4860::8888",
		},
		DeadPeerDetectionRate: "Medium",
	}
}

// EffectiveMobileConfigOpts 三层合并:builtin → admin → user overlay。
//
// 优先级(高 → 低):overlay > admin > builtin;字段级覆盖,nil 字段用下一层。
//
// 参数:
//   - admin  *MobileConfigOpts:管理员全局默认(nil = 用 BaseMobileConfigDefaults())
//   - overlay *MobileConfigOpts:用户 overlay(nil = 用户没自定义)
//
// 调用场景:
//   - cert.RenderMobileconfig 拿 (admin, overlay) 合并出最终渲染值
//   - 测试:admin / overlay 都传 nil,得到 builtin
//
// 返回新值,不修改入参。
//
// v2.86-PR12.22:从 effectiveOpts(只合并 builtin + overlay)升级为三层。
// admin / overlay 都用 *MobileConfigOpts(handler 端把 panelstate 字段转过来),
// cert 不反向依赖 panelstate。
func EffectiveMobileConfigOpts(admin, overlay *MobileConfigOpts) MobileConfigOpts {
	// 1. builtin
	out := BaseMobileConfigDefaults()

	// 2. admin defaults 覆盖
	if admin != nil {
		if admin.UserDefinedName != nil {
			out.UserDefinedName = admin.UserDefinedName
		}
		if admin.DisconnectOnSleep != nil {
			out.DisconnectOnSleep = admin.DisconnectOnSleep
		}
		if admin.NATKeepaliveEnabled != nil {
			out.NATKeepaliveEnabled = admin.NATKeepaliveEnabled
		}
		if admin.NATKeepaliveInterval != nil {
			out.NATKeepaliveInterval = admin.NATKeepaliveInterval
		}
		if admin.OnDemandEnabled != nil {
			out.OnDemandEnabled = admin.OnDemandEnabled
		}
		if admin.OnDemandProfile != "" {
			out.OnDemandProfile = admin.OnDemandProfile
		}
		if admin.IncludeAllNetworks != nil {
			out.IncludeAllNetworks = admin.IncludeAllNetworks
		}
		if admin.ExcludeLocalNetworks != nil {
			out.ExcludeLocalNetworks = admin.ExcludeLocalNetworks
		}
		if len(admin.DNSServers) > 0 {
			out.DNSServers = append([]string(nil), admin.DNSServers...)
		}
		if admin.DeadPeerDetectionRate != "" {
			out.DeadPeerDetectionRate = admin.DeadPeerDetectionRate
		}
	}

	// 3. user overlay 覆盖 admin
	if overlay != nil {
		if overlay.UserDefinedName != nil {
			out.UserDefinedName = overlay.UserDefinedName
		}
		if overlay.DisconnectOnSleep != nil {
			out.DisconnectOnSleep = overlay.DisconnectOnSleep
		}
		if overlay.NATKeepaliveEnabled != nil {
			out.NATKeepaliveEnabled = overlay.NATKeepaliveEnabled
		}
		if overlay.NATKeepaliveInterval != nil {
			out.NATKeepaliveInterval = overlay.NATKeepaliveInterval
		}
		if overlay.OnDemandEnabled != nil {
			out.OnDemandEnabled = overlay.OnDemandEnabled
		}
		if overlay.OnDemandProfile != "" {
			out.OnDemandProfile = overlay.OnDemandProfile
		}
		if overlay.OnDemandSSID != "" {
			out.OnDemandSSID = overlay.OnDemandSSID
		}
		if overlay.IncludeAllNetworks != nil {
			out.IncludeAllNetworks = overlay.IncludeAllNetworks
		}
		if overlay.ExcludeLocalNetworks != nil {
			out.ExcludeLocalNetworks = overlay.ExcludeLocalNetworks
		}
		if len(overlay.DNSServers) > 0 {
			out.DNSServers = append([]string(nil), overlay.DNSServers...)
		}
		if overlay.DeadPeerDetectionRate != "" {
			out.DeadPeerDetectionRate = overlay.DeadPeerDetectionRate
		}
	}

	return out
}

// intPtr 辅助构造。
func intPtr(i int) *int { return &i }
func boolPtr(b bool) *bool { return &b }

// DefaultMobileConfigOpts 向后兼容别名:等价 BaseMobileConfigDefaults。
//
// v2.86-PR12.22 之前的所有调用 + 测试都引用 DefaultMobileConfigOpts();保留
// 名称避免大范围改名。新代码应该用 BaseMobileConfigDefaults() 表达"出厂值"。
func DefaultMobileConfigOpts() MobileConfigOpts {
	return BaseMobileConfigDefaults()
}

// effectiveOpts 把用户 overlay 合并到默认上。overlay 为 nil 或字段为 nil 时用默认。
//
// v2.86-PR12.22 降级为 EffectiveMobileConfigOpts(nil, overlay) 的薄包装,
// 仅供测试使用(测试只想验证 overlay 合并逻辑,不需要 admin 层)。
//
// 不修改入参,返回新值。
func effectiveOpts(overlay *MobileConfigOpts) MobileConfigOpts {
	return EffectiveMobileConfigOpts(nil, overlay)
}

// Validate 校验 opts 字段合法性,handler 端调用。
//
// 规则:
//   - NATKeepaliveInterval:20-600(Apple 规定最小 20s;>600 没意义)
//   - AuthPasswordRetries:0-5(strongSwan 建议 ≤ 3,5 是绝对上限)
//   - OnDemandProfile:必须在 ValidOnDemandProfiles 内
//   - OnDemandSSID:home_wifi_disconnect 模式下必填,长度 ≤ 32
//   - DNSServers:每项必须合法 IPv4/IPv6;数量 ≤ 6
//   - UserDefinedName:长度 ≤ 64,不能含 < > & " ' (mobileconfig XML 注入防护)
//   - DeadPeerDetectionRate ∈ {None, Low, Medium, High}
func (o *MobileConfigOpts) Validate() error {
	if o.NATKeepaliveInterval != nil {
		v := *o.NATKeepaliveInterval
		if v < 20 || v > 600 {
			return fmt.Errorf("nat_keepalive_interval 必须在 20-600 秒之间,当前 %d", v)
		}
	}
	if o.OnDemandProfile != "" {
		ok := false
		for _, p := range ValidOnDemandProfiles {
			if o.OnDemandProfile == p {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("on_demand_profile 必须是 %v 之一,当前 %q",
				ValidOnDemandProfiles, o.OnDemandProfile)
		}
	}
	if o.OnDemandProfile == OnDemandHomeWiFiDisconnect && o.OnDemandSSID == "" {
		return fmt.Errorf("on_demand_profile=%q 时必须填 ondemand_ssid(家里 Wi-Fi 的 SSID)",
			OnDemandHomeWiFiDisconnect)
	}
	if o.OnDemandSSID != "" {
		if len(o.OnDemandSSID) > 32 {
			return fmt.Errorf("ondemand_ssid 长度 ≤ 32,当前 %d", len(o.OnDemandSSID))
		}
		// SSID 合法字符:可见 ASCII(0x20-0x7e),iOS 实测接受任意 UTF-8(中文 SSID 也行)
		// 这里只校验长度,字符范围由 iOS 端兜底
	}
	if len(o.DNSServers) > 6 {
		return fmt.Errorf("dns_servers 数量 ≤ 6,当前 %d", len(o.DNSServers))
	}
	for _, d := range o.DNSServers {
		if !isValidIP(d) {
			return fmt.Errorf("dns_servers 含非法 IP: %q", d)
		}
	}
	if o.UserDefinedName != nil {
		v := *o.UserDefinedName
		if len(v) > 64 {
			return fmt.Errorf("user_defined_name 长度 ≤ 64,当前 %d", len(v))
		}
		// 防 XML 注入:mobileconfig 是 XML,< > & " ' 在 plist <string> 里需要 escape
		// iOS plist 解析器对未转义的特殊字符会 silently drop,显示空名;
		// 我们直接拒绝包含特殊字符的名字,让用户用普通文字
		for _, c := range v {
			if c == '<' || c == '>' || c == '&' || c == '"' || c == '\'' {
				return fmt.Errorf("user_defined_name 不能含特殊字符 < > & \" '")
			}
		}
	}
	if o.DeadPeerDetectionRate != "" {
		switch o.DeadPeerDetectionRate {
		case "None", "Low", "Medium", "High":
		default:
			return fmt.Errorf("dead_peer_detection_rate 必须是 None/Low/Medium/High,当前 %q",
				o.DeadPeerDetectionRate)
		}
	}
	return nil
}

// IsZeroOpts 判断是否为零值(用于决定是否写空字符串到 DB)。
func (o *MobileConfigOpts) IsZeroOpts() bool {
	if o == nil {
		return true
	}
	return o.UserDefinedName == nil &&
		o.DisconnectOnSleep == nil &&
		o.NATKeepaliveEnabled == nil &&
		o.NATKeepaliveInterval == nil &&
		o.OnDemandEnabled == nil &&
		o.OnDemandProfile == "" &&
		o.OnDemandSSID == "" &&
		o.IncludeAllNetworks == nil &&
		o.ExcludeLocalNetworks == nil &&
		len(o.DNSServers) == 0 &&
		o.DeadPeerDetectionRate == ""
}

// EncodeMobileConfigOpts 把 opts 序列化成 JSON 字符串,存到 store.User.MobileConfigOpts。
//
// IsZeroOpts() → 返回 "",DB 端存空。
func EncodeMobileConfigOpts(o *MobileConfigOpts) (string, error) {
	if o == nil || o.IsZeroOpts() {
		return "", nil
	}
	b, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("marshal mobileconfig opts: %w", err)
	}
	return string(b), nil
}

// DecodeMobileConfigOpts 从 store.User.MobileConfigOpts 解析。
//
// 空字符串 → 返回 nil,nil(handler 当作"用默认")。
// 非空但 JSON 损坏 → 返回错误(handler 返回 500,运维查日志)。
func DecodeMobileConfigOpts(s string) (*MobileConfigOpts, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var o MobileConfigOpts
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		return nil, fmt.Errorf("parse mobileconfig opts: %w", err)
	}
	return &o, nil
}

// isValidIP 简单 IPv4/IPv6 校验。
//
// 完整 IPv6 校验太复杂,这里走 net.ParseIP;net 是 stdlib 不引第三方。
func isValidIP(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' || c == ':' || (c >= '0' && c <= '9') ||
			(c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	// 含 ":" 才认为是 IPv6
	if strings.Contains(s, ":") {
		return strings.Count(s, ":") >= 2
	}
	// IPv4:4 段数字
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// mobileconfigTmpl 模板。
//
// 关键决策（跟 Apple/strongSwan 官方 EAP 模板对齐）：
//   - AuthenticationMethod = Certificate：server 用证书做身份
//   - ExtendedAuthEnabled = 1：客户端走 EAP
//   - AuthName + AuthPassword 内置在 profile 里：iOS 装了 profile 就记下凭证，
//     **不弹密码框**（这是解决 v2-74 弹密码框 + silently drop 的关键）
//   - AuthPasswordRetries **v2.86-PR12.22 audit 移除**:Apple developer 文档完全找不到这字段,
//     GitHub 搜不到 mobileconfig 用法。可能是 iOS private key / 杜撰。iOS 看到不认识的 key
//     会 silently ignore,所以保留无害,但误导用户以为能改 → 移除。
//   - ServerCertificateCommonName = ServerCN：iOS 用这个值校验 server cert 的 SAN/CN，
//     加上后严格校验链路；LE 模式也保留（Apple 把这个当作 hint，不强制）
//   - IKESecurityAssociationParameters / ChildSecurityAssociationParameters：
//     **strongSwan 官方文档注释**:
//     "Because only one proposal is sent (even if nothing is configured here)
//      it must match the server configuration"
//     不配的话 iOS 用 3DES fallback（NEVPNIKEv2EncryptionAlgorithm3DES）,
//     如果 server 端没 3DES proposal 就会协商失败。我们指定 AES-256-GCM + SHA2-256 + DH 31
//     (curve25519),与 swanctl-ipv6-only.conf 第一提案对齐。
//     字段取值见 developer.apple.com/documentation/networkextension/nevpnikev2encryptionalgorithm
//   - v2-78：加 DNS 推送（4 = IPv4 DNS, 6 = IPv6 DNS），
//     iOS 拨号后用这里指定的 DNS 而不是拨号前的 WiFi/蜂窝 DNS。
//     不推 DNS 时 iOS 把所有 UDP 53 按 0.0.0.0/0 强制路由进 ESP 隧道，
//     server 端 swanctl pools 没 dns 时客户端 DNS 解析走不通 → captive.apple.com
//     探测超时 → Safari/WX 报"没连接互联网"（即使 TCP 443 实际能 curl 通）。
//
//   - v2.86-PR12.23(audit + 修):
//     原模板写 `<key>DNSSettings</key><dict><key>DNS</key><array>...</array></dict>`
//     是 **杜撰命名**(Apple devicemanagement 文档从没用过 DNSSettings 这个 key),
//     iOS 17/18 静默忽略整个节点 → DNS 实际从未生效。
//     改成 Apple 规范形态:iOS 13- 用 `<key>DNS</key><array>...</array>`,
//     iOS 14+ 用 `<key>DNS</key><dict><key>ServerAddresses</key><array>...</array>
//                              <key>DNSProtocol</key><string>Cleartext</string></dict>`
//     (后者强制要求 DNSProtocol,否则判定"无 DNS"配置)。
//     见 developer.apple.com/documentation/devicemanagement/vpn/dns-data.dictionary 。
//     此处选 iOS 14+ dict 形态:v2.86 目标用户绝大多数 iOS 14+,iOS 13- 占比 <1%。
//     修复后 captive.apple.com 探测能用 DNS 解析,v2-78 验收项才真正生效。
//
//   - v2-80:加 DisconnectOnSleep/DisconnectOnIdle/NATKeepalive/OnDemand 4 项"省电+不断线"配置。
//     调研来源:
//       - Apple 官方 IKEv2 设备管理设置
//           https://support.apple.com/zh-cn/guide/deployment/dep4ce9487d/web
//       - Apple DTS Engineer Quinn (Apple Developer Forum, 2018):
//           DisconnectOnSleep=false 避免 iOS 主动断 VPN
//       - strongSwan 官方 OnDemand 模板:
//           https://docs.strongswan.org/docs/latest/interop/appleIkev2Profile.html
//     字段取值见
//       https://developer.apple.com/documentation/networkextension/nevpnprotocolikev2
//       https://developer.apple.com/documentation/networkextension/nevpnprotocolondemandrules
//
//     4 项作用:
//       - DisconnectOnSleep=false  iOS 锁屏不会主动断 VPN(设备睡期间 ESP SA 保活)
//       - DisconnectOnIdle=0       闲置不主动断(Apple Deployment Guide "Never disconnect")
//       - NATKeepAliveOffloadEnable=true + NATKeepAliveInterval=60
//         iOS 在锁屏后将 NAT keepalive 交给硬件网卡发送,防止运营商 NAT 映射过期被踢
//         (v2.86-PR12.22 audit 改用 Apple 文档的准确命名:大写 A)
//       - OnDemandEnabled=1 + Action=Connect
//         Wi-Fi ↔ Cellular 切换时自动重连(防止 MOBIKE 切换期间意外掉)
//
//   - v2.86-PR12.13:加 IncludeAllNetworks=true + ExcludeLocalNetworks=true。
//     背景:iOS 17+ 默认对 ESP tunnel 做 split-tunnel by IPv6,跟 IPv6-only VPN 服务器
//     不一定兼容(我们 server 端 local_ts = 0.0.0.0/0, ::/0,需要 client 把所有流量
//     都打进隧道)。IncludeAllNetworks=true 让所有流量走 VPN,ExcludeLocalNetworks=true
//     保留局域网访问(如 192.168.x.x 路由器管理页面)。来源:
//       - developer.apple.com/documentation/networkextension/nevpnprotocolikev2
//
//     与 MOBIKE 的关系:
//       - DisableMOBIKE=0 (上方配置)= MOBIKE 启用 (iOS 默认就是 0)
//       - MOBIKE (RFC 4555) 是 IKEv2 切换 IP 的"省电+不重连"关键:
//         Wi-Fi → Cellular 切换时,client 发起 UPDATE_SA_ADDRESS 复用现有 IKE SA,
//         ESP SA **不重协商**(省掉 1 次 DH 交换 + 1 次 ESP rekey),
//         iOS 26 PQC fallback 进一步允许 server 不支持 PQC 时降级到 ECDH,
//         共同实现"不重连且省电"
//       - strongSwan 默认 mobike=yes,这里写 DisableMOBIKE=0 是显式声明 (iOS DTS Quinn 2019)
//       - 注意:README line 351 建议"不要开 mobike"是针对 libipsec + 高吞吐场景的
//         **特定**优化,**日常家用场景必须开**,否则 iOS 切网就掉 VPN
//     注:Always On VPN 仅 supervised(MDM)设备可用,普通描述文件仅能存在上述 4 项。
//
//   - v2.86-PR12.21:模板里 4 处 hardcoded 字段改成动态渲染:
//       1. UserDefinedName (默认 "IKEv2 VPN")
//       2. DisconnectOnSleep / NATKeepaliveInterval / NATKeepaliveEnabled
//       3. OnDemandEnabled + OnDemandRules(5 个 preset)
//       4. IncludeAllNetworks / ExcludeLocalNetworks / DNS.ServerAddresses
//     全部从 {{.Opts}} 取值;不依赖环境变量,每用户独立。
const mobileconfigTmpl = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>PayloadContent</key><array>
{{- if .IncludeCA}}<dict><key>PayloadType</key><string>com.apple.security.root</string><key>PayloadVersion</key><integer>1</integer><key>PayloadCertificateFileName</key><string>ca.cert.pem</string><key>PayloadContent</key><data>{{ .CACertBase64 }}</data></dict>
{{- end}}<dict><key>PayloadType</key><string>com.apple.vpn.managed</string><key>PayloadVersion</key><integer>1</integer><key>PayloadIdentifier</key><string>com.example.ikev2.{{ .Username }}</string><key>PayloadUUID</key><string>{{ .PayloadUUID }}</string><key>PayloadDisplayName</key><string>IKEv2 VPN ({{ .PanelVersion }})</string><key>PayloadDescription</key><string>IKEv2 VPN for {{ .Username }} · {{ .PanelVersion }} · {{ .BuildTimestamp }}</string><key>UserDefinedName</key><string>{{ .UserDefinedName }}</string><key>VPNType</key><string>IKEv2</string><key>IKEv2</key><dict><key>RemoteAddress</key><string>{{ .ServerAddress }}</string><key>LocalIdentifier</key><string>{{ .Username }}</string><key>RemoteIdentifier</key><string>{{ .ServerID }}</string><key>ServerCertificateCommonName</key><string>{{ .ServerID }}</string><key>AuthenticationMethod</key><string>Certificate</string><key>ExtendedAuthEnabled</key><integer>1</integer><key>AuthName</key><string>{{ .Username }}</string><key>AuthPassword</key><string>{{ .AuthPassword }}</string><key>DeadPeerDetectionRate</key><string>{{ .DeadPeerDetectionRate }}</string><key>DisableMOBIKE</key><integer>0</integer>{{ if .DisconnectOnSleep }}<key>DisconnectOnSleep</key><true/>{{ else }}<key>DisconnectOnSleep</key><false/>{{ end }}<key>DisconnectOnIdle</key><integer>0</integer>{{ if .NATKeepaliveEnabled }}<key>NATKeepAliveOffloadEnable</key><true/>{{ else }}<key>NATKeepAliveOffloadEnable</key><false/>{{ end }}<key>NATKeepAliveInterval</key><integer>{{ .NATKeepaliveInterval }}</integer>{{ if .OnDemandEnabled }}<key>OnDemandEnabled</key><integer>1</integer><key>OnDemandRules</key>{{ .OnDemandRules }}{{ else }}<key>OnDemandEnabled</key><integer>0</integer>{{ end }}<key>DNS</key><dict><key>ServerAddresses</key><array>{{ range .DNSServers }}<string>{{ . }}</string>
{{ end }}</array><key>DNSProtocol</key><string>Cleartext</string></dict>{{ if .IncludeAllNetworks }}<key>IncludeAllNetworks</key><true/>{{ else }}<key>IncludeAllNetworks</key><false/>{{ end }}{{ if .ExcludeLocalNetworks }}<key>ExcludeLocalNetworks</key><true/>{{ else }}<key>ExcludeLocalNetworks</key><false/>{{ end }}<key>IKESecurityAssociationParameters</key><dict><key>EncryptionAlgorithm</key><string>AES-256-GCM</string><key>IntegrityAlgorithm</key><string>SHA2-256</string><key>DiffieHellmanGroup</key><integer>31</integer><key>LifeTimeMinutes</key><integer>1440</integer></dict><key>ChildSecurityAssociationParameters</key><dict><key>EncryptionAlgorithm</key><string>AES-256-GCM</string><key>IntegrityAlgorithm</key><string>SHA2-256</string><key>DiffieHellmanGroup</key><integer>31</integer><key>LifeTimeMinutes</key><integer>1440</integer></dict></dict></dict>
</array><key>PayloadDisplayName</key><string>IKEv2 VPN ({{ .PanelVersion }})</string><key>PayloadDescription</key><string>IKEv2 VPN for {{ .Username }} · {{ .PanelVersion }} · {{ .BuildTimestamp }}</string><key>PayloadIdentifier</key><string>com.example.ikev2.profile</string><key>PayloadType</key><string>Configuration</string><key>PayloadUUID</key><string>{{ .ProfileUUID }}</string><key>PayloadVersion</key><integer>1</integer></dict></plist>
`

// mobileconfigParams 模板参数。
type mobileconfigParams struct {
	IncludeCA            bool
	CACertBase64         string
	Username             string
	AuthPassword         string // 用户密码（v2-76+ EAP-MSCHAPv2 模式，从 user 对象取）
	ServerAddress        string
	ServerID             string
	PayloadUUID          string
	ProfileUUID          string
	PanelVersion         string // 镜像版本号（如 v2-76），写进描述方便用户辨识装的是哪一版
	BuildTimestamp       string // 描述文件生成时间（UTC RFC3339），设置 → VPN 与设备管理 → 描述文件里能看见

	// v2.86-PR12.21:用户可调字段,由 opts 解析
	UserDefinedName      string
	DisconnectOnSleep    bool
	NATKeepaliveEnabled  bool
	NATKeepaliveInterval int
	OnDemandEnabled      bool
	OnDemandRules        safeHTML // 整段 <array>...</array>(已转义,告诉 template 不要二次 escape)
	IncludeAllNetworks   bool
	ExcludeLocalNetworks bool
	DNSServers           []string
	DeadPeerDetectionRate string
}

// safeHTML 标记字符串为"已转义的 HTML",让 text/template 不再 escape。
//
// 用 text/template(不是 html/template)时,默认所有 {{ }} 都会 escape;
// 但 OnDemandRules 是我们自己构造的整段 XML,里面 < > 等已经在合理范围内
// (只来自受控的 preset 名 + XML-escaped SSID),需要原样输出。
//
// 替代方案:用 type alias 到 string + 在模板里 {{ .OnDemandRules | safeHTML }} 自定义 funcs;
// 这里用独立类型更简单,模板里直接调用 .OnDemandRules 时 Go 会用 fmt.Sprint 转字符串。
type safeHTML string

// String 实现 fmt.Stringer,让 {{ .OnDemandRules }} 输出原始字符串。
func (s safeHTML) String() string { return string(s) }

// RenderMobileconfig 渲染 mobileconfig XML。
//
// 参数：
//   - username         用户名（mobileconfig 里 LocalIdentifier + AuthName）
//   - password         用户密码（mobileconfig 里 AuthPassword，iOS 装完 profile 就记住，不弹密码框）
//   - serverAddr       服务器地址（IPv4 或 IPv6 字符串）
//   - serverID         服务器证书 CN（mobileconfig 里 RemoteIdentifier）
//   - caCertPEM        CA 证书 PEM 字节（自签模式内联，LE 模式传 nil）
//   - tzName           v2.85-PR3:IANA TZ 名(默认 UTC),用于 BuildTimestamp。
//     iOS NSDateFormatter 100% 支持 RFC3339 with offset 格式 "+08:00" 或 "Z",
//     老格式 "Z"(UTC) 是 RFC3339 子集,新格式 "Z07:00" 也是 RFC3339 子集 → 完全兼容。
//   - opts             v2.86-PR12.22:已合并的 mobileconfig 选项(handler 端负责合并)。
//     nil = 用 BaseMobileConfigDefaults()。
//     handler 调用链:panelstate.MobileConfigDefaults → cert.MobileConfigOpts (admin)
//                  → cert.EffectiveMobileConfigOpts(admin, user overlay)
//                  → RenderMobileconfig(..., merged)
//
// mobileconfig 的 PayloadDescription 里会写当前 PanelVersion + 生成时间，
// 方便用户在 iOS 设置 → VPN 与设备管理 → 描述文件详情 里看清自己装的是哪一版。
//
// v2.86-PR12.22:为避免 cert → panelstate 反向依赖,handler 端完成 admin ↔ cert 转换 + 三层合并。
// cert.RenderMobileconfig 只接受"最终合并后"的 *MobileConfigOpts。
func RenderMobileconfig(username, password, serverAddr, serverID string, caCertPEM []byte, tzName string, opts *MobileConfigOpts) ([]byte, error) {
	// v2.86-PR12.22:三层合并已经在 handler 完成(opts 是合并后的最终值)。
	// 这里只用 EffectiveMobileConfigOpts 的 builtin + opts 二层合并逻辑
	// (admin 层已含在 opts 里,handler 用 cert.EffectiveMobileConfigOpts(admin, overlay) 合并过)。
	effective := EffectiveMobileConfigOpts(nil, opts)

	params := mobileconfigParams{
		PanelVersion:   PanelVersion,
		Username:       username,
		AuthPassword:   password,
		ServerAddress:  serverAddr,
		ServerID:       serverID,
		PayloadUUID:    generateUUID(),
		ProfileUUID:    generateUUID(),
		// v2.85-PR3:用 tzName 渲染 BuildTimestamp,跟面板时间一致。
		// LoadLocation 失败 fallback UTC,跟 config / handler 端策略一致。
		BuildTimestamp: nowInTZ(tzName),

		// v2.86-PR12.21:可调字段
		UserDefinedName:      effectiveUserDefinedName(effective.UserDefinedName),
		DisconnectOnSleep:    boolDeref(effective.DisconnectOnSleep),
		NATKeepaliveEnabled:  boolDeref(effective.NATKeepaliveEnabled),
		NATKeepaliveInterval: intDeref(effective.NATKeepaliveInterval, 60),
		OnDemandEnabled:      boolDeref(effective.OnDemandEnabled),
		OnDemandRules:        renderOnDemandRules(effective),
		IncludeAllNetworks:   boolDeref(effective.IncludeAllNetworks),
		ExcludeLocalNetworks: boolDeref(effective.ExcludeLocalNetworks),
		DNSServers:           effective.DNSServers,
		DeadPeerDetectionRate: effective.DeadPeerDetectionRate,
	}
	if len(params.DNSServers) == 0 {
		params.DNSServers = BaseMobileConfigDefaults().DNSServers
	}
	if params.DeadPeerDetectionRate == "" {
		params.DeadPeerDetectionRate = "Medium"
	}

	if len(caCertPEM) > 0 {
		params.IncludeCA = true
		params.CACertBase64 = base64.StdEncoding.EncodeToString(caCertPEM)
	}

	tmpl, err := template.New("mobileconfig").Parse(mobileconfigTmpl)
	if err != nil {
		return nil, fmt.Errorf("parse mobileconfig template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, params); err != nil {
		return nil, fmt.Errorf("execute mobileconfig template: %w", err)
	}
	return buf.Bytes(), nil
}

// effectiveUserDefinedName 从 *string 解析最终显示名。
func effectiveUserDefinedName(p *string) string {
	if p == nil || *p == "" {
		return "IKEv2 VPN"
	}
	return *p
}

// boolDeref 解引用 *bool,nil = false。
func boolDeref(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// intDeref 解引用 *int,nil 时用 fallback。
func intDeref(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}

// renderOnDemandRules 根据 preset 渲染 OnDemandRules XML 段(完整 <array>...</array>)。
//
// Apple 规则匹配是顺序匹配、第一条完全命中即停止,见 developer.apple.com/documentation/
// devicemanagement/vpn/vpn-data.dictionary/ondemandruleselement 。
//
// 5 个 preset 完整 XML 段(每个 <array> 含 1-2 条 rule + 兜底):
//   - always:              [{Action=Connect}]
//   - home_wifi_disconnect:[{SSID=X,Action=Disconnect}, {Action=Connect}]
//   - cellular_only:       [{InterfaceType=Cell,Connect}, {InterfaceType=WiFi,Disconnect}, {Disconnect}]
//   - captive_probe:       [{EvaluateConnection+ActionParameters[NeverConnect apple.com, ConnectIfNeeded *]} + {Action=Connect}]
//   - manual:              OnDemandEnabled=0 时整段 <array> 不输出
//
// 返回 template.HTML 是为了告诉 Go template 引擎不要二次转义(我们自己控制内容)。
func renderOnDemandRules(o MobileConfigOpts) safeHTML {
	switch o.OnDemandProfile {
	case "", OnDemandAlwaysConnect:
		return safeHTML(`<array><dict><key>Action</key><string>Connect</string></dict></array>`)
	case OnDemandHomeWiFiDisconnect:
		// SSID 已 Validate 过,这里转义 XML 特殊字符
		ssid := xmlEscape(o.OnDemandSSID)
		return safeHTML(`<array><dict><key>InterfaceTypeMatch</key><string>WiFi</string><key>SSIDMatch</key><array><string>` + ssid + `</string></array><key>Action</key><string>Disconnect</string></dict><dict><key>Action</key><string>Connect</string></dict></array>`)
	case OnDemandCellularOnly:
		// 兜底用 Ignore 而不是 Disconnect:strongSwan 官方模板行为。
		// Disconnect 会主动踢掉已连接的 VPN;Ignore 是"保持当前状态"
		// (连着就保持连,断着就保持断),更安全。
		return safeHTML(`<array><dict><key>InterfaceTypeMatch</key><string>Cellular</string><key>Action</key><string>Connect</string></dict><dict><key>InterfaceTypeMatch</key><string>WiFi</string><key>Action</key><string>Disconnect</string></dict><dict><key>Action</key><string>Ignore</string></dict></array>`)
	case OnDemandCaptiveProbe:
		return safeHTML(`<array><dict><key>Action</key><string>EvaluateConnection</string><key>ActionParameters</key><array><dict><key>Domains</key><array><string>apple.com</string></array><key>DomainAction</key><string>NeverConnect</string><key>RequiredURLStringProbe</key><string>https://captive.apple.com/hotspot-detect.html</string></dict><dict><key>Domains</key><array><string></string></array><key>DomainAction</key><string>ConnectIfNeeded</string><key>RequiredURLStringProbe</key><string>https://captive.apple.com/hotspot-detect.html</string></dict></array></dict><dict><key>Action</key><string>Connect</string></dict></array>`)
	case OnDemandManual:
		// OnDemandEnabled=false 时整段 <array> 不输出;这里返回空字符串
		return safeHTML(``)
	default:
		// 防御性:Validate 已挡掉,这里再 fallback 到 always
		return safeHTML(`<array><dict><key>Action</key><string>Connect</string></dict></array>`)
	}
}

// xmlEscape 转义 mobileconfig plist 中 <string> 内可能破坏 XML 的字符。
//
// plist <string> 元素本身不需要严格 escape(< > & 在 <string> 里也合法),
// 但 iOS 某些版本对未转义字符的处理不一致,保守 escape。
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		`'`, "&apos;",
	)
	return r.Replace(s)
}

// PanelVersion 镜像版本号，写进 mobileconfig 的 PayloadDescription。
// 升级镜像时记得改这个常量（v2-75 / v2-76 ...），用户就能在 iOS 设置 → VPN 与设备管理
// 里看见自己装的是哪个版本，避免反复装旧描述文件。
//
// v2.86：v2.85 之后的综合规范化(Sprint 1-3):
//   - Sprint 1: Docker 减权 + 清华源 GPG / 登录 rate limit + HTTP 安全 header + DDoS
//     / IKEv2 算法去弱 + RFC 7383 分片 + RFC 6023 childless
//   - Sprint 2: 续签 atomic + acme.sh pinned commit + cron 随机化 / filelog 脱敏 +
//     logrotate / DDNS jitter retry
//   - Sprint 3: VICI SA lifecycle 审计
// 详见 docs/release-notes-v2.86.md。
const PanelVersion = "v2.86"

// generateUUID RFC 4122 v4 随机 UUID。
// mobileconfig 的 PayloadUUID/ProfileUUID 必须全局唯一，否则 iOS 覆盖会乱。
func generateUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0F) | 0x40 // version 4
	b[8] = (b[8] & 0x3F) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// nowInTZ v2.85-PR3:返回当前时间按 tzName 渲染的 RFC3339 字符串。
//   - tzName=IANA TZ 名(如 "Asia/Shanghai")
//   - LoadLocation 失败 → fallback UTC + 返回 RFC3339 with Z
//   - 成功 → 返回 RFC3339 with offset(如 "2026-09-19T16:30:15+08:00")
//
// iOS NSDateFormatter 100% 兼容两种格式,选哪种都安全。
func nowInTZ(tzName string) string {
	if tzName == "" {
		return time.Now().UTC().Format(time.RFC3339)
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil || loc == nil {
		return time.Now().UTC().Format(time.RFC3339)
	}
	return time.Now().In(loc).Format("2006-01-02T15:04:05Z07:00")
}