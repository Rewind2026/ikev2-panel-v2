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
//   - ServerCertificateIssuerCommonName 缺失时不发送 CERTREQ；
//     server 端必须在 swanctl.conf 里 send_cert = always（见 swanctl-ipv6-only.conf）
//   - v2-78：加 DNSSettings 数组（4 = IPv4 DNS, 6 = IPv6 DNS），
//     iOS 拨号后用这里指定的 DNS 而不是拨号前的 WiFi/蜂窝 DNS。
//     不推 DNS 时 iOS 把所有 UDP 53 按 0.0.0.0/0 强制路由进 ESP 隧道，
//     server 端 swanctl pools 没 dns 时客户端 DNS 解析走不通 → captive.apple.com
//     探测超时 → Safari/WX 报"没连接互联网"（即使 TCP 443 实际能 curl 通）。
const mobileconfigTmpl = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>PayloadContent</key><array>
{{- if .IncludeCA}}<dict><key>PayloadType</key><string>com.apple.security.root</string><key>PayloadVersion</key><integer>1</integer><key>PayloadCertificateFileName</key><string>ca.cert.pem</string><key>PayloadContent</key><data>{{ .CACertBase64 }}</data></dict>
{{- end}}<dict><key>PayloadType</key><string>com.apple.vpn.managed</string><key>PayloadVersion</key><integer>1</integer><key>PayloadIdentifier</key><string>com.example.ikev2.{{ .Username }}</string><key>PayloadUUID</key><string>{{ .PayloadUUID }}</string><key>PayloadDisplayName</key><string>IKEv2 VPN ({{ .PanelVersion }})</string><key>PayloadDescription</key><string>IKEv2 VPN for {{ .Username }} · {{ .PanelVersion }} · {{ .BuildTimestamp }}</string><key>UserDefinedName</key><string>IKEv2 VPN</string><key>VPNType</key><string>IKEv2</string><key>IKEv2</key><dict><key>RemoteAddress</key><string>{{ .ServerAddress }}</string><key>LocalIdentifier</key><string>{{ .Username }}</string><key>RemoteIdentifier</key><string>{{ .ServerID }}</string><key>AuthenticationMethod</key><string>Certificate</string><key>ExtendedAuthEnabled</key><integer>1</integer><key>AuthName</key><string>{{ .Username }}</string><key>AuthPassword</key><string>{{ .AuthPassword }}</string><key>AuthPasswordRetries</key><integer>3</integer><key>DeadPeerDetectionRate</key><string>Medium</string><key>DisableMOBIKE</key><integer>0</integer><key>DNSSettings</key><dict><key>DNS</key><array><string>1.1.1.1</string><string>8.8.8.8</string><string>2606:4700:4700::1111</string><string>2001:4860:4860::8888</string></array></dict></dict></dict>
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
//
// mobileconfig 的 PayloadDescription 里会写当前 PanelVersion + UTC 生成时间，
// 方便用户在 iOS 设置 → VPN 与设备管理 → 描述文件详情 里看清自己装的是哪一版。
func RenderMobileconfig(username, password, serverAddr, serverID string, caCertPEM []byte) ([]byte, error) {
	params := mobileconfigParams{
		PanelVersion:   PanelVersion,
		Username:       username,
		AuthPassword:   password,
		ServerAddress:  serverAddr,
		ServerID:       serverID,
		PayloadUUID:    generateUUID(),
		ProfileUUID:    generateUUID(),
		BuildTimestamp: time.Now().UTC().Format(time.RFC3339),
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
const PanelVersion = "v2-79.1"

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
