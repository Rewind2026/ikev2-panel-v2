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
	"fmt"
	"text/template"
	"time"
)

// mobileconfigTmpl 模板。
//
// 关键决策（跟 Apple/strongSwan 官方 EAP 模板对齐）：
//   - AuthenticationMethod = Certificate：server 用证书做身份
//   - ExtendedAuthEnabled = 1：客户端走 EAP
//   - AuthName + AuthPassword 内置在 profile 里：iOS 装了 profile 就记下凭证，
//     **不弹密码框**（这是解决 v2-74 弹密码框 + silently drop 的关键）
//   - AuthPasswordRetries = 3：iOS 客户端 EAP 失败时最多重试 3 次（strongSwan 官方建议）
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
//   - v2-78：加 DNSSettings 数组（4 = IPv4 DNS, 6 = IPv6 DNS），
//     iOS 拨号后用这里指定的 DNS 而不是拨号前的 WiFi/蜂窝 DNS。
//     不推 DNS 时 iOS 把所有 UDP 53 按 0.0.0.0/0 强制路由进 ESP 隧道，
//     server 端 swanctl pools 没 dns 时客户端 DNS 解析走不通 → captive.apple.com
//     探测超时 → Safari/WX 报"没连接互联网"（即使 TCP 443 实际能 curl 通）。
//
//   - v2-80:加 DisconnectOnSleep/DisconnectOnIdle/NATKeepalive/OnDemand 4 项“省电+不断线”配置。
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
//       - DisconnectOnIdle=0       闲置不主动断(Apple Deployment Guide “Never disconnect”)
//       - NATKeepaliveEnabled=true + NATKeepaliveInterval=60
//         iOS 在锁屏后将 NAT keepalive 交给硬件网卡发送,防止运营商 NAT 映射过期被踢
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
//       - MOBIKE (RFC 4555) 是 IKEv2 切换 IP 的“省电+不重连”关键:
//         Wi-Fi → Cellular 切换时,client 发起 UPDATE_SA_ADDRESS 复用现有 IKE SA,
//         ESP SA **不重协商**(省掉 1 次 DH 交换 + 1 次 ESP rekey),
//         iOS 26 PQC fallback 进一步允许 server 不支持 PQC 时降级到 ECDH,
//         共同实现"不重连且省电"
//       - strongSwan 默认 mobike=yes,这里写 DisableMOBIKE=0 是显式声明 (iOS DTS Quinn 2019)
//       - 注意:README line 351 建议“不要开 mobike”是针对 libipsec + 高吞吐场景的
//         **特定**优化,**日常家用场景必须开**,否则 iOS 切网就掉 VPN
//     注:Always On VPN 仅 supervised(MDM)设备可用,普通描述文件仅能存在上述 4 项。
const mobileconfigTmpl = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>PayloadContent</key><array>
{{- if .IncludeCA}}<dict><key>PayloadType</key><string>com.apple.security.root</string><key>PayloadVersion</key><integer>1</integer><key>PayloadCertificateFileName</key><string>ca.cert.pem</string><key>PayloadContent</key><data>{{ .CACertBase64 }}</data></dict>
{{- end}}<dict><key>PayloadType</key><string>com.apple.vpn.managed</string><key>PayloadVersion</key><integer>1</integer><key>PayloadIdentifier</key><string>com.example.ikev2.{{ .Username }}</string><key>PayloadUUID</key><string>{{ .PayloadUUID }}</string><key>PayloadDisplayName</key><string>IKEv2 VPN ({{ .PanelVersion }})</string><key>PayloadDescription</key><string>IKEv2 VPN for {{ .Username }} · {{ .PanelVersion }} · {{ .BuildTimestamp }}</string><key>UserDefinedName</key><string>IKEv2 VPN</string><key>VPNType</key><string>IKEv2</string><key>IKEv2</key><dict><key>RemoteAddress</key><string>{{ .ServerAddress }}</string><key>LocalIdentifier</key><string>{{ .Username }}</string><key>RemoteIdentifier</key><string>{{ .ServerID }}</string><key>ServerCertificateCommonName</key><string>{{ .ServerID }}</string><key>AuthenticationMethod</key><string>Certificate</string><key>ExtendedAuthEnabled</key><integer>1</integer><key>AuthName</key><string>{{ .Username }}</string><key>AuthPassword</key><string>{{ .AuthPassword }}</string><key>AuthPasswordRetries</key><integer>3</integer><key>DeadPeerDetectionRate</key><string>Medium</string><key>DisableMOBIKE</key><integer>0</integer><key>DisconnectOnSleep</key><false/><key>DisconnectOnIdle</key><integer>0</integer><key>NATKeepaliveEnabled</key><true/><key>NATKeepaliveInterval</key><integer>60</integer><key>OnDemandEnabled</key><integer>1</integer><key>OnDemandRules</key><array><dict><key>Action</key><string>Connect</string></dict></array><key>DNSSettings</key><dict><key>DNS</key><array><string>1.1.1.1</string><string>8.8.8.8</string><string>2606:4700:4700::1111</string><string>2001:4860:4860::8888</string></array></dict><key>IncludeAllNetworks</key><true/><key>ExcludeLocalNetworks</key><true/><key>IKESecurityAssociationParameters</key><dict><key>EncryptionAlgorithm</key><string>AES-256-GCM</string><key>IntegrityAlgorithm</key><string>SHA2-256</string><key>DiffieHellmanGroup</key><integer>31</integer><key>LifeTimeMinutes</key><integer>1440</integer></dict><key>ChildSecurityAssociationParameters</key><dict><key>EncryptionAlgorithm</key><string>AES-256-GCM</string><key>IntegrityAlgorithm</key><string>SHA2-256</string><key>DiffieHellmanGroup</key><integer>31</integer><key>LifeTimeMinutes</key><integer>1440</integer></dict></dict></dict>
</array><key>PayloadDisplayName</key><string>IKEv2 VPN ({{ .PanelVersion }})</string><key>PayloadDescription</key><string>IKEv2 VPN for {{ .Username }} · {{ .PanelVersion }} · {{ .BuildTimestamp }}</string><key>PayloadIdentifier</key><string>com.example.ikev2.{{ .Username }}</string><key>PayloadType</key><string>Configuration</string><key>PayloadUUID</key><string>{{ .ProfileUUID }}</string><key>PayloadVersion</key><integer>1</integer></dict></plist>
`

// mobileconfigParams 模板参数。
type mobileconfigParams struct {
	IncludeCA      bool
	CACertBase64   string
	Username       string
	AuthPassword   string // 用户密码（v2-76+ EAP-MSCHAPv2 模式，从 user 对象取）
	ServerAddress  string
	ServerID       string
	PayloadUUID    string
	ProfileUUID    string
	PanelVersion   string // 镜像版本号（如 v2-76），写进描述方便用户辨识装的是哪一版
	BuildTimestamp string // 描述文件生成时间（UTC RFC3339），设置 → VPN 与设备管理 → 描述文件里能看见
}

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
//
// mobileconfig 的 PayloadDescription 里会写当前 PanelVersion + 生成时间，
// 方便用户在 iOS 设置 → VPN 与设备管理 → 描述文件详情 里看清自己装的是哪一版。
func RenderMobileconfig(username, password, serverAddr, serverID string, caCertPEM []byte, tzName string) ([]byte, error) {
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
