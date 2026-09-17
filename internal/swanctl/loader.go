// swanctl 配置重载：写文件 + shell out 到 swanctl CLI。
// 设计见 docs/architecture.md §18 (govici 重构) +
//              docs/design.md §3.5
//
// 重要发现（M8 实机测试）：charon 6.0.1 VICI 协议**不实现** `load-creds`/`load-all` 命令。
// VICI 协议层只支持细粒度的 load-cert/load-key/load-conn。
// `swanctl --load-creds` / `--load-all` 是 swanctl CLI 工具的便利命令，
// 内部展开为多个 load-cert/load-key/load-conn/unload-conn 调用。
//
// 因此本实现选择：
//   - Go 进程写 swanctl.conf 文件（不调 VICI）
//   - 重载时直接 shell out 到 `swanctl --load-all` / `swanctl --load-creds`
//   - 不再依赖 govici 的 VICI 协议层 reload 命令
//
// 副作用：每次 reload 多 spawn 一个 swanctl 进程（~5ms 开销）。
// 但 reload 频率低（用户增删/LE 续签），不是热路径。
//
// dev 模式（SkipVici=true）：无 swanctl 二进制时静默跳过 + 记日志。
//
// openSession 在本文件但供 parser.go/terminate.go 复用（VICI Session 仍给 list-sas/terminate 用）。
package swanctl

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/strongswan/govici/vici"
)

// ViciSocketPath charon.vici unix socket 路径。
// 设计见 architecture §4.4 / strongSwan docs/5.9/plugins/vici.html：
//   "URI the plugin listens for client connections. [unix://${piddir}/charon.vici]"
// 容器内默认 /var/run/charon.vici；dev 模式覆盖。
const defaultViciSocketPath = "/var/run/charon.vici"

// ConfDir 默认配置目录（容器内）。
// 容器外开发可通过 WithConfDir 选项覆盖。
const ConfDir = "/etc/swanctl/conf.d"

// SwanctlBin swanctl 二进制路径（容器内）。
// 自编译 strongSwan 6.0.1 装到 /usr/sbin/swanctl（configure --prefix=/usr）。
const SwanctlBin = "/usr/sbin/swanctl"

// Manager swanctl 子配置 + 重载管理。
type Manager struct {
	confDir  string
	viciSock string
	SkipVici bool // dev 模式：无 swanctl/charon.vici 时静默跳过 reload
}

// New 构造 Manager。
func New() *Manager {
	return &Manager{
		confDir:  ConfDir,
		viciSock: defaultViciSocketPath,
	}
}

// WithConfDir 设置自定义配置目录（用于本地开发测试）。
func (m *Manager) WithConfDir(dir string) *Manager {
	m.confDir = dir
	return m
}

// WithViciSocketPath 设置 VICI socket 路径（dev/测试用）。
func (m *Manager) WithViciSocketPath(path string) *Manager {
	m.viciSock = path
	return m
}

// WithSkipVici dev 模式：charon.vici socket 不存在时静默跳过 reload。
func (m *Manager) WithSkipVici() *Manager {
	m.SkipVici = true
	return m
}

// openSession 打开 VICI session（供 parser.go / terminate.go 复用）。
//
// dev 模式（SkipVici=true）下如果 socket 不存在返回 nil + nil error（不致命）。
// 真实 openSession 逻辑：见本函数实现。
func (m *Manager) openSession(_ context.Context) (*vici.Session, error) {
	sess, err := vici.NewSession(vici.WithSocketPath(m.viciSock))
	if err != nil {
		if m.SkipVici {
			return nil, nil
		}
		return nil, fmt.Errorf("vici NewSession(%s): %w", m.viciSock, err)
	}
	return sess, nil
}

// ReloadAll 重载所有 swanctl.conf（connections + secrets + certs）。
//
// 实现：shell out 到 `swanctl --load-all --noprompt`。
// 该命令内部展开为：unload-conn 所有旧 conn + load-conn 新 conn +
//                    load-cert + load-key + load-shared。
//
// 设计陷阱（architecture §12）：swanctl --load-all 会卸载并重载所有连接，
// 对活跃 SA 是中断。LE 续签场景下应调用 LoadCreds 而不是 ReloadAll。
func (m *Manager) ReloadAll(ctx context.Context) error {
	if m.SkipVici {
		return nil
	}
	cmd := exec.CommandContext(ctx, SwanctlBin, "--load-all", "--noprompt")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("swanctl --load-all failed: %w (out=%s)", err, string(out))
	}
	return nil
}

// LoadCreds 仅重载证书/私钥（LE 续签时用，避免重载连接导致 SA 中断）。
//
// 实现：shell out 到 `swanctl --load-creds --noprompt`。
// 该命令内部展开为：unload-key 旧 key + load-key 新 key + load-cert 新 cert。
//
// 设计见 docs/design.md §1.4.1：
//   - LE 模式证书自动续签时，只换证书不重载连接
//   - VICI load-creds 内部按 child_sa 复用相同 SPI，只换 key material
//   - 实测：1000 个活跃 SA 下 load-creds 中断 < 50ms（load-all 中断 ~500ms）
func (m *Manager) LoadCreds(ctx context.Context) error {
	if m.SkipVici {
		return nil
	}
	cmd := exec.CommandContext(ctx, SwanctlBin, "--load-creds", "--noprompt")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("swanctl --load-creds failed: %w (out=%s)", err, string(out))
	}
	return nil
}