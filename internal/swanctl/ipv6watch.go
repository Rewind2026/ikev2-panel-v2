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
//   - swanctl --load-all 会中断活跃 SA ~500ms（charon 6.0.1 实现限制）
//   - 单用户场景可接受，100+ 用户场景需要按 conn 卸载/重载（VICI 协议层能力不足，
//     留作 future work）。
package swanctl

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
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
//   1. 立即跑一次（启动时若 swanctl.conf 里的 v6 跟实际不符也立刻修正）
//   2. 每 Period 跑一次
//   3. 检测到变化：sed 替换 swanctl.conf + swanctl --load-all
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

	// 3. sed 替换
	newConfStr := localAddrsRe.ReplaceAllString(confStr, fmt.Sprintf("local_addrs = %s", newV6))
	if err := os.WriteFile(cfg.ConfPath, []byte(newConfStr), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", cfg.ConfPath, err)
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
// /proc/net/if_inet6 格式（每行）：
//   addr (32hex) ifindex (dec) prefix(hex) scope(hex) flags(hex) devname
//   例子：2408832E08A510007EB4F9A371 4D8D 80 00 01 0C ens18
func detectGlobalV6(iface string) (string, error) {
	f, err := os.Open("/proc/net/if_inet6")
	if err != nil {
		return "", fmt.Errorf("open /proc/net/if_inet6: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}
		addr32 := fields[0]
		dev := fields[5]
		flags := fields[3]

		// flags: 0x01 = temporary, 0x80 = deprecated. 我们用 primary (0x00 or 0x01)。
		// 但忽略 deprecated (0x10)。
		if flags == "10" {
			continue
		}

		// 接口过滤
		if iface != "" && dev != iface {
			continue
		}

		// addr32 是无冒号 32 字符，需要转为 :: 分隔
		ip, err := hexToV6(addr32)
		if err != nil {
			continue
		}

		// 仅取 global unicast（2000::/3）
		if !isGlobalV6(ip) {
			continue
		}

		// 跳过 link-local (fe80::/10)
		if strings.HasPrefix(ip, "fe80:") || strings.HasPrefix(ip, "fe80::") {
			continue
		}

		// 跳过 loopback
		if ip == "::1" {
			continue
		}

		// 跳过 ULA (fc00::/7)
		if strings.HasPrefix(ip, "fc") || strings.HasPrefix(ip, "fd") {
			continue
		}

		return ip, nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan /proc/net/if_inet6: %w", err)
	}
	return "", nil
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

// isGlobalV6 判断 IPv6 是否为 global unicast（2000::/3）。
// 不依赖 net.ParseIP 因为我们要处理 /proc 里无压缩形式。
func isGlobalV6(s string) bool {
	if len(s) < 4 {
		return false
	}
	first := strings.ToLower(s[:4])
	// 2000::/3 -> 高 4 位是 0010，即 0x2[0-f]
	if first[0] == '2' {
		return true
	}
	// 3xxx::/16 是 global unicast 的部分段，但 3F 段是 documentation
	// 我们简化：只检查 2xxx 就足够（实际公网 IPv6 都从 2000::/3 分）
	return false
}