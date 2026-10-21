// v2.94:证书卡 / 客户端 IP 段卡 的「保存反馈」回归测试。
//
// 真实用户反馈(2026-10-21):
//
//	「配置证书时,保存失败的提示直接跳转到新页面  这不对吧
//	  然后客户端IP段 相关配置保存 清除等 也没有成功失败提示」
//
// 对应的现场截图:浏览器地址栏停在 https://192.168.50.66:8443/api/cert/save,
// 整页只有一行黑底白字「保存失败: letsencrypt 模式必须填域名(IKEV2_DOMAIN)」。
//
// 根因:这两张卡的 handler 是全项目最后两个还在用 http.Error 的写操作。
//   - cert/save    → http.Error → 浏览器裸文本页,表单内容全丢、无返回路径
//   - cert/clear   → http.Error → 同上
//   - subnet/save  → http.Error(校验失败) + 裸 302(成功,页面零提示)
//   - subnet/clear → 同上
// 而阿里云卡(v2.86-pr23l)和面板设置卡(批次 D)早就改成 flash + 302 +
// 卡片内联 banner 了 —— 就是「每张卡各写各的,漏一处就多一个裸文本页」。
//
// v2.94 修法:抽出 respondFlashSlot 作为所有写操作的唯一响应出口,
// cert / subnet 各占一个 flash slot,回到首页时就近渲染 banner。
//
// 本文件锁死三件事:
//  1. 失败不再返回裸文本页(302 + flash,不是 text/plain)
//  2. 成功也必须有提示(之前 subnet 成功是裸 302,页面上什么都没有)
//  3. 反馈真的渲染在**对应卡片内**,不串到别的卡
package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
	"github.com/yourname/ikev2-panel-v2/internal/swanctl"
)

// grabCSRF 从首页 HTML 里抓 csrf_token(hidden input)。
func grabCSRF(t *testing.T, env *pr9Env, sid string) string {
	t.Helper()
	html := getHome(t, env, sid)
	idx := strings.Index(html, `name="csrf_token" value="`)
	if idx < 0 {
		t.Fatalf("no csrf_token input in home HTML")
	}
	rest := html[idx+len(`name="csrf_token" value="`):]
	end := strings.Index(rest, `"`)
	return rest[:end]
}

// postConfigForm 以「普通浏览器表单」的方式提交(save/clear 都用 formaction 区分路径)。
//
// 关键:不设 Accept 头 —— 真实浏览器原生 form 提交发的是
// `Accept: text/html,application/xhtml+xml,...`,必须走 flash + 302 分支。
func postConfigForm(t *testing.T, env *pr9Env, sid, csrf, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form.Set("csrf_token", csrf)
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "ikev2_session", Value: sid})
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

// assertFlashRedirect 断言「没有裸文本页」:必须 302 回 /,且 flash 落到了指定 slot。
func assertFlashRedirect(t *testing.T, env *pr9Env, sid, slot, wantKind, mustContain string) Flash {
	t.Helper()
	f, err := env.Srv.FlashStore.ConsumeFor(sid, slot)
	if err != nil {
		t.Fatalf("flash slot %q should be set, got err=%v", slot, err)
	}
	if f.Kind != wantKind {
		t.Errorf("flash kind = %q, want %q (msg=%q)", f.Kind, wantKind, f.Message)
	}
	if !strings.Contains(f.Message, mustContain) {
		t.Errorf("flash message %q should contain %q", f.Message, mustContain)
	}
	return f
}

// assertNoBareTextPage 断言响应不是浏览器裸文本错误页。
func assertNoBareTextPage(t *testing.T, rec *httptest.ResponseRecorder, ctx string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/plain") {
		t.Fatalf("%s: returned text/plain bare error page (CT=%q), body=%q",
			ctx, ct, p1Trunc(rec.Body.String()))
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("%s: status = %d, want 302 redirect to home (body=%q)",
			ctx, rec.Code, p1Trunc(rec.Body.String()))
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("%s: Location = %q, want /", ctx, loc)
	}
}

// ---------- 1. 证书卡 ----------

// TestConfigCard_Cert_ValidationFailure_NotBareTextPage 是用户截图里的那个场景。
//
// LE 模式 + 域名留空 → Validate 失败。v2.94 之前:400 text/plain,
// 浏览器停在 /api/cert/save,显示「保存失败: letsencrypt 模式必须填域名」。
func TestConfigCard_Cert_ValidationFailure_NotBareTextPage(t *testing.T) {
	env := newPR9Env(t)
	env.Srv.CertConfigStore = panelstate.NewCertConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	sid := loginAsAdmin(t, env, "admin", "test123")
	csrf := grabCSRF(t, env, sid)

	rec := postConfigForm(t, env, sid, csrf, "/api/cert/save", url.Values{
		"cert_mode": {"letsencrypt"},
		"domain":    {""}, // ← 触发「必须填域名」
	})

	assertNoBareTextPage(t, rec, "cert/save validation failure")
	assertFlashRedirect(t, env, sid, "cert", "error", "必须填域名")
}

// TestConfigCard_Cert_Success_ShowsFeedback 成功也必须有提示(要重启)。
func TestConfigCard_Cert_Success_ShowsFeedback(t *testing.T) {
	env := newPR9Env(t)
	env.Srv.CertConfigStore = panelstate.NewCertConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	sid := loginAsAdmin(t, env, "admin", "test123")
	csrf := grabCSRF(t, env, sid)

	rec := postConfigForm(t, env, sid, csrf, "/api/cert/save", url.Values{
		"cert_mode": {"letsencrypt"},
		"domain":    {"vpn.example.com"},
	})

	assertNoBareTextPage(t, rec, "cert/save success")
	assertFlashRedirect(t, env, sid, "cert", "success", "重启")
}

// TestConfigCard_Cert_Clear_FailureAndSuccess 清除路径同样两条都要有反馈。
func TestConfigCard_Cert_Clear_FailureAndSuccess(t *testing.T) {
	env := newPR9Env(t)
	certStore := panelstate.NewCertConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	env.Srv.CertConfigStore = certStore
	sid := loginAsAdmin(t, env, "admin", "test123")
	csrf := grabCSRF(t, env, sid)

	// 先存一份,让 clear 有东西可清
	rec := postConfigForm(t, env, sid, csrf, "/api/cert/save", url.Values{
		"cert_mode": {"letsencrypt"},
		"domain":    {"vpn.example.com"},
	})
	assertNoBareTextPage(t, rec, "cert/save (setup)")
	_, _ = env.Srv.FlashStore.ConsumeFor(sid, "cert") // 清掉 setup 的 flash

	rec2 := postConfigForm(t, env, sid, csrf, "/api/cert/clear", url.Values{})
	assertNoBareTextPage(t, rec2, "cert/clear success")
	assertFlashRedirect(t, env, sid, "cert", "success", "已清除")
}

// TestConfigCard_Cert_FlashRenderedInsideCertCard 反馈必须落在证书卡内部。
//
// 拿到首页 HTML 后,断言 banner 出现在 cert-card 的 </details> 之内,
// 而不是飘到页面顶部或别的卡里 —— 否则用户还是要在整页里找。
func TestConfigCard_Cert_FlashRenderedInsideCertCard(t *testing.T) {
	env := newPR9Env(t)
	env.Srv.CertConfigStore = panelstate.NewCertConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	sid := loginAsAdmin(t, env, "admin", "test123")

	env.Srv.FlashStore.SetFor(sid, "cert", Flash{
		Kind:    "error",
		Message: "保存失败：letsencrypt 模式必须填域名(IKEV2_DOMAIN)",
	})

	html := getHome(t, env, sid)
	assertBannerInsideCard(t, html, `id="cert-card"`, "cert-action-flash", "cert flash")
	if !strings.Contains(html, "letsencrypt 模式必须填域名") {
		t.Error("cert flash message should be rendered in home HTML")
	}
}

// assertBannerInsideCard 断言 banner 的 class 出现在指定 id 的 <details> 区间内。
func assertBannerInsideCard(t *testing.T, html, cardID, bannerClass, ctx string) {
	t.Helper()
	start := strings.Index(html, cardID)
	if start < 0 {
		t.Fatalf("%s: card anchor %s not found", ctx, cardID)
	}
	// <details ...> ... </details> 的区间:从 cardID 往后找第一个 </details>
	end := strings.Index(html[start:], "</details>")
	if end < 0 {
		t.Fatalf("%s: </details> after %s not found", ctx, cardID)
	}
	region := html[start : start+end]
	if !strings.Contains(region, bannerClass) {
		t.Errorf("%s: banner class %q should render inside card %s", ctx, bannerClass, cardID)
	}
}

// ---------- 2. 客户端 IP 段卡 ----------

// TestConfigCard_Subnet_ValidationFailure_NotBareTextPage IPv4 段非法。
func TestConfigCard_Subnet_ValidationFailure_NotBareTextPage(t *testing.T) {
	env := newPR9Env(t)
	env.Srv.SubnetConfigStore = panelstate.NewSubnetConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	writeSwanctlPool(t, env, "10.10.0.0/24", "fd00:1::/64")
	sid := loginAsAdmin(t, env, "admin", "test123")
	csrf := grabCSRF(t, env, sid)

	rec := postConfigForm(t, env, sid, csrf, "/api/subnet/save", url.Values{
		"ipv4_subnet": {"not-a-cidr"},
		"ipv6_subnet": {"fd00:1::/64"},
	})

	assertNoBareTextPage(t, rec, "subnet/save validation failure")
	assertFlashRedirect(t, env, sid, "subnet", "error", "不合法")
}

// TestConfigCard_Subnet_Success_ShowsFeedback 这是用户说「没有成功失败提示」的正主。
//
// 之前成功路径是裸 302 → 回首页后页面跟没点过一样,用户不知道生效没有、
// 客户端要不要重新拨号。现在必须有 success 提示。
func TestConfigCard_Subnet_Success_ShowsFeedback(t *testing.T) {
	env := newPR9Env(t)
	env.Srv.SubnetConfigStore = panelstate.NewSubnetConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	writeSwanctlPool(t, env, "10.10.0.0/24", "fd00:1::/64")
	sid := loginAsAdmin(t, env, "admin", "test123")
	csrf := grabCSRF(t, env, sid)

	// swanctl 二进制在测试环境不存在 → reload 会失败,但 panelstate 已写入。
	// 无论走哪条分支,都必须有 flash 反馈,不能是裸文本页或静默。
	rec := postConfigForm(t, env, sid, csrf, "/api/subnet/save", url.Values{
		"ipv4_subnet": {"10.13.0.0/24"},
		"ipv6_subnet": {"fd00:5::/64"},
	})

	assertNoBareTextPage(t, rec, "subnet/save (swanctl reload unavailable in test env)")
	f, err := env.Srv.FlashStore.ConsumeFor(sid, "subnet")
	if err != nil {
		t.Fatalf("subnet flash should be set after save, got err=%v", err)
	}
	if f.Message == "" {
		t.Fatal("subnet flash message must not be empty")
	}
}

// TestConfigCard_Subnet_Clear_ShowsFeedback 清除也要有提示。
//
// dev 模式(读不到启动段)那条分支尤其重要 —— 之前它也是裸 302,
// 用户以为"已经回到上一段了",其实要重启容器才回落到 env。
func TestConfigCard_Subnet_Clear_ShowsFeedback(t *testing.T) {
	env := newPR9Env(t)
	subnetStore := panelstate.NewSubnetConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	if err := subnetStore.WriteSubnetConfig(panelstate.SubnetConfig{
		IPv4Subnet: "10.13.0.0/24",
		IPv6Subnet: "fd00:5::/64",
	}); err != nil {
		t.Fatal(err)
	}
	env.Srv.SubnetConfigStore = subnetStore
	// StartupIPv4Subnet / StartupIPv6Subnet 留空 → 走 dev 分支
	env.Srv.Swanctl = swanctl.New().WithSwanctlConfPath(filepath.Join(env.DataDir, "swanctl.conf"))

	sid := loginAsAdmin(t, env, "admin", "test123")
	csrf := grabCSRF(t, env, sid)

	rec := postConfigForm(t, env, sid, csrf, "/api/subnet/clear", url.Values{})
	assertNoBareTextPage(t, rec, "subnet/clear success")

	f := assertFlashRedirect(t, env, sid, "subnet", "success", "重启")
	if !strings.Contains(f.Message, "未读到启动时的 IP 段") {
		t.Errorf("dev-mode clear must tell the user a restart is needed, got %q", f.Message)
	}

	// panelstate 必须真的被清掉
	if subnetStore.SubnetConfigExists() {
		t.Error("subnet.conf should be removed after clear")
	}
}

// TestConfigCard_Subnet_FlashRenderedInsideSubnetCard 反馈落在 IP 段卡内部。
func TestConfigCard_Subnet_FlashRenderedInsideSubnetCard(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")

	env.Srv.FlashStore.SetFor(sid, "subnet", Flash{
		Kind:    "success",
		Message: "客户端 IP 段已保存并立即生效。",
	})

	html := getHome(t, env, sid)
	assertBannerInsideCard(t, html, `id="subnet-card"`, "subnet-action-flash", "subnet flash")
}

// TestConfigCard_FlashSlotsAreIsolated 两个 slot 不能互相覆盖。
//
// 这是加 slot 的唯一风险:证书卡保存成功 → 立刻 IP 段卡保存失败,
// 如果共用一个 slot,用户只会看到最后那条,证书卡的成功反馈凭空消失。
func TestConfigCard_FlashSlotsAreIsolated(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")

	env.Srv.FlashStore.SetFor(sid, "cert", Flash{Kind: "success", Message: "证书配置已保存"})
	env.Srv.FlashStore.SetFor(sid, "subnet", Flash{Kind: "error", Message: "subnet 坏了"})

	html := getHome(t, env, sid)
	if !strings.Contains(html, "证书配置已保存") {
		t.Error("cert flash should survive when subnet flash is also set")
	}
	if !strings.Contains(html, "subnet 坏了") {
		t.Error("subnet flash should survive when cert flash is also set")
	}
}

// ---------- 3. API 契约:wantsJSON 分支不能被改坏 ----------

// TestConfigCard_Cert_JSONContract 外部 API 调用方仍能拿到 JSON。
//
// v2.94 把 cert 的 `Accept` 判定从精确相等换成 wantsJSON()
// (原先 `r.Header.Get("Accept") == "application/json"`),
// 这里确认 JSON 分支的字段没丢:ok / msg / restart_required。
func TestConfigCard_Cert_JSONContract(t *testing.T) {
	env := newPR9Env(t)
	env.Srv.CertConfigStore = panelstate.NewCertConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	sid := loginAsAdmin(t, env, "admin", "test123")
	csrf := grabCSRF(t, env, sid)

	form := url.Values{"cert_mode": {"letsencrypt"}, "domain": {"vpn.example.com"}}
	form.Set("csrf_token", csrf)
	req := httptest.NewRequest("POST", "/api/cert/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.AddCookie(&http.Cookie{Name: "ikev2_session", Value: sid})
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("JSON branch: status = %d, want 200; body=%q", rec.Code, p1Trunc(rec.Body.String()))
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body=%q)", err, p1Trunc(rec.Body.String()))
	}
	if body["ok"] != true {
		t.Errorf(`body["ok"] = %v, want true`, body["ok"])
	}
	if rr, ok := body["restart_required"].(bool); !ok || !rr {
		t.Errorf(`body["restart_required"] = %v, want true`, body["restart_required"])
	}
	if msg, _ := body["msg"].(string); msg == "" {
		t.Error(`body["msg"] must not be empty`)
	}
	// JSON 分支不写 flash —— 它是给 API 调用方用的,不该污染浏览器会话。
	if _, err := env.Srv.FlashStore.ConsumeFor(sid, "cert"); err == nil {
		t.Error("JSON branch should NOT write a browser flash")
	}
}

// TestConfigCard_Subnet_JSONErrorContract 失败时也走 JSON(而不是 text/plain)。
func TestConfigCard_Subnet_JSONErrorContract(t *testing.T) {
	env := newPR9Env(t)
	env.Srv.SubnetConfigStore = panelstate.NewSubnetConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	writeSwanctlPool(t, env, "10.10.0.0/24", "fd00:1::/64")
	sid := loginAsAdmin(t, env, "admin", "test123")
	csrf := grabCSRF(t, env, sid)

	form := url.Values{"ipv4_subnet": {"nope"}, "ipv6_subnet": {"fd00:1::/64"}}
	form.Set("csrf_token", csrf)
	req := httptest.NewRequest("POST", "/api/subnet/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(&http.Cookie{Name: "ikev2_session", Value: sid})
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/plain") {
		t.Fatalf("JSON request got text/plain: %q body=%q", ct, p1Trunc(rec.Body.String()))
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body["ok"] != false {
		t.Errorf(`body["ok"] = %v, want false`, body["ok"])
	}
	if body["kind"] != "error" {
		t.Errorf(`body["kind"] = %v, want error`, body["kind"])
	}
}

// TestConfigCard_Cert_ConfigNotPersistedOnFailure 校验失败不得留下脏配置。
//
// 反馈改好之后更容易踩的坑:失败也返回 200/302,用户以为存上了,
// 下次进面板发现还是旧值。所以这里断言 cert.conf 根本没被创建。
func TestConfigCard_Cert_ConfigNotPersistedOnFailure(t *testing.T) {
	env := newPR9Env(t)
	dir := filepath.Join(env.DataDir, "panel-state")
	env.Srv.CertConfigStore = panelstate.NewCertConfigStoreWithDir(dir)
	sid := loginAsAdmin(t, env, "admin", "test123")
	csrf := grabCSRF(t, env, sid)

	postConfigForm(t, env, sid, csrf, "/api/cert/save", url.Values{
		"cert_mode": {"letsencrypt"},
		"domain":    {""},
	})

	if _, err := os.Stat(filepath.Join(dir, "cert.conf")); !os.IsNotExist(err) {
		t.Errorf("cert.conf must NOT be written when validation fails (err=%v)", err)
	}
}
