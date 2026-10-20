// v2.86-PR13.3:runtime.Merge 的 subnet 合并测试。
//
// 覆盖:
//  1. panelstate subnet.conf 存在 → 优先于 env
//  2. panelstate 不存在 → 用 env 值(可能为空)
//  3. panelstate 文件存在但 v6 为空 → v4 用 panelstate,v6 保留 env
//  4. panelstate 文件损坏 → WARN + fallback env,Source 标 env-default
//
// v2.86 批次 D 追加 settings.json 合并段:
//  5. settings.json 非 nil 字段覆盖 env,nil 字段保持 env
//  6. 显式的 false 压过 env 的 true(三态最容易回归的一条)
//  7. 无 settings.json → 完全跟随 env,Source 标 env-default
package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/config"
	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
)

func TestMerge_PanelStateSubnetWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	// 写 subnet.conf
	cfg := struct {
		IPv4Subnet string `json:"ipv4_subnet"`
		IPv6Subnet string `json:"ipv6_subnet"`
	}{"10.10.20.0/24", "fd00:1::/64"}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "subnet.conf"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	env := &config.Config{
		IPv4Subnet:         "10.10.0.0/24",
		IPv6Subnet:         "fd00:99::/64",
		SubnetConfigSource: "env-default",
	}

	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("Merge err=%v", err)
	}
	if r.IPv4Subnet != "10.10.20.0/24" {
		t.Errorf("v4 = %q, want panelstate value 10.10.20.0/24", r.IPv4Subnet)
	}
	if r.IPv6Subnet != "fd00:1::/64" {
		t.Errorf("v6 = %q, want panelstate value fd00:1::/64", r.IPv6Subnet)
	}
	if r.SubnetConfigSource != "panelstate" {
		t.Errorf("source = %q, want panelstate", r.SubnetConfigSource)
	}
}

func TestMerge_PanelStateMissing_FallsBackToEnv(t *testing.T) {
	dir := t.TempDir()
	// 不写 subnet.conf

	env := &config.Config{
		IPv4Subnet:         "10.13.0.0/24",
		IPv6Subnet:         "fd00:5::/64",
		SubnetConfigSource: "env-default",
	}

	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("Merge err=%v", err)
	}
	if r.IPv4Subnet != "10.13.0.0/24" {
		t.Errorf("v4 = %q, want env value", r.IPv4Subnet)
	}
	if r.IPv6Subnet != "fd00:5::/64" {
		t.Errorf("v6 = %q, want env value", r.IPv6Subnet)
	}
	if r.SubnetConfigSource != "env-default" {
		t.Errorf("source = %q, want env-default", r.SubnetConfigSource)
	}
}

func TestMerge_PanelStateV6Empty_PreservesEnvV6(t *testing.T) {
	dir := t.TempDir()

	// v6 字段为空 → 保留 env 的 v6
	cfg := struct {
		IPv4Subnet string `json:"ipv4_subnet"`
		IPv6Subnet string `json:"ipv6_subnet"`
	}{"10.10.20.0/24", ""}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "subnet.conf"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	env := &config.Config{
		IPv4Subnet:         "10.10.0.0/24",
		IPv6Subnet:         "fd00:99::/64",
		SubnetConfigSource: "env-default",
	}

	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("Merge err=%v", err)
	}
	if r.IPv4Subnet != "10.10.20.0/24" {
		t.Errorf("v4 = %q, want panelstate value", r.IPv4Subnet)
	}
	if r.IPv6Subnet != "fd00:99::/64" {
		t.Errorf("v6 = %q, want env fallback (panelstate v6 was empty)", r.IPv6Subnet)
	}
}

func TestMerge_PanelStateCorrupted_FallsBackToEnv(t *testing.T) {
	dir := t.TempDir()

	// 写坏 JSON
	if err := os.WriteFile(filepath.Join(dir, "subnet.conf"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := &config.Config{
		IPv4Subnet:         "10.13.0.0/24",
		IPv6Subnet:         "fd00:5::/64",
		SubnetConfigSource: "env-default",
	}

	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("Merge err=%v", err)
	}
	if r.IPv4Subnet != "10.13.0.0/24" {
		t.Errorf("v4 = %q, want env fallback after parse error", r.IPv4Subnet)
	}
	if r.SubnetConfigSource != "env-default" {
		t.Errorf("source = %q, want env-default after parse error", r.SubnetConfigSource)
	}
}

// mergeWithDir 等价于 Merge(env),但 panelstate 的三个 Store 都用指定目录。
//
// 这是个 helper,因为 Merge 直接调 panelstate.NewXxxStore()(默认 /data/panel-state),
// 测试不能污染它。
func mergeWithDir(env *config.Config, dir string) (*Runtime, error) {
	if env == nil {
		return nil, nil
	}
	r := &Runtime{Config: env}

	// 阿里云凭证(走默认 Store,跳过 panelstate 不存在的场景下不会读盘)
	credStore := panelstate.NewStore()
	if c, err := credStore.LoadAliyun(); err == nil && c != nil {
		r.AliyunAccessKeyID = c.KeyID
		r.AliyunAccessKeySecret = c.KeySecret
		r.AliyunAccessKeySource = "panelstate"
	}

	// 证书配置(走默认 Store)
	certStore := panelstate.NewCertConfigStore()
	if c, err := certStore.LoadCertConfig(); err == nil && c != nil {
		r.CertMode = pickStr(c.CertMode, env.CertMode)
		r.Domain = pickStr(c.Domain, env.Domain)
		r.ServerCN = pickStr(c.ServerCN, env.ServerCN)
		r.ACMEEmail = pickStr(c.ACMEEmail, env.ACMEEmail)
		r.CertConfigSource = "panelstate"
	} else {
		r.CertMode = env.CertMode
		r.Domain = env.Domain
		r.ServerCN = env.ServerCN
		r.ACMEEmail = env.ACMEEmail
		r.CertConfigSource = env.CertConfigSource
	}

	// 关键差异:用测试目录的 SubnetConfigStore
	subnetStore := panelstate.NewSubnetConfigStoreWithDir(dir)
	if c, err := subnetStore.LoadSubnetConfig(); err == nil && c != nil {
		r.IPv4Subnet = pickStr(c.IPv4Subnet, env.IPv4Subnet)
		r.IPv6Subnet = pickStr(c.IPv6Subnet, env.IPv6Subnet)
		r.SubnetConfigSource = "panelstate"
	} else {
		if err != nil {
			stderrWarn("panelstate subnet.conf parse failed", err)
		}
		r.IPv4Subnet = env.IPv4Subnet
		r.IPv6Subnet = env.IPv6Subnet
		r.SubnetConfigSource = env.SubnetConfigSource
	}

	// v2.86 批次 D:面板设置(测试目录的 SettingsStore)
	settingsStore := panelstate.NewSettingsStoreWithDir(dir)
	if st, err := settingsStore.LoadSettings(); err == nil && st != nil {
		if st.DisplayTimezone != nil {
			r.DisplayTimezone = *st.DisplayTimezone
		}
		if st.SessionTTL != nil {
			if d, perr := time.ParseDuration(*st.SessionTTL); perr == nil {
				r.SessionTTL = d
			} else {
				stderrWarn("panelstate settings.json session_ttl invalid", perr)
			}
		}
		if st.CookieSecure != nil {
			r.CookieSecure = *st.CookieSecure
		}
		if st.TrustedProxy != nil {
			r.TrustedProxy = *st.TrustedProxy
		}
		if st.DefaultUserPasswordLen != nil {
			r.DefaultUserPasswordLen = *st.DefaultUserPasswordLen
		}
		if st.LogLevel != nil {
			r.LogLevel = *st.LogLevel
		}
		if st.LogFormat != nil {
			r.LogFormat = *st.LogFormat
		}
		if st.AuditRetentionDays != nil {
			r.AuditRetentionDays = *st.AuditRetentionDays
		}
		if st.ConnLimitPerIP != nil {
			r.ConnLimitPerIP = *st.ConnLimitPerIP
		}
		if st.HashLimitPerMin != nil {
			r.HashLimitPerMin = *st.HashLimitPerMin
		}
		if st.DisableMSSClamp != nil {
			r.DisableMSSClamp = *st.DisableMSSClamp
		}
		if st.DisableDDoSProtection != nil {
			r.DisableDDoSProtection = *st.DisableDDoSProtection
		}
		if st.NftFamily != nil {
			r.NftFamily = *st.NftFamily
		}
		r.SettingsSource = "panelstate"
	} else {
		if err != nil {
			stderrWarn("panelstate settings.json parse failed", err)
		}
		r.SettingsSource = "env-default"
	}

	return r, nil
}

// TestMerge_SettingsPanelStateWinsOverEnv 验证批次 D 的合并段:
// settings.json 里非 nil 的字段覆盖 env,nil 的字段保持 env。
func TestMerge_SettingsPanelStateWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	// 只显式设时区 + 密码长度,其余留 nil 表示"跟随 env"
	tz := "Asia/Shanghai"
	n := 20
	if err := panelstate.NewSettingsStoreWithDir(dir).WriteSettings(panelstate.Settings{
		DisplayTimezone:        &tz,
		DefaultUserPasswordLen: &n,
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	env := &config.Config{
		DisplayTimezone:        "UTC",
		SessionTTL:             8 * time.Hour,
		CookieSecure:           true,
		TrustedProxy:           true,
		DefaultUserPasswordLen: 12,
	}
	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("mergeWithDir: %v", err)
	}

	if r.DisplayTimezone != "Asia/Shanghai" {
		t.Errorf("DisplayTimezone = %q, want Asia/Shanghai (panelstate 覆盖 env)", r.DisplayTimezone)
	}
	if r.DefaultUserPasswordLen != 20 {
		t.Errorf("DefaultUserPasswordLen = %d, want 20", r.DefaultUserPasswordLen)
	}
	// nil 字段必须保持 env 值
	if r.SessionTTL != 8*time.Hour {
		t.Errorf("SessionTTL = %s, want 8h (nil 应保持 env)", r.SessionTTL)
	}
	if !r.CookieSecure {
		t.Errorf("CookieSecure = false, want true (nil 应保持 env)")
	}
	if !r.TrustedProxy {
		t.Errorf("TrustedProxy = false, want true (nil 应保持 env)")
	}
	if r.SettingsSource != "panelstate" {
		t.Errorf("SettingsSource = %q, want panelstate", r.SettingsSource)
	}
}

// TestMerge_SettingsExplicitFalseBeatsEnvTrue 保护三态里最容易回归的一条:
// settings.json 里显式的 false 必须压过 env 的 true。
func TestMerge_SettingsExplicitFalseBeatsEnvTrue(t *testing.T) {
	dir := t.TempDir()
	f := false
	if err := panelstate.NewSettingsStoreWithDir(dir).WriteSettings(panelstate.Settings{
		CookieSecure: &f,
		TrustedProxy: &f,
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	env := &config.Config{CookieSecure: true, TrustedProxy: true}
	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("mergeWithDir: %v", err)
	}
	if r.CookieSecure {
		t.Errorf("CookieSecure = true, want false (面板显式 false 应压过 env true)")
	}
	if r.TrustedProxy {
		t.Errorf("TrustedProxy = true, want false (面板显式 false 应压过 env true)")
	}
}

// TestMerge_SettingsMissing_FallsBackToEnv 没有任何 settings.json 时全部跟随 env。
func TestMerge_SettingsMissing_FallsBackToEnv(t *testing.T) {
	env := &config.Config{
		DisplayTimezone:        "Asia/Tokyo",
		SessionTTL:             time.Hour,
		CookieSecure:           true,
		DefaultUserPasswordLen: 16,
	}
	r, err := mergeWithDir(env, t.TempDir())
	if err != nil {
		t.Fatalf("mergeWithDir: %v", err)
	}
	if r.DisplayTimezone != "Asia/Tokyo" || r.SessionTTL != time.Hour ||
		!r.CookieSecure || r.DefaultUserPasswordLen != 16 {
		t.Errorf("无 settings.json 时应完全跟随 env,实际 %+v", r.Config)
	}
	if r.SettingsSource != "env-default" {
		t.Errorf("SettingsSource = %q, want env-default", r.SettingsSource)
	}
}

// ---------------------------------------------------------------------------
// v2.86 批次 E:settings.json 新增 8 项的合并
// ---------------------------------------------------------------------------

func ptrStr(s string) *string { return &s }

func ptrBool(b bool) *bool { return &b }

func ptrInt(i int) *int { return &i }

func writeSettingsJSON(t *testing.T, dir string, st panelstate.Settings) {
	t.Helper()
	if err := panelstate.NewSettingsStoreWithDir(dir).WriteSettings(st); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
}

// writeRawSettings 绕过 Validate 直接落盘,用于模拟"人工编辑写坏"的场景。
func writeRawSettings(t *testing.T, dir, raw string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("write raw settings.json: %v", err)
	}
}

// TestMerge_SettingsBatchE 批次 E 的 8 项同样走"指针三态 + 面板优先"。
//
// 重点保护两条:
//   - nil(面板没设过)不能覆盖 env
//   - 显式 false / 显式 0 能覆盖 env 的 true / 非零值
func TestMerge_SettingsBatchE(t *testing.T) {
	dir := t.TempDir()
	st := panelstate.Settings{
		LogLevel:              ptrStr("debug"),
		LogFormat:             ptrStr("json"),
		AuditRetentionDays:    ptrInt(180),
		ConnLimitPerIP:        ptrInt(120),
		HashLimitPerMin:       ptrInt(60),
		DisableMSSClamp:       ptrBool(true),
		DisableDDoSProtection: ptrBool(false), // 显式 false 压不过?这里 env 也是 false
		NftFamily:             ptrStr("v6"),
	}
	writeSettingsJSON(t, dir, st)

	env := &config.Config{
		LogLevel:              "info",
		LogFormat:             "text",
		AuditRetentionDays:    90,
		ConnLimitPerIP:        50,
		HashLimitPerMin:       30,
		DisableMSSClamp:       false,
		DisableDDoSProtection: true, // env 是 true,面板要能显式关掉
		NftFamily:             "v4",
	}
	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("mergeWithDir: %v", err)
	}

	if r.LogLevel != "debug" || r.LogFormat != "json" {
		t.Errorf("日志项未合并: level=%q format=%q", r.LogLevel, r.LogFormat)
	}
	if r.AuditRetentionDays != 180 || r.ConnLimitPerIP != 120 || r.HashLimitPerMin != 60 {
		t.Errorf("数值项未合并: %+v", r.Config)
	}
	if !r.DisableMSSClamp {
		t.Errorf("disable_mss_clamp=true 未合并")
	}
	// 关键:env 是 true,面板显式 false 必须压过去
	if r.DisableDDoSProtection {
		t.Errorf("面板显式 false 应压过 env 的 true,实际 %v", r.DisableDDoSProtection)
	}
	if r.NftFamily != "v6" {
		t.Errorf("nft_family 未合并: %q", r.NftFamily)
	}
	if r.SettingsSource != "panelstate" {
		t.Errorf("SettingsSource = %q, want panelstate", r.SettingsSource)
	}
}

// TestMerge_SettingsBatchENilKeepsEnv 只设了部分项时,其余必须保持 env 值。
func TestMerge_SettingsBatchENilKeepsEnv(t *testing.T) {
	dir := t.TempDir()
	writeSettingsJSON(t, dir, panelstate.Settings{LogLevel: ptrStr("debug")})

	env := &config.Config{
		LogLevel:           "info",
		LogFormat:          "json",
		AuditRetentionDays: 365,
		ConnLimitPerIP:     200,
		HashLimitPerMin:    99,
		DisableMSSClamp:    true,
		NftFamily:          "v4",
	}
	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("mergeWithDir: %v", err)
	}
	if r.LogLevel != "debug" {
		t.Errorf("已设项应被覆盖,实际 %q", r.LogLevel)
	}
	if r.LogFormat != "json" || r.AuditRetentionDays != 365 || r.ConnLimitPerIP != 200 ||
		r.HashLimitPerMin != 99 || !r.DisableMSSClamp || r.NftFamily != "v4" {
		t.Errorf("未设项被错误覆盖: %+v", r.Config)
	}
}

// TestMerge_SettingsBatchEInvalidRejectedWholeFile 保护严格策略:
// settings.json 只要有任一字段非法,LoadSettings 就整份拒绝(返回 err),
// Merge 随之完全回退 env —— 而不是"部分生效"。人工手改文件写坏时,
// 半生半熟的状态比全量回退更危险。
func TestMerge_SettingsBatchEInvalidRejectedWholeFile(t *testing.T) {
	dir := t.TempDir()
	// session_ttl 非法,但同文件的 log_level 是合法的
	raw := `{"schema_version":1,"session_ttl":"这不是时长","log_level":"debug"}`
	writeRawSettings(t, dir, raw)

	env := &config.Config{SessionTTL: 6 * time.Hour, LogLevel: "info"}
	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("坏值不应让 Merge 失败: %v", err)
	}
	if r.SessionTTL != 6*time.Hour {
		t.Errorf("非法 session_ttl 应保留 env 值,实际 %s", r.SessionTTL)
	}
	if r.LogLevel != "info" {
		t.Errorf("整份文件应被拒绝,log_level 不应半生效,实际 %q", r.LogLevel)
	}
	if r.SettingsSource != "env-default" {
		t.Errorf("SettingsSource = %q, want env-default (整份拒绝)", r.SettingsSource)
	}
}
