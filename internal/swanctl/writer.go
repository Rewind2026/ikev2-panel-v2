// swanctl 子配置写入与删除。
// 设计见 docs/design.md §9.2
//
// Manager / New / WithConfDir / WithSkipVici 定义在 loader.go（同 package）。
// 这里只放文件 CRUD 方法。
package swanctl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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