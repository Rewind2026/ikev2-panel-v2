package config

import (
	"log/slog"
	"os"
	"testing"
)

// ---------------------------------------------------------------------------
// v2.86 批次 E:日志级别运行期切换 + 新增 env 字段的默认值
// ---------------------------------------------------------------------------

func TestSetLogLevelRuntime(t *testing.T) {
	t.Cleanup(func() { logLevelVar.Set(slog.LevelInfo) })

	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"debug", "debug", true},
		{"info", "info", true},
		{"warn", "warn", true},
		{"error", "error", true},
		{"DEBUG", "debug", true}, // 大小写宽容
		{" warn ", "warn", true},
		{"flase", "", false},
		{"verbose", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got := SetLogLevel(c.in)
		if got != c.ok {
			t.Fatalf("SetLogLevel(%q) 返回 %v,期望 %v", c.in, got, c.ok)
		}
		if c.ok && CurrentLogLevel() != c.want {
			t.Fatalf("SetLogLevel(%q) 后 CurrentLogLevel=%q,期望 %q",
				c.in, CurrentLogLevel(), c.want)
		}
	}
}

// TestNewLoggerRespectsEnvLevel 启动期 NewLogger 必须按 env 值设定级别。
//
// 这条不变量保证 runtime.Merge 把 settings.json 的 log_level 合进 cfg 后,
// main.go 里紧接着的 NewLogger() 建出来的就是新级别的 logger。
func TestNewLoggerRespectsEnvLevel(t *testing.T) {
	t.Cleanup(func() { logLevelVar.Set(slog.LevelInfo) })

	c := &Config{LogLevel: "warn", LogFormat: "text"}
	c.NewLogger()
	if got := CurrentLogLevel(); got != "warn" {
		t.Fatalf("NewLogger 后级别应为 warn,实际 %q", got)
	}

	c2 := &Config{LogLevel: "乱写", LogFormat: "text"}
	c2.NewLogger()
	if got := CurrentLogLevel(); got != "info" {
		t.Fatalf("非法 env 级别应退到 info,实际 %q", got)
	}
}

func TestNormalizeNftFamily(t *testing.T) {
	cases := map[string]string{
		"v4":   "v4",
		"v6":   "v6",
		"V6":   "v6",
		" v4 ": "v4",
		"":     "", // 空 = 自动判断
		"dual": "", // dual 是 DDNS 的值,不是 nft 地址族
		"ipv6": "",
		"乱写":   "",
	}
	for in, want := range cases {
		if got := normalizeNftFamily(in); got != want {
			t.Fatalf("normalizeNftFamily(%q)=%q,期望 %q", in, got, want)
		}
	}
}

func TestLoadBatchEDefaults(t *testing.T) {
	for _, k := range []string{
		"IKEV2_AUDIT_RETENTION_DAYS", "IKEV2_CONNLIMIT_PER_IP", "IKEV2_HASHLIMIT_PER_MIN",
		"IKEV2_DISABLE_MSS_CLAMP", "IKEV2_DISABLE_DDOS_PROTECTION", "IKEV2_FAMILY",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("IKEV2_CERT_MODE", "self-signed")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 默认值必须跟 entrypoint.sh 的 `:-` 兜底一字不差,
	// 否则面板只读区会显示出跟 shell 实际行为不符的数。
	if c.AuditRetentionDays != 90 {
		t.Fatalf("AuditRetentionDays 默认应为 90,实际 %d", c.AuditRetentionDays)
	}
	if c.ConnLimitPerIP != 50 {
		t.Fatalf("ConnLimitPerIP 默认应为 50,实际 %d", c.ConnLimitPerIP)
	}
	if c.HashLimitPerMin != 30 {
		t.Fatalf("HashLimitPerMin 默认应为 30,实际 %d", c.HashLimitPerMin)
	}
	if c.DisableMSSClamp || c.DisableDDoSProtection {
		t.Fatalf("MSS clamp / DDoS 默认应保持开启")
	}
	if c.NftFamily != "" {
		t.Fatalf("NftFamily 默认应为空(自动判断),实际 %q", c.NftFamily)
	}
}

func TestLoadBatchEFromEnv(t *testing.T) {
	t.Setenv("IKEV2_CERT_MODE", "self-signed")
	t.Setenv("IKEV2_AUDIT_RETENTION_DAYS", "180")
	t.Setenv("IKEV2_CONNLIMIT_PER_IP", "120")
	t.Setenv("IKEV2_HASHLIMIT_PER_MIN", "60")
	t.Setenv("IKEV2_DISABLE_MSS_CLAMP", "true")
	t.Setenv("IKEV2_DISABLE_DDOS_PROTECTION", "1")
	t.Setenv("IKEV2_FAMILY", "v6")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AuditRetentionDays != 180 || c.ConnLimitPerIP != 120 || c.HashLimitPerMin != 60 {
		t.Fatalf("数值项读取错误: %+v", c)
	}
	if !c.DisableMSSClamp || !c.DisableDDoSProtection {
		t.Fatalf("布尔项读取错误: mss=%v ddos=%v", c.DisableMSSClamp, c.DisableDDoSProtection)
	}
	if c.NftFamily != "v6" {
		t.Fatalf("NftFamily 读取错误: %q", c.NftFamily)
	}
}
