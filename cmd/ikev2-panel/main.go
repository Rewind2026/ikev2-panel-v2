// 入口：flag 解析 + wire 各模块 + HTTP serve
// M1 骨架 / M3 接入 / M4 用户管理 / M5 客户端配置 + HTTPS
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/cert"
	"github.com/yourname/ikev2-panel-v2/internal/config"
	"github.com/yourname/ikev2-panel-v2/internal/expiry"
	"github.com/yourname/ikev2-panel-v2/internal/installtoken"
	"github.com/yourname/ikev2-panel-v2/internal/limit"
	"github.com/yourname/ikev2-panel-v2/internal/store"
	"github.com/yourname/ikev2-panel-v2/internal/swanctl"
	"github.com/yourname/ikev2-panel-v2/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	logger := cfg.NewLogger()
	slog.SetDefault(logger)

	logger.Info("ikev2-panel starting",
		"listen_addr", cfg.ListenAddr,
		"data_dir", cfg.DataDir,
		"cert_mode", cfg.CertMode,
		"ipv6_only", cfg.IPv6Only,
	)

	// ---------- 存储层 ----------
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		logger.Error("mkdir data dir", "err", err)
		os.Exit(1)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		logger.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// ---------- swanctl conf 初始化 + ReloadAll ----------
	// v2-76：回到 EAP-MSCHAPv2 模式。每个用户的密码在 /etc/swanctl/conf.d/<username>.conf
	// （由 handlers_users 调 WriteUserConf 写入）。这里触发一次 ReloadAll 让 charon 加载
	// 主配置（保证 swanctl.conf 里的 send_cert=always、eap-mschapv2 等设置生效）。
	scm := swanctl.New()
	if _, err := os.Stat("/etc/swanctl"); err == nil {
		// EAP 模式：swanctl.conf 自身没 secret，但仍调一次 ReloadAll 确保 conn 注册到 charon。
		if err := scm.ReloadAll(ctx); err != nil {
			logger.Warn("swanctl --load-all at startup", "err", err)
		} else {
			logger.Info("swanctl --load-all ok (EAP mode, per-user secrets in conf.d)")
		}
	} else {
		devConfDir := filepath.Join(cfg.DataDir, "swanctl-conf.d")
		_ = os.MkdirAll(devConfDir, 0o755)
		scm = scm.WithConfDir(devConfDir).WithSkipVici()
		logger.Info("dev mode: swanctl conf dir", "dir", devConfDir)
	}

	// ---------- 默认管理员（首次启动）----------
	defaultAdminUsername := "admin"
	defaultPassword, err := auth.GeneratePassword(cfg.DefaultUserPasswordLen)
	if err != nil {
		logger.Error("generate default password", "err", err)
		os.Exit(1)
	}
	hash, err := auth.BaseHashPassword(defaultPassword)
	if err != nil {
		logger.Error("hash default password", "err", err)
		os.Exit(1)
	}
	created, err := st.EnsureDefaultAdmin(ctx, defaultAdminUsername, hash)
	if err != nil {
		logger.Error("ensure default admin", "err", err)
		os.Exit(1)
	}
	if created {
		fmt.Println("========================================================")
		fmt.Printf(" 默认管理员已创建\n")
		fmt.Printf("   用户名: %s\n", defaultAdminUsername)
		fmt.Printf("   密  码: %s\n", defaultPassword)
		fmt.Printf("   （请立刻登录并修改密码）\n")
		fmt.Println("========================================================")
	}

	// ---------- 证书 ----------
	var (
		caCertPEM     []byte
		caKeyPEM      []byte
		serverCertPEM []byte
		serverKeyPEM  []byte
		includeCA     bool
	)
	if cfg.CertMode == "self-signed" {
		logger.Info("cert mode: self-signed, generating CA + server cert + panel cert")
		caCertPEM, caKeyPEM, err = cert.EnsureCA(cfg.DataDir)
		if err != nil {
			logger.Error("ensure CA", "err", err)
			os.Exit(1)
		}
		serverCertPEM, serverKeyPEM, err = cert.EnsureServerCert(cfg.DataDir, cfg.ServerCN, caCertPEM, caKeyPEM)
		if err != nil {
			logger.Error("ensure server cert", "err", err)
			os.Exit(1)
		}
		_, _, err = cert.EnsurePanelCert(cfg.DataDir, cfg.ServerCN, caCertPEM, caKeyPEM)
		if err != nil {
			logger.Error("ensure panel cert", "err", err)
			os.Exit(1)
		}
		includeCA = true
	} else {
		// LE 模式：acme.sh 由 entrypoint.sh 在容器启动时调用，本进程不做 ACME。
		// 这里只检查证书是否就位，否则退出提示用户先跑起 entrypoint。
		//
		// 证书持久化路径：/data/le/（持久化卷），不是容器内 /etc/swanctl/
		//   - fullchain.pem  fullchain.cer（含中间 CA）
		//   - privkey.pem     私钥
		leCertPath := filepath.Join(cfg.DataDir, "le", "fullchain.pem")
		leKeyPath := filepath.Join(cfg.DataDir, "le", "privkey.pem")
		if _, err := os.Stat(leCertPath); err != nil {
			logger.Error("LE mode but cert not found",
				"expected", leCertPath,
				"hint", "entrypoint.sh should have run acme.sh --issue before starting the Go process")
			os.Exit(1)
		}
		logger.Info("cert mode: letsencrypt, using ACME-issued cert", "cert", leCertPath)
		serverCertPEM, err = os.ReadFile(leCertPath)
		if err != nil {
			logger.Error("read LE cert", "err", err)
			os.Exit(1)
		}
		serverKeyPEM, err = os.ReadFile(leKeyPath)
		if err != nil {
			logger.Error("read LE key", "err", err)
			os.Exit(1)
		}
		// 面板 HTTPS 也要把证书复制到 /data/panel-tls/（兼容 cert.ServerCN 调用）
		if err := writePanelTLSCerts(cfg.DataDir, serverCertPEM, serverKeyPEM); err != nil {
			logger.Error("write panel-tls from LE cert", "err", err)
			os.Exit(1)
		}
		includeCA = false // LE 根证书已内置于客户端信任库
	}

	// 构建面板 HTTPS 证书（自签模式）
	var tlsCert *tls.Certificate
	if len(serverCertPEM) > 0 && len(serverKeyPEM) > 0 {
		c, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
		if err != nil {
			logger.Error("tls X509KeyPair", "err", err)
			os.Exit(1)
		}
		tlsCert = &c
	}

	// ---------- 模板 ----------
	templatesDir := filepath.Join("web", "templates")
	if _, err := os.Stat(templatesDir); err != nil {
		templatesDir = "/app/web/templates"
	}
	staticDir := filepath.Join("web", "static")
	if _, err := os.Stat(staticDir); err != nil {
		staticDir = "/app/web/static"
	}
	tmpl, err := web.LoadTemplates(templatesDir)
	if err != nil {
		logger.Error("load templates", "err", err, "dir", templatesDir)
		os.Exit(1)
	}

	// ---------- ServerCN 兜底（LE 模式用域名） ----------
	if cfg.CertMode == "letsencrypt" && cfg.Domain != "" && cfg.ServerCN == "" {
		cfg.ServerCN = cfg.Domain
	}

	// ---------- 自签模式证书安装到 swanctl（LE 模式证书由 entrypoint §7.5 装好）----------
	// 注意：scm 已在 store 打开后初始化（swanctl 用户密码同步见那段代码）。
	// 这里只补 self-signed 模式的 cert 安装 + load-creds。
	if cfg.CertMode == "self-signed" && includeCA {
		if _, err := os.Stat("/etc/swanctl"); err == nil {
			if err := installCertsToSwanctl(cfg.DataDir); err != nil {
				logger.Error("install certs to swanctl", "err", err)
				os.Exit(1)
			}
			if err := scm.LoadCreds(ctx); err != nil {
				logger.Error("vici load-creds (initial)", "err", err)
				// 不致命：用户第一次添加时也会触发
			} else {
				logger.Info("vici load-creds (initial) ok")
			}
		}
	}

	// ---------- v2-76 回 EAP 模式后，把现有用户密码写到 /etc/swanctl/conf.d/ ----------
	// 背景：v2-75 PSK 模式期间 entrypoint 清空了 conf.d/，DB 里已有的 user 没有 EAP secret。
	// 容器重启后 conf.d 落在容器内层会被清空，老 user 拨号会 MSCHAPV2 失败。
	// 这里遍历 store.User 给每个 enabled 的重新写一份（带 reload），保证老用户恢复拨号。
	// 不分证书模式：self-signed 和 LE 都要跑（容器内 conf.d 都不持久化）。
	if _, err := os.Stat("/etc/swanctl"); err == nil {
		users, err := st.ListUsers(ctx)
		if err != nil {
			logger.Warn("v2-76 recovery: list users", "err", err)
		} else {
			written := 0
			for _, u := range users {
				if !u.Enabled {
					continue
				}
				if !scm.UserConfExists(u.Username) {
					if err := scm.WriteUserConf(u.Username, u.Password); err != nil {
						logger.Warn("v2-76 recovery: write user conf", "user", u.Username, "err", err)
						continue
					}
					written++
				}
			}
			if written > 0 {
				if err := scm.ReloadAll(ctx); err != nil {
					logger.Warn("v2-76 recovery: reload after backfill", "err", err)
				} else {
					logger.Info("v2-76 recovery: backfilled EAP secrets for existing users", "count", written)
				}
			}
		}
	}

	// ---------- 限速文件目录 ----------
	lm := limit.New()
	if _, err := os.Stat("/var/lib/ikev2-panel/limits"); err == nil {
		logger.Info("limit dir: /var/lib/ikev2-panel/limits (container)")
	} else {
		devLimitDir := filepath.Join(cfg.DataDir, "limits")
		if err := os.MkdirAll(devLimitDir, 0o755); err != nil {
			logger.Error("mkdir dev limit dir", "err", err)
			os.Exit(1)
		}
		lm = lm.WithDir(devLimitDir)
		logger.Info("limit dir: " + devLimitDir + " (dev mode)")
	}

	// 选取 mobileconfig RemoteAddress（IKEv2 拨号目标，**不带端口**——IKEv2 走 UDP 500/4500）
	// LE 模式优先用域名（IKEv2 服务器身份 = SAN/CN = 域名），客户端填域名能正确校验证书
	mcAddr := cfg.ServerAddrV6
	if cfg.CertMode == "letsencrypt" && cfg.Domain != "" {
		mcAddr = cfg.Domain
	} else if !cfg.IPv6Only && cfg.ServerAddrV4 != "" {
		mcAddr = cfg.ServerAddrV4
	}
	// 端口从 mcAddr 剥掉（即便 env 误填了端口，iPhone 拨号也不需要）
	if mcAddr != "" {
		if h, _, err := net.SplitHostPort(mcAddr); err == nil {
			mcAddr = h
		}
	}
	if mcAddr == "" {
		logger.Warn("no server address configured; mobileconfig will fail until set IKEV2_SERVER_ADDR_V6/V4 or IKEV2_DOMAIN")
	}

	// 扫码 URL host（带端口，用于面板下载 mobileconfig 文件）
	host, listenPort, _ := net.SplitHostPort(cfg.ListenAddr)
	panelHost := mcAddr
	if panelHost != "" && listenPort != "" {
		// 8443 默认是端口，需要带；443/80 端口省略（标准 HTTPS）
		if listenPort != "443" && listenPort != "80" {
			panelHost = net.JoinHostPort(panelHost, listenPort)
		}
	}
	_ = host // unused

	// ---------- Web server ----------
	installTokens := installtoken.New(10 * time.Minute)
	srv := &web.Server{
		Store:         st,
		Swanctl:       scm,
		Limiter:       lm,
		Templates:     tmpl,
		SessionTTL:    cfg.SessionTTL,
		Cert:          tlsCert,
		Secure:        cfg.CookieSecure,
		ServerAddr:    mcAddr,
		PanelHost:     panelHost,
		ServerCN:      cfg.ServerCN,
		CACertPEM:     caCertPEM,
		CertIncludeCA: includeCA,
		InstallTokens: installTokens,
		Logger:        logger,
	}
	handler := web.New(srv, staticDir)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         nil, // HTTPS 模式下用 ListenAndServeTLS 直接传证书
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ---------- 后台 goroutines ----------
	go func() {
		// 流量采集（每 5 分钟）
		c := limit.NewCollector(scm, st, logger)
		c.Run(rootCtx)
	}()
	go func() {
		// 过期扫描（每 60 秒）
		c := expiry.NewChecker(scm, st, logger)
		c.Run(rootCtx)
	}()
	if cfg.CertMode == "letsencrypt" {
		go func() {
			// LE 续签健康检查（每 60 秒）
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			leCertPath := filepath.Join(cfg.DataDir, "le", "fullchain.pem")
			for {
				select {
				case <-rootCtx.Done():
					return
				case <-ticker.C:
					st := cert.CheckLERenewStatus(leCertPath)
					if st.LastRenewFailed {
						logger.Warn("LE cert: last renew FAILED, see LAST_RENEW_FAILED",
							"fallback", "service still using old cert")
					} else if cert.ShouldWarn(st.DaysLeft) {
						logger.Warn("LE cert expiring soon", "days_left", st.DaysLeft)
					} else {
						logger.Debug("LE cert ok", "days_left", st.DaysLeft)
					}
				}
			}
		}()
	}
	// IPv6 watch-dog：监控接口的 global IPv6，prefix 变化时自动更新 swanctl.conf
	// 适用场景：ISP 家宽 IPv6 prefix 动态下发（路由器重拨 / SLAAC 更新）
	// LE 模式 + IPv6_ONLY 才需要（IPv4 模式地址通常固定）
	if cfg.IPv6Only {
		go func() {
			iface := os.Getenv("IKEV2_OUT_IF")
			err := swanctl.RunIPv6Watch(rootCtx, swanctl.IPv6WatchConfig{
				ConfPath: "/etc/swanctl/swanctl.conf",
				Iface:    iface,
				Period:   60 * time.Second,
				Logger:   logger,
			})
			if err != nil {
				logger.Warn("ipv6watch exited", "err", err)
			}
		}()
	}
	logger.Info("background goroutines started: collector, expiry-checker" + func() string {
		if cfg.CertMode == "letsencrypt" {
			return ", le-certcheck"
		}
		return ""
	}() + func() string {
		if cfg.IPv6Only {
			return ", ipv6watch"
		}
		return ""
	}())
	go func() {
		// install token 过期清扫（每 5 分钟），防止 map 无限增长
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-ticker.C:
				if n := installTokens.Sweep(); n > 0 {
					logger.Debug("install token sweep", "removed", n)
				}
			}
		}
	}()

	go func() {
		if tlsCert != nil {
			logger.Info("https listening", "addr", cfg.ListenAddr)
			// 我们用 PEM 文件路径传入
			certPath := filepath.Join(cfg.DataDir, "panel-tls", "cert.pem")
			keyPath := filepath.Join(cfg.DataDir, "panel-tls", "key.pem")
			if err := httpSrv.ListenAndServeTLS(certPath, keyPath); err != nil && err != http.ErrServerClosed {
				logger.Error("https server error", "err", err)
				stop()
			}
		} else {
			logger.Info("http listening (no TLS)", "addr", cfg.ListenAddr)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("http server error", "err", err)
				stop()
			}
		}
	}()

	<-rootCtx.Done()
	logger.Info("wait-shutdown signal received, draining")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	logger.Info("bye")
}

// writePanelTLSCerts 把面板 HTTPS 证书写到 /data/panel-tls/{cert,key}.pem。
//
// 用途：自签模式由 cert.EnsurePanelCert 写，LE 模式由本函数写。
// httpSrv.ListenAndServeTLS 直接读这两个文件。
func writePanelTLSCerts(dataDir string, certPEM, keyPEM []byte) error {
	dir := filepath.Join(dataDir, "panel-tls")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, 0o644); err != nil {
		return fmt.Errorf("write cert.pem: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key.pem: %w", err)
	}
	return nil
}

// installCertsToSwanctl 把 cert.EnsureCA/EnsureServerCert 生成的 PEM 复制到
// strongSwan 默认查找路径 /etc/swanctl/{x509,private, cacerts}。
// 这样 VICI load-creds 之后，swanctl --list-certs 才能看到 server.cert.pem，
// 用户 conf 里的 `certs = server.cert.pem` 才能匹配。
//
// 注意：/etc/swanctl/ 目录强 Sown 6.0.1 启动时已创建（Dockerfile 阶段建好）。
func installCertsToSwanctl(dataDir string) error {
	// v2-79.1：按文件类型拆权限——cert 0644 可被其他进程读，key 必须 0600 只 root 可读
	// （之前所有 dst 都用 0o644，server.key.pem 私钥被同主机任何用户可读）
	type pair struct{ src, dst string; isKey bool }
	pairs := []pair{
		{filepath.Join(dataDir, "ca", "ca.cert.pem"), "/etc/swanctl/x509ca/ca.cert.pem", false},
		{filepath.Join(dataDir, "server", "server.cert.pem"), "/etc/swanctl/x509/server.cert.pem", false},
		{filepath.Join(dataDir, "server", "server.key.pem"), "/etc/swanctl/private/server.key.pem", true},
	}
	for _, p := range pairs {
		mode := os.FileMode(0o644)
		if p.isKey {
			mode = 0o600
		}
		in, err := os.Open(p.src)
		if err != nil {
			return fmt.Errorf("open src %s: %w", p.src, err)
		}
		defer in.Close()
		out, err := os.OpenFile(p.dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return fmt.Errorf("open dst %s: %w", p.dst, err)
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return fmt.Errorf("copy %s -> %s: %w", p.src, p.dst, err)
		}
		out.Close()
	}
	return nil
}