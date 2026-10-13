// 客户端配置下载 handler：mobileconfig / sswan / CA cert / 二维码 / 一次性安装 token。
// 见 docs/API.md §3
package web

import (
	"fmt"
	"net/http"

	qrcode "github.com/skip2/go-qrcode"
	"github.com/yourname/ikev2-panel-v2/internal/cert"
	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// resolveEffectiveOpts 把 admin defaults + user overlay 合并成最终 *cert.MobileConfigOpts。
//
// v2.86-PR12.22 三层优先级:
//   - cert.BaseMobileConfigDefaults()(builtin 出厂值)
//   - panelstate.MobileConfigDefaults(管理员全局)
//   - store.User.MobileConfigOpts(每用户 overlay,已 decode)
//
// 调用场景:任何要渲染 mobileconfig 的 handler(GET /users/{id}/mobileconfig
// 或 /install/{token}),都从这里拿最终值,避免重复合并逻辑。
//
// 出错时 nil 字段 → handler 用 builtin 默认;整个 helper 不会 panic / 报错。
func (s *Server) resolveEffectiveOpts(userOverlayJSON string) *cert.MobileConfigOpts {
	// 1) admin defaults
	var adminOpts *cert.MobileConfigOpts
	if s.MobileConfigDefaults != nil {
		if d, err := s.MobileConfigDefaults.ReadMobileConfigDefaults(); err == nil && d != nil {
			adminOpts = d.ToMobileConfigOpts()
		} else if err != nil {
			s.Logger.Warn("resolveEffectiveOpts: read admin defaults", "err", err)
		}
	}

	// 2) user overlay
	userOpts, err := cert.DecodeMobileConfigOpts(userOverlayJSON)
	if err != nil {
		// JSON 损坏 caller 会自己处理,这里只 log 不 fail
		s.Logger.Warn("resolveEffectiveOpts: decode user overlay", "err", err)
		userOpts = nil
	}

	// 3) 三层合并,返回最终 opts(handler 传 nil 时全部用 builtin)
	return mergedOptsFromLayers(adminOpts, userOpts)
}

// mergedOptsFromLayers 调 cert.EffectiveMobileConfigOpts 三层合并。
//
// 这里 thin wrap 的目的:跟 cert.BaseMobileConfigDefaults() 解耦(handler 不直接 import
// cert.BaseMobileConfigDefaults 是因为 PR12.22 之前 cert.DefaultMobileConfigOpts 已被多处引用,
// 但 PR12.22 之后语义是 "已合并值",这里显式合并更直观)。
func mergedOptsFromLayers(admin, overlay *cert.MobileConfigOpts) *cert.MobileConfigOpts {
	eff := cert.EffectiveMobileConfigOpts(admin, overlay)
	return &eff
}

// handleUserMobileconfig GET /users/{id}/mobileconfig
// 自签模式：mobileconfig 内联 CA 证书；LE 模式：不内联
func (s *Server) handleUserMobileconfig(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireUserID(w, r)
	if !ok {
		return
	}
	u, err := s.Store.GetUserByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	serverAddr := s.ServerAddr
	if serverAddr == "" {
		http.Error(w, "服务器地址未配置", http.StatusInternalServerError)
		return
	}

	var caPEM []byte
	if s.CertIncludeCA {
		caPEM = s.CACertPEM
	}

	// v2-76+：EAP-MSCHAPv2 模式，AuthName/AuthPassword 直接写进 profile，iOS 不弹密码框。
	// v2.85-PR3:透传 s.DisplayTimezone,BuildTimestamp 用配置的时区渲染。
	// v2.86-PR12.21:读 user.MobileConfigOpts,空 → nil → 用 cert 默认值。
	// v2.86-PR12.22:三层合并 builtin + admin defaults + user overlay。
	//
	// 防御性:用户 overlay JSON 损坏 → 返回 500 而不是悄悄用 builtin(iOS 装了 profile
	// 才知道错就更糟)。admin defaults JSON 损坏 resolveEffectiveOpts 内部 log + 降级 builtin。
	if u.MobileConfigOpts != "" {
		if _, err := cert.DecodeMobileConfigOpts(u.MobileConfigOpts); err != nil {
			s.Logger.Error("decode mobileconfig opts", "user_id", id, "err", err)
			http.Error(w, fmt.Sprintf("mobileconfig 配置损坏,请联系管理员: %v", err),
				http.StatusInternalServerError)
			return
		}
	}
	opts := s.resolveEffectiveOpts(u.MobileConfigOpts)
	out, err := cert.RenderMobileconfig(u.Username, u.Password, serverAddr, s.ServerCN, caPEM, s.DisplayTimezone, opts)
	if err != nil {
		s.Logger.Error("render mobileconfig", "err", err)
		http.Error(w, "生成描述文件失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-apple-aspen-config; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.mobileconfig"`, u.Username))
	_, _ = w.Write(out)
}

// handleUserMobileconfigQR GET /users/{id}/mobileconfig.qr.png
// 二维码内容是一次性安装 URL（扫码 → 浏览器打开 → 服务器返回 mobileconfig →
// iOS 看到正确的 MIME 类型自动弹"安装描述文件"，无需登录）
//
// 为什么不用 base64 data URL（A 方案）：mobileconfig + base64 后 ~1.5KB，
// QR 库默认会超容量；强制 Version 40（177x177 模块、High 容错 30%）也不行 ——
// 二维码密度到了物理识别极限，普通手机相机扫不出来。
// 用一次性 token URL 后，QR 里只编码 ~70 字节的短 URL，二维码密度正常，
// 任何手机都能扫。
//
// 流程：
//  1. 管理员点 QR → 本 handler 生成 token → 返回 QR PNG
//  2. 手机扫码 → 浏览器访问 https://panel/install/{token}
//  3. handleInstallByToken 验证后 stream mobileconfig → iOS 弹安装
//  4. token 一次性消费，立即失效
func (s *Server) handleUserMobileconfigQR(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireUserID(w, r)
	if !ok {
		return
	}
	if _, err := s.Store.GetUserByID(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	if s.PanelHost == "" {
		http.Error(w, "服务器地址未配置", http.StatusInternalServerError)
		return
	}

	// 生成一次性 token（10 分钟 TTL，扫码 + 装配置必须 < 10 分钟完成）
	// v2.86-PR19(B4):Issue 现在返回 error;crypto/rand 失败时写 500 而不是 panic 挂进程。
	token, err := s.InstallTokens.Issue(id)
	if err != nil {
		s.Logger.Error("installtoken issue", "err", err, "user_id", id)
		http.Error(w, "生成安装令牌失败", http.StatusInternalServerError)
		return
	}

	// QR 内容：scheme://panelHost/install/{token}，极短（~70 字节），
	// 二维码密度正常，手机相机好扫。
	scheme := "https"
	if !s.Secure {
		scheme = "http"
	}
	installURL := fmt.Sprintf("%s://%s/install/%s", scheme, s.PanelHost, token)

	// 默认 Version + Medium 容错就够（< 100 字节内容远低于容量上限）。
	// 模块尺寸 -8（每模块 8px），二维码实际 ~256x256px。
	q, err := qrcode.New(installURL, qrcode.Medium)
	if err != nil {
		s.Logger.Error("qr new", "err", err, "url_len", len(installURL))
		http.Error(w, "生成二维码失败", http.StatusInternalServerError)
		return
	}
	png, err := q.PNG(-8)
	if err != nil {
		s.Logger.Error("qr png", "err", err)
		http.Error(w, "生成二维码失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// handleInstallByToken GET /install/{token}
// 一次性 token 消费：验证 → 查找 user → 渲染 mobileconfig → stream 给浏览器。
//
// iOS 看到 Content-Type=application/x-apple-aspen-config 自动弹"安装描述文件"。
//
// 安全：token 是 256 bit 随机数 + 一次性消费 + 10 分钟 TTL。不要求登录（扫码场景
// 跟登录态互斥 —— 让用户没账号密码也能装配置）。
func (s *Server) handleInstallByToken(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.NotFound(w, r)
		return
	}
	userID, err := s.InstallTokens.Consume(token)
	if err != nil {
		// 一次性 + 过期 / 不存在都返回 404 + 友好提示页（不是裸 404）
		s.renderInstallError(w, "链接已失效或不存在。\n请在管理面板重新生成二维码。")
		return
	}
	u, err := s.Store.GetUserByID(r.Context(), userID)
	if err != nil {
		s.Logger.Error("install: user gone", "user_id", userID, "err", err)
		s.renderInstallError(w, "用户不存在或已被删除。")
		return
	}
	serverAddr := s.ServerAddr
	if serverAddr == "" {
		http.Error(w, "服务器地址未配置", http.StatusInternalServerError)
		return
	}

	var caPEM []byte
	if s.CertIncludeCA {
		caPEM = s.CACertPEM
	}
	// v2-76+：EAP-MSCHAPv2 模式，AuthName/AuthPassword 直接写进 profile，iOS 不弹密码框。
	// v2.85-PR3:透传 s.DisplayTimezone。
	// v2.86-PR12.21:同上,读 user overlay。
	// v2.86-PR12.22:三层合并 builtin + admin defaults + user overlay。
	if u.MobileConfigOpts != "" {
		if _, err := cert.DecodeMobileConfigOpts(u.MobileConfigOpts); err != nil {
			s.Logger.Error("install: decode mobileconfig opts", "user_id", userID, "err", err)
			http.Error(w, "描述文件配置损坏", http.StatusInternalServerError)
			return
		}
	}
	opts := s.resolveEffectiveOpts(u.MobileConfigOpts)
	out, err := cert.RenderMobileconfig(u.Username, u.Password, serverAddr, s.ServerCN, caPEM, s.DisplayTimezone, opts)
	if err != nil {
		s.Logger.Error("install: render mobileconfig", "err", err)
		http.Error(w, "生成描述文件失败", http.StatusInternalServerError)
		return
	}

	// iOS 关键 Content-Type：application/x-apple-aspen-config
	// iOS 看到这类型 + 用户点"安装"按钮 → 自动弹"安装描述文件"对话框
	w.Header().Set("Content-Type", "application/x-apple-aspen-config; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.mobileconfig"`, u.Username))
	// 防止 CDN / 代理缓存过期 mobileconfig
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}

// renderInstallError token 失效时给用户看的中文提示页（HTML，浏览器扫码时显示）。
func (s *Server) renderInstallError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	body := fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>链接失效</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #f5f5f7; margin: 0; padding: 40px 20px; text-align: center; color: #1d1d1f; }
  .card { max-width: 360px; margin: 0 auto; background: white; border-radius: 14px; padding: 32px 24px; box-shadow: 0 2px 12px rgba(0,0,0,0.08); }
  h1 { font-size: 18px; margin: 0 0 12px; }
  p { font-size: 14px; line-height: 1.5; margin: 0; color: #6e6e73; white-space: pre-line; }
  .emoji { font-size: 48px; margin-bottom: 16px; }
</style></head><body><div class="card">
  <div class="emoji">⚠️</div>
  <h1>链接失效</h1>
  <p>%s</p>
</div></body></html>`, msg)
	_, _ = w.Write([]byte(body))
}

// handleUserSSwan GET /users/{id}/sswan
// strongSwan 客户端配置（Android / Linux）
func (s *Server) handleUserSSwan(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireUserID(w, r)
	if !ok {
		return
	}
	u, err := s.Store.GetUserByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	serverAddr := s.ServerAddr
	if serverAddr == "" {
		http.Error(w, "服务器地址未配置", http.StatusInternalServerError)
		return
	}

	conf := renderSSwanTemplate(u.Username, s.ServerCN, serverAddr)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.sswan"`, u.Username))
	_, _ = w.Write([]byte(conf))
}

// renderSSwanTemplate 生成 strongSwan 客户端配置(Android app / Linux 共用)。
//
// 抽成独立函数而不是内联在 handler 里,是为了让
// TestSSwanConfig_* 能在不启 HTTP server 的前提下断言渲染结果 ——
// 这个模板的 Sprintf 格式符与实参数量必须严格配对,而 Go 编译期不检查,
// 数量错了只会在运行时产出 %!s(MISSING) 之类的垃圾。
func renderSSwanTemplate(username, serverCN, serverAddr string) string {
	connID := "ikev2-rw-" + username
	return fmt.Sprintf(`# strongSwan IKEv2 客户端配置
# 用户名: %[1]s
# 服务端: %[3]s (CN: %[2]s)
#
# Linux strongSwan 客户端:
#   1. 写入 /etc/swanctl/conf.d/%[1]s.conf
#   2. swanctl --load-all
#   3. swanctl -i -c %[4]s
#
# Android strongSwan app:
#   strongSwan App → 设置 → 导入配置 → 选本文件(%[1]s.sswan)
#   导入后手动填写密码(本文件不含密码,secret 留空)。
#   LE 模式(Lets Encrypt)下**无需**额外装 CA,系统已信任 ISRG Root;
#   自签模式才需要先把面板 CA 装到系统信任存储。
#   导入后还要在连接属性里确认 "本地 ID" 已填,%[1]s。
#
# iOS / macOS 不走本文件 —— 它们用 .mobileconfig(面板二维码)。

connections {
    %[4]s {
        local {
            auth = eap-mschapv2
            eap_id = %[1]s
        }
        remote {
            auth = pubkey
            id = %[2]s
        }
        remote_addrs = %[3]s
        children {
            %[4]s {
                remote_ts = 0.0.0.0/0, ::/0
                mode = tunnel
                esp_proposals = aes256gcm16-sha256, aes128gcm16-sha256
            }
        }
        version = 2
        mobike = yes
        # v2.86-PR19(A15):对齐服务端 proposals(见 configs/swanctl-ipv6-only.conf)。
        #
        # 原来这里只发 modp2048:
        #     aes256gcm16-sha256-modp2048, aes128gcm16-sha256-modp2048
        # 服务端虽有 modp2048 兜底(不会协商失败),但客户端**只发一条** proposal
        # 时,best-of 的结果必然是 modp2048 —— 即每次都落到最弱的 DH 组。
        # 这与服务端 :66-98 刻意做的"算法去弱"(curve25519 / ecp256 / modp3072
        # 优先,modp2048 仅兜底)完全背道而驰。
        #
        # strongSwan 的 proposals 列表顺序 = 优先级(preferred 在前),服务端会
        # 挑双方都支持的最强那一条。这里与服务端保持同序即可。
        proposals = aes256gcm16-sha256-curve25519, aes128gcm16-sha256-curve25519, aes256gcm16-sha256-ecp256, aes128gcm16-sha256-ecp256, aes256gcm16-sha256-modp3072, aes128gcm16-sha256-modp3072, aes256gcm16-sha256-modp2048, aes128gcm16-sha256-modp2048
        send_certreq = yes
        unique = replace
        rekey_time = 24h
    }
}

secrets {
    eap-%[1]s {
        id = %[1]s
        secret = ""
    }
}
`, username, serverCN, serverAddr, connID)
}

// handleUserCACert GET /ca.cert.pem
// Android 客户端单独下载 CA 证书
func (s *Server) handleUserCACert(w http.ResponseWriter, r *http.Request) {
	if !s.CertIncludeCA || len(s.CACertPEM) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="ca.cert.pem"`)
	_, _ = w.Write(s.CACertPEM)
}

// requireUserID 从 path 解析 id，错误返回 false。
func (s *Server) requireUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

// androidConfigData Android 11+ 原生客户端配置页模板数据。
type androidConfigData struct {
	PageMeta   // P1-B
	User       *store.User
	ServerAddr string
	ServerCN   string
	CACertPEM  []byte
	Flash      string
	Error      string
	// v2.86-PR19(A15):CertIncludeCA=false 时(LE 模式)页面不该渲染 /ca.cert.pem
	// 链接 —— 该路由在 LE 模式下返回 404,点了就是死链。
	CertIncludeCA bool
}

// handleUserAndroidConfig GET /users/{id}/android
//
// Android 11+ 系统设置 → 网络和互联网 → VPN → 添加 → "IKEv2/IPSec MSCHAPv2" 选项，
// 直接用用户名 + 密码 + 证书走 EAP-MSCHAPv2 认证（AOSP 11+ 原生支持，跟 iOS 同一段）。
// 这个页面展示 4 个字段（server / IPSec ID / 用户名 / 密码）和 CA 证书下载链接，
// 让用户复制粘贴到 Android 系统设置里。
func (s *Server) handleUserAndroidConfig(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireUserID(w, r)
	if !ok {
		return
	}
	u, err := s.Store.GetUserByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())

	// v2.86-pr23x 安全修复:不再从 query string 读取 flash。
	// 旧读法 `r.URL.Query().Get("flash")` 会把任意用户输入灌进 banner 的 .Msg。
	// 该参数无 producer(没有任何 redirect 带 ?flash=),属死代码,故直接移除而非转义。
	// 需要提示用户时走 POST → FlashStore.Set(见 flashConfigError / 各 handler)。
	// 参见 docs/architecture.md §12.9「FlashStore:为什么 flash 不走 query string」。
	flash := ""
	if s.ServerAddr == "" {
		flash = "ServerAddr 未配置（容器未设置 IKEV2_SERVER_ADDR_V6/V4 或 IKEV2_DOMAIN）。"
	}

	s.RenderPage(w, r, http.StatusOK, "android", androidConfigData{
		PageMeta:      PageMeta{Page: "android", Title: "Android 配置", PageKey: "users", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
		User:          u,
		ServerAddr:    s.ServerAddr,
		ServerCN:      s.ServerCN,
		CACertPEM:     s.CACertPEM,
		Flash:         flash,
		CertIncludeCA: s.CertIncludeCA,
	})
}
