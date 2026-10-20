// swanctl 子包：监控 IPv6 公网地址变化，动态更新 swanctl.conf 的 local_addrs。
//
// 背景（M9 v2-72 设计）：
//   - ISP 家用墙给用户下发 /64 前缀，重拨或 SLAAC 变化时主机 IPv6 会变
//   - swanctl.conf 的 `local_addrs = <v6>` 是写死的，prefix 变后客户端找不到 VPN
//   - entrypoint 启动时探测一次，运行时 prefix 变不会更新
//
// 实现：
//   - 定期（默认 60 秒）读 /proc/net/if_inet6 或 netlink 拿指定接口的 global v6
//   - 与 swanctl.conf 里的 local_addrs 比对，变了就 sed 替换 + swanctl --load-all
//
// 副作用：
//   - swanctl --load-all 会中断活跃 SA ~500ms（charon 重载的固有行为）
//   - 单用户场景可接受；本项目只有一条连接，按 conn 细粒度重载无收益
//     （见 loader.go 的 "P1-8 已撤销"）。
package swanctl

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/fsutil"
)

// IPv6WatchConfig IPv6 watch-dog 配置。
type IPv6WatchConfig struct {
	// ConfPath 要监控的 swanctl.conf 完整路径（容器内默认 /etc/swanctl/swanctl.conf）
	ConfPath string
	// Iface 要监控的接口名（容器内 host 网络下 = 宿主接口，如 ens18）
	Iface string
	// Period 探测间隔（默认 60 秒）
	Period time.Duration
	// Logger
	Logger *slog.Logger
	// v2.86-PR16:可选 Reloader。nil 时 fallback 到 fork-exec `swanctl --load-all`;
	// 非 nil 时走 Manager.ReloadAll(复用 reloadMu,避免和 web handler 并发读到半截 conf.d — H2)。
	Reloader interface {
		ReloadAll(ctx context.Context) error
	}
}

// RunIPv6Watch 启动 IPv6 watch-dog，阻塞到 ctx.Done。
//
// 行为：
//  1. 立即跑一次（启动时若 swanctl.conf 里的 v6 跟实际不符也立刻修正）
//  2. 每 Period 跑一次
//  3. 检测到变化：sed 替换 swanctl.conf + swanctl --load-all
func RunIPv6Watch(ctx context.Context, cfg IPv6WatchConfig) error {
	if cfg.ConfPath == "" {
		cfg.ConfPath = "/etc/swanctl/swanctl.conf"
	}
	if cfg.Period <= 0 {
		cfg.Period = 60 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Iface == "" {
		// 默认探测所有接口的 global v6（取第一个）
	}

	ticker := time.NewTicker(cfg.Period)
	defer ticker.Stop()

	// 立即跑一次
	if err := checkAndUpdateIPv6(cfg); err != nil {
		cfg.Logger.Warn("ipv6watch initial check failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := checkAndUpdateIPv6(cfg); err != nil {
				cfg.Logger.Warn("ipv6watch check failed", "err", err)
			}
		}
	}
}

// localAddrsRe 匹配 swanctl.conf 里 `local_addrs = <addr>` 的 v6 地址。
var localAddrsRe = regexp.MustCompile(`(?m)^\s*local_addrs\s*=\s*([0-9a-fA-F:]+)\s*$`)

// checkAndUpdateIPv6 读接口的 global v6，跟 swanctl.conf 的 local_addrs 比对，
// 不一致就替换 + reload。
func checkAndUpdateIPv6(cfg IPv6WatchConfig) error {
	// 1. 读 swanctl.conf 当前 local_addrs
	confBytes, err := os.ReadFile(cfg.ConfPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", cfg.ConfPath, err)
	}
	confStr := string(confBytes)
	match := localAddrsRe.FindStringSubmatch(confStr)
	if len(match) < 2 {
		return fmt.Errorf("no local_addrs line found in %s", cfg.ConfPath)
	}
	oldV6 := strings.TrimSpace(match[1])

	// 2. 探测当前接口的 global IPv6
	newV6, err := detectGlobalV6(cfg.Iface)
	if err != nil {
		return fmt.Errorf("detect global v6 on %q: %w", cfg.Iface, err)
	}
	if newV6 == "" {
		// 没 IPv6（IPv4-only 场景）不动
		return nil
	}
	if newV6 == oldV6 {
		// 没变
		return nil
	}

	cfg.Logger.Info("ipv6watch: IPv6 address changed, updating swanctl.conf",
		"old", oldV6, "new", newV6)

	// 3. sed 替换 + 原子写
	// v2.86-PR15 P0 修复:之前用 os.WriteFile 直接覆盖,容器 OOM kill / 重启
	// 发生在 write 中途 → swanctl.conf 半截 → charon 拒启动 → VPN 全断。
	// 走 fsutil.AtomicWriteFile(write tmp + rename),保证观察者永远看到
	// 完整文件(要么旧要么新,不会半截)。
	newConfStr := localAddrsRe.ReplaceAllString(confStr, fmt.Sprintf("local_addrs = %s", newV6))
	if err := fsutil.AtomicWriteFile(cfg.ConfPath, []byte(newConfStr), 0o644); err != nil {
		return fmt.Errorf("atomic write %s: %w", cfg.ConfPath, err)
	}

	// 4. reload。优先用 Manager.ReloadAll(复用 reloadMu);nil 时 fallback 到 fork-exec `swanctl --load-all`。
	if cfg.Reloader != nil {
		if err := cfg.Reloader.ReloadAll(context.Background()); err != nil {
			return fmt.Errorf("Manager.ReloadAll failed: %w", err)
		}
		cfg.Logger.Info("ipv6watch: swanctl reload complete (via Manager.ReloadAll)", "new_v6", newV6)
		return nil
	}
	cmd := exec.CommandContext(context.Background(), SwanctlBin, "--load-all", "--noprompt")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("swanctl --load-all failed: %w (out=%s)", err, string(out))
	}
	cfg.Logger.Info("ipv6watch: swanctl reload complete", "new_v6", newV6)
	return nil
}

// DetectGlobalV6 探测指定接口（空 = 任意接口）的 global IPv6 地址（2000::/3）。
//
// 导出包装(给 ddns 包复用,避免循环 import)。内部实现见 detectGlobalV6。
func DetectGlobalV6(iface string) (string, error) {
	return detectGlobalV6(iface)
}

// detectGlobalV6 探测指定接口（空 = 任意接口）的 global IPv6 地址（2000::/3）。
// 实现：扫 /proc/net/if_inet6，简单稳定，不依赖 netlink。
//
// /proc/net/if_inet6 格式（每行 6 个字段，空格分隔）：
//
//	addr(32hex) ifindex(hex) prefixlen(hex) scope(hex) flags(hex) devname
//	例子：2408832e08a45be0be2411fffefb01bb 02 40 00 00 ens18
//
// ⚠️ flags 是**第 5 个**字段（索引 4），第 4 个是 scope。写错索引会让
// DAD 状态过滤完全失效 —— 详见 parseIfInet6Line 里的位定义。
func detectGlobalV6(iface string) (string, error) {
	f, err := os.Open("/proc/net/if_inet6")
	if err != nil {
		return "", fmt.Errorf("open /proc/net/if_inet6: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if ip, ok := parseIfInet6Line(scanner.Text(), iface); ok {
			return ip, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan /proc/net/if_inet6: %w", err)
	}
	return "", nil
}

// /proc/net/if_inet6 第 5 字段（flags）的位定义，抄自内核
// include/uapi/linux/in6.h。我们只关心这三个"不可用"状态。
const (
	ifInet6DadFailed   = 0x08 // DAD 冲突，地址不可用
	ifInet6Deprecated  = 0x10 // 前缀过期，地址已不该再用
	ifInet6Tentative   = 0x40 // DAD 尚未完成，地址还不能用
)

// parseIfInet6Line 解析一行 /proc/net/if_inet6，返回该行的 global unicast v6。
// 第二个返回值为 false 表示"这行不能用"：字段不足 / 网卡不匹配 / flags 不可用
// / 不是 GUA。调用方据此继续扫下一行。
//
// 抽成独立函数是为了能脱离真实网卡做单元测试 —— 状态过滤是这个函数的
// 全部价值所在，而它恰好是最容易写错字段索引的地方。
func parseIfInet6Line(line, wantIface string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 6 {
		return "", false
	}

	if wantIface != "" && fields[5] != wantIface {
		return "", false
	}

	// flags 是十六进制字符串。内核给的是两位零填充（"00"/"80"/"c0"），
	// ParseUint 两种都能吃。
	flagBits, err := strconv.ParseUint(fields[4], 16, 32)
	if err != nil {
		return "", false
	}
	// DAD 冲突 / DAD 未收敛 / 已过期这三种必须排除：把尚未确认可用的地址
	// 交给 charon，症状是客户端连得上却拿不到地址，且日志里没有任何异常。
	//
	// 0x20 TEMPORARY（隐私地址）是稳定态，**不排除**。
	// 0x80 PERMANENT（静态地址）自然通过。
	if flagBits&(ifInet6DadFailed|ifInet6Deprecated|ifInet6Tentative) != 0 {
		return "", false
	}

	ip, err := hexToV6(fields[0])
	if err != nil {
		return "", false
	}
	// 仅 2000::/3；link-local(fe80::/10)、ULA(fc00::/7)、::1 的首 hextet
	// 首位分别是 e/f/空，都不落在 2-3 区间，isGlobalV6 一并排除。
	if !isGlobalV6(ip) {
		return "", false
	}
	return ip, true
}

// hexToV6 把 /proc/net/if_inet6 里 32 个十六进制字符（无冒号）转为标准 :: 分隔形式。
func hexToV6(s string) (string, error) {
	if len(s) != 32 {
		return "", fmt.Errorf("invalid addr hex length: %d", len(s))
	}
	groups := make([]string, 8)
	for i := 0; i < 8; i++ {
		groups[i] = s[i*4 : i*4+4]
	}
	ip := strings.Join(groups, ":")
	// 压缩最长 0 段
	return compressV6(ip), nil
}

// compressV6 简单 IPv6 压缩：找最长全 0 段，加 ::。
// 不处理多段平局情况（取最长的）。
func compressV6(s string) string {
	parts := strings.Split(s, ":")
	// 找最长全 0 段（"0000" 形式）
	maxStart, maxLen := -1, 0
	curStart, curLen := -1, 0
	for i, p := range parts {
		if p == "0000" {
			if curStart < 0 {
				curStart = i
				curLen = 1
			} else {
				curLen++
			}
			if curLen > maxLen {
				maxStart = curStart
				maxLen = curLen
			}
		} else {
			curStart = -1
			curLen = 0
		}
	}
	if maxLen < 2 {
		// 没找到可压缩段，每段去前导 0
		return trimLeadingZerosRFC5952(s)
	}
	// 替换 [maxStart, maxStart+maxLen) 为 "::"
	left := parts[:maxStart]
	right := parts[maxStart+maxLen:]
	leftStr := trimLeadingZerosRFC5952(strings.Join(left, ":"))
	rightStr := trimLeadingZerosRFC5952(strings.Join(right, ":"))
	if leftStr == "" {
		return "::" + rightStr
	}
	if rightStr == "" {
		return leftStr + "::"
	}
	return leftStr + "::" + rightStr
}

// trimLeadingZerosRFC5952 RFC 5952 推荐的 IPv6 文本表示：每段去掉前导 0。
func trimLeadingZerosRFC5952(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(s, ":")
	for i, p := range parts {
		p = strings.TrimLeft(p, "0")
		if p == "" {
			p = "0"
		}
		parts[i] = p
	}
	return strings.Join(parts, ":")
}

// isGlobalV6 判断 IPv6 是否为 global unicast（2000::/3，即 2000:: ~ 3fff::）。
// 不依赖 net.ParseIP 因为我们要处理 /proc 里无压缩形式。
func isGlobalV6(s string) bool {
	if len(s) < 4 {
		return false
	}
	// 首 hextet 的首位是 2 或 3 即落在 2000::/3 内。
	// link-local(fe80::/10) 是 e、ULA(fd00::/8) 是 f、::1 是空 —— 天然不匹配。
	first := s[0]
	return first == '2' || first == '3'
}
