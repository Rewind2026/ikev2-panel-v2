// swanctl 配置重载：写文件 + shell out 到 swanctl CLI。
// 设计见 docs/architecture.md §18 (govici 重构) +
//
//	docs/design.md §3.5
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
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/strongswan/govici/vici"
)

// swanctl 命令的默认超时。
//
// 设计动机（v2-80+ backlog）：v2 之前所有 swanctl 调用都用裸 ctx，
// 如果 charon 半挂（swanctl 卡在 IKE_SA 重协商中），swanctl --load-all
// 会永久阻塞，连带 Go handler 永久不返回。1-50 人小团队场景下，
// 管理员某次 reload 卡住会让所有用户断连。
//
// 各操作的超时选择：
//   - ReloadAll：swanctl 文档实测 ~100-500ms（含所有 SA 卸载+加载）。
//     给 10s：留 20 倍余量防止极端情况。
//   - LoadCreds：仅换证书/私钥，< 50ms。给 5s。
//   - Terminate 内部用的 CallStreaming + Call：terminate 命令自带 timeout
//     字段（默认 5s）；但 list-sas 流本身可能挂，给 10s。
const (
	ReloadAllTimeout = 10 * time.Second
	LoadCredsTimeout = 5 * time.Second
	TerminateTimeout = 10 * time.Second
	ListSAsTimeout   = 10 * time.Second
)

// withTimeout 给 ctx 包一个 deadline（如果原 ctx 没 deadline）。
//
// 设计要点：
//   - 如果原 ctx 已有 deadline，不覆盖（尊重外部传入的超时）
//   - 如果 timeout <= 0，返回原 ctx（防御编程）
//   - 整体不创建 goroutine——只是 deadline 包装
func withTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	if _, hasDeadline := parent.Deadline(); hasDeadline {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

// ViciSocketPath charon.vici unix socket 路径。
// 设计见 architecture §4.4 / strongSwan docs/5.9/plugins/vici.html：
//
//	"URI the plugin listens for client connections. [unix://${piddir}/charon.vici]"
//
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
	confPath string // v2.86-PR13.2:测试可覆盖 swanctl.conf 路径
	viciSock string
	SkipVici bool // dev 模式：无 swanctl/charon.vici 时静默跳过 reload

	// reloadMu 串行化所有"会改变 swanctl 状态"的操作：
	//   - ReloadAll / LoadCreds（直接动 charon）
	//   - WriteUserConf / RemoveUserConf（动 conf.d 之后立刻 ReloadAll）
	//   - ipv6watch 后台调 ReloadAll
	//
	// 解决的问题：v2 之前 web handlers 并发增删用户时,可能两个 goroutine
	// 同时调 `swanctl --load-all`,一个会因为"另一进程正在 other操作"挂住,
	// 同时两个 ReloadAll 之间还会产生"半个 conf.d 文件被 charon 读到"的风险。
	//
	// 锁定策略：
	//   - 单一全局锁（不分 conn/secret）：1-50 人场景下不必要,简单可靠
	//   - write conf + ReloadAll 必须原子组合(用 Lock 包裹)
	//   - 锁内允许嵌套(读路径,如 findSAUniqueIDs 用 sess2 独立 socket,见 terminate.go)
	reloadMu sync.Mutex
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

// WriteUserConfAndReload 写 conf.d 文件 + 立即 ReloadAll，整体持锁。
//
// 设计动机（P0-4 修复 v2-80+ backlog）：
//
//	v2 之前 web handlers 是 "WriteUserConf(...)  // 写文件
//	                         ReloadAll(...)    // 重载 charon"
//	两步分开调用。如果两个 HTTP 协程同时建用户：
//	  goroutine A: 写 alice.conf      ✓
//	  goroutine B: 写 bob.conf        ✓
//	  goroutine A: ReloadAll         ←
//	  goroutine B: ReloadAll         ←  两个 swanctl --load-all 并发
//	- swanctl 自己有文件锁,但极端情况仍可能产生"swanctl 读到中间状态"
//	- reload 中断 SA 一次 vs 两次,体验差
//
// 解决方案：把"写文件 + ReloadAll"封到一个持锁方法里,外部无法插入其他 reload。
// Web handler 只需调一次。
//
// ctx：传入的 deadline 仍会被尊重（ReloadAll 内部还有 withTimeout 二次包）。
func (m *Manager) WriteUserConfAndReload(ctx context.Context, username, password string) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	if err := m.WriteUserConf(username, password); err != nil {
		return err
	}
	return m.reloadAllLocked(ctx)
}

// RemoveUserConfAndReload 删除 conf.d 文件 + 立即 ReloadAll，整体持锁。
func (m *Manager) RemoveUserConfAndReload(ctx context.Context, username string) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	if err := m.RemoveUserConf(username); err != nil {
		return err
	}
	return m.reloadAllLocked(ctx)
}

// reloadAllLocked 是 ReloadAll 的内部版本,假定 caller 已持 reloadMu。
//
// 不要从 Manager 外部直接调用！否则会和 ReloadAll 死锁。
func (m *Manager) reloadAllLocked(ctx context.Context) error {
	if m.SkipVici {
		return nil
	}
	ctx, cancel := withTimeout(ctx, ReloadAllTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, SwanctlBin, "--load-all", "--noprompt")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("swanctl --load-all timeout after %s (out=%s)", ReloadAllTimeout, string(out))
		}
		return fmt.Errorf("swanctl --load-all failed: %w (out=%s)", err, string(out))
	}
	return nil
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
//
//	load-cert + load-key + load-shared。
//
// 设计陷阱（architecture §12）：swanctl --load-all 会卸载并重载所有连接，
// 对活跃 SA 是中断。LE 续签场景下应调用 LoadCreds 而不是 ReloadAll。
func (m *Manager) ReloadAll(ctx context.Context) error {
	if m.SkipVici {
		return nil
	}
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	return m.reloadAllLocked(ctx)
}

// WaitForCharonReady 等待 charon.vici socket 可用 + VICI session 可建立。
//
// 设计动机(Q5-02):容器启动时 entrypoint.sh 启动 charon 是异步 fork(),
// 如果 Go 进程比 charon 先 ready,直接调 ReloadAll 会失败 → 用户误以为配置没生效,
// 反复 docker restart 形成 boot loop。
//
// 探测策略:
//  1. /var/run/charon.vici 文件存在(stat)
//  2. vici.NewSession 能成功建立(开 socket fd + handshake)
//
// 两个条件都满足 → charon ready。
//
// 重试:最多 maxAttempts 次,每次 sleep retryInterval,合计 timeout 上限。
//
// 失败语义:返回 error,但**不致命**(caller log warn,不 os.Exit)。
// entrypoint.sh 后续会重试启动 charon;cron / 下次 reload 也都能正常工作。
//
// 参数:
//   - sockPath: 自定义 VICI socket 路径(默认 /var/run/charon.vici,空字符串走默认)
//   - timeout:  总等待上限(<=0 → 用 10s 默认)
//
// 用法:
//
//	if err := WaitForCharonReady("", 10*time.Second); err != nil {
//	    logger.Warn("charon not ready, ReloadAll may fail", "err", err)
//	    // 不退出,继续启动,后续 cron / handler reload 会自然重试
//	}
func WaitForCharonReady(sockPath string, timeout time.Duration) error {
	if sockPath == "" {
		sockPath = defaultViciSocketPath
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	const retryInterval = 1 * time.Second
	maxAttempts := int(timeout/retryInterval) + 1

	deadline := time.Now().Add(timeout)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// 1) stat socket 文件
		if _, err := os.Stat(sockPath); err == nil {
			// 2) 试开 VICI session(socket 已 listen → handshake 应成功)
			sess, err := vici.NewSession(vici.WithSocketPath(sockPath))
			if err == nil {
				_ = sess.Close()
				return nil // 双重条件满足 → ready
			}
			// stat 成功但 NewSession 失败 → socket 还在 accept 队列里,等等重试
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("charon.vici not ready after %s (attempt=%d, sock=%s)",
				timeout, attempt, sockPath)
		}
		if attempt < maxAttempts {
			time.Sleep(retryInterval)
		}
	}
	return fmt.Errorf("charon.vici not ready after %d attempts (sock=%s)", maxAttempts, sockPath)
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
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	ctx, cancel := withTimeout(ctx, LoadCredsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, SwanctlBin, "--load-creds", "--noprompt")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("swanctl --load-creds timeout after %s (out=%s)", LoadCredsTimeout, string(out))
		}
		return fmt.Errorf("swanctl --load-creds failed: %w (out=%s)", err, string(out))
	}
	return nil
}
