// v2.86-PR19(P2-18):叶证书 EKU 与 SAN 语义的回归测试。
//
// 修复前的两个问题：
//  1. ExtKeyUsage 同时含 ServerAuth + ClientAuth —— ClientAuth 对 IKEv2
//     服务端证书是**多余的授权**（strongSwan 从不拿它做客户端认证），
//     留着等于扩大证书可用范围。
//  2. SAN 无条件写 DNSNames=[cn] + 无条件带 127.0.0.1：
//     - CN 是 IP 字面量时把 IP 写进 dNSName 是 RFC 5280 语义错误
//     （dNSName 只能是 DNS 名），严格 TLS 栈会判证书畸形
//     - 127.0.0.1 面板从不对外提供，写进去只是无意义的暴露面
//
// 本文件验证：EKU 只含 ServerAuth；SAN 按 CN 类型二选一；
// extraIPs 仍能正确附加；证书仍能被 x509.Verify 用 CA 验签通过。
package cert

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
)

// parseLeaf 解析生成的叶证书
func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		t.Fatal("failed to decode PEM")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return c
}

// TestLeafCertHasNoClientAuth 叶证书不得含 ClientAuth（P2-18 核心）。
func TestLeafCertHasNoClientAuth(t *testing.T) {
	caCertPEM, caKeyPEM, err := EnsureCA(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	leafPEM, _, err := EnsureServerCert(t.TempDir(), "vpn.example.com", caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}

	leaf := parseLeaf(t, leafPEM)
	var hasServer, hasClient bool
	for _, u := range leaf.ExtKeyUsage {
		switch u {
		case x509.ExtKeyUsageServerAuth:
			hasServer = true
		case x509.ExtKeyUsageClientAuth:
			hasClient = true
		}
	}
	if !hasServer {
		t.Error("leaf cert MUST keep ServerAuth (iOS 13+ requires it)")
	}
	if hasClient {
		t.Error("leaf cert must NOT contain ClientAuth — IKEv2 server identity never needs it, " +
			"and it widens who can be authenticated by this cert")
	}
}

// TestLeafCertSANByCNType SAN 必须按 CN 类型二选一（P2-18）。
func TestLeafCertSANByCNType(t *testing.T) {
	caDir := t.TempDir()
	caCertPEM, caKeyPEM, err := EnsureCA(caDir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	t.Run("域名 CN → 只进 DNSNames", func(t *testing.T) {
		leafPEM, _, err := EnsureServerCert(t.TempDir(), "vpn.example.com", caCertPEM, caKeyPEM)
		if err != nil {
			t.Fatalf("EnsureServerCert: %v", err)
		}
		leaf := parseLeaf(t, leafPEM)
		if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "vpn.example.com" {
			t.Errorf("DNSNames = %v, want [vpn.example.com]", leaf.DNSNames)
		}
		if len(leaf.IPAddresses) != 0 {
			t.Errorf("IPAddresses = %v, want empty (域名 CN 不该产生 IP SAN)", leaf.IPAddresses)
		}
	})

	t.Run("IPv4 字面量 CN → 只进 IPAddresses", func(t *testing.T) {
		leafPEM, _, err := EnsureServerCert(t.TempDir(), "192.168.1.10", caCertPEM, caKeyPEM)
		if err != nil {
			t.Fatalf("EnsureServerCert: %v", err)
		}
		leaf := parseLeaf(t, leafPEM)
		// 关键：IP 绝不能出现在 DNSNames 里（RFC 5280 语义错误）
		if len(leaf.DNSNames) != 0 {
			t.Errorf("DNSNames = %v, want empty — IP literal must not be a dNSName", leaf.DNSNames)
		}
		found := false
		for _, ip := range leaf.IPAddresses {
			if ip.Equal(net.ParseIP("192.168.1.10")) {
				found = true
			}
		}
		if !found {
			t.Errorf("IPAddresses = %v, want to contain 192.168.1.10", leaf.IPAddresses)
		}
	})

	t.Run("IPv6 字面量 CN → 只进 IPAddresses", func(t *testing.T) {
		leafPEM, _, err := EnsureServerCert(t.TempDir(), "2001:db8::1", caCertPEM, caKeyPEM)
		if err != nil {
			t.Fatalf("EnsureServerCert: %v", err)
		}
		leaf := parseLeaf(t, leafPEM)
		if len(leaf.DNSNames) != 0 {
			t.Errorf("DNSNames = %v, want empty", leaf.DNSNames)
		}
		found := false
		for _, ip := range leaf.IPAddresses {
			if ip.Equal(net.ParseIP("2001:db8::1")) {
				found = true
			}
		}
		if !found {
			t.Errorf("IPAddresses = %v, want to contain 2001:db8::1", leaf.IPAddresses)
		}
	})
}

// TestLeafCertNoHardcodedLoopback 不得再无条件塞 127.0.0.1（P2-18）。
func TestLeafCertNoHardcodedLoopback(t *testing.T) {
	caCertPEM, caKeyPEM, err := EnsureCA(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	leafPEM, _, err := EnsureServerCert(t.TempDir(), "vpn.example.com", caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}
	leaf := parseLeaf(t, leafPEM)
	for _, ip := range leaf.IPAddresses {
		if ip.IsLoopback() {
			t.Errorf("leaf cert contains loopback SAN %v — 面板从不对 localhost 提供服务", ip)
		}
	}
}

// TestLeafCertExtraIPsStillAppended extraIPs 机制不能被 P2-18 改坏。
//
// 这是 v2.86-PR12.13 加的能力：mobileconfig 用 IP 字面量时 iOS 也要能校验。
func TestLeafCertExtraIPsStillAppended(t *testing.T) {
	caCertPEM, caKeyPEM, err := EnsureCA(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	leafPEM, _, err := EnsureServerCert(t.TempDir(), "vpn.example.com", caCertPEM, caKeyPEM,
		"203.0.113.7", "  ", "not-an-ip", "2001:db8::99")
	if err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}
	leaf := parseLeaf(t, leafPEM)
	var haveV4, haveV6 bool
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("203.0.113.7")) {
			haveV4 = true
		}
		if ip.Equal(net.ParseIP("2001:db8::99")) {
			haveV6 = true
		}
	}
	if !haveV4 {
		t.Errorf("extra IPv4 not appended: %v", leaf.IPAddresses)
	}
	if !haveV6 {
		t.Errorf("extra IPv6 not appended: %v", leaf.IPAddresses)
	}
	// 域名 CN 仍要保留在 DNSNames
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "vpn.example.com" {
		t.Errorf("DNSNames = %v, want [vpn.example.com]", leaf.DNSNames)
	}
}

// TestLeafCertStillVerifiesAgainstCA 防止 P2-18 改坏签名链。
func TestLeafCertStillVerifiesAgainstCA(t *testing.T) {
	caDir := t.TempDir()
	caCertPEM, caKeyPEM, err := EnsureCA(caDir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	leafPEM, _, err := EnsureServerCert(t.TempDir(), "vpn.example.com", caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}

	caLeaf := parseLeaf(t, caCertPEM)
	pool := x509.NewCertPool()
	pool.AddCert(caLeaf)

	leaf := parseLeaf(t, leafPEM)
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:     pool,
			DNSName:   "vpn.example.com",
			KeyUsages: []x509.ExtKeyUsage{usage},
		}); err != nil {
			t.Errorf("verify failed for usage %v: %v", usage, err)
		}
	}
}

// TestPanelCertAlsoHasNoClientAuth Panel 证书同样不该有 ClientAuth。
func TestPanelCertAlsoHasNoClientAuth(t *testing.T) {
	caCertPEM, caKeyPEM, err := EnsureCA(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	leafPEM, _, err := EnsurePanelCert(t.TempDir(), "panel.example.com", caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("EnsurePanelCert: %v", err)
	}
	leaf := parseLeaf(t, leafPEM)
	for _, u := range leaf.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth {
			t.Error("panel cert must NOT contain ClientAuth")
		}
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "panel.example.com" {
		t.Errorf("DNSNames = %v, want [panel.example.com]", leaf.DNSNames)
	}
}
