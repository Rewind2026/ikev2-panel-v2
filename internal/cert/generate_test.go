package cert

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureCA(t *testing.T) {
	dir := t.TempDir()

	cert1, _, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert1) == 0 {
		t.Fatal("empty CA cert")
	}

	// 第二次调用应直接读文件，不重复生成
	cert2, _, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(cert1) != string(cert2) {
		t.Error("second call should return same cert")
	}

	// 解析 cert 验证可用
	block, _ := pem.Decode(cert1)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("no CERTIFICATE PEM block")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.IsCA {
		t.Error("cert is not a CA")
	}
	if parsed.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA cert missing KeyUsageCertSign")
	}
}

func TestEnsureServerCert(t *testing.T) {
	dir := t.TempDir()

	caCertPEM, caKeyPEM, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	srvCert, _, err := EnsureServerCert(dir, "vpn.example.com", caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	block, _ := pem.Decode(srvCert)
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	if parsed.Subject.CommonName != "vpn.example.com" {
		t.Errorf("CN mismatch: %s", parsed.Subject.CommonName)
	}
	if len(parsed.DNSNames) != 1 || parsed.DNSNames[0] != "vpn.example.com" {
		t.Errorf("SAN missing: %v", parsed.DNSNames)
	}
	if parsed.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("server cert missing KeyUsageDigitalSignature")
	}
	// v2-76 关键检查：server cert 必须有 ExtKeyUsage=ServerAuth（iOS 13+ 强制要求）。
	if len(parsed.ExtKeyUsage) == 0 {
		t.Error("server cert missing ExtKeyUsage")
	}
	hasServerAuth := false
	for _, u := range parsed.ExtKeyUsage {
		if u == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
			break
		}
	}
	if !hasServerAuth {
		t.Error("server cert missing ExtKeyUsageServerAuth (required by iOS 13+)")
	}
}

func TestRenderMobileconfig(t *testing.T) {
	caCert := []byte("-----BEGIN CERTIFICATE-----\nMIIBfake\n-----END CERTIFICATE-----\n")

	// v2-76：EAP-MSCHAPv2 模式。参数顺序：username, password, serverAddr, serverID, caPEM
	out, err := RenderMobileconfig("alice", "alicePass123", "vpn.example.com", "vpn.example.com", caCert)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	if !strings.Contains(s, "alice") {
		t.Error("missing username")
	}
	if !strings.Contains(s, "vpn.example.com") {
		t.Error("missing server address")
	}
	if !strings.Contains(s, "IKEv2") {
		t.Error("missing IKEv2 reference")
	}
	if !strings.Contains(s, base64.StdEncoding.EncodeToString(caCert)) {
		t.Error("CA cert not base64-inlined")
	}
	if !strings.Contains(s, "DisableMOBIKE") {
		t.Error("DisableMOBIKE not set")
	}
	// v2-76：EAP-MSCHAPv2 模式。AuthenticationMethod=Certificate + ExtendedAuthEnabled=1 +
	// AuthName/AuthPassword 内置在 profile 里（解决 v2-74 弹密码框 + silently drop）。
	if !strings.Contains(s, "<key>AuthenticationMethod</key><string>Certificate</string>") {
		t.Error("AuthenticationMethod must be Certificate for EAP-MSCHAPv2 mode")
	}
	if !strings.Contains(s, "<key>ExtendedAuthEnabled</key><integer>1</integer>") {
		t.Error("ExtendedAuthEnabled must be 1 to enable EAP")
	}
	if !strings.Contains(s, "<key>AuthName</key><string>alice</string>") {
		t.Error("AuthName must equal username (no iOS password prompt)")
	}
	if !strings.Contains(s, "<key>AuthPassword</key><string>alicePass123</string>") {
		t.Error("AuthPassword must be embedded in profile (no iOS password prompt)")
	}
	if !strings.Contains(s, "<key>AuthPasswordRetries</key><integer>3</integer>") {
		t.Error("AuthPasswordRetries should be 3 (strongSwan recommendation)")
	}
	// v2-75 PSK 模式已废：确保不再出现
	if strings.Contains(s, "<key>AuthenticationMethod</key><string>SharedSecret</string>") {
		t.Error("AuthenticationMethod=SharedSecret is v2-75 PSK; v2-76 uses EAP")
	}
	if strings.Contains(s, "<key>SharedSecret</key>") {
		t.Error("SharedSecret field is v2-75 PSK; v2-76 uses AuthPassword")
	}
	if strings.Contains(s, "<key>AuthenticationMethod</key><string>None</string>") {
		t.Error("AuthenticationMethod=None will skip auth; iOS will fail IKE_AUTH")
	}
	if strings.Contains(s, "ExtendedAuthenticationEnabled") {
		t.Error("deprecated ExtendedAuthenticationEnabled should not appear")
	}
	if strings.Contains(s, "IKEv2Settings") {
		t.Error("deprecated IKEv2Settings dict should be IKEv2 dict")
	}
	// iOS 校验：PayloadContent 数组里的每个 payload 都必须带 PayloadVersion=1。
	// CA payload 在 IncludeCA 模式下渲染一次，VPN payload 始终渲染一次，外层 profile 渲染一次，期望 3 次。
	if got := strings.Count(s, "<key>PayloadVersion</key><integer>1</integer>"); got != 3 {
		t.Errorf("PayloadVersion count = %d, want 3 (CA + VPN + outer profile)", got)
	}
	// 镜像版本号 + 生成时间必须写进 PayloadDescription，便于用户在 iOS 设置里辨识装的是哪一版。
	if !strings.Contains(s, PanelVersion) {
		t.Errorf("PayloadDescription missing PanelVersion %q", PanelVersion)
	}
	if !strings.Contains(s, time.Now().UTC().Format("2006-01-02")) {
		t.Error("PayloadDescription missing today's UTC date")
	}

	// LE 模式：无内联 CA
	out2, err := RenderMobileconfig("bob", "bobPass456", "vpn.example.com", "vpn.example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	s2 := string(out2)
	if strings.Contains(s2, "com.apple.security.root") {
		t.Error("LE mode should not include CA payload")
	}
	// LE 模式下 AuthPassword 仍然要嵌入
	if !strings.Contains(s2, "<key>AuthPassword</key><string>bobPass456</string>") {
		t.Error("LE mode should still embed AuthPassword in profile")
	}
}

func TestGenerateUUID(t *testing.T) {
	u1 := generateUUID()
	u2 := generateUUID()
	if u1 == u2 {
		t.Error("UUID collision")
	}
	if len(u1) != 36 {
		t.Errorf("UUID len: got %d want 36", len(u1))
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	caCertPEM, caKeyPEM, _ := EnsureCA(dir)
	_ = caCertPEM
	_ = caKeyPEM

	caPath := filepath.Join(dir, "ca", "ca.cert.pem")
	if _, err := os.Stat(caPath); err != nil {
		t.Errorf("CA cert file not created: %v", err)
	}
}
