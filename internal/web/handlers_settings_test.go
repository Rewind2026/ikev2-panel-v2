// v2.86 批次 D:「面板设置」handler 单元测试
//
// 覆盖:表单三态解析 / env 兜底合并 / 运行期生效 / 明文 HTTP 闸门 /
//
//	预配置下拉的"当前值不在列表里"追加逻辑 / 时长归一化。
package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/config"
	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
)

// newSettingsServer 造一个带 settings store 的 Server(不注入 Env*,用于测出厂兜底)。
func newSettingsServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(t)
	srv.SettingsStore = panelstate.NewSettingsStoreWithDir(t.TempDir())
	// applyRuntimeSettings 会写 auth 包里的 trustedProxyRuntime 全局量,
	// 测试结束复位,避免影响同包其它用例(优先级:测试 override > 面板 runtime > env)。
	t.Cleanup(func() { auth.SetTrustedProxy(false) })
	return srv
}

// requireTZ 写 settings.json 会走 Validate → time.LoadLocation,
// 缺少 tzdata 的环境直接跳过,避免把环境问题报成代码回归。
func requireTZ(t *testing.T) {
	t.Helper()
	if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
		t.Skipf("运行环境缺少 tzdata,跳过依赖 IANA 时区库的用例: %v", err)
	}
}

func TestOptString(t *testing.T) {
	tests := []struct {
		name string
		form url.Values
		want *string // nil = 跟随 env
	}{
		{name: "key 缺失 → nil", form: url.Values{}},
		{name: "空串 → nil", form: url.Values{"tz": {""}}},
		{name: "全空格 → nil", form: url.Values{"tz": {"  "}}},
		{name: "有值 → 指针", form: url.Values{"tz": {"Asia/Shanghai"}}, want: ptrS("Asia/Shanghai")},
		{name: "多值取最后一个(浏览器 hidden+select 语义)", form: url.Values{"tz": {"", "UTC"}}, want: ptrS("UTC")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := optString(tc.form, "tz")
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("期望 nil,实际 %q", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("期望 %q,实际 nil", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("期望 %q,实际 %q", *tc.want, *got)
			}
		})
	}
}

func TestOptBool(t *testing.T) {
	tr, fa := true, false
	tests := []struct {
		name string
		form url.Values
		want *bool
	}{
		// 未勾选的 checkbox + hidden 空值兜底 → 显式 false
		{name: "hidden 兜底未勾选 → false", form: url.Values{"k": {""}}, want: &fa},
		// hidden 在前、checkbox 在后,勾选后提交两个值
		{name: "勾选(hidden+checkbox) → true", form: url.Values{"k": {"", "true"}}, want: &tr},
		{name: "仅 checkbox 勾选 → true", form: url.Values{"k": {"true"}}, want: &tr},
		// "跟随默认"复选框优先于一切
		{name: "follow 优先 → nil", form: url.Values{"k": {"true"}, "k_follow": {"1"}}, want: nil},
		{name: "follow + 未勾选主开关 → nil", form: url.Values{"k": {""}, "k_follow": {"1"}}, want: nil},
		// key 完全没提交(disabled checkbox 等)→ nil
		{name: "key 缺失 → nil", form: url.Values{}, want: nil},
		{name: "值为空数组 → nil", form: url.Values{"k": {}}, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := optBool(tc.form, "k")
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("期望 nil,实际 %t", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("期望 %t,实际 nil", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("期望 %t,实际 %t", *tc.want, *got)
			}
		})
	}
}

func TestOptInt(t *testing.T) {
	tests := []struct {
		name    string
		form    url.Values
		wantNil bool // true = 期望解析为 nil(跟随默认)
		wantErr bool // true = 期望报错(非法输入被拦截)
		want    int  // wantNil=false && wantErr=false 时的期望值
	}{
		{name: "key 缺失 → nil", form: url.Values{}, wantNil: true},
		{name: "空串 → nil", form: url.Values{"n": {""}}, wantNil: true},
		{name: "全空格 → nil", form: url.Values{"n": {"   "}}, wantNil: true},
		// v2.86 批次 E:非数字必须报错,不能静默退化成"跟随 env"。
		// 退化成 nil 时用户填了 abc 却看到"保存成功",页面显示的还是 env 值,
		// 心里以为已经改过 —— 限速阈值这种参数这样很危险。
		{name: "非数字 → 报错", form: url.Values{"n": {"abc"}}, wantErr: true},
		{name: "小数 → 报错", form: url.Values{"n": {"16.5"}}, wantErr: true},
		{name: "混入单位 → 报错", form: url.Values{"n": {"50个"}}, wantErr: true},
		{name: "合法数字", form: url.Values{"n": {"16"}}, want: 16},
		{name: "前后空格仍可解析", form: url.Values{"n": {" 16 "}}, want: 16},
		// 范围校验交给 Validate,optInt 只负责"是不是整数"
		{name: "负数解析成功(范围校验交给 Validate)", form: url.Values{"n": {"-3"}}, want: -3},
		{name: "零解析成功", form: url.Values{"n": {"0"}}, want: 0},
		// 多个值取最后一个(hidden 兜底在前、真实输入在后)的浏览器语义
		{name: "多值取最后一个", form: url.Values{"n": {"", "42"}}, want: 42},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := optInt(tc.form, "n")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错,实际 nil(值 %v)", got)
				}
				// 报错时不能同时给出值 —— 否则调用方可能误用
				if got != nil {
					t.Fatalf("报错时不应返回指针,实际 %d", *got)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望不报错,实际 %v", err)
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("期望 nil,实际 %d", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("期望 %d,实际 nil", tc.want)
			}
			if *got != tc.want {
				t.Fatalf("期望 %d,实际 %d", tc.want, *got)
			}
		})
	}
}

func ptrS(s string) *string { return &s }
func ptrB(b bool) *bool     { return &b }

// TestEffectiveSettingsFactoryDefaults 未注入 Env* 时全部落到出厂值,
// 且每项 source 标记都是"未显式设置"。
func TestEffectiveSettingsFactoryDefaults(t *testing.T) {
	srv := newSettingsServer(t)

	eff := srv.effectiveSettings()
	if eff.displayTimezone != "UTC" {
		t.Fatalf("出厂时区应为 UTC,实际 %q", eff.displayTimezone)
	}
	if eff.sessionTTL != 24*time.Hour {
		t.Fatalf("出厂会话时长应为 24h,实际 %s", eff.sessionTTL)
	}
	if eff.defaultUserPasswordLen != defaultPasswordLen {
		t.Fatalf("出厂密码长度应为 %d,实际 %d", defaultPasswordLen, eff.defaultUserPasswordLen)
	}
	if eff.displayTimezoneSet || eff.sessionTTLSet || eff.cookieSecureSet ||
		eff.trustedProxySet || eff.passwordLenSet {
		t.Fatalf("未写 settings.json 时不应有任何 Set 标记")
	}
}

// TestEffectiveSettingsPanelOverridesEnv 验证优先级 panelstate > env。
func TestEffectiveSettingsPanelOverridesEnv(t *testing.T) {
	requireTZ(t)
	srv := newSettingsServer(t)
	srv.EnvDisplayTimezone = "Asia/Tokyo"
	srv.EnvSessionTTL = 8 * time.Hour
	srv.EnvCookieSecure = true
	srv.EnvTrustedProxy = true
	srv.EnvDefaultUserPasswordLen = 12

	// 只有时区被面板接管,其余跟随 env
	tz := "Europe/Berlin"
	if err := srv.SettingsStore.WriteSettings(panelstate.Settings{DisplayTimezone: &tz}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	eff := srv.effectiveSettings()
	if eff.displayTimezone != "Europe/Berlin" || !eff.displayTimezoneSet {
		t.Fatalf("时区应被面板覆盖,实际 %q set=%t", eff.displayTimezone, eff.displayTimezoneSet)
	}
	if eff.sessionTTL != 8*time.Hour || eff.sessionTTLSet {
		t.Fatalf("会话时长应跟随 env,实际 %s set=%t", eff.sessionTTL, eff.sessionTTLSet)
	}
	if !eff.cookieSecure || eff.cookieSecureSet {
		t.Fatalf("cookie_secure 应跟随 env=true,实际 %t set=%t", eff.cookieSecure, eff.cookieSecureSet)
	}

	// 显式 false 必须压过 env=true —— 三态里最容易写错的一条
	if err := srv.SettingsStore.WriteSettings(panelstate.Settings{
		DisplayTimezone: &tz,
		CookieSecure:    ptrB(false),
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	eff2 := srv.effectiveSettings()
	if eff2.cookieSecure {
		t.Fatalf("面板显式 cookie_secure=false 应压过 env=true")
	}
	if !eff2.cookieSecureSet {
		t.Fatalf("cookieSecureSet 应为 true")
	}
	if pickSource(eff2.cookieSecureSet) != "panelstate" {
		t.Fatalf("pickSource(true) 应为 panelstate")
	}
	if pickSource(false) != "env" {
		t.Fatalf("pickSource(false) 应为 env")
	}
}

// TestApplyRuntimeSettingsImmediate 保护"UI 改完不用重启"这条承诺。
func TestApplyRuntimeSettingsImmediate(t *testing.T) {
	srv := newSettingsServer(t)
	srv.SessionTTL = time.Hour
	srv.DefaultUserPasswordLen = 20

	srv.applyRuntimeSettings(settingsEffective{
		sessionTTL:             30 * time.Minute,
		cookieSecure:           false,
		trustedProxy:           true,
		defaultUserPasswordLen: 24,
	})

	if got := srv.sessionTTL(); got != 30*time.Minute {
		t.Fatalf("sessionTTL 应立即变为 30m,实际 %s", got)
	}
	if srv.cookieSecure() {
		t.Fatalf("cookieSecure 应立即变为 false")
	}
	if got := srv.randomPasswordLen(); got != 24 {
		t.Fatalf("randomPasswordLen 应立即变为 24,实际 %d", got)
	}
	// 清掉运行期覆盖后回落到启动期字段
	srv.settingsRT.Store(nil)
	if got := srv.sessionTTL(); got != time.Hour {
		t.Fatalf("无运行期快照时应回落 SessionTTL,实际 %s", got)
	}
	if got := srv.randomPasswordLen(); got != 20 {
		t.Fatalf("无运行期快照时应回落 DefaultUserPasswordLen,实际 %d", got)
	}
}

func TestRandomPasswordLenFactoryFallback(t *testing.T) {
	srv := newTestServer(t) // 未注入 DefaultUserPasswordLen
	if got := srv.randomPasswordLen(); got != defaultPasswordLen {
		t.Fatalf("未配置时应返回出厂值 %d,实际 %d", defaultPasswordLen, got)
	}
}

// TestEnablePlaintextHTTPForcesInsecure 覆盖"明文入口打开后,
// 用户在 UI 勾 Cookie Secure 也不会把自己锁在门外"。
func TestEnablePlaintextHTTPForcesInsecure(t *testing.T) {
	srv := newSettingsServer(t)
	srv.Secure = true
	srv.settingsRT.Store(&settingsRuntime{cookieSecure: true, sessionTTL: time.Hour})

	srv.EnablePlaintextHTTP()

	if srv.Secure {
		t.Fatalf("EnablePlaintextHTTP 应把 Secure 压成 false")
	}
	if !srv.PlainHTTPEnabled {
		t.Fatalf("PlainHTTPEnabled 应为 true")
	}
	if srv.cookieSecure() {
		t.Fatalf("运行期快照里的 cookieSecure 也应被压成 false")
	}

	// 即使 effectiveSettings 算出 true(比如 env 写着 true),apply 也要压平
	srv.applyRuntimeSettings(settingsEffective{cookieSecure: true, sessionTTL: time.Hour})
	if srv.cookieSecure() {
		t.Fatalf("明文入口下 applyRuntimeSettings 必须压平 cookieSecure")
	}
}

// TestHandleSettingsSaveRejectsCookieSecureOnPlainHTTP 写盘前就要拦住,
// 不能先落盘再"启动时压平"(排查时文件与实际不一致更难查)。
func TestHandleSettingsSaveRejectsCookieSecureOnPlainHTTP(t *testing.T) {
	srv := newSettingsServer(t)
	srv.PlainHTTPEnabled = true

	form := url.Values{"cookie_secure": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "/api/settings/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()

	srv.handleSettingsSave(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应返回 400,实际 %d", rec.Code)
	}
	if srv.SettingsStore.SettingsExists() {
		t.Fatalf("被拒绝的保存不能落盘")
	}
}

func TestHandleSettingsSaveWritesAndApplies(t *testing.T) {
	requireTZ(t)
	srv := newSettingsServer(t)
	srv.EnvSessionTTL = 24 * time.Hour
	srv.EnvDefaultUserPasswordLen = 12

	form := url.Values{
		"display_timezone":          {"Asia/Shanghai"},
		"session_ttl":               {"8h"},
		"cookie_secure":             {"", "true"},
		"trusted_proxy":             {""},
		"default_user_password_len": {"20"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/settings/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()

	srv.handleSettingsSave(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("保存应成功,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !srv.SettingsStore.SettingsExists() {
		t.Fatalf("settings.json 应已写入")
	}
	st, err := srv.SettingsStore.ReadSettings()
	if err != nil || st == nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if st.SessionTTL == nil || *st.SessionTTL != "8h" {
		t.Fatalf("session_ttl 落盘错误: %+v", st.SessionTTL)
	}
	if st.TrustedProxy == nil || *st.TrustedProxy != false {
		t.Fatalf("未勾选的 trusted_proxy 应显式落盘 false,实际 %+v", st.TrustedProxy)
	}
	// 运行期立即生效
	if got := srv.sessionTTL(); got != 8*time.Hour {
		t.Fatalf("sessionTTL 未运行期生效: %s", got)
	}
	if got := srv.randomPasswordLen(); got != 20 {
		t.Fatalf("password_len 未运行期生效: %d", got)
	}
	if !srv.cookieSecure() {
		t.Fatalf("cookie_secure=true 未运行期生效")
	}
}

func TestHandleSettingsSaveInvalidValue(t *testing.T) {
	requireTZ(t)
	srv := newSettingsServer(t)

	// 时区不在下拉预置里,但 Validate 通过(合法 IANA 名)→ 应写入
	req := httptest.NewRequest(http.MethodPost, "/api/settings/save",
		strings.NewReader(url.Values{"display_timezone": {"Europe/Paris"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	srv.handleSettingsSave(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法 IANA 名应写入,实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 密码长度越界 → Validate 拒绝,不落盘
	srv2 := newSettingsServer(t)
	req2 := httptest.NewRequest(http.MethodPost, "/api/settings/save",
		strings.NewReader(url.Values{"default_user_password_len": {"2"}}.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Accept", "application/json")
	rec2 := httptest.NewRecorder()
	srv2.handleSettingsSave(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("越界密码长度应 400,实际 %d", rec2.Code)
	}
	if srv2.SettingsStore.SettingsExists() {
		t.Fatalf("越界值不应落盘")
	}
}

// TestHandleSettingsSaveAllEmptyClears 所有项都表达"未设置"时,应清文件而不是写空配置。
//
// 注意布尔的"未设置"表达:**不是**空字符串。模板里 hidden 空值兜底会让
// 未勾选的 checkbox 提交 cookie_secure="",optBool 据此判定为"显式 false"
// (这是三态里 false 必须能显式落盘的关键)。
// 真正让某项回到 env 的是 *_follow 复选框(optBool 优先检查它)或干脆不提交该 key。
func TestHandleSettingsSaveAllEmptyClears(t *testing.T) {
	requireTZ(t)

	// 用例 1:key 完全不提交(API 调用方 / 明文 HTTP 下 checkbox 被禁用)
	srv := newSettingsServer(t)
	tz := "Asia/Shanghai"
	if err := srv.SettingsStore.WriteSettings(panelstate.Settings{DisplayTimezone: &tz}); err != nil {
		t.Fatalf("前置写入失败: %v", err)
	}
	srv.applyRuntimeSettings(srv.effectiveSettings())

	req := httptest.NewRequest(http.MethodPost, "/api/settings/save",
		strings.NewReader(url.Values{
			"display_timezone":          {""},
			"session_ttl":               {""},
			"default_user_password_len": {""},
		}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()

	srv.handleSettingsSave(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应成功,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if srv.SettingsStore.SettingsExists() {
		t.Fatalf("全部未设置时应清文件而不是写空配置")
	}
	if got := srv.effectiveSettings().displayTimezone; got != "UTC" {
		t.Fatalf("清除后应回退出厂时区 UTC,实际 %q", got)
	}

	// 用例 2:勾了"跟随 env 默认"复选框 = 同样回到未设置
	srv2 := newSettingsServer(t)
	yes := true
	if err := srv2.SettingsStore.WriteSettings(panelstate.Settings{CookieSecure: &yes}); err != nil {
		t.Fatalf("前置写入失败: %v", err)
	}
	srv2.applyRuntimeSettings(srv2.effectiveSettings())

	req2 := httptest.NewRequest(http.MethodPost, "/api/settings/save",
		strings.NewReader(url.Values{
			"display_timezone":     {""},
			"session_ttl":          {""},
			"cookie_secure_follow": {"1"},
			"trusted_proxy_follow": {"1"},
		}.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Accept", "application/json")
	rec2 := httptest.NewRecorder()

	srv2.handleSettingsSave(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("应成功,实际 %d body=%s", rec2.Code, rec2.Body.String())
	}
	if srv2.SettingsStore.SettingsExists() {
		t.Fatalf("勾选「跟随 env 默认」后应清文件,而不是把 true 固化成 false")
	}
	if srv2.cookieSecure() {
		t.Fatalf("清除后 cookie_secure 应回退到 env/false")
	}
}

func TestHandleSettingsClear(t *testing.T) {
	srv := newSettingsServer(t)
	srv.EnvSessionTTL = 6 * time.Hour
	ttl := "1h"
	if err := srv.SettingsStore.WriteSettings(panelstate.Settings{SessionTTL: &ttl}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	srv.applyRuntimeSettings(srv.effectiveSettings())
	if srv.sessionTTL() != time.Hour {
		t.Fatalf("前置运行期值异常: %s", srv.sessionTTL())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/settings/clear", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	srv.handleSettingsClear(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("清除应成功,实际 %d", rec.Code)
	}
	if got := srv.sessionTTL(); got != 6*time.Hour {
		t.Fatalf("清除后应回落到 env 的 6h,实际 %s", got)
	}
}

func TestHandleSettingsStatus(t *testing.T) {
	requireTZ(t)
	srv := newSettingsServer(t)
	srv.EnvDisplayTimezone = "Asia/Tokyo"
	srv.EnvSessionTTL = 12 * time.Hour
	tz := "Europe/London"

	req := httptest.NewRequest(http.MethodGet, "/api/settings/status", nil)
	rec := httptest.NewRecorder()
	srv.handleSettingsStatus(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"source":"env-default"`) {
		t.Fatalf("未配置时 source 应为 env-default,实际: %s", body)
	}
	if !strings.Contains(body, `"display_timezone":"Asia/Tokyo"`) {
		t.Fatalf("status 应返回 env 生效值,实际: %s", body)
	}
	if !strings.Contains(body, `"display_timezone_source":"env"`) {
		t.Fatalf("display_timezone_source 应为 env,实际: %s", body)
	}

	if err := srv.SettingsStore.WriteSettings(panelstate.Settings{DisplayTimezone: &tz}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	rec2 := httptest.NewRecorder()
	srv.handleSettingsStatus(rec2, httptest.NewRequest(http.MethodGet, "/api/settings/status", nil))

	body2 := rec2.Body.String()
	if !strings.Contains(body2, `"source":"panelstate"`) {
		t.Fatalf("已配置时整体 source 应为 panelstate,实际: %s", body2)
	}
	if !strings.Contains(body2, `"display_timezone_source":"panelstate"`) {
		t.Fatalf("被接管的项 source 应为 panelstate,实际: %s", body2)
	}
	if !strings.Contains(body2, `"session_ttl_source":"env"`) {
		t.Fatalf("未被接管的项 source 仍应是 env,实际: %s", body2)
	}
}

func TestBuildTimezoneOptions(t *testing.T) {
	// 1) 当前值在预置里 → 选中它,"跟随默认"不选中
	opts := buildTimezoneOptions("Asia/Shanghai")
	if opts[0].Value != "" || opts[0].Label != "跟随 env 默认" {
		t.Fatalf("首项应为「跟随 env 默认」,实际 %+v", opts[0])
	}
	if opts[0].Selected {
		t.Fatalf("当前值命中预置时,首项不应选中")
	}
	if !optionSelected(opts, "Asia/Shanghai") {
		t.Fatalf("Asia/Shanghai 应被选中")
	}

	// 2) 当前值不在预置里(如 .env 写了 Asia/Chongqing)
	//    必须追加一条"当前生效",否则浏览器会显示首项,用户一保存就静默改了时区
	opts2 := buildTimezoneOptions("Asia/Chongqing")
	if !opts2[0].Selected {
		t.Fatalf("当前值不在预置时,「跟随默认」应被选中")
	}
	if !optionSelected(opts2, "Asia/Chongqing") {
		t.Fatalf("必须追加当前生效值 Asia/Chongqing")
	}
	found := false
	for _, o := range opts2 {
		if o.Value == "Asia/Chongqing" && !strings.HasPrefix(o.Label, "当前生效") {
			t.Fatalf("追加项文案应标明「当前生效」,实际 %q", o.Label)
		}
		if o.Value == "Asia/Chongqing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未追加当前值")
	}

	// 3) 命中预置时不追加重复项
	opts3 := buildTimezoneOptions("UTC")
	for _, o := range opts3 {
		if o.Value == "UTC" && strings.HasPrefix(o.Label, "当前生效") {
			t.Fatalf("命中预置时不应追加重复的「当前生效」项")
		}
	}
}

func TestBuildSessionTTLOptions(t *testing.T) {
	opts := buildSessionTTLOptions("24h")
	if opts[0].Selected {
		t.Fatalf("命中预置时首项不应选中")
	}
	if !optionSelected(opts, "24h") {
		t.Fatalf("24h 应被选中")
	}
	// 不在预置里的时长(如 env 写了 45m)同样要追加
	opts2 := buildSessionTTLOptions("45m")
	if !opts2[0].Selected {
		t.Fatalf("不在预置时首项应选中")
	}
	if !optionSelected(opts2, "45m") {
		t.Fatalf("必须追加当前生效值 45m")
	}
}

func optionSelected(opts []settingsOption, value string) bool {
	for _, o := range opts {
		if o.Value == value {
			return o.Selected
		}
	}
	return false
}

// TestNormalizeTTLString 直接用 Duration.String() 会把 30m 变成 "30m0s",
// 在下拉里既不匹配预置也会显示成一条多余的"当前生效"项。
func TestNormalizeTTLString(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{in: 30 * time.Minute, want: "30m"},
		{in: time.Hour, want: "1h"},
		{in: 8 * time.Hour, want: "8h"},
		{in: 24 * time.Hour, want: "24h"},
		{in: 168 * time.Hour, want: "168h"},
		{in: 720 * time.Hour, want: "720h"},
		{in: 0, want: "24h"},
		{in: -time.Hour, want: "24h"},
		{in: 45 * time.Minute, want: "45m0s"}, // 不在预置里,原样输出
	}
	for _, tc := range tests {
		if got := normalizeTTLString(tc.in); got != tc.want {
			t.Fatalf("normalizeTTLString(%s) = %q,期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestLoadSettingsStatusTemplateData 校验卡片数据:三态标记 + env 对照 + 边界。
func TestLoadSettingsStatusTemplateData(t *testing.T) {
	requireTZ(t)
	srv := newSettingsServer(t)
	srv.EnvDisplayTimezone = "Asia/Tokyo"
	srv.EnvSessionTTL = 24 * time.Hour
	srv.EnvDefaultUserPasswordLen = 12

	d := loadSettingsStatus(srv)
	if d.Configured {
		t.Fatalf("未写入时 Configured 应为 false")
	}
	if d.EnvDisplayTimezone != "Asia/Tokyo" {
		t.Fatalf("EnvDisplayTimezone 对照值不对: %q", d.EnvDisplayTimezone)
	}
	if d.PasswordLenMin != panelstate.MinPasswordLen || d.PasswordLenMax != panelstate.MaxPasswordLen {
		t.Fatalf("密码长度边界应与 panelstate 同源: %d~%d", d.PasswordLenMin, d.PasswordLenMax)
	}
	if len(d.TimezoneOptions) != len(presetTimezones)+1 {
		t.Fatalf("env 值命中预置时选项数应为 预置+1,实际 %d", len(d.TimezoneOptions))
	}
	// env 值命中预置时首项"跟随 env 默认"不该选中 —— 反过来会导致用户
	// 只是打开卡片点保存,就把 env 的时区固化成面板设置(静默改值)。
	if d.TimezoneOptions[0].Selected {
		t.Fatalf("env 时区命中预置时首项不该选中,实际 %+v", d.TimezoneOptions[0])
	}
	if !optionSelected(d.TimezoneOptions, "Asia/Tokyo") {
		t.Fatalf("env 时区命中预置时应选中 Asia/Tokyo 那一项")
	}

	tz := "America/New_York"
	if err := srv.SettingsStore.WriteSettings(panelstate.Settings{DisplayTimezone: &tz}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	d2 := loadSettingsStatus(srv)
	if !d2.Configured || d2.Source != "panelstate" {
		t.Fatalf("写入后应标记为 panelstate 接管: %+v", d2)
	}
	if !d2.SetDisplayTimezone {
		t.Fatalf("SetDisplayTimezone 应为 true")
	}
	if d2.SetSessionTTL {
		t.Fatalf("未设置的项 Set 标记应为 false")
	}
	if d2.DisplayTimezone != "America/New_York" {
		t.Fatalf("当前生效值应来自 panelstate,实际 %q", d2.DisplayTimezone)
	}
	if !optionSelected(d2.TimezoneOptions, "America/New_York") {
		t.Fatalf("下拉应回显面板设置的值")
	}
	// 显示时长应是预置友好写法
	if d2.SessionTTL != "24h" {
		t.Fatalf("SessionTTL 应归一化为 24h,实际 %q", d2.SessionTTL)
	}
}

// ---------------------------------------------------------------------------
// v2.86 批次 E:日志 / 防火墙 / 审计这 8 项
// ---------------------------------------------------------------------------

// strPtr / boolPtr / intPtr 构造可空字段的小工具。
func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func intPtr(i int) *int       { return &i }

func TestEffectiveSettingsBatchEFactoryDefaults(t *testing.T) {
	srv := newSettingsServer(t)
	// 不注入任何 Env* → 全部应落到出厂值,且 Set 标记全 false。
	eff := srv.effectiveSettings()

	if eff.logLevel != "info" || eff.logFormat != "text" {
		t.Fatalf("日志出厂默认不对: level=%q format=%q", eff.logLevel, eff.logFormat)
	}
	if eff.auditRetentionDays != 90 {
		t.Fatalf("audit_retention_days 出厂默认应为 90,实际 %d", eff.auditRetentionDays)
	}
	if eff.connLimitPerIP != 50 || eff.hashLimitPerMin != 30 {
		t.Fatalf("限速出厂默认不对: conn=%d hash=%d", eff.connLimitPerIP, eff.hashLimitPerMin)
	}
	if eff.disableMSSClamp || eff.disableDDoSProtection {
		t.Fatalf("MSS clamp / DDoS 防护出厂默认应是保持开启(false)")
	}
	if eff.nftFamily != "" {
		t.Fatalf("nft_family 出厂默认应是空(自动判断),实际 %q", eff.nftFamily)
	}
	if eff.logLevelSet || eff.logFormatSet || eff.auditDaysSet ||
		eff.connLimitSet || eff.hashLimitSet || eff.mssClampSet || eff.ddosSet || eff.nftFamilySet {
		t.Fatalf("未写入 settings.json 时所有批次 E 的 Set 标记都应为 false")
	}
}

func TestEffectiveSettingsBatchEPanelOverridesEnv(t *testing.T) {
	srv := newSettingsServer(t)
	// 模拟 entrypoint 启动时把 settings.json 的值 export 回 env 后的样子:
	// Env* 拿到的是"上次重启时的生效值"。
	srv.EnvLogLevel = "info"
	srv.EnvConnLimitPerIP = 50
	srv.EnvDisableDDoSProtection = false

	st := panelstate.Settings{
		LogLevel:              strPtr("debug"),
		ConnLimitPerIP:        intPtr(120),
		DisableDDoSProtection: boolPtr(true),
		NftFamily:             strPtr("v6"),
	}
	if err := srv.SettingsStore.WriteSettings(st); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	eff := srv.effectiveSettings()
	if eff.logLevel != "debug" || !eff.logLevelSet {
		t.Fatalf("log_level 应被面板设置覆盖: %q set=%v", eff.logLevel, eff.logLevelSet)
	}
	if eff.connLimitPerIP != 120 || !eff.connLimitSet {
		t.Fatalf("connlimit_per_ip 应被覆盖: %d", eff.connLimitPerIP)
	}
	// 显式 false 的反面:面板显式 true,env 是 false → 必须以面板为准。
	if !eff.disableDDoSProtection || !eff.ddosSet {
		t.Fatalf("disable_ddos_protection 显式 true 应覆盖 env false")
	}
	if eff.nftFamily != "v6" || !eff.nftFamilySet {
		t.Fatalf("nft_family 应被覆盖: %q", eff.nftFamily)
	}
	// 没设的项仍跟随 env / 出厂
	if eff.auditRetentionDays != 90 {
		t.Fatalf("未设的项应保持出厂默认,实际 %d", eff.auditRetentionDays)
	}
	if eff.hashLimitPerMin != 30 {
		t.Fatalf("未设的项应保持 env/出厂值,实际 %d", eff.hashLimitPerMin)
	}
}

func TestApplyRuntimeSettingsLogLevelTakesEffect(t *testing.T) {
	srv := newSettingsServer(t)

	// 先落到 info,再改成 debug,验证运行期真的换了级别。
	srv.applyRuntimeSettings(settingsEffective{
		sessionTTL: 24 * time.Hour, logLevel: "info", nftFamily: "",
		auditRetentionDays: 90, connLimitPerIP: 50, hashLimitPerMin: 30,
	})
	if got := srv.logLevel(); got != "info" {
		t.Fatalf("初始级别应为 info,实际 %q", got)
	}
	if got := config.CurrentLogLevel(); got != "info" {
		t.Fatalf("config 侧级别应为 info,实际 %q", got)
	}

	srv.applyRuntimeSettings(settingsEffective{
		sessionTTL: 24 * time.Hour, logLevel: "debug", nftFamily: "",
		auditRetentionDays: 90, connLimitPerIP: 50, hashLimitPerMin: 30,
	})
	if got := srv.logLevel(); got != "debug" {
		t.Fatalf("logLevel 运行期未生效: %q", got)
	}
	// 关键:config 侧的 LevelVar 也变了 —— 说明已有 logger 会立刻按新级别输出,
	// 而不是只在下次 NewLogger 时才生效。
	if got := config.CurrentLogLevel(); got != "debug" {
		t.Fatalf("slog.LevelVar 未运行期切换: %q", got)
	}

	// 非法值不应把级别打乱
	srv.applyRuntimeSettings(settingsEffective{
		sessionTTL: 24 * time.Hour, logLevel: "verbose", nftFamily: "",
		auditRetentionDays: 90, connLimitPerIP: 50, hashLimitPerMin: 30,
	})
	if got := config.CurrentLogLevel(); got != "debug" {
		t.Fatalf("非法级别应保持原值,实际 %q", got)
	}

	t.Cleanup(func() { config.SetLogLevel("info") })
}

func TestHandleSettingsSaveBatchE(t *testing.T) {
	requireTZ(t)
	srv := newSettingsServer(t)
	srv.EnvLogLevel = "info"

	form := url.Values{
		"log_level":               {"debug"},
		"log_format":              {"json"},
		"audit_retention_days":    {"180"},
		"connlimit_per_ip":        {"120"},
		"hashlimit_per_min":       {"60"},
		"disable_mss_clamp":       {"", "true"},
		"disable_ddos_protection": {""},
		"nft_family":              {"v6"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/settings/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	srv.handleSettingsSave(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("保存应成功,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	st, err := srv.SettingsStore.ReadSettings()
	if err != nil || st == nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if st.LogLevel == nil || *st.LogLevel != "debug" {
		t.Fatalf("log_level 落盘错误: %+v", st.LogLevel)
	}
	if st.LogFormat == nil || *st.LogFormat != "json" {
		t.Fatalf("log_format 落盘错误: %+v", st.LogFormat)
	}
	if st.AuditRetentionDays == nil || *st.AuditRetentionDays != 180 {
		t.Fatalf("audit_retention_days 落盘错误: %+v", st.AuditRetentionDays)
	}
	if st.ConnLimitPerIP == nil || *st.ConnLimitPerIP != 120 {
		t.Fatalf("connlimit_per_ip 落盘错误: %+v", st.ConnLimitPerIP)
	}
	if st.HashLimitPerMin == nil || *st.HashLimitPerMin != 60 {
		t.Fatalf("hashlimit_per_min 落盘错误: %+v", st.HashLimitPerMin)
	}
	// 未勾选 → hidden 空值兜底 → 显式 false 落盘
	if st.DisableDDoSProtection == nil || *st.DisableDDoSProtection != false {
		t.Fatalf("未勾选的 disable_ddos_protection 应显式落盘 false,实际 %+v", st.DisableDDoSProtection)
	}
	if st.DisableMSSClamp == nil || *st.DisableMSSClamp != true {
		t.Fatalf("勾选的 disable_mss_clamp 应落盘 true,实际 %+v", st.DisableMSSClamp)
	}
	if st.NftFamily == nil || *st.NftFamily != "v6" {
		t.Fatalf("nft_family 落盘错误: %+v", st.NftFamily)
	}

	// log_level 运行期生效
	if got := srv.logLevel(); got != "debug" {
		t.Fatalf("log_level 未运行期生效: %q", got)
	}
	t.Cleanup(func() { config.SetLogLevel("info") })
}

func TestHandleSettingsSaveBatchERejectsOutOfRange(t *testing.T) {
	cases := []struct {
		name  string
		form  url.Values
		field string
	}{
		{"审计天数 0 会删光审计表", url.Values{"audit_retention_days": {"0"}}, "audit_retention_days"},
		{"审计天数过大", url.Values{"audit_retention_days": {"99999"}}, "audit_retention_days"},
		{"并发上限 0 等于全员连不上", url.Values{"connlimit_per_ip": {"0"}}, "connlimit_per_ip"},
		{"协商速率 0", url.Values{"hashlimit_per_min": {"0"}}, "hashlimit_per_min"},
		{"日志级别拼错", url.Values{"log_level": {"flase"}}, "log_level"},
		{"日志格式拼错", url.Values{"log_format": {"yaml"}}, "log_format"},
		{"nft 地址族非法", url.Values{"nft_family": {"dual"}}, "nft_family"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSettingsServer(t)
			req := httptest.NewRequest(http.MethodPost, "/api/settings/save",
				strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Accept", "application/json")
			rec := httptest.NewRecorder()
			srv.handleSettingsSave(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s 应被 Validate 拒绝,实际 %d body=%s",
					tc.field, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.field) {
				t.Fatalf("错误信息应点名 %s,实际 %s", tc.field, rec.Body.String())
			}
			if srv.SettingsStore.SettingsExists() {
				t.Fatalf("校验失败不应落盘")
			}
		})
	}
}

func TestHandleSettingsSaveRejectsNonInteger(t *testing.T) {
	// v2.86 批次 E:非整数输入必须被 optInt 拦下(400),
	// 而不是静默退化成"跟随 env 默认"。
	//
	// 为什么单独测:越界值由 panelstate.Validate 拒,本来就 400;
	// 而"abc"这类值走不到 Validate —— 如果 optInt 吞掉它,
	// 用户会看到"保存成功"而实际值是 env 默认值,且页面上完全看不出没改成功。
	// 这是最难发现的一类错配,必须有测试钉住。
	cases := []struct {
		name  string
		form  url.Values
		field string
	}{
		{"并发上限填了文字", url.Values{"connlimit_per_ip": {"abc"}}, "connlimit_per_ip"},
		{"协商速率填了文字", url.Values{"hashlimit_per_min": {"很多"}}, "hashlimit_per_min"},
		{"审计天数填了文字", url.Values{"audit_retention_days": {"一年"}}, "audit_retention_days"},
		{"随机密码长度填了小数", url.Values{"default_user_password_len": {"16.5"}}, "default_user_password_len"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSettingsServer(t)
			req := httptest.NewRequest(http.MethodPost, "/api/settings/save",
				strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Accept", "application/json")
			rec := httptest.NewRecorder()
			srv.handleSettingsSave(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s 填了非整数应 400,实际 %d body=%s",
					tc.field, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.field) {
				t.Fatalf("错误信息应点名 %s,实际 %s", tc.field, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "整数") {
				t.Fatalf("错误信息应说明是整数问题,实际 %s", rec.Body.String())
			}
			if srv.SettingsStore.SettingsExists() {
				t.Fatalf("非法输入不应落盘")
			}
		})
	}
}

// TestHandleSettingsSaveNonIntegerBlocksWholeForm 保证"一个字段填错 → 整张表单不落盘"。
// 4 个数值字段是先全部解析、再统一构造 Settings 的,所以后面的字段即使合法
// 也不会被单独写进去 —— 半生半熟的配置比全量拒绝更难排查。
func TestHandleSettingsSaveNonIntegerBlocksWholeForm(t *testing.T) {
	requireTZ(t)
	srv := newSettingsServer(t)

	form := url.Values{
		"log_level":            {"debug"},
		"audit_retention_days": {"180"},
		"connlimit_per_ip":     {"abc"}, // 唯一非法项
	}
	req := httptest.NewRequest(http.MethodPost, "/api/settings/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	srv.handleSettingsSave(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应 400,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if srv.SettingsStore.SettingsExists() {
		st, _ := srv.SettingsStore.ReadSettings()
		t.Fatalf("同表单里的合法字段也不应落盘,实际写入了: %+v", st)
	}
}

func TestHandleSettingsSaveBatchEFollowRevertsToEnv(t *testing.T) {
	srv := newSettingsServer(t)
	srv.EnvDisableDDoSProtection = true

	// 先显式关掉
	form := url.Values{"disable_ddos_protection": {""}}
	req := httptest.NewRequest(http.MethodPost, "/api/settings/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	srv.handleSettingsSave(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("首次保存应成功:%d %s", rec.Code, rec.Body.String())
	}

	// 再勾"跟随 env 默认" → 该 key 不落盘
	form2 := url.Values{"disable_ddos_protection_follow": {"1"}}
	req2 := httptest.NewRequest(http.MethodPost, "/api/settings/save", strings.NewReader(form2.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Accept", "application/json")
	rec2 := httptest.NewRecorder()
	srv.handleSettingsSave(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("follow 保存应成功:%d %s", rec2.Code, rec2.Body.String())
	}

	// follow 让它变回"什么都没设" → 整个文件被清掉
	if srv.SettingsStore.SettingsExists() {
		st, _ := srv.SettingsStore.ReadSettings()
		if st != nil && st.DisableDDoSProtection != nil {
			t.Fatalf("勾了 follow 后 disable_ddos_protection 不应落盘,实际 %+v", st.DisableDDoSProtection)
		}
	}
	eff := srv.effectiveSettings()
	if !eff.disableDDoSProtection {
		t.Fatalf("follow 后应回到 env 值 true")
	}
}

func TestRestartFieldsChanged(t *testing.T) {
	base := settingsEffective{
		displayTimezone: "UTC", sessionTTL: time.Hour, logLevel: "info",
		logFormat: "text", auditRetentionDays: 90, connLimitPerIP: 50,
		hashLimitPerMin: 30, nftFamily: "",
	}

	// 只有 log_level 变 → 不需要重启
	lvlOnly := base
	lvlOnly.logLevel = "debug"
	if restartFieldsChanged(base, lvlOnly) {
		t.Fatalf("log_level 是运行期生效的,不该被判为需重启")
	}

	// 其余每一项变了都要提示重启
	for name, mut := range map[string]func(s *settingsEffective){
		"displayTimezone":       func(s *settingsEffective) { s.displayTimezone = "Asia/Tokyo" },
		"logFormat":             func(s *settingsEffective) { s.logFormat = "json" },
		"auditRetentionDays":    func(s *settingsEffective) { s.auditRetentionDays = 180 },
		"connLimitPerIP":        func(s *settingsEffective) { s.connLimitPerIP = 120 },
		"hashLimitPerMin":       func(s *settingsEffective) { s.hashLimitPerMin = 60 },
		"disableMSSClamp":       func(s *settingsEffective) { s.disableMSSClamp = true },
		"disableDDoSProtection": func(s *settingsEffective) { s.disableDDoSProtection = true },
		"nftFamily":             func(s *settingsEffective) { s.nftFamily = "v6" },
	} {
		changed := base
		mut(&changed)
		if !restartFieldsChanged(base, changed) {
			t.Fatalf("%s 变了应提示需重启", name)
		}
	}

	if restartFieldsChanged(base, base) {
		t.Fatalf("无变化时不该提示需重启")
	}
}

func TestBuildEnumOptions(t *testing.T) {
	// 当前值命中预置 → 首项"跟随 env 默认"不选中,命中项选中
	opts := buildEnumOptions(presetLogLevels, "debug")
	if opts[0].Selected {
		t.Fatalf("当前值命中预置时首项不该选中: %+v", opts[0])
	}
	if !optionSelected(opts, "debug") {
		t.Fatalf("应选中 debug")
	}
	// 选项数 = 1(跟随) + 预置数,不该多出"当前生效"重复项
	if len(opts) != len(presetLogLevels)+1 {
		t.Fatalf("命中预置时不应追加重复项,实际 %d 项", len(opts))
	}

	// 当前值不在预置里(人工编辑过 settings.json)→ 追加一条并选中,
	// 首项也选中(表示"现在既不是预置、也没被面板接管"这一事实)。
	opts2 := buildEnumOptions(presetLogLevels, "trace")
	if !opts2[0].Selected {
		t.Fatalf("当前值不在预置里时首项应选中")
	}
	found := false
	for _, o := range opts2 {
		if o.Value == "trace" {
			found = true
			if !o.Selected {
				t.Fatalf("追加的当前值项应选中: %+v", o)
			}
		}
	}
	if !found {
		t.Fatalf("应追加当前值 trace")
	}
}

func TestLoadSettingsStatusBatchE(t *testing.T) {
	requireTZ(t)
	srv := newSettingsServer(t)
	srv.EnvLogLevel = "info"
	srv.EnvNftFamily = ""
	srv.EnvConnLimitPerIP = 50

	d := loadSettingsStatus(srv)

	// 边界常量必须与 panelstate 同源,否则 UI 允许输入的值会被服务端拒
	if d.AuditDaysMin != panelstate.MinAuditRetentionDays || d.AuditDaysMax != panelstate.MaxAuditRetentionDays {
		t.Fatalf("审计天数边界应与 panelstate 同源: %d~%d", d.AuditDaysMin, d.AuditDaysMax)
	}
	if d.ConnLimitMin != panelstate.MinConnLimitPerIP || d.ConnLimitMax != panelstate.MaxConnLimitPerIP {
		t.Fatalf("并发上限边界应与 panelstate 同源")
	}
	if d.HashLimitMin != panelstate.MinHashLimitPerMin || d.HashLimitMax != panelstate.MaxHashLimitPerMin {
		t.Fatalf("协商速率边界应与 panelstate 同源")
	}
	if len(d.LogLevelOptions) != len(panelstate.ValidLogLevels)+1 {
		t.Fatalf("日志级别下拉应等于白名单+1,实际 %d", len(d.LogLevelOptions))
	}
	if len(d.LogFormatOptions) != len(panelstate.ValidLogFormats)+1 {
		t.Fatalf("日志格式下拉应等于白名单+1")
	}
	if len(d.NftFamilyOptions) != len(panelstate.ValidNftFamilies)+1 {
		t.Fatalf("nft 地址族下拉应等于白名单+1")
	}
	// env 是 info(命中预置)→ 首项不该选中,防止"打开卡片点保存"就固化 env 值
	if d.LogLevelOptions[0].Selected {
		t.Fatalf("env 命中预置时首项不该选中")
	}
	if d.EnvNftFamily != "auto" {
		t.Fatalf("env nft_family 为空时应展示为 auto,实际 %q", d.EnvNftFamily)
	}

	// 部署级只读清单必须非空且覆盖关键项
	if len(d.DeployOnly) == 0 {
		t.Fatalf("部署级只读清单不应为空")
	}
	seen := map[string]bool{}
	for _, p := range d.DeployOnly {
		seen[p.Env] = true
	}
	for _, must := range []string{
		"IKEV2_NETWORK_MODE", "IKEV2_LISTEN_ADDR", "IKEV2_DATA_DIR",
		"IKEV2_IPV6_ONLY", "IKEV2_OUT_IF", "IKEV2_L2_PARENT",
	} {
		if !seen[must] {
			t.Fatalf("部署级清单缺少 %s", must)
		}
	}
}

func TestHandleSettingsStatusBatchE(t *testing.T) {
	srv := newSettingsServer(t)
	if err := srv.SettingsStore.WriteSettings(panelstate.Settings{
		LogLevel:       strPtr("warn"),
		ConnLimitPerIP: intPtr(200),
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/settings/status", nil)
	rec := httptest.NewRecorder()
	srv.handleSettingsStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status 应 200,实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"log_level":"warn"`) {
		t.Fatalf("响应应含 log_level=warn,实际 %s", body)
	}
	if !strings.Contains(body, `"log_level_source":"panelstate"`) {
		t.Fatalf("响应应标明来源 panelstate,实际 %s", body)
	}
	if !strings.Contains(body, `"connlimit_per_ip":200`) {
		t.Fatalf("响应应含 connlimit_per_ip=200,实际 %s", body)
	}
	// 没设的项来源应是 env
	if !strings.Contains(body, `"log_format_source":"env"`) {
		t.Fatalf("未设置的 log_format 来源应为 env,实际 %s", body)
	}
}

// ---------------------------------------------------------------------------
// v2.86 批次 E:前端校验的模板级回归保护
//
// 为什么扫模板源码而不只靠浏览器测:浏览器测只能证明"当前这版是对的",
// 证明不了"下一个人改模板时没把 data-val-* 删掉"。校验一旦被摘掉,
// 服务端的 400 仍然拦得住,但用户要靠一次失败往返才知道自己填错了 ——
// 那正是这层前端校验要消除的体验。这两个测试把"属性还在"钉住。
// ---------------------------------------------------------------------------

func findTemplate(t *testing.T, name string) string {
	t.Helper()
	for _, p := range []string{"../../web/templates/" + name, "../../../web/templates/" + name} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("%s not found(cwd=%s)", name, mustGetwd())
	return ""
}

// findTagWithName 返回含 name="<field>" 的那个标签的原文(截到 > 为止)。
func findTagWithName(html, field string) string {
	idx := strings.Index(html, `name="`+field+`"`)
	if idx < 0 {
		return ""
	}
	start := strings.LastIndex(html[:idx], "<")
	if start < 0 {
		return ""
	}
	end := strings.Index(html[idx:], ">")
	if end < 0 {
		return ""
	}
	return html[start : idx+end]
}

// TestSettingsTemplateValidationAttributes 每个受校验字段都必须同时有
// data-val-* 属性和配套的错误位容器。少任何一半都会出问题:
// 只有属性没有错误位 → 拦下了但用户不知道哪里错;
// 只有错误位没有属性 → validateForm 的选择器扫不到它,校验等于没生效。
func TestSettingsTemplateValidationAttributes(t *testing.T) {
	raw, err := os.ReadFile(findTemplate(t, "home_content.html"))
	if err != nil {
		t.Fatalf("read home_content.html: %v", err)
	}
	html := stripGoTemplateComments(string(raw))

	// 数值字段:data-val-int + 上下界 + 展示名
	for _, field := range []string{
		"default_user_password_len", "audit_retention_days",
		"connlimit_per_ip", "hashlimit_per_min",
	} {
		tag := findTagWithName(html, field)
		if tag == "" {
			t.Fatalf("模板里找不到 name=%q 的控件", field)
		}
		for _, attr := range []string{"data-val-int", "data-val-min=", "data-val-max=", "data-val-name="} {
			if !strings.Contains(tag, attr) {
				t.Errorf("name=%q 的控件缺少 %s —— 数值校验会失效", field, attr)
			}
		}
		// 上下界必须来自模板变量,不能写死 —— 写死就和 panelstate 的
		// 常量成两份真相,以后改了一处就会前后端不一致。
		if !strings.Contains(tag, "{{.Settings.") {
			t.Errorf("name=%q 的上下界应来自模板变量,不该写死", field)
		}
	}

	// 枚举字段:白名单必须与 panelstate 的 Valid* 同源
	for field, valid := range map[string][]string{
		"log_level":  panelstate.ValidLogLevels,
		"log_format": panelstate.ValidLogFormats,
		"nft_family": panelstate.ValidNftFamilies,
	} {
		tag := findTagWithName(html, field)
		if tag == "" {
			t.Fatalf("模板里找不到 name=%q 的控件", field)
		}
		if !strings.Contains(tag, "data-val-enum=") {
			t.Fatalf("name=%q 的下拉缺少 data-val-enum", field)
		}
		for _, v := range valid {
			if !strings.Contains(tag, v) {
				t.Errorf("name=%q 的 data-val-enum 缺少合法值 %q", field, v)
			}
		}
	}

	// 开关字段:必须成对出现 —— hidden 兜底(value="")在前、checkbox(value="true")在后。
	// 顺序反了的话取消勾选时表单里一个值都不提交,optBool 会把"取消勾选"
	// 读成 nil(跟随 env)而不是 false(面板显式关闭),而且不报任何错。
	// 运行时 checkBool 也会查一遍,这里查的是模板本身没写错。
	for _, field := range []string{"disable_mss_clamp", "disable_ddos_protection"} {
		hiddenIdx := strings.Index(html, `type="hidden" name="`+field+`" value=""`)
		if hiddenIdx < 0 {
			t.Fatalf("name=%q 缺少 hidden 兜底 input", field)
		}
		cbRel := strings.Index(html[hiddenIdx:], `name="`+field+`" value="true"`)
		if cbRel < 0 {
			t.Fatalf("name=%q 缺少 value=\"true\" 的 checkbox —— optBool 只认这个字面量", field)
		}
		// 取到 checkbox 标签的 > 为止,data-val-bool 写在 value 之后
		cbStart := hiddenIdx + cbRel
		cbEnd := strings.Index(html[cbStart:], ">")
		if cbEnd < 0 {
			t.Fatalf("name=%q 的 checkbox 标签未闭合", field)
		}
		if !strings.Contains(html[cbStart:cbStart+cbEnd], "data-val-bool") {
			t.Errorf("name=%q 的 checkbox 缺少 data-val-bool", field)
		}
	}

	// 每个受校验字段都要有错误位容器
	for _, field := range []string{
		"default_user_password_len", "audit_retention_days",
		"connlimit_per_ip", "hashlimit_per_min",
		"log_level", "log_format", "nft_family",
	} {
		if !strings.Contains(html, `data-error-for="`+field+`"`) {
			t.Errorf("name=%q 缺少错误位容器 data-error-for —— 拦住提交后用户看不到原因", field)
		}
	}
	for _, field := range []string{"disable_mss_clamp", "disable_ddos_protection"} {
		if !strings.Contains(html, `data-error-for="`+field+`"`) {
			t.Errorf("name=%q 缺少错误位容器 data-error-for", field)
		}
	}

	if !strings.Contains(html, "data-settings-error-summary") {
		t.Errorf("缺少汇总条 data-settings-error-summary")
	}
	if !strings.Contains(html, `data-settings-form novalidate`) {
		t.Errorf("表单应带 novalidate,否则浏览器原生气泡会盖掉中文提示")
	}
}

// TestCheckEnumTreatsEmptyAsFollowEnv 钉住"空串 = 跟随 env 默认"这条语义。
//
// 这条是浏览器实测抓出来的真 bug:初版 checkEnum 只认白名单里的值,
// 把空串判成非法。而 <select> 首项「跟随 env 默认」的 value 恰恰是空串,
// 面板没接管过时全部下拉都选中首项 —— 于是用户第一次点保存就被自己的
// 默认值拦住,错误文案还写成 '当前是 ""',看着像配置坏了。
//
// 静态测试钉不住这个(HTML 里看不出哪个 option 是首项),所以这里直接
// 断言 JS 源码里"先放行空串"这件事,并说明为什么顺序不能反。
func TestCheckEnumTreatsEmptyAsFollowEnv(t *testing.T) {
	raw, err := os.ReadFile(findLayoutPath(t))
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	html := stripGoTemplateComments(string(raw))

	i := strings.Index(html, "function checkEnum(el)")
	if i < 0 {
		t.Fatalf("layout.html 里没有 checkEnum")
	}
	body := html[i:]
	if end := strings.Index(body, "\n          }"); end > 0 {
		body = body[:end]
	}

	// 空串放行必须发生在白名单比对之前
	emptyIdx := strings.Index(body, "=== ''")
	allowedIdx := strings.Index(body, "allowed.indexOf")
	if emptyIdx < 0 {
		t.Fatalf("checkEnum 没有放行空串 —— 「跟随 env 默认」会被判成非法,用户第一次保存就被拦")
	}
	if allowedIdx < 0 {
		t.Fatalf("checkEnum 里找不到白名单比对")
	}
	if emptyIdx > allowedIdx {
		t.Fatalf("checkEnum 必须先放行空串再比白名单,否则空串会落到白名单比对里被判非法")
	}
}

// TestInitSettingsValidationWired initSettingsValidation 必须挂进 initAll,
// 否则函数定义了但永远不执行 —— 这种"看起来有校验其实没生效"的 bug
// 光看函数定义发现不了。
func TestInitSettingsValidationWired(t *testing.T) {
	raw, err := os.ReadFile(findLayoutPath(t))
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	html := stripGoTemplateComments(string(raw))

	if !strings.Contains(html, "function initSettingsValidation()") {
		t.Fatalf("layout.html 里没有 initSettingsValidation")
	}
	i := strings.Index(html, "function initAll()")
	if i < 0 {
		t.Fatalf("layout.html 里没有 initAll")
	}
	body := html[i:]
	if end := strings.Index(body, "\n        }"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "initSettingsValidation();") {
		t.Fatalf("initAll 里没有调用 initSettingsValidation() —— 校验不会被执行")
	}
	// CSP 无 unsafe-inline,内联事件处理器一律不合法
	for _, bad := range []string{"onsubmit=", "oninvalid=", "oninput=\""} {
		if strings.Contains(html, bad) {
			t.Errorf("layout.html 出现内联事件处理器 %s,会被 CSP script-src 'nonce-...' 拒绝", bad)
		}
	}
}
