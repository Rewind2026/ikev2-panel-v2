// Let's Encrypt 集成（acme.sh）。
// 设计见 docs/design.md §1.4.1
//
// 工具选型：acme.sh（纯 shell + curl，容器内轻量；certbot 需要 Python 太重）
// 流程：
//   - 首次启动：entrypoint.sh 检测 LE 模式 → 调 acme.sh --issue + --install-cert
//   - 续签：容器内 cron 每日跑 renew-cert.sh（backup + renew + rollback）
//   - 健康检查：Go 进程 60s 检查 LAST_RENEW_FAILED 文件
//
// 本文件提供 Go 端的 LE 状态读取（健康检查用），不实现 ACME 协议本身。
package cert

import (
	"os"
	"path/filepath"
	"time"

	"crypto/x509"
	"encoding/pem"
)

// LERenewStatus 续签状态。
type LERenewStatus struct {
	LastRenewFailed bool      // 是否最近一次续签失败
	LastChecked     time.Time // 检查时间
	CertExpires     time.Time // 当前证书过期时间
	DaysLeft        int        // 距过期天数
}

// CheckLERenewStatus 读取 LE 模式续签状态。
//
//   - LAST_RENEW_FAILED 文件：续签脚本失败时会 touch 此文件
//   - 当前证书：/etc/swanctl/x509/$DOMAIN.pem
//
// 设计见 design §1.4.1 + architecture §3.10
func CheckLERenewStatus(certPath string) LERenewStatus {
	st := LERenewStatus{LastChecked: time.Now()}

	// LAST_RENEW_FAILED 标志：renew-cert.sh 失败时会 touch 此文件
	// 路径在 /data/le/LAST_RENEW_FAILED（持久化卷，容器重建后仍可读到）
	failFlag := filepath.Join(filepath.Dir(certPath), "LAST_RENEW_FAILED")
	if _, err := os.Stat(failFlag); err == nil {
		st.LastRenewFailed = true
	}

	// 证书有效期
	if certPath == "" {
		return st
	}
	data, err := os.ReadFile(certPath)
	if err != nil {
		return st
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return st
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return st
	}
	st.CertExpires = cert.NotAfter
	st.DaysLeft = int(time.Until(cert.NotAfter).Hours() / 24)
	return st
}

// CertModeFromEnv 简化：调用方传 certMode。
// LE 模式下 health 横幅：根据状态显示不同告警。
func ShouldWarn(daysLeft int) bool {
	return daysLeft > 0 && daysLeft < 14
}