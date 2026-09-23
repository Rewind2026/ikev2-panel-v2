// DDNS 状态文件读写(v2-84 独立文件)。
//
// 解决 v2-83 留下的两个问题(P0 评审):
//
//  1. 多 key 写入 race:
//     v2-83 writeStateFile 直接 os.WriteFile,不支持多 key。
//     v2-84 ddns.conf 变成 INI-style key=value(enabled + family),
//     写半途被 SIGKILL 会留下半截文件 → 下次启动读错值。
//
//  2. v2-83 老格式兼容:
//     v2-83 ddns.conf 内容是裸 "true"/"false"。
//     v2-84 升级用户的文件不能丢,必须:
//     a) 启动时识别老格式
//     b) 立即重写成新格式(自动迁移)
//     c) 之后正常走新格式读/写
//
// 实现:全部用 swanctl.AtomicWriteFile(write-tmp + rename),
// 失败时原文件不动;读用 parseStateFile(同时识别新老两种格式)。
package ddns

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yourname/ikev2-panel-v2/internal/swanctl"
)

// stateFileFormat 当前状态文件格式说明(注释 + 一行 key=value,以后扩展只追加 key)。
const (
	// stateFileHeader 注释行,运维手动编辑时能看出文件用途。
	stateFileHeader = "# ikev2-panel DDNS runtime state (managed by ikev2-panel; do not edit while service is running)"
)

// validFamily 是 family 字段的白名单(SetFamily / parseStateFile 都用)。
// 与 internal/config.parseFamily 的合法值一致,这里再写一遍避免循环 import。
func validFamily(f string) bool {
	return f == "v4" || f == "v6" || f == "dual"
}

// allRRChars v2.86-pr23a:校验 RR 字符集([a-zA-Z0-9_-],1-63 字符)。
//
// 跟 panelstate.validRR 同 pattern,这里再写一遍避免 import 循环
// (panelstate -> ddns 没问题;ddns -> panelstate 会反向)。
func allRRChars(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// boolStr v2.86-pr23a:statefile 输出 "true"/"false" 字符串。
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// stateRecord 是解析后的运行时状态。
//
// Enabled:DDNS 总开关。
// Family:DDNS family 选择(v4 / v6 / dual)。v2.86-pr23a:deprecated —
// EnableA/EnableAAAA 两个独立 bool 替代。保留字段做向后兼容(老 statefile
// 仍能读;Family() getter 会自动从 (EnableA, EnableAAAA) 反推回 v4/v6/dual)。
// RR:主机记录(vpn.example.com 中的 vpn),面板可改。
// EnableA / EnableAAAA:v2.86-pr23a 取代 family 枚举,两个独立 bool。
// PeriodSec:同步周期秒数(v2.86-pr23a 新增,之前是 env 启动值)。
// LastSyncA / LastSyncAAAA:节流时间戳(unix 秒)— v2.85-PR6 (Q5-01) 新增,
// 用于重启后保留 throttle 窗口,避免撞 alidns 30 QPS 限流。
// Both zero values are valid defaults(调用方决定何时用 env fallback)。
type stateRecord struct {
	Enabled      bool
	Family       string
	RR           string // v2.86-pr23a:主机记录(空 = "@")
	EnableA      bool   // v2.86-pr23a:同步 A 记录(IPv4)
	EnableAAAA   bool   // v2.86-pr23a:同步 AAAA 记录(IPv6)
	PeriodSec    int    // v2.86-pr23a:同步周期(秒);0 = 用 env 默认 60
	LastSyncA    int64  // unix 秒;0 = 从未同步
	LastSyncAAAA int64  // 同上
}

// parseStateFile 读 /etc/ikev2/ddns.conf,返回 stateRecord。
//
// 支持三种格式(按优先级):
//  1. INI-style key=value:每行 "enabled=true" / "family=dual",忽略 # 开头注释和空行
//  2. v2-83 旧格式:整文件 == "true" → enabled=true,family 用 caller 提供的 defaultFamily
//  3. v2-83 旧格式:整文件 == "false" → enabled=false
//
// 文件不存在 → return (zero, nil),caller 用 env / 默认值 fallback。
// 其他错误 → return (zero, err)。
//
// 注释 / 空行 / 不认识的 key → 静默忽略(forward-compat)。
func parseStateFile(path, defaultFamily string) (stateRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return stateRecord{}, nil
		}
		return stateRecord{}, fmt.Errorf("read state file %s: %w", path, err)
	}

	raw := strings.TrimSpace(string(data))

	// 格式 2/3:老 v2-83 裸 true/false
	if raw == "true" {
		return stateRecord{Enabled: true, Family: defaultFamily}, nil
	}
	if raw == "false" {
		return stateRecord{Enabled: false, Family: defaultFamily}, nil
	}

	// 格式 1:INI-style 多行
	rec := stateRecord{}
	hasEnabled := false
	hasFamily := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			// 不是 key=value 行 → 忽略(forward-compat)
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		switch key {
		case "enabled":
			rec.Enabled = (val == "true" || val == "1" || val == "on")
			hasEnabled = true
		case "family":
			if validFamily(val) {
				rec.Family = val
				hasFamily = true
			}
			// 非法 family 值 → 忽略(不要 panic,让 caller 用 default)
		case "last_sync_a":
			// v2.85-PR6 (Q5-01):节流时间戳持久化。
			// 解析失败或负数 → 静默忽略,fallback 0 = 无节流历史。
			if n, perr := strconv.ParseInt(val, 10, 64); perr == nil && n >= 0 {
				rec.LastSyncA = n
			}
		case "last_sync_aaaa":
			if n, perr := strconv.ParseInt(val, 10, 64); perr == nil && n >= 0 {
				rec.LastSyncAAAA = n
			}
		case "rr":
			// v2.86-pr23a:主机记录(只接受合法字符;不合法保留空走 default)
			if val == "@" || (len(val) <= 63 && allRRChars(val)) {
				rec.RR = val
			}
		case "enable_a":
			// v2.86-pr23a:独立 bool("true"/"false"/"1"/"0"/"on"/"off")
			rec.EnableA = (val == "true" || val == "1" || val == "on")
		case "enable_aaaa":
			rec.EnableAAAA = (val == "true" || val == "1" || val == "on")
		case "period_seconds":
			// v2.86-pr23a:同步周期秒数,clamp [10, 3600],越界保留 0 走 default
			if n, perr := strconv.Atoi(val); perr == nil && n >= 10 && n <= 3600 {
				rec.PeriodSec = n
			}
		}
	}
	// family 缺省 → 用 defaultFamily(env 透传的值,通常 "dual")
	if !hasFamily {
		rec.Family = defaultFamily
	}
	// enabled 缺省 → 视为 false(这是 v2-83 的语义:文件不存在 = enabled=false)
	if !hasEnabled {
		rec.Enabled = false
	}
	return rec, nil
}

// writeStateFile 把 stateRecord 原子写入 path。
//
// 步骤:
//  1. 序列化(rec → bytes)
//  2. swanctl.AtomicWriteFile(write tmp + rename)
//
// 失败 → 原文件不变,error 返回。
func writeStateFile(path string, rec stateRecord) error {
	var b strings.Builder
	b.WriteString(stateFileHeader)
	b.WriteByte('\n')
	if rec.Enabled {
		b.WriteString("enabled=true\n")
	} else {
		b.WriteString("enabled=false\n")
	}
	if rec.Family != "" {
		b.WriteString("family=")
		b.WriteString(rec.Family)
		b.WriteByte('\n')
	}
	// v2.86-pr23a:面板可配字段(rr / enable_a / enable_aaaa / period_seconds)。
	// 全部写,即使跟 env 默认值相同 — statefile 是"用户显式配置"的 source of truth。
	rr := rec.RR
	if rr == "" {
		rr = "@"
	}
	fmt.Fprintf(&b, "rr=%s\n", rr)
	fmt.Fprintf(&b, "enable_a=%s\n", boolStr(rec.EnableA))
	fmt.Fprintf(&b, "enable_aaaa=%s\n", boolStr(rec.EnableAAAA))
	if rec.PeriodSec > 0 {
		fmt.Fprintf(&b, "period_seconds=%d\n", rec.PeriodSec)
	}
	// v2.85-PR6 (Q5-01):节流时间戳持久化(unix 秒)。
	// > 0 才写(0 = 从未同步,无需持久化;启动时默认就是 0)。
	if rec.LastSyncA > 0 {
		fmt.Fprintf(&b, "last_sync_a=%d\n", rec.LastSyncA)
	}
	if rec.LastSyncAAAA > 0 {
		fmt.Fprintf(&b, "last_sync_aaaa=%d\n", rec.LastSyncAAAA)
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	return swanctl.AtomicWriteFile(path, []byte(b.String()), 0o644)
}

// migrateStateFileIfNeeded 老 v2-83 裸 true/false → INI-style 自动迁移。
//
// 如果当前文件是裸 "true"/"false",重写为新格式。
// INI-style 文件 / 文件不存在 / 其他格式 → 不动。
//
// 这个函数在 NewSync 时调一次,把状态文件"现代化"。
// 写失败也不 fatal(parseStateFile 仍能正确读老格式)。
func migrateStateFileIfNeeded(path string, defaultFamily string, logger *slog.Logger) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // 文件不存在或其他错误,跳过
	}
	raw := strings.TrimSpace(string(data))
	if raw != "true" && raw != "false" {
		return // 已经是 INI-style 或其他格式,不打扰
	}

	// 老格式 → 重写
	rec := stateRecord{
		Enabled: raw == "true",
		Family:  defaultFamily,
	}
	if err := writeStateFile(path, rec); err != nil {
		if logger != nil {
			logger.Warn("ddns: state file migration failed; will retry next start",
				"path", path, "err", err)
		}
		return
	}
	if logger != nil {
		logger.Info("ddns: state file migrated to INI format",
			"path", path, "enabled", rec.Enabled, "family", rec.Family)
	}
}
