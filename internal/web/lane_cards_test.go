// v2.93:首页「服务就绪链路」8 张 lane 卡片的显示一致性测试。
//
// 背景（真实用户反馈）：admin 在面板里改完密码后，首页第一张
// 「管理口令」卡片**仍然显示「默认」**（红色 danger 态）。
//
// 根因：loadIsDefaultPassword() 只判断 panel-state/INITIAL_ADMIN_PASSWORD.txt
// **文件是否存在**，而 v2.85-PR2 留下的欠账是「admin 改密后要删这个文件」——
// 当时注释写着「本 PR 不做,留 v3.0 admin 改密页」,但 v2.86-pr21 已经把
// admin 改密页做出来了(POST /api/account/password),欠账一直没还。
//
// v2.93 修复：改密成功后删除该文件 → 卡片翻成「已改」,顶部横幅消失。
//
// 本文件覆盖两件事：
//  1. 改密后 INITIAL_ADMIN_PASSWORD.txt 必须被删,且卡片文案随之翻转;
//  2. 8 张卡片各自的判定逻辑 —— 显示值必须与数据字段一致。
package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
	"github.com/yourname/ikev2-panel-v2/internal/swanctl"
)

// initialPasswordFilePath 测试用的 INITIAL_ADMIN_PASSWORD.txt 路径。
//
// 与 initialPasswordPath() 保持一致：DataDir != "/data" 时落在
// <DataDir>/panel-state/ 下。pr9Env 的 DataDir 是临时目录,不会碰生产路径。
func initialPasswordFilePath(env *pr9Env) string {
	return filepath.Join(env.DataDir, "panel-state", "INITIAL_ADMIN_PASSWORD.txt")
}

// writeInitialPasswordFile 模拟 main.go 首次启动写入初始密码文件。
func writeInitialPasswordFile(t *testing.T, env *pr9Env, content string) string {
	t.Helper()
	p := initialPasswordFilePath(env)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// getHome 登录后 GET /，返回 HTML body。
func getHome(t *testing.T, env *pr9Env, sessionID string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "ikev2_session", Value: sessionID})
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// laneCardText 抽出 lane-rail 里第 n 张卡片的「标签 + 值」文本。
//
// 模板结构固定：
//
//	<a class="lane" data-state="..." href="...">
//	  <span class="lane-go">↗</span>
//	  <span class="lane-k">管理口令</span>
//	  <span class="lane-v">默认</span>
//	</a>
//
// 所以按 lane-k 找到标签,再往后找下一个 lane-v 即可。
func laneCardText(t *testing.T, html, key string) (value string, state string) {
	t.Helper()
	anchor := `class="lane-k">` + key + `</span>`
	idx := strings.Index(html, anchor)
	if idx < 0 {
		t.Fatalf("lane card %q not found in HTML", key)
	}
	// 往前找最近的 data-state（同一张卡片的开头）
	head := html[:idx]
	if s := strings.LastIndex(head, `data-state="`); s >= 0 {
		rest := head[s+len(`data-state="`):]
		if e := strings.Index(rest, `"`); e >= 0 {
			state = rest[:e]
		}
	}
	// 往后找 lane-v
	tail := html[idx:]
	vIdx := strings.Index(tail, `class="lane-v">`)
	if vIdx < 0 {
		t.Fatalf("lane card %q has no lane-v element", key)
	}
	rest := tail[vIdx+len(`class="lane-v">`):]
	if e := strings.Index(rest, `</span>`); e >= 0 {
		value = rest[:e]
	}
	return value, state
}

// ---------- 1. 管理口令卡片 ----------

// TestLane_DefaultPasswordCard_FlipsAfterChange 覆盖用户反馈的那个 bug。
func TestLane_DefaultPasswordCard_FlipsAfterChange(t *testing.T) {
	env := newPR9Env(t)
	pwPath := writeInitialPasswordFile(t, env, "initial-secret")

	sid := loginAsAdmin(t, env, "admin", "test123")

	// 改密前：文件在 → 卡片显示「默认」+ danger
	html := getHome(t, env, sid)
	v, state := laneCardText(t, html, "管理口令")
	if v != "默认" {
		t.Errorf("before change: lane value = %q, want 默认", v)
	}
	if state != "danger" {
		t.Errorf("before change: lane state = %q, want danger", state)
	}
	// 顶部横幅也应该在
	if !strings.Contains(html, "你仍在使用默认管理员密码") {
		t.Errorf("before change: default-password banner missing from HTML")
	}

	// 改密
	rec := postAccountPassword(t, env, sid, "test123", "newsecret123", "newsecret123")
	if rec.Code != http.StatusFound {
		t.Fatalf("change password: status = %d, want 302; body = %s", rec.Code, rec.Body.String())
	}

	// 关键断言 1：文件被删了
	if _, err := os.Stat(pwPath); !os.IsNotExist(err) {
		t.Errorf("INITIAL_ADMIN_PASSWORD.txt still exists after change (err = %v), want removed", err)
	}

	// 关键断言 2：卡片翻成「已改」+ ok
	html2 := getHome(t, env, sid)
	v2, state2 := laneCardText(t, html2, "管理口令")
	if v2 != "已改" {
		t.Errorf("after change: lane value = %q, want 已改", v2)
	}
	if state2 != "ok" {
		t.Errorf("after change: lane state = %q, want ok", state2)
	}
	// 顶部默认密码横幅也必须一起消失
	if strings.Contains(html2, "你仍在使用默认管理员密码") {
		t.Errorf("after change: default-password banner still present in HTML")
	}
}

// TestLane_DefaultPasswordCard_AlreadyChanged_NoFile 边界：
// 文件本来就不存在（老用户早就改过密码）→ 卡片应直接显示「已改」。
func TestLane_DefaultPasswordCard_AlreadyChanged_NoFile(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")

	html := getHome(t, env, sid)
	v, state := laneCardText(t, html, "管理口令")
	if v != "已改" {
		t.Errorf("lane value = %q, want 已改", v)
	}
	if state != "ok" {
		t.Errorf("lane state = %q, want ok", state)
	}
}

// TestLane_DefaultPasswordCard_ChangeFail_KeepsFile 边界：
// 改密**失败**（旧密码错）时绝不能删文件 —— 否则用户会被误导成「已改」，
// 而密码其实没改成。
func TestLane_DefaultPasswordCard_ChangeFail_KeepsFile(t *testing.T) {
	env := newPR9Env(t)
	pwPath := writeInitialPasswordFile(t, env, "initial-secret")
	sid := loginAsAdmin(t, env, "admin", "test123")

	rec := postAccountPassword(t, env, sid, "wrong-old-pw", "newsecret123", "newsecret123")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("change password: status = %d, want 422", rec.Code)
	}

	if _, err := os.Stat(pwPath); err != nil {
		t.Errorf("INITIAL_ADMIN_PASSWORD.txt removed after FAILED change (err = %v), want kept", err)
	}
}

// ---------- 2. 其余 7 张卡片 ----------

// TestLane_AllEightCardsPresent 锁死「8 张卡片都在」这个结构契约。
//
// 少渲染一张 → 用户看不到那条链路的断点，比显示错更严重。
func TestLane_AllEightCardsPresent(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")
	html := getHome(t, env, sid)

	for _, key := range []string{
		"管理口令", "IP 池", "服务端证书", "LE 续签",
		"阿里云凭证", "DNS 同步", "IKE_SA", "面板设置",
	} {
		if _, state := laneCardText(t, html, key); state == "" {
			t.Errorf("lane card %q missing data-state attribute", key)
		}
	}
}

// TestLane_PanelSettingsCounter 面板设置卡片的计数逻辑。
//
// 0 项被显式设置 → "0/8" + warn；全部设置 → "8/8" + ok。
// 判据是「8 项都设过才给绿」,部分设过给黄 —— 给绿会让人误以为全部由面板接管。
func TestLane_PanelSettingsCounter(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")

	html := getHome(t, env, sid)
	v, state := laneCardText(t, html, "面板设置")
	if v != "0/8" {
		t.Errorf("panel settings lane value = %q, want 0/8", v)
	}
	if state != "warn" {
		t.Errorf("panel settings lane state = %q, want warn", state)
	}
}

// TestLane_OffStateCardsAreNotDanger 离线/未配置的卡片不应报 danger。
//
// 语义:danger = 故障（需要人立刻管），off = 未启用（正常状态）。
// 把「未配置」误报成 danger 会让首页永远一片红,用户分不清真假告警。
func TestLane_OffStateCardsAreNotDanger(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")
	html := getHome(t, env, sid)

	// 全新环境:IP 池 / 服务端证书 / 阿里云凭证 / DNS 同步 都该是 off
	for _, key := range []string{"IP 池", "服务端证书", "阿里云凭证", "DNS 同步"} {
		_, state := laneCardText(t, html, key)
		if state == "danger" {
			t.Errorf("lane card %q state = danger, want off/warn on a fresh env", key)
		}
	}
}

// ---------- 3. IP 池卡片(判据必须是运行态) ----------

// writeSwanctlPool 造一份假的 swanctl.conf,内容跟生产模板同构
// (pools.ikev2-pool.addrs = <v4>, <v6>),并把 Server.Swanctl 指过去。
func writeSwanctlPool(t *testing.T, env *pr9Env, v4, v6 string) {
	t.Helper()
	p := filepath.Join(env.DataDir, "swanctl.conf")
	content := "pools {\n  ikev2-pool {\n    addrs = " + v4 + ", " + v6 + "\n  }\n}\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env.Srv.Swanctl = swanctl.New().WithSwanctlConfPath(p)
}

// TestLane_IPPool_RuntimePoolWithoutPanelConfig 是 192.168.50.63 的真实形态。
//
// 现场:swanctl.conf 里有 ikev2-pool(addrs = 10.10.0.0/24, fd00:1::/64),
// `swanctl --list-pools` 显示 254 个地址在用 —— 池是活的。
// 但用户从没在面板里改过 IP 段 → 没有 subnet.conf。
//
// v2.93 之前判据是 and .SubnetConfigured .SubnetCurrentIPv4,SubnetConfigured
// 为 false 直接短路 → 卡片显示「未配置」(灰),而池明明在跑。
// 同一页的 header-note 却用 {{else if .SubnetCurrentIPv4}} 兜底显示出了 CIDR,
// 两处说法打架。
func TestLane_IPPool_RuntimePoolWithoutPanelConfig(t *testing.T) {
	env := newPR9Env(t)
	writeSwanctlPool(t, env, "10.10.0.0/24", "fd00:1::/64")
	sid := loginAsAdmin(t, env, "admin", "test123")

	html := getHome(t, env, sid)
	v, state := laneCardText(t, html, "IP 池")
	if v != "10.10.0.0/24" {
		t.Errorf("IP pool lane value = %q, want 10.10.0.0/24", v)
	}
	if state != "ok" {
		t.Errorf("IP pool lane state = %q, want ok (pool is actually in effect)", state)
	}
}

// TestLane_IPPool_PanelConfiguredButNotEffective 面板配了但运行态读不到。
//
// 这时绝不能报绿 —— 绿代表"配置已落地",而实际 swanctl 还没用上。
func TestLane_IPPool_PanelConfiguredButNotEffective(t *testing.T) {
	env := newPR9Env(t)

	subnetStore := panelstate.NewSubnetConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	if err := subnetStore.WriteSubnetConfig(panelstate.SubnetConfig{
		IPv4Subnet: "10.13.0.0/24",
		IPv6Subnet: "fd00:5::/64",
	}); err != nil {
		t.Fatal(err)
	}
	env.Srv.SubnetConfigStore = subnetStore
	// swanctl.conf 不存在 → 运行态读不到
	env.Srv.Swanctl = swanctl.New().WithSwanctlConfPath(filepath.Join(env.DataDir, "missing-swanctl.conf"))

	sid := loginAsAdmin(t, env, "admin", "test123")
	html := getHome(t, env, sid)
	v, state := laneCardText(t, html, "IP 池")
	if v != "未生效" {
		t.Errorf("IP pool lane value = %q, want 未生效", v)
	}
	if state != "warn" {
		t.Errorf("IP pool lane state = %q, want warn", state)
	}
}

// TestLane_IPPool_DriftDetected 面板配的段跟运行态对不上。
//
// 成因:面板保存 subnet.conf 成功,但 swanctl --load-all 失败(charon 正忙)→
// 接口 500,panelstate 不回滚 → 两边对不上。此时也必须报「未生效」。
func TestLane_IPPool_DriftDetected(t *testing.T) {
	env := newPR9Env(t)

	subnetStore := panelstate.NewSubnetConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	if err := subnetStore.WriteSubnetConfig(panelstate.SubnetConfig{
		IPv4Subnet: "10.13.0.0/24",
		IPv6Subnet: "fd00:5::/64",
	}); err != nil {
		t.Fatal(err)
	}
	env.Srv.SubnetConfigStore = subnetStore
	// 运行态还是老段
	writeSwanctlPool(t, env, "10.10.0.0/24", "fd00:1::/64")

	sid := loginAsAdmin(t, env, "admin", "test123")
	html := getHome(t, env, sid)
	v, state := laneCardText(t, html, "IP 池")
	if v != "未生效" {
		t.Errorf("IP pool lane value = %q, want 未生效 (config drifted from runtime)", v)
	}
	if state != "warn" {
		t.Errorf("IP pool lane state = %q, want warn", state)
	}
}

// ---------- 4. 服务端证书 / LE 续签卡片(判据必须是运行态) ----------

// writeSelfSignedCert 造出自签证书文件(main.go 自签分支生成的那个路径)。
func writeSelfSignedCert(t *testing.T, env *pr9Env) {
	t.Helper()
	dir := filepath.Join(env.DataDir, "server")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.cert.pem"), []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLane_ServerCert_SelfSignedInPlace 也是 63 的真实形态。
//
// 现场:IKEV2_CERT_MODE=self-signed,没有 cert.conf,但
// /data/server/server.cert.pem + /data/panel-tls/cert.pem 都在,8443 正常监听。
// v2.93 之前判据用 CertConfigured(= cert.conf 存不存在)→ false → 显示「未配置」(灰),
// 而实际 HTTPS 握手正常、证书在用。
func TestLane_ServerCert_SelfSignedInPlace(t *testing.T) {
	env := newPR9Env(t)
	writeSelfSignedCert(t, env)
	sid := loginAsAdmin(t, env, "admin", "test123")

	html := getHome(t, env, sid)
	v, state := laneCardText(t, html, "服务端证书")
	if v != "自签" {
		t.Errorf("server cert lane value = %q, want 自签", v)
	}
	if state != "warn" {
		t.Errorf("server cert lane state = %q, want warn (self-signed needs manual CA trust)", state)
	}
}

// TestLane_ServerCert_NotConfigured 什么证书都没有 → 未配置 + off。
func TestLane_ServerCert_NotConfigured(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")

	html := getHome(t, env, sid)
	v, state := laneCardText(t, html, "服务端证书")
	if v != "未配置" {
		t.Errorf("server cert lane value = %q, want 未配置", v)
	}
	if state != "off" {
		t.Errorf("server cert lane state = %q, want off", state)
	}
}

// TestLane_ServerCert_ConfigLEButNotRestarted 「配置说 LE、实际还是自签」。
//
// 用户在面板里把证书模式切成 LE 并保存 → cert.conf 写成 letsencrypt,
// 但**没重启容器** → 运行中还是自签证书。
// 卡片必须报运行态(自签),不能报配置值(LE)—— 否则就是在报假象。
func TestLane_ServerCert_ConfigLEButNotRestarted(t *testing.T) {
	env := newPR9Env(t)

	certStore := panelstate.NewCertConfigStoreWithDir(filepath.Join(env.DataDir, "panel-state"))
	if err := certStore.WriteCertConfig(panelstate.CertConfig{
		CertMode: "letsencrypt",
		Domain:   "vpn.example.com",
		ServerCN: "vpn.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	env.Srv.CertConfigStore = certStore
	// 运行态:启动时的模式还是自签,自签证书文件在位
	env.Srv.CertMode = "self-signed"
	writeSelfSignedCert(t, env)

	sid := loginAsAdmin(t, env, "admin", "test123")
	html := getHome(t, env, sid)

	v, state := laneCardText(t, html, "服务端证书")
	if v != "自签" {
		t.Errorf("server cert lane value = %q, want 自签 (runtime is still self-signed)", v)
	}
	if state != "warn" {
		t.Errorf("server cert lane state = %q, want warn", state)
	}

	// LE 续签卡同理:运行态是自签 → 不适用(而不是假装在续签 LE)
	lv, ls := laneCardText(t, html, "LE 续签")
	if lv != "不适用" {
		t.Errorf("LE renew lane value = %q, want 不适用", lv)
	}
	if ls != "off" {
		t.Errorf("LE renew lane state = %q, want off", ls)
	}

	// 证书卡必须明确提示"改了但没重启"
	if !strings.Contains(html, "docker compose restart") {
		t.Errorf("cert card should warn that a restart is required for the pending LE config")
	}
}

// TestLane_LERenew_NotApplicableForSelfSigned 自签模式下 LE 续签卡是 off。
func TestLane_LERenew_NotApplicableForSelfSigned(t *testing.T) {
	env := newPR9Env(t)
	writeSelfSignedCert(t, env)
	sid := loginAsAdmin(t, env, "admin", "test123")

	html := getHome(t, env, sid)
	v, state := laneCardText(t, html, "LE 续签")
	if v != "不适用" {
		t.Errorf("LE renew lane value = %q, want 不适用", v)
	}
	if state != "off" {
		t.Errorf("LE renew lane state = %q, want off", state)
	}
}

// ---------- 5. IKE_SA 卡片(0 在线 vs conn 未加载) ----------

// fakeSwanctlBin 造一个只响应 --list-conns 的假 swanctl。
//
// output = ""  → 模拟"charon 里一个 conn 都没加载"(63 的故障态)
// output 非空  → 模拟"conn 已加载"
func fakeSwanctlBin(t *testing.T, dir, output string) string {
	t.Helper()
	p := filepath.Join(dir, "fake-swanctl")
	body := "#!/bin/sh\nif [ \"$1\" = \"--list-conns\" ]; then\ncat <<'EOF'\n" + output + "EOF\nexit 0\nfi\nexit 0\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLane_IKESA_ConnNotLoaded charon 里没有已加载的 conn → danger「未加载」。
//
// SA 数同样是 0,但这两种 0 的含义完全相反:
//   - "0 在线"  = 正常,当前没人连
//   - "未加载"  = 故障,客户端根本建立不了隧道
// 63 实机踩过后者(自签模式 conn 没注册进 charon),卡片当时显示"0 在线"。
func TestLane_IKESA_ConnNotLoaded(t *testing.T) {
	env := newPR9Env(t)
	env.Srv.Swanctl = swanctl.New().WithSwanctlBin(fakeSwanctlBin(t, env.DataDir, ""))
	sid := loginAsAdmin(t, env, "admin", "test123")

	html := getHome(t, env, sid)
	v, state := laneCardText(t, html, "IKE_SA")
	if v != "未加载" {
		t.Errorf("IKE_SA lane value = %q, want 未加载", v)
	}
	if state != "danger" {
		t.Errorf("IKE_SA lane state = %q, want danger", state)
	}
}

// TestLane_IKESA_NoConnButQueryFailed 查不了 conn ≠ conn 没加载。
//
// swanctl 查不到(没二进制 / 超时)时不能误报故障,仍按"没人连"显示。
func TestLane_IKESA_NoConnButQueryFailed(t *testing.T) {
	env := newPR9Env(t)
	// 指向不存在的二进制 → ListLoadedConns 报错
	env.Srv.Swanctl = swanctl.New().WithSwanctlBin(filepath.Join(env.DataDir, "no-such-swanctl"))
	sid := loginAsAdmin(t, env, "admin", "test123")

	html := getHome(t, env, sid)
	v, state := laneCardText(t, html, "IKE_SA")
	if v != "0 在线" {
		t.Errorf("IKE_SA lane value = %q, want 0 在线 (query failure must not fake a fault)", v)
	}
	if state != "off" {
		t.Errorf("IKE_SA lane state = %q, want off", state)
	}
}
