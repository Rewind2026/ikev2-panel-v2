// v2.86 批次 D:Settings(面板设置)单元测试
//
// 覆盖三态语义 / Validate 边界 / atomic 写 / Clear 幂等 / 缓存回落。
// 重点保护两条不变量:
//  1. 显式的 false / 0 必须能落盘(key 存在),不能被 omitempty 吞掉;
//  2. nil(未设置)必须不落盘,否则 panelstate 会抢走 env 的值。
package panelstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ptrString / ptrBool / ptrInt 便于在表驱动用例里构造指针字段。
func ptrString(s string) *string { return &s }
func ptrBool(b bool) *bool       { return &b }
func ptrInt(n int) *int          { return &n }

// requireTZ 依赖 IANA 时区库的用例在缺少 tzdata 的环境(Windows / scratch 镜像)
// 直接失败会产生误导,这里显式跳过。
func requireTZ(t *testing.T) {
	t.Helper()
	if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
		t.Skipf("运行环境缺少 tzdata,跳过依赖 IANA 时区库的用例: %v", err)
	}
}

func TestSettingsValidate(t *testing.T) {
	requireTZ(t)
	tests := []struct {
		name    string
		st      Settings
		wantErr bool
		errSub  string
	}{
		{
			name: "全 nil 合法(全部跟随 env)",
			st:   Settings{},
		},
		{
			name: "标准组合合法",
			st: Settings{
				DisplayTimezone:        ptrString("Asia/Shanghai"),
				SessionTTL:             ptrString("24h"),
				CookieSecure:           ptrBool(true),
				TrustedProxy:           ptrBool(false),
				DefaultUserPasswordLen: ptrInt(16),
			},
		},
		{
			name: "显式 false 也必须合法(不能因为零值被拒)",
			st:   Settings{CookieSecure: ptrBool(false), TrustedProxy: ptrBool(false)},
		},
		{
			name: "时区空串失败(要跟随默认请不提交该项)",
			st:   Settings{DisplayTimezone: ptrString("")},
			// optString 已经把空串转成 nil,这里是防御人工编辑文件的场景
			wantErr: true,
			errSub:  "display_timezone",
		},
		{
			name:    "时区非法失败",
			st:      Settings{DisplayTimezone: ptrString("Not/AZone")},
			wantErr: true,
			errSub:  "IANA",
		},
		{
			name: "TTL 下界 OK(5m)",
			st:   Settings{SessionTTL: ptrString("5m")},
		},
		{
			name: "TTL 低于下界失败",
			st:   Settings{SessionTTL: ptrString("1m")},
			// 再短的话管理员改一个参数就把自己踢下线
			wantErr: true,
			errSub:  "session_ttl",
		},
		{
			name: "TTL 上界 OK(720h)",
			st:   Settings{SessionTTL: ptrString("720h")},
		},
		{
			name:    "TTL 超上界失败",
			st:      Settings{SessionTTL: ptrString("2160h")},
			wantErr: true,
			errSub:  "session_ttl",
		},
		{
			name:    "TTL 非法字符串失败",
			st:      Settings{SessionTTL: ptrString("24")},
			wantErr: true,
			errSub:  "session_ttl",
		},
		{
			name: "密码长度下界 OK",
			st:   Settings{DefaultUserPasswordLen: ptrInt(MinPasswordLen)},
		},
		{
			name: "密码长度上界 OK",
			st:   Settings{DefaultUserPasswordLen: ptrInt(MaxPasswordLen)},
		},
		{
			name:    "密码长度低于下界失败",
			st:      Settings{DefaultUserPasswordLen: ptrInt(5)},
			wantErr: true,
			errSub:  "default_user_password_len",
		},
		{
			name:    "密码长度高于上界失败",
			st:      Settings{DefaultUserPasswordLen: ptrInt(65)},
			wantErr: true,
			errSub:  "default_user_password_len",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.st.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错,实际 nil")
				}
				if tc.errSub != "" && !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("错误信息 %q 不含期望子串 %q", err.Error(), tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望通过,实际报错: %v", err)
			}
		})
	}
}

func TestSettingsIsZero(t *testing.T) {
	var nilPtr *Settings
	if !nilPtr.IsZero() {
		t.Fatalf("nil 指针 IsZero 应为 true")
	}
	if !(&Settings{}).IsZero() {
		t.Fatalf("空结构 IsZero 应为 true")
	}
	// 显式 false 也是"设过",不算 zero —— 这是三态的核心。
	if (&Settings{CookieSecure: ptrBool(false)}).IsZero() {
		t.Fatalf("显式 cookie_secure=false 不应算 zero")
	}
	if (&Settings{DefaultUserPasswordLen: ptrInt(0)}).IsZero() {
		t.Fatalf("显式 password_len=0 不应算 zero")
	}
}

// TestSettingsJSONTriState 保护 omitempty 的行为契约:
//   - nil 字段不落盘(key 缺失 = 跟随 env)
//   - 显式零值必须落盘(key 存在 = 显式覆盖)
//
// 这条如果不成立,「跟随 env 默认」和「显式关掉」就分不出来了。
func TestSettingsJSONTriState(t *testing.T) {
	requireTZ(t)
	dir := t.TempDir()
	st := NewSettingsStoreWithDir(dir)

	if err := st.WriteSettings(Settings{
		DisplayTimezone: ptrString("Asia/Shanghai"),
		CookieSecure:    ptrBool(false),
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := generic["cookie_secure"]; !ok {
		t.Fatalf("显式 false 必须落盘,实际文件: %s", raw)
	}
	if got, ok := generic["cookie_secure"].(bool); !ok || got {
		t.Fatalf("cookie_secure 落盘值应为 false,实际: %v", generic["cookie_secure"])
	}
	for _, key := range []string{"session_ttl", "trusted_proxy", "default_user_password_len"} {
		if _, ok := generic[key]; ok {
			t.Fatalf("%s 未设置就不该落盘(nil = 跟随 env),实际文件: %s", key, raw)
		}
	}
	if got := generic["schema_version"]; got != float64(SettingsSchemaVersion) {
		t.Fatalf("schema_version 应为 %d,实际 %v", SettingsSchemaVersion, got)
	}
	if _, ok := generic["updated_at"]; !ok {
		t.Fatalf("updated_at 应由 WriteSettings 自动写入")
	}
}

func TestSettingsStoreLoadMissing(t *testing.T) {
	st := NewSettingsStoreWithDir(t.TempDir())

	got, err := st.LoadSettings()
	if err != nil {
		t.Fatalf("文件不存在不应报错,实际: %v", err)
	}
	if got != nil {
		t.Fatalf("文件不存在应返回 nil,实际 %+v", got)
	}
	if st.SettingsExists() {
		t.Fatalf("SettingsExists 应为 false")
	}
}

func TestSettingsStoreLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := NewSettingsStoreWithDir(dir)

	// 损坏必须报错而不是 silent 用出厂值 —— 否则用户以为改了其实没生效。
	if _, err := st.LoadSettings(); err == nil {
		t.Fatalf("JSON 损坏应报错")
	}
}

func TestSettingsStoreLoadInvalid(t *testing.T) {
	dir := t.TempDir()
	// 人工编辑写入非法时区
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"display_timezone":"Not/AZone"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st := NewSettingsStoreWithDir(dir)

	if _, err := st.LoadSettings(); err == nil {
		t.Fatalf("非法字段应报错(防御人工编辑)")
	}
}

func TestSettingsStoreRoundTrip(t *testing.T) {
	requireTZ(t)
	dir := t.TempDir()
	st := NewSettingsStoreWithDir(dir)

	in := Settings{
		DisplayTimezone:        ptrString("Asia/Hong_Kong"),
		SessionTTL:             ptrString("8h"),
		CookieSecure:           ptrBool(false),
		TrustedProxy:           ptrBool(true),
		DefaultUserPasswordLen: ptrInt(20),
	}
	if err := st.WriteSettings(in); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	out, err := st.LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if out == nil {
		t.Fatalf("LoadSettings 返回 nil")
	}
	if out.DisplayTimezone == nil || *out.DisplayTimezone != "Asia/Hong_Kong" {
		t.Fatalf("display_timezone 往返不一致: %+v", out.DisplayTimezone)
	}
	if out.SessionTTL == nil || *out.SessionTTL != "8h" {
		t.Fatalf("session_ttl 往返不一致: %+v", out.SessionTTL)
	}
	// 这一条最关键:false 必须还是 false,不能变回 nil
	if out.CookieSecure == nil || *out.CookieSecure != false {
		t.Fatalf("cookie_secure=false 往返后丢失了显式覆盖语义")
	}
	if out.TrustedProxy == nil || *out.TrustedProxy != true {
		t.Fatalf("trusted_proxy 往返不一致")
	}
	if out.DefaultUserPasswordLen == nil || *out.DefaultUserPasswordLen != 20 {
		t.Fatalf("default_user_password_len 往返不一致")
	}
	if out.UpdatedAt == 0 {
		t.Fatalf("UpdatedAt 未写入")
	}
}

// TestSettingsStoreWriteAtomic 校验:
//   - 目录被自动创建且权限 0700
//   - 文件权限 0600
//   - 不留临时文件(重启时不读到半截文件)
func TestSettingsStoreWriteAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "panel-state")
	st := NewSettingsStoreWithDir(dir)

	if err := st.WriteSettings(Settings{SessionTTL: ptrString("24h")}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("目录未自动创建: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("目录权限应为 0700,实际 %o", perm)
	}

	fi, err := os.Stat(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("文件权限应为 0600,实际 %o", perm)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "settings.json" {
		t.Fatalf("目录应只剩 settings.json,实际 %d 项", len(entries))
	}
}

func TestSettingsStoreWriteRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	st := NewSettingsStoreWithDir(dir)

	if err := st.WriteSettings(Settings{DefaultUserPasswordLen: ptrInt(1)}); err == nil {
		t.Fatalf("非法密码长度应被 WriteSettings 拒绝")
	}
	// 校验失败发生在写盘之前,不能留下文件
	if st.SettingsExists() {
		t.Fatalf("校验失败不应产生文件")
	}
}

func TestSettingsStoreReadCacheFallback(t *testing.T) {
	dir := t.TempDir()
	st := NewSettingsStoreWithDir(dir)

	if err := st.WriteSettings(Settings{SessionTTL: ptrString("1h")}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	// 读缓存(刚写完)
	got, err := st.ReadSettings()
	if err != nil || got == nil || got.SessionTTL == nil || *got.SessionTTL != "1h" {
		t.Fatalf("ReadSettings 缓存路径异常: %+v err=%v", got, err)
	}

	// 缓存空(未 Load 过)→ 降级读磁盘
	fresh := NewSettingsStoreWithDir(dir)
	got2, err := fresh.ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings 磁盘回退报错: %v", err)
	}
	if got2 == nil || got2.SessionTTL == nil || *got2.SessionTTL != "1h" {
		t.Fatalf("ReadSettings 磁盘回退异常: %+v", got2)
	}
}

func TestSettingsStoreClear(t *testing.T) {
	dir := t.TempDir()
	st := NewSettingsStoreWithDir(dir)

	if err := st.WriteSettings(Settings{SessionTTL: ptrString("24h")}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	if !st.SettingsExists() {
		t.Fatalf("写入后 SettingsExists 应为 true")
	}

	if err := st.ClearSettings(); err != nil {
		t.Fatalf("ClearSettings: %v", err)
	}
	if st.SettingsExists() {
		t.Fatalf("清除后文件应不存在")
	}
	// 内存缓存也要清,否则清除后 handler 还读到旧值
	got, err := st.ReadSettings()
	if err != nil {
		t.Fatalf("清除后 ReadSettings 报错: %v", err)
	}
	if got != nil {
		t.Fatalf("清除后 ReadSettings 应返回 nil,实际 %+v", got)
	}
	// 幂等:重复清除不报错
	if err := st.ClearSettings(); err != nil {
		t.Fatalf("重复 ClearSettings 应幂等,实际: %v", err)
	}
}

// TestSettingsStoreNewConstructorDir 默认构造函数指向 Dir,防止误写测试目录。
func TestSettingsStoreNewConstructorDir(t *testing.T) {
	if got := NewSettingsStore().settingsPath(); got != filepath.Join(Dir, "settings.json") {
		t.Fatalf("默认路径应为 %s,实际 %s", filepath.Join(Dir, "settings.json"), got)
	}
}

// ---------------------------------------------------------------------------
// v2.86 批次 E:新增 8 个字段的三态 / 校验 / 落盘
// ---------------------------------------------------------------------------

func TestSettingsValidateBatchE(t *testing.T) {
	ok := func(name string, s Settings) {
		t.Helper()
		if err := s.Validate(); err != nil {
			t.Fatalf("%s 应通过校验,实际 %v", name, err)
		}
	}
	bad := func(name, wantField string, s Settings) {
		t.Helper()
		err := s.Validate()
		if err == nil {
			t.Fatalf("%s 应被拒绝", name)
		}
		if !strings.Contains(err.Error(), wantField) {
			t.Fatalf("%s 错误信息应点名 %s,实际 %v", name, wantField, err)
		}
	}

	sp := func(s string) *string { return &s }
	ip := func(i int) *int { return &i }
	bp := func(b bool) *bool { return &b }

	ok("日志级别", Settings{LogLevel: sp("debug")})
	ok("日志格式", Settings{LogFormat: sp("json")})
	ok("nft v4", Settings{NftFamily: sp("v4")})
	ok("nft v6", Settings{NftFamily: sp("v6")})
	ok("审计天数下界", Settings{AuditRetentionDays: ip(MinAuditRetentionDays)})
	ok("审计天数上界", Settings{AuditRetentionDays: ip(MaxAuditRetentionDays)})
	ok("限速下界", Settings{ConnLimitPerIP: ip(1), HashLimitPerMin: ip(1)})
	ok("限速上界", Settings{ConnLimitPerIP: ip(MaxConnLimitPerIP), HashLimitPerMin: ip(MaxHashLimitPerMin)})
	ok("显式 false", Settings{DisableMSSClamp: bp(false), DisableDDoSProtection: bp(false)})

	bad("空日志级别", "log_level", Settings{LogLevel: sp("")})
	bad("拼错的日志级别", "log_level", Settings{LogLevel: sp("flase")})
	bad("未知日志格式", "log_format", Settings{LogFormat: sp("yaml")})
	// dual 是 DDNS 的 family 值,不是 nft 的地址族 —— 混进来必须拒绝
	bad("nft 用 dual", "nft_family", Settings{NftFamily: sp("dual")})
	bad("nft 用 ipv6", "nft_family", Settings{NftFamily: sp("ipv6")})
	bad("审计天数 0", "audit_retention_days", Settings{AuditRetentionDays: ip(0)})
	bad("审计天数负数", "audit_retention_days", Settings{AuditRetentionDays: ip(-1)})
	bad("审计天数过大", "audit_retention_days", Settings{AuditRetentionDays: ip(MaxAuditRetentionDays + 1)})
	bad("并发 0", "connlimit_per_ip", Settings{ConnLimitPerIP: ip(0)})
	bad("并发过大", "connlimit_per_ip", Settings{ConnLimitPerIP: ip(MaxConnLimitPerIP + 1)})
	bad("速率 0", "hashlimit_per_min", Settings{HashLimitPerMin: ip(0)})
}

func TestSettingsIsZeroBatchE(t *testing.T) {
	sp := func(s string) *string { return &s }
	ip := func(i int) *int { return &i }
	bp := func(b bool) *bool { return &b }

	cases := map[string]Settings{
		"log_level":               {LogLevel: sp("debug")},
		"log_format":              {LogFormat: sp("json")},
		"audit_retention_days":    {AuditRetentionDays: ip(180)},
		"connlimit_per_ip":        {ConnLimitPerIP: ip(80)},
		"hashlimit_per_min":       {HashLimitPerMin: ip(40)},
		"disable_mss_clamp":       {DisableMSSClamp: bp(false)},
		"disable_ddos_protection": {DisableDDoSProtection: bp(true)},
		"nft_family":              {NftFamily: sp("v6")},
	}
	for name, s := range cases {
		if s.IsZero() {
			t.Fatalf("只设了 %s 时 IsZero 应为 false", name)
		}
	}
	// 显式 false 也算"设过" —— 否则用户点保存会被当成"什么都没改"而清文件
	if (&Settings{DisableMSSClamp: bp(false)}).IsZero() {
		t.Fatalf("显式 false 不应被判为 IsZero")
	}
}

func TestSettingsWriteReadRoundTripBatchE(t *testing.T) {
	dir := t.TempDir()
	st := NewSettingsStoreWithDir(dir)

	sp := func(s string) *string { return &s }
	ip := func(i int) *int { return &i }
	bp := func(b bool) *bool { return &b }

	in := Settings{
		LogLevel:              sp("debug"),
		LogFormat:             sp("json"),
		AuditRetentionDays:    ip(180),
		ConnLimitPerIP:        ip(120),
		HashLimitPerMin:       ip(60),
		DisableMSSClamp:       bp(false),
		DisableDDoSProtection: bp(true),
		NftFamily:             sp("v6"),
	}
	if err := st.WriteSettings(in); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	out, err := NewSettingsStoreWithDir(dir).LoadSettings()
	if err != nil || out == nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if out.LogLevel == nil || *out.LogLevel != "debug" {
		t.Fatalf("log_level 往返失败: %+v", out.LogLevel)
	}
	if out.LogFormat == nil || *out.LogFormat != "json" {
		t.Fatalf("log_format 往返失败: %+v", out.LogFormat)
	}
	if out.AuditRetentionDays == nil || *out.AuditRetentionDays != 180 {
		t.Fatalf("audit_retention_days 往返失败")
	}
	if out.ConnLimitPerIP == nil || *out.ConnLimitPerIP != 120 {
		t.Fatalf("connlimit_per_ip 往返失败")
	}
	if out.HashLimitPerMin == nil || *out.HashLimitPerMin != 60 {
		t.Fatalf("hashlimit_per_min 往返失败")
	}
	if out.DisableMSSClamp == nil || *out.DisableMSSClamp != false {
		t.Fatalf("disable_mss_clamp 显式 false 应落盘并往返,实际 %+v", out.DisableMSSClamp)
	}
	if out.DisableDDoSProtection == nil || *out.DisableDDoSProtection != true {
		t.Fatalf("disable_ddos_protection 往返失败")
	}
	if out.NftFamily == nil || *out.NftFamily != "v6" {
		t.Fatalf("nft_family 往返失败")
	}
	if out.SchemaVersion != SettingsSchemaVersion {
		t.Fatalf("schema_version 应为 %d,实际 %d", SettingsSchemaVersion, out.SchemaVersion)
	}
}

// TestSettingsBackwardCompatBatchE 老文件(只有批次 D 的 5 个 key)必须照常读。
//
// 这是本次扩展最容易踩的坑:如果新增字段没加 omitempty,或者 LoadSettings
// 对缺失字段报错,老用户的 settings.json 就会在升级后直接失效。
func TestSettingsBackwardCompatBatchE(t *testing.T) {
	dir := t.TempDir()
	old := `{
  "schema_version": 1,
  "display_timezone": "Asia/Shanghai",
  "session_ttl": "8h",
  "cookie_secure": true,
  "trusted_proxy": false,
  "default_user_password_len": 16,
  "updated_at": 1700000000
}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(old), 0o600); err != nil {
		t.Fatalf("写老文件: %v", err)
	}

	st, err := NewSettingsStoreWithDir(dir).LoadSettings()
	if err != nil {
		t.Fatalf("老文件必须能正常加载: %v", err)
	}
	if st == nil {
		t.Fatalf("老文件不应返回 nil")
	}
	if st.DisplayTimezone == nil || *st.DisplayTimezone != "Asia/Shanghai" {
		t.Fatalf("老字段应保留")
	}
	// 新增的 8 个字段全是 nil = 跟随 env,行为跟升级前完全一致
	if st.LogLevel != nil || st.LogFormat != nil || st.AuditRetentionDays != nil ||
		st.ConnLimitPerIP != nil || st.HashLimitPerMin != nil ||
		st.DisableMSSClamp != nil || st.DisableDDoSProtection != nil || st.NftFamily != nil {
		t.Fatalf("老文件里新增字段应全为 nil,实际 %+v", st)
	}
	// 老文件里批次 D 的 5 项都有值 → IsZero 必须为 false,
	// 否则 handler 里的 "全空就清文件" 分支会在老用户点保存时误删配置。
	if st.IsZero() {
		t.Fatalf("老文件有值,IsZero 应为 false")
	}
}

// TestSettingsInvalidBatchEOnLoad 人工编辑写坏的值必须在加载时报错,
// 而不是静默用出厂值 —— 后者会让用户以为设置生效了。
func TestSettingsInvalidBatchEOnLoad(t *testing.T) {
	dir := t.TempDir()
	bad := `{"schema_version":1,"connlimit_per_ip":0}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(bad), 0o600); err != nil {
		t.Fatalf("写坏文件: %v", err)
	}
	if _, err := NewSettingsStoreWithDir(dir).LoadSettings(); err == nil {
		t.Fatalf("越界值应在加载时报错")
	}
}
