// 限速文件写入器（Go 端管理文件，updown 脚本读取）。
// 设计见 docs/design.md §9.2.1 + architecture §3.8
//
// v2.85-PR7:新增 BuildTcCommands 纯函数,根据 IP 协议族生成 tc 命令序列。
// v2 默认 IKEV2_IPV6_ONLY=true,IPv6 用户之前因 tc u32 只识别 IPv4 而被静默绕过限速,
// 现在用 `tc flower` (Linux 4.1+) 补 IPv6 限速路径。
package limit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LimitDir 默认限速目录（updown 脚本读取位置）。
// 容器内：/var/lib/ikev2-panel/limits/
// 本地 dev：dataDir/limits/
const LimitDir = "/var/lib/ikev2-panel/limits"

// Limiter 管理限速文件。
type Limiter struct {
	dir string
}

// New 构造 Limiter。
func New() *Limiter {
	return &Limiter{dir: LimitDir}
}

// WithDir 自定义目录（用于本地开发测试）。
func (l *Limiter) WithDir(dir string) *Limiter {
	l.dir = dir
	return l
}

// WriteLimitFile 写一个用户的限速文件（内容是数字 Mbps，0 = 不限速）。
func (l *Limiter) WriteLimitFile(username string, mbps int) error {
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", l.dir, err)
	}
	path := filepath.Join(l.dir, username)
	return os.WriteFile(path, []byte(fmt.Sprintf("%d", mbps)), 0o644)
}

// RemoveLimitFile 删除限速文件。文件不存在视为成功。
func (l *Limiter) RemoveLimitFile(username string) error {
	path := filepath.Join(l.dir, username)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// ---- v2.85-PR7:tc 命令生成（IPv6 限速支持）----

// Family 表示 VPN 隧道承载的 IP 协议族。
//   - FamilyIPv4:仅 IPv4 client/filter
//   - FamilyIPv6:仅 IPv6 client/filter
//   - FamilyDual :v4 + v6 双栈,共享同一个 class ID
type Family string

const (
	FamilyIPv4 Family = "ipv4"
	FamilyIPv6 Family = "ipv6"
	FamilyDual Family = "dual"
)

// Valid 报告 family 是否为已知值。
func (f Family) Valid() bool {
	switch f {
	case FamilyIPv4, FamilyIPv6, FamilyDual:
		return true
	}
	return false
}

// TcCommand 是单条 tc 子命令 + 参数。
//  Op   = "qdisc" | "class" | "filter"
//  Args = 子命令后的参数列表（不含 "tc" 和 Op 本身）
type TcCommand struct {
	Op   string
	Args []string
}

// Line 返回单行可执行的 shell 命令（调试/输出用；updown 实际按 Op+Args 逐项调 exec）。
func (c TcCommand) Line() string {
	if len(c.Args) == 0 {
		return "tc " + c.Op
	}
	return "tc " + c.Op + " " + c.Args[0] + " " + strings.Join(c.Args[1:], " ")
}

// BuildTcCommands 生成给定 family 下的限速 tc 命令序列。
//
// 关键设计:
//   - 纯函数：不调 tc，只生成命令。updown 脚本负责 exec。
//   - family=dual 时 v4+v6 filter **共享同一个 class ID**(HTB class 是 L3 无关的)。
//   - rate/ceil 由调用方在 Args 里填实际 Mbps（这里写 1000mbit 是占位,
//     updown 脚本会通过 setRate() 替换）。
//   - family 无效或 verb 不是 up/down → 返回空切片（不 panic）。
//
// 参数:
//   family : "ipv4" | "ipv6" | "dual"
//   verb   : "up" | "down"
//   outIf  : 出接口，如 "eth0"
//   classID: 1-9099(由 updown 脚本持久化到 /var/lib/ikev2-panel/classids/<user>)
//   srcVIP : 客户端虚拟 IP(v4: "10.13.0.5";v6: "fd00:1::5")
//   dstVIP : 服务器 VPN 内网 IP(可选,down 时不需要;预留供将来 ingress 限速用)
func BuildTcCommands(family Family, verb, outIf string, classID int, srcVIP, dstVIP string) []TcCommand {
	if !family.Valid() {
		return nil
	}
	if verb != "up" && verb != "down" {
		return nil
	}
	if outIf == "" {
		return nil
	}

	cmds := make([]TcCommand, 0, 6)
	wantV4 := family == FamilyIPv4 || family == FamilyDual
	wantV6 := family == FamilyIPv6 || family == FamilyDual
	classRef := fmt.Sprintf("1:%d", classID)

	// --- IPv4 路径（沿用 u32:Linux < 4.5 也支持）---
	// 关键:dual 模式只创建 1 个 class(v4 + v6 filter 共用)
	// classAddOnce flag 保证 wantV4 + wantV6 同时为 true 时 class 只 add 一次
	if wantV4 && verb == "up" {
		cmds = append(cmds,
			TcCommand{"class", []string{
				"add", "dev", outIf, "parent", "1:", "classid", classRef, "htb",
				"rate", "1000mbit", "ceil", "1000mbit",
			}},
			TcCommand{"filter", []string{
				"add", "dev", outIf, "parent", "1:", "protocol", "ip", "prio", "1", "u32",
				"match", "ip", "src", srcVIP, "flowid", classRef,
			}},
		)
	}
	if wantV4 && verb == "down" {
		// 先删 filter 再删 class(class 关联的 filter 还在会报 "HTB class in use")
		cmds = append(cmds,
			TcCommand{"filter", []string{"del", "dev", outIf, "parent", "1:", "protocol", "ip", "prio", "1"}},
			TcCommand{"class", []string{"del", "dev", outIf, "classid", classRef}},
		)
	}

	// --- IPv6 路径（v2.85-PR7 新增 flower:Linux 4.1+）---
	// dual 模式 class 已由 wantV4 分支建好,这里只加 filter
	//
	// 关键:flower classifier 用 `src_ip` (不是 ip6_src),
	// tc 内部根据 IP 字面量格式(是否含 `:`)自动判断 v4/v6。
	// 配合 `protocol ipv6` 让内核只在 IPv6 包上评估。
	if wantV6 && verb == "up" {
		if family != FamilyDual {
			cmds = append(cmds,
				TcCommand{"class", []string{
					"add", "dev", outIf, "parent", "1:", "classid", classRef, "htb",
					"rate", "1000mbit", "ceil", "1000mbit",
				}},
			)
		}
		cmds = append(cmds,
			TcCommand{"filter", []string{
				"add", "dev", outIf, "parent", "1:", "protocol", "ipv6", "prio", "1", "flower",
				"src_ip", srcVIP, "flowid", classRef,
			}},
		)
	}
	if wantV6 && verb == "down" {
		// dual 模式 class 已在 wantV4 分支删过,这里跳过 class del
		// 顺序:filter 先,class 后(避免 "HTB class in use")
		if family != FamilyDual {
			cmds = append(cmds,
				TcCommand{"filter", []string{"del", "dev", outIf, "parent", "1:", "protocol", "ipv6", "prio", "1"}},
				TcCommand{"class", []string{"del", "dev", outIf, "classid", classRef}},
			)
		} else {
			// dual:只发 v6 filter del(class 已在 wantV4 分支删过)
			cmds = append(cmds,
				TcCommand{"filter", []string{"del", "dev", outIf, "parent", "1:", "protocol", "ipv6", "prio", "1"}},
			)
		}
	}

	return cmds
}

// SetRate 替换所有 class 命令中的 rate/ceil 值。
// updown 脚本通过这个把硬编码的 1000mbit 占位换成实际 LIMIT Mbps。
// 行为:
//   - 只替换 args 含 "class" 的命令（filter 命令不受影响）
//   - 找到 "rate" 后第 1 个参数是 rate,"ceil" 后第 1 个参数是 ceil
//   - in-place 修改（返回新切片，不改输入）
func SetRate(cmds []TcCommand, mbps int) []TcCommand {
	out := make([]TcCommand, len(cmds))
	for i, c := range cmds {
		args := make([]string, len(c.Args))
		copy(args, c.Args)
		if c.Op == "class" {
			for j := 0; j < len(args)-1; j++ {
				if args[j] == "rate" || args[j] == "ceil" {
					args[j+1] = fmt.Sprintf("%dmbit", mbps)
					j++ // skip next, already replaced
				}
			}
		}
		out[i] = TcCommand{Op: c.Op, Args: args}
	}
	return out
}
