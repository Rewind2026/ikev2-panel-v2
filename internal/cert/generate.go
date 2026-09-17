// 自签证书生成（CA + Server + Panel）。
// 设计见 docs/design.md §1.4
//
// 关键约束（architecture §12）：
//   - 默认 RSA 2048 兼容所有客户端版本（含 iOS 16 / 老 Android / Win7 strongSwan）
//     ECDSA (P-256) server cert 在 iOS 17+/strongSwan 5.7+ 也支持，但暂未启用——
//     切换 ECDSA 会破坏现有 mobileconfig（CN 重新签发 + 用户重装 profile）
//   - 必须包含 SAN（modern 客户端都校验 SAN）
//   - Server cert CN 必须与 mobileconfig RemoteIdentifier 一致
package cert

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// 自签证书有效期：10 年。
// v2 内置证书极少重签（除非改 CN/Server IP）；10 年足够。
const certValidity = 10 * 365 * 24 * time.Hour

// EnsureCA 确保 CA 证书存在（缺失则生成）。
// 返回 CA 证书 + 私钥的 PEM。
func EnsureCA(dataDir string) (certPEM, keyPEM []byte, err error) {
	caDir := filepath.Join(dataDir, "ca")
	certPath := filepath.Join(caDir, "ca.cert.pem")
	keyPath := filepath.Join(caDir, "ca.key.pem")

	if exists(certPath) && exists(keyPath) {
		certPEM, err = os.ReadFile(certPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read ca cert: %w", err)
		}
		keyPEM, err = os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read ca key: %w", err)
		}
		return certPEM, keyPEM, nil
	}

	if err := os.MkdirAll(caDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir ca: %w", err)
	}

	caCert, caKey, err := generateCA()
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA: %w", err)
	}
	if err := os.WriteFile(certPath, caCert, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write ca cert: %w", err)
	}
	if err := os.WriteFile(keyPath, caKey, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write ca key: %w", err)
	}
	return caCert, caKey, nil
}

// EnsureServerCert 确保服务器证书（strongSwan 用）存在。
// CN 是用户配置的 IKEV2_SERVER_CN。
func EnsureServerCert(dataDir, cn string, caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte, err error) {
	srvDir := filepath.Join(dataDir, "server")
	certPath := filepath.Join(srvDir, "server.cert.pem")
	keyPath := filepath.Join(srvDir, "server.key.pem")

	if exists(certPath) && exists(keyPath) {
		certPEM, err = os.ReadFile(certPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read server cert: %w", err)
		}
		keyPEM, err = os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read server key: %w", err)
		}
		return certPEM, keyPEM, nil
	}

	if err := os.MkdirAll(srvDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir server: %w", err)
	}

	certPEM, keyPEM, err = generateServerCert(cn, caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write server cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write server key: %w", err)
	}
	return certPEM, keyPEM, nil
}

// EnsurePanelCert 确保面板 HTTPS 证书存在。
// 用同一个 CN（证书复用），但用途不同。
func EnsurePanelCert(dataDir, cn string, caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte, err error) {
	pDir := filepath.Join(dataDir, "panel-tls")
	certPath := filepath.Join(pDir, "cert.pem")
	keyPath := filepath.Join(pDir, "key.pem")

	if exists(certPath) && exists(keyPath) {
		certPEM, err = os.ReadFile(certPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read panel cert: %w", err)
		}
		keyPEM, err = os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read panel key: %w", err)
		}
		return certPEM, keyPEM, nil
	}

	if err := os.MkdirAll(pDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir panel-tls: %w", err)
	}

	certPEM, keyPEM, err = generateServerCert(cn, caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write panel cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write panel key: %w", err)
	}
	return certPEM, keyPEM, nil
}

// generateCA 生成自签 CA。
func generateCA() (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("rsa.GenerateKey: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("rand serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"IKEv2 Panel"},
			CommonName:   "IKEv2 Panel Root CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(certValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("x509.CreateCertificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return certPEM, keyPEM, nil
}

// generateServerCert 用 CA 签发服务器证书。
// CN 是用户配置的 IKEV2_SERVER_CN；SANs 自动从 CN 提取。
func generateServerCert(cn string, caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte, err error) {
	caCert, err := parseCert(caCertPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}
	caKey, err := parseKey(caKeyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("rsa.GenerateKey: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("rand serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"IKEv2 Panel"},
			CommonName:   cn,
		},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().Add(certValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		// SAN 必须有：modern 客户端（iOS、strongSwan app）都校验 SAN
		DNSNames: []string{cn},
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
		},
	}

	// 如果 cn 长得像 IP，加进 IPAddresses
	if ip := net.ParseIP(cn); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("x509.CreateCertificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return certPEM, keyPEM, nil
}

func parseCert(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}