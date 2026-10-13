// 自签证书生成（CA + Server + Panel）。
// 见 docs/architecture.md §4.6
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
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 自签证书有效期：10 年。
// v2 内置证书极少重签（除非改 CN/Server IP）；10 年足够。
const certValidity = 10 * 365 * 24 * time.Hour

// errInconsistentCert 表示证书与私钥只存在其一（另一半丢失/被截断/被删）。
//
// 为什么必须报错而不是补齐另一半：CA 一旦重签，新 CA 与已装进客户端 profile 的
// 旧 CA 之间不再有信任关系，所有用户的 VPN 会同时断连，而面板这边看不出任何异常。
// 静默"修好"比报错危险得多，所以这类状态一律 FATAL，由运维显式处置。
var errInconsistentCert = errors.New(
	"证书与私钥只存在其一（半缺失）。重签会断连：所有已装 profile 的客户端内联的旧 CA " +
		"与新 CA 无信任关系，VPN 会集体断线且面板无提示。请人工确认保留哪一份，" +
		"把另一半恢复后再启动；确认要废弃旧 CA 时，先通知所有用户重新安装 profile，再删除这两个文件。")

// loadPair 读取一对证书/私钥文件。三种状态：
//
//   - 两个都不在 → 返回 (nil, nil, false)，调用方该走首次生成。
//   - 两个都在且非空 → 返回内容，调用方直接用。
//   - 半缺失 / 0 字节 → 返回 errInconsistentCert，**禁止**补齐或重签。
//
// 注意 0 字节算损坏：裸 WriteFile 写到一半被杀就是这个样子，
// 此时 exists() 为 true 但内容是空的，照常使用会拿空 PEM 去签发。
func loadPair(certPath, keyPath, what, certName, keyName string) (certPEM, keyPEM []byte, ok bool, err error) {
	certExists := exists(certPath)
	keyExists := exists(keyPath)

	switch {
	case !certExists && !keyExists:
		return nil, nil, false, nil

	case certExists != keyExists:
		present, missing := certName, keyName
		if keyExists {
			present, missing = keyName, certName
		}
		return nil, nil, false, fmt.Errorf("%s: %s 在但 %s 不在: %w", what, present, missing, errInconsistentCert)

	default:
		certPEM, err = os.ReadFile(certPath)
		if err != nil {
			return nil, nil, false, fmt.Errorf("read cert %s: %w", certPath, err)
		}
		keyPEM, err = os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, false, fmt.Errorf("read key %s: %w", keyPath, err)
		}
		if len(certPEM) == 0 || len(keyPEM) == 0 {
			return nil, nil, false, fmt.Errorf("%s: 文件存在但为 0 字节（写入被中断）: %w", what, errInconsistentCert)
		}
		return certPEM, keyPEM, true, nil
	}
}

// EnsureCA 确保 CA 证书存在（缺失则生成）。
// 返回 CA 证书 + 私钥的 PEM。
func EnsureCA(dataDir string) (certPEM, keyPEM []byte, err error) {
	caDir := filepath.Join(dataDir, "ca")
	certPath := filepath.Join(caDir, "ca.cert.pem")
	keyPath := filepath.Join(caDir, "ca.key.pem")

	certPEM, keyPEM, ok, err := loadPair(certPath, keyPath, "CA", "ca.cert.pem", "ca.key.pem")
	if err != nil {
		return nil, nil, err
	}
	if ok {
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
//
// v2.86-PR12.13:加 serverIPs 参数,把额外 IP 字面量（IPv4 公网 / IPv6 公网）加进
// IPAddresses SAN。这是 mobileconfig RemoteAddress 用 IP 字面量时 iOS 校验必需。
// 已有证书不重签,只在首次生成时使用。
func EnsureServerCert(dataDir, cn string, caCertPEM, caKeyPEM []byte, serverIPs ...string) (certPEM, keyPEM []byte, err error) {
	srvDir := filepath.Join(dataDir, "server")
	certPath := filepath.Join(srvDir, "server.cert.pem")
	keyPath := filepath.Join(srvDir, "server.key.pem")

	certPEM, keyPEM, ok, err := loadPair(certPath, keyPath, "server", "server.cert.pem", "server.key.pem")
	if err != nil {
		return nil, nil, err
	}
	if ok {
		return certPEM, keyPEM, nil
	}

	if err := os.MkdirAll(srvDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir server: %w", err)
	}

	certPEM, keyPEM, err = generateServerCert(cn, caCertPEM, caKeyPEM, serverIPs...)
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
func EnsurePanelCert(dataDir, cn string, caCertPEM, caKeyPEM []byte, serverIPs ...string) (certPEM, keyPEM []byte, err error) {
	pDir := filepath.Join(dataDir, "panel-tls")
	certPath := filepath.Join(pDir, "cert.pem")
	keyPath := filepath.Join(pDir, "key.pem")

	certPEM, keyPEM, ok, err := loadPair(certPath, keyPath, "panel-tls", "cert.pem", "key.pem")
	if err != nil {
		return nil, nil, err
	}
	if ok {
		return certPEM, keyPEM, nil
	}

	if err := os.MkdirAll(pDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir panel-tls: %w", err)
	}

	certPEM, keyPEM, err = generateServerCert(cn, caCertPEM, caKeyPEM, serverIPs...)
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
// CN 是用户配置的 IKEV2_SERVER_CN；SANs 自动从 CN + extraIPs 提取。
//
// v2.86-PR12.13:加 extraIPs 可变参数,把额外 IP 字面量加进 IPAddresses SAN。
// 调用方传 ServerAddrV6 / ServerAddrV4,在 mobileconfig RemoteAddress 用 IP 字面量时
// iOS 能正确校验 IPAddresses SAN 而不是 dNSName SAN。
func generateServerCert(cn string, caCertPEM, caKeyPEM []byte, extraIPs ...string) (certPEM, keyPEM []byte, err error) {
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

	// EKU:仅 ServerAuth。
	// v2.86-PR19(P2-18):原写 ServerAuth + ClientAuth 是多余授权 —— IKEv2 服务端
	// 身份从不被当作客户端认证用,留着反而扩大证书可用范围。strongSwan 官方
	// 文档也只要求 serverAuth(https://docs.strongswan.org/docs/latest/interop/windowsCertRequirements.html)。
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"IKEv2 Panel"},
			CommonName:   cn,
		},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().Add(certValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// SAN 必须有:modern 客户端(iOS、strongSwan app)都校验 SAN。
		// v2.86-PR19(P2-18):SAN 必须按 CN 类型二选一 —— dNSName 只能是 DNS 名
		// (RFC 5280),iPAddress 才是 IP 字面量;CN 是 IP 时写进 DNSNames 是协议
		// 语义错误,严格 TLS 栈会判证书畸形。
	}

	if ip := net.ParseIP(cn); ip != nil {
		// CN 是 IP 字面量:仅写 IPAddresses,绝不写 DNSNames。
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		// CN 是域名:仅写 DNSNames,不附带 127.0.0.1(面板从不对 localhost 提供服务)。
		tmpl.DNSNames = []string{cn}
	}

	// v2.86-PR12.13:把额外 IP 字面量(IPv4 公网 / IPv6 公网)加进 IPAddresses SAN,
	// 让 mobileconfig RemoteAddress 用 IP 字面量时 iOS 能校验通过。
	// 注意:CN 是 IP 字面量时,上一步已用 [ip] 初始化 IPAddresses;extraIPs 用 append 续上。
	for _, s := range extraIPs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
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
