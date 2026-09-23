// swanctl 子配置写入与删除。
// 设计见 docs/design.md §9.2
//
// Manager / New / WithConfDir / WithSkipVici 定义在 loader.go（同 package）。
// 这里只放文件 CRUD 方法。
package swanctl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SwanctlConfPath swanctl 主配置文件路径(由 UpdatePools 改写的目标)。
//
// 容器内固定 /etc/swanctl/swanctl.conf。
// 测试可通过 Manager.WithSwanctlConfPath 覆盖。
const SwanctlConfPath = "/etc/swanctl/swanctl.conf"

// poolAddrsRe 匹配 swanctl.conf 里 `addrs = X, Y` 一行(任意空白)。
//
// v2-79 模板里 addrs 后面跟的是 v4 CIDR + v6 CIDR,逗号分隔,中间任意空白。
// 我们的 UpdatePools 把整行替换为 `addrs = <new_v4>, <new_v6>`。
var poolAddrsRe = regexp.MustCompile(`(?m)^\s*addrs\s*=\s*[^#\n]+$`)

// poolAddrsCaptureRe 跟 poolAddrsRe 类似,但额外捕获 2 个 CIDR(v4, v6)。
//
// 严格格式:逗号分隔,中间可有空白;每段是合法 CIDR 字符串。
// 我们的 UpdatePools 写出来的就是这种格式,所以读取时用同一个 regex 反向解析。
var poolAddrsCaptureRe = regexp.MustCompile(`(?m)^\s*addrs\s*=\s*(\S+)\s*,\s*(\S+)\s*$`)

// ReadCurrentPools 读 swanctl.conf 当前 addrs 段的 IPv4 / IPv6 CIDR。
//
// 返回值:
//   - ipv4, ipv6 字符串
//   - err:文件不存在 / 读失败 / regex 没匹配 / 字段数不够
//
// dev 模式下文件不存在 → 返回 "", "", nil(handler 用 ok 判断)
func ReadCurrentPools(confContent []byte) (ipv4, ipv6 string, err error) {
	match := poolAddrsCaptureRe.FindStringSubmatch(string(confContent))
	if len(match) < 3 {
		return "", "", fmt.Errorf("no pool addrs line found")
	}
	return match[1], match[2], nil
}

// ReadCurrentPoolsFromFile 是 ReadCurrentPools 的"读文件"版本,失败时返回 "", "", nil
// 而不是 error — 用于 handler 状态查询(读不到就当 dev 模式,不报错)。
func (m *Manager) ReadCurrentPoolsFromFile() (ipv4, ipv6 string) {
	data, err := os.ReadFile(m.swanctlConfPath())
	if err != nil {
		return "", ""
	}
	ipv4, ipv6, err = ReadCurrentPools(data)
	if err != nil {
		return "", ""
	}
	return ipv4, ipv6
}

// WriteUserConf 写一个用户的子配置文件（secrets 块）。
//
// 关键设计（design §9.2）：
//   - 每用户独立 .conf 文件
//   - 删除用户 = 删除文件 + VICI load-all，比修改大文件更安全
//   - 文件权限 0600（charon 读取时要求）
//   - P0-3 改进：写临时文件 → os.Rename 原子替换
//     防止 write 中断（如容器重启、OOM kill）留下半截 conf → charon 启动读半截挂掉
func (m *Manager) WriteUserConf(username, password string) error {
	if err := validateUsername(username); err != nil {
		return err
	}

	// swanctl secrets 块语法（v2 EAP-MSCHAPv2 模式）：
	//   secrets {
	//       eap-<username> {
	//           id = <username>
	//           secret = "<password>"
	//       }
	//   }
	//
	// 注意：password 必须用 swanctl 字符串语法转义（双引号包裹，反斜杠 + 美元符号 转义）。
	// 我们的密码只含字母数字，无特殊字符，但仍稳妥起见用 %q：
	content := fmt.Sprintf(`secrets {
    eap-%s {
        id = %s
        secret = %q
    }
}
`, username, username, password)

	path := filepath.Join(m.confDir, username+".conf")
	return AtomicWriteFile(path, []byte(content), 0o600)
}

// RemoveUserConf 删除一个用户的子配置文件。
// 文件不存在视为成功。
func (m *Manager) RemoveUserConf(username string) error {
	if err := validateUsername(username); err != nil {
		return err
	}
	path := filepath.Join(m.confDir, username+".conf")
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// UserConfExists 检查某用户配置是否存在。
func (m *Manager) UserConfExists(username string) bool {
	if err := validateUsername(username); err != nil {
		return false
	}
	path := filepath.Join(m.confDir, username+".conf")
	_, err := os.Stat(path)
	return err == nil
}

// validateUsername 防路径穿越：禁止 / . .. 等。
func validateUsername(u string) error {
	if u == "" {
		return fmt.Errorf("username is empty")
	}
	if strings.ContainsAny(u, "/\\. \t\n") {
		return fmt.Errorf("username %q contains invalid characters", u)
	}
	return nil
}

// swanctlConfPath 返回 Manager 当前使用的 swanctl.conf 路径。
//
// 默认 SwanctlConfPath(/etc/swanctl/swanctl.conf);测试可覆盖。
func (m *Manager) swanctlConfPath() string {
	if m.confPath != "" {
		return m.confPath
	}
	return SwanctlConfPath
}

// WithSwanctlConfPath 覆盖 swanctl.conf 路径(测试用)。
//
// 返回 m 以便链式调用。
func (m *Manager) WithSwanctlConfPath(p string) *Manager {
	m.confPath = p
	return m
}

// UpdatePools 改写 swanctl.conf 里的 pools.ikev2-pool.addrs,返回新文件内容。
//
// 行为:
//   - 读 swanctl.conf
//   - 用 poolAddrsRe 找到 `addrs = ...` 行(模板里只有 1 个 addrs,即 pool 段里的那个)
//   - 替换为 `addrs = <ipv4>, <ipv6>`(强 S wan 期望逗号分隔,中间空格可选)
//   - 不写文件,不 reload。返回新内容,调用方负责落盘 + reload
//
// 为什么拆成"返回内容 + 单独写"而不是直接写文件:
//   - 跟 WriteUserConfAndReload / RemoveUserConfAndReload 一致:reloadMu 内"改文件 + reload"
//   - 但 UpdatePools 需要原子:写 tmp + rename + reload 全持锁,所以导出 helper 不持锁
//
// ipv4 / ipv6 都必填(强 S wan pool addrs 至少 1 个,我们的模板固定 2 个)。
//
// 错误返回:
//   - 文件不存在 / 读失败 / poolAddrsRe 没匹配 / 写 tmp 失败 → 返回 error
func UpdatePools(confContent []byte, ipv4, ipv6 string) ([]byte, error) {
	if ipv4 == "" || ipv6 == "" {
		return nil, fmt.Errorf("ipv4 和 ipv6 都必填,ipv4=%q ipv6=%q", ipv4, ipv6)
	}
	newLine := fmt.Sprintf("addrs = %s, %s", ipv4, ipv6)
	loc := poolAddrsRe.FindIndex(confContent)
	if loc == nil {
		return nil, fmt.Errorf("未找到 pool addrs 行(swanctl.conf 模板可能变了?)")
	}
	// poolAddrsRe 不会跨行,loc[0] ~ loc[1] 就是整行范围。
	// 但行尾可能有 \n,我们保留行尾换行符(否则下面 file 拼接错)。
	// 简化做法:直接把整段替换为 "newLine\n"。
	out := make([]byte, 0, len(confContent))
	out = append(out, confContent[:loc[0]]...)
	out = append(out, []byte(newLine)...)
	// 跳过原 addrs 行(到下一个换行符为止,保留换行符本身)
	i := loc[1]
	for i < len(confContent) && confContent[i] != '\n' {
		i++
	}
	out = append(out, confContent[i:]...)
	return out, nil
}

// UpdatePoolsAndReload 是 UpdatePools 的"swanctl 重载一体化"版本:
//
//   - 整体持 reloadMu(避免跟 WriteUserConfAndReload / RemoveUserConfAndReload 并发)
//   - 写 tmp + rename 原子替换 swanctl.conf
//   - 立即 ReloadAll 让 charon 拿到新 pool 段
//
// 副作用:
//   - charon 会卸载当前活跃 SA(swanctl --load-all 的固有行为,~500ms 中断)
//   - 客户端需要重新拨号才能拿到新 IP 段
//
// 错误:文件不存在/没找到 pool 段/write tmp 失败/Rename 失败/ReloadAll 失败 →
//   返回 error,**但已写入的文件保留**(避免回滚复杂),调用方决定是否回滚。
//
// dev 模式(/etc/swanctl/swanctl.conf 不存在)→ 静默返回 nil。
// 原因:dev server 没有 charon / swanctl.conf,UpdatePools 没有写入目标。
// panelstate 已经被 handler 写好了,真机部署 / 容器化后启动 entrypoint
// 会把 panelstate 覆盖回 env/auto(目前没这个逻辑,后续 PR 可加)。
// 现阶段 dev 模式就当"配了但还没部署"处理,不报错给用户看。
func (m *Manager) UpdatePoolsAndReload(ctx context.Context, ipv4, ipv6 string) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	confPath := m.swanctlConfPath()

	// 1. 读(dev 模式文件不存在 → 静默成功,不报错)
	orig, err := os.ReadFile(confPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // dev 模式:没有 swanctl.conf → 没东西可改,接受
		}
		return fmt.Errorf("read %s: %w", confPath, err)
	}

	// 2. 改
	updated, err := UpdatePools(orig, ipv4, ipv6)
	if err != nil {
		return err
	}

	// 3. 写 tmp + rename 原子替换(避免 charon reload 读到半截)
	tmp, err := os.CreateTemp(filepath.Dir(confPath), ".swanctl.conf.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// 出错时清 tmp(成功路径下 tmp 已被 rename,Stat 会失败,自然跳过)
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(updated); err != nil {
		tmp.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	// 保留原文件 mode(0644),不强制改
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpName, confPath); err != nil {
		return fmt.Errorf("rename tmp -> %s: %w", confPath, err)
	}

	// 4. reload(走 reloadMu 内版本,reloadAllLocked 在 loader.go)
	if err := m.reloadAllLocked(ctx); err != nil {
		return fmt.Errorf("swanctl reload: %w", err)
	}
	return nil
}