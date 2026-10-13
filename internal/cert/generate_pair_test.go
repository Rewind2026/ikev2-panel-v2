package cert

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v2.86-PR19(A13)回归测试:证书对半缺失时必须 fail fast,绝不静默重签。
//
// 背景:EnsureCA/EnsureServerCert/EnsurePanelCert 原来判定条件是
// `exists(cert) && exists(key)`。半缺失(只有 cert 或只有 key)时条件为 false,
// 于是走"缺失则生成"分支重新签一套全新 CA —— 所有客户端 mobileconfig 里
// 内联的旧 CA 与新 CA 无信任关系,VPN 集体断连,且面板无任何提示。

func TestEnsureCA_HalfMissingKey_RefusesToRegenerate(t *testing.T) {
	dir := t.TempDir()
	caDir := filepath.Join(dir, "ca")
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(caDir, "ca.cert.pem")

	// 第一次:正常生成完整的一对
	cert, key, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("first EnsureCA: %v", err)
	}
	if len(cert) == 0 || len(key) == 0 {
		t.Fatal("EnsureCA returned empty pem")
	}
	originalCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟"只剩 cert,key 丢失"(迁移只拷一半 / 旧版裸 WriteFile 中途被杀)
	if err := os.Remove(filepath.Join(caDir, "ca.key.pem")); err != nil {
		t.Fatal(err)
	}

	_, _, err = EnsureCA(dir)
	if err == nil {
		t.Fatal("half-missing CA pair was silently regenerated — this breaks every enrolled client")
	}
	if !errors.Is(err, errInconsistentCert) {
		t.Fatalf("err = %v, want errInconsistentCert", err)
	}
	// 关键:磁盘上的 cert 绝不能被覆盖
	after, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(originalCert) {
		t.Fatal("existing ca.cert.pem was overwritten during failed regeneration")
	}
	// 且不能偷偷把 key 补回来
	if _, statErr := os.Stat(filepath.Join(caDir, "ca.key.pem")); statErr == nil {
		t.Fatal("ca.key.pem was recreated despite half-missing state")
	}
}

func TestEnsureCA_HalfMissingCert_RefusesToRegenerate(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := EnsureCA(dir); err != nil {
		t.Fatalf("first EnsureCA: %v", err)
	}
	caDir := filepath.Join(dir, "ca")
	if err := os.Remove(filepath.Join(caDir, "ca.cert.pem")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureCA(dir); !errors.Is(err, errInconsistentCert) {
		t.Fatalf("err = %v, want errInconsistentCert", err)
	}
}

func TestEnsureCA_EmptyFileTreatedAsCorrupt(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := EnsureCA(dir); err != nil {
		t.Fatalf("first EnsureCA: %v", err)
	}
	caDir := filepath.Join(dir, "ca")
	// key 存在但被截断为 0 字节(裸 WriteFile 中途被杀的确切形态)
	if err := os.WriteFile(filepath.Join(caDir, "ca.key.pem"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureCA(dir); !errors.Is(err, errInconsistentCert) {
		t.Fatalf("empty key should be treated as corrupt, got err=%v", err)
	}
}

func TestEnsureCA_BothMissing_Generates(t *testing.T) {
	dir := t.TempDir()
	cert, key, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	// 生成物必须是可解析的 PEM
	if _, err := parseCert(cert); err != nil {
		t.Fatalf("generated CA cert not parseable: %v", err)
	}
	if _, err := parseKey(key); err != nil {
		t.Fatalf("generated CA key not parseable: %v", err)
	}
}

func TestEnsureCA_Idempotent_NoResign(t *testing.T) {
	dir := t.TempDir()
	c1, _, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	c2, _, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(c1) != string(c2) {
		t.Fatal("EnsureCA re-signed an already-complete CA — clients would break")
	}
	// 权限必须是 0600
	info, err := os.Stat(filepath.Join(dir, "ca", "ca.key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("CA key perm = %o, want 600", info.Mode().Perm())
	}
}

func TestEnsureServerCert_HalfMissing_Refuses(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureServerCert(dir, "vpn.example.com", caCert, caKey, "203.0.113.1"); err != nil {
		t.Fatalf("first EnsureServerCert: %v", err)
	}
	srvDir := filepath.Join(dir, "server")
	if err := os.Remove(filepath.Join(srvDir, "server.key.pem")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureServerCert(dir, "vpn.example.com", caCert, caKey, "203.0.113.1"); !errors.Is(err, errInconsistentCert) {
		t.Fatalf("err = %v, want errInconsistentCert", err)
	}
}

func TestEnsurePanelCert_HalfMissing_Refuses(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsurePanelCert(dir, "vpn.example.com", caCert, caKey); err != nil {
		t.Fatalf("first EnsurePanelCert: %v", err)
	}
	pDir := filepath.Join(dir, "panel-tls")
	if err := os.Remove(filepath.Join(pDir, "cert.pem")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsurePanelCert(dir, "vpn.example.com", caCert, caKey); !errors.Is(err, errInconsistentCert) {
		t.Fatalf("err = %v, want errInconsistentCert", err)
	}
}

// 写出的证书必须自带 SAN（回归:writePair 改造不能破坏内容正确性）。
func TestEnsureServerCert_WritesUsablePEMWithSAN(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := EnsureServerCert(dir, "vpn.example.com", caCert, caKey, "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM block")
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if crt.Subject.CommonName != "vpn.example.com" {
		t.Errorf("CN = %q", crt.Subject.CommonName)
	}
	if err := crt.VerifyHostname("vpn.example.com"); err != nil {
		t.Errorf("SAN missing domain: %v", err)
	}
	if err := crt.VerifyHostname("203.0.113.1"); err != nil {
		t.Errorf("SAN missing IP literal: %v", err)
	}
}

// 错误信息必须可操作（提到"重签会断连"和处置方式），否则运维仍会误操作。
func TestErrInconsistentCert_MessageIsActionable(t *testing.T) {
	msg := errInconsistentCert.Error()
	for _, kw := range []string{"重签", "断连"} {
		if !strings.Contains(msg, kw) {
			t.Errorf("error message missing %q: %s", kw, msg)
		}
	}
}
