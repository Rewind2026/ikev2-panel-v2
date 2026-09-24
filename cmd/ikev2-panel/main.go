// 入口：flag 解析 + wire 各模块 + HTTP serve
// M1 骨架 / M3 接入 / M4 用户管理 / M5 客户端配置 + HTTPS
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/cert"
	"github.com/yourname/ikev2-panel-v2/internal/config"
	"github.com/yourname/ikev2-panel-v2/internal/ddns"
	"github.com/yourname/ikev2-panel-v2/internal/expiry"
	"github.com/yourname/ikev2-panel-v2/internal/installtoken"
	"github.com/yourname/ikev2-panel-v2/internal/limit"
	"github.com/yourname/ikev2-panel-v2/internal/metrics"
	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
	rt "github.com/yourname/ikev2-panel-v2/internal/runtime"
	"github.com/yourname/ikev2-panel-v2/internal/store"
	"github.com/yourname/ikev2-panel-v2/internal/swanctl"
	"github.com/yourname/ikev2-panel-v2/internal/web"
)

func main() {
	envCfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	// v2.86-PR13.0:env + panelstate 合并集中在 runtime 包。
	// 之前是 config.Load() 内部直接 import panelstate,造成模块反向依赖。
	cfg, err := rt.Merge(envCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime merge error: %v\n", err)
		os.Exit(1)
	}
	// cfg.CookieSecure / cfg.DisplayTimezone 等都从嵌入的 *config.Config 直接读
	// 这里用 cfg.NewLogger() 时 cfg 是 *Runtime,*config.Config 的方法集是 promoted 的
	logger := cfg.Config.NewLogger()
	slog.SetDefault(logger)

	// v2.85-PR1:从 /etc/ikev2-panel-version 读构建期注入的版本号。
	// dev 模式(本地 go build)无此文件 → fallback "dev"。
	panelVersion := "dev"
	if b, err := os.ReadFile("/etc/ikev2-panel-version"); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			panelVersion = v
		}
	}

	logger.Info("ikev2-panel starting",
		"version", panelVersion,
		"go_version", runtime.Version(),
		"listen_addr", cfg.ListenAddr,
		"data_dir", cfg.DataDir,
		"cert_mode", cfg.CertMode,
		"ipv6_only", cfg.IPv6Only,
		"aliyun_creds_source", cfg.AliyunAccessKeySource,
		"aliyun_creds_set", cfg.AliyunAccessKeyID != "",
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
		// v2.85-PR4 (Q5-02):等 charon ready(最多 10s),避免首次启动时 ReloadAll 失败 → boot loop。
		// entrypoint.sh 启动 charon 是异步 fork(),Go 进程可能比 charon 先 ready。
		// 探测失败**不退出**,后续 cron / handler reload 会自然重试。
		if err := swanctl.WaitForCharonReady("", 10*time.Second); err != nil {
			logger.Warn("charon not ready, ReloadAll may fail (will retry via cron/reload later)",
				"err", err,
				"hint", "this is normal if ipsec is still starting; entrypoint.sh will retry")
		} else {
			logger.Info("charon ready")
		}

		// EAP 模式：swanctl.conf 自身没 secret,但仍调一次 ReloadAll 确保 conn 注册到 charon。
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
		// v2.85-PR2:除 stdout 外,同步把密码写到 panel-state 目录,
		// 让管理员在容器销毁/日志轮转后仍能找回。
		// 文件存在时**不覆盖**(避免重启时把新生成的密码覆盖旧密码)。
		// 面板 home 检测此文件存在 → 显示"还在用默认密码"横幅。
		// dev 模式(cfg.DataDir 不是 /data)→ 文件落到 <DataDir>/panel-state/,
		//   这样 dev 本地开发也能验证横幅效果(不污染生产容器)。
		pwDir := panelstate.Dir
		if cfg.DataDir != "/data" {
			pwDir = filepath.Join(cfg.DataDir, "panel-state")
		}
		pwPath := filepath.Join(pwDir, "INITIAL_ADMIN_PASSWORD.txt")
		if _, statErr := os.Stat(pwPath); os.IsNotExist(statErr) {
			if err := os.MkdirAll(pwDir, 0o700); err != nil {
				logger.Warn("mkdir panel-state dir for initial admin password", "err", err)
			} else if err := os.WriteFile(pwPath, []byte(defaultPassword), 0o600); err != nil {
				logger.Warn("write initial admin password file", "err", err)
			} else {
				logger.Info("initial admin password persisted", "path", pwPath)
			}
		}
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
		serverCertPEM, serverKeyPEM, err = cert.EnsureServerCert(cfg.DataDir, cfg.ServerCN, caCertPEM, caKeyPEM, cfg.ServerAddrV6, cfg.ServerAddrV4)
		if err != nil {
			logger.Error("ensure server cert", "err", err)
			os.Exit(1)
		}
		_, _, err = cert.EnsurePanelCert(cfg.DataDir, cfg.ServerCN, caCertPEM, caKeyPEM, cfg.ServerAddrV6, cfg.ServerAddrV4)
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

	// ---------- panelstate 文件预热 ----------
	// v2-83 / v2.86-PR12.5 / v2.86-PR12.22 / v2.86-PR13.2 四个 panelstate store 启动时读磁盘。
	// aliyun.creds 在 DDNSEnabled=true 分支已 LoadAliyun() 过,这里只补 cert.conf /
	// mobileconfig.defaults / subnet.conf。
	//
	// 为什么不 fatal:文件缺失 = 用户没在面板改过,启动失败反而阻挡部署。
	// 运行时改完 → 进程内缓存即时生效(handler 端用 ReadMobileConfigDefaults / ReadSubnetConfig 拿最新值)。
	//
	// v2.86-PR13.2:subnet.conf 启动时预读是为了日志一致性(可观察"是否从 panelstate 加载了 subnet"),
	// 真正的运行期生效是 handler 端调 Manager.UpdatePoolsAndReload。
	certCfgStore := panelstate.NewCertConfigStore()
	if _, err := certCfgStore.LoadCertConfig(); err != nil {
		logger.Warn("load cert config", "err", err, "fallback", "env")
	} else {
		logger.Info("cert config loaded")
	}
	subnetCfgStore := panelstate.NewSubnetConfigStore()
	if subnetCfg, err := subnetCfgStore.LoadSubnetConfig(); err != nil {
		logger.Warn("load subnet config", "err", err, "fallback", "env/auto")
	} else if subnetCfg != nil {
		logger.Info("subnet config loaded from panelstate",
			"ipv4_subnet", subnetCfg.IPv4Subnet,
			"ipv6_subnet", subnetCfg.IPv6Subnet,
		)
	}
	mcDefaultsStore := panelstate.NewMobileConfigDefaultsStore()
	if _, err := mcDefaultsStore.LoadMobileConfigDefaults(); err != nil {
		logger.Warn("load mobileconfig defaults", "err", err, "fallback", "builtin")
	} else {
		logger.Info("mobileconfig defaults loaded")
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
	tmpl, err := web.LoadTemplates(templatesDir, staticDir, cfg.DisplayTimezone)
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
	//
	// v2.86-PR12.13 优先级链（高 → 低）：
	//   1. DDNS 启用 + 有 ALIYUN_RR + ALIYUN_DOMAIN → 用 DDNS 域名（iOS 校验 SAN 时匹配证书 dNSName）
	//   2. LE 模式 + IKEV2_DOMAIN → 用 LE 域名（同上）
	//   3. 双栈 + ServerAddrV4 → 用 v4 字面量（仅自签模式）
	//   4. IPv6-only + ServerAddrV6 → 用 v6 字面量（自签模式 fallback；需要证书加 IPv6 SAN，
	//      见 internal/cert/generate.go:generateServerCert 改进 1）
	// 关键：iOS 拨号用 IP 字面量时,会校验证书 IPAddresses SAN；用域名时校验 dNSName SAN。
	// 优先用域名避免"拨号 OK 但 iOS 校验证书失败 → 静默断线"问题。
	mcAddr := ""
	if cfg.DDNSEnabled && cfg.AliyunDomain != "" && cfg.AliyunRR != "" {
		mcAddr = cfg.AliyunRR + "." + cfg.AliyunDomain
		logger.Info("mobileconfig RemoteAddress using DDNS domain", "addr", mcAddr)
	}
	if mcAddr == "" && cfg.CertMode == "letsencrypt" && cfg.Domain != "" {
		mcAddr = cfg.Domain
	}
	if mcAddr == "" && !cfg.IPv6Only && cfg.ServerAddrV4 != "" {
		mcAddr = cfg.ServerAddrV4
	}
	if mcAddr == "" {
		mcAddr = cfg.ServerAddrV6
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
	_, listenPort, _ := net.SplitHostPort(cfg.ListenAddr)
	panelHost := mcAddr
	if panelHost != "" && listenPort != "" {
		// 8443 默认是端口，需要带；443/80 端口省略（标准 HTTPS）
		if listenPort != "443" && listenPort != "80" {
			panelHost = net.JoinHostPort(panelHost, listenPort)
		}
	}

	// ---------- DDNS 同步器(v2-82 + v2-84 family-aware)提前创建 ----------
	// 在 web.Server 装配之前创建,这样 srv.DDNSSync 可以直接赋值(面板
	// 立即能查询状态,即使 goroutine 还没起来)。
	//   - IKEV2_DDNS_ENABLED=true → 创建 sync(family 由 cfg.DDNSFamily 决定,默认 dual)
	//   - IKEV2_DDNS_ENABLED=false → ddnsSync = nil,面板不显示 DDNS 卡片
	//
	// v2-84 改动:
	//   - 装配条件改为 cfg.DDNSEnabled(去掉了 cfg.IPv6Only 强约束)
	//   - 加 cfg.DDNSFamily 注入(cfg.DDNSProbeTarget 已废弃)
	//   - 注入 IPv4 探测 swanctl.DetectGlobalV4
	//
	// v2.86-pr23a:面板可改 RR / EnableA / EnableAAAA / Period,启动时从
	// panelstate 读出来覆盖 env(用户改了面板配置就以面板为准)。
	//
	// v2-83:CredentialGetter 让 DDNS 每次 tick 重新读 /data/panel-state/aliyun.creds,
	// 面板 UI 改凭证后 DDNS 下次 tick 自动用新凭证,无需重启容器。
	//
	// panelstate 预读:用户可能在面板改过配置(/api/ddns/config 写入 ddns.conf),
	// 启动时读出来覆盖 env 的 AliyunRR / DDNSFamily / Period,真正做到面板是 source of truth。
	ddnsCfgStore := panelstate.NewDDNSConfigStore()
	ddnsCfg, ddnsCfgErr := ddnsCfgStore.LoadDDNSConfig()
	if ddnsCfgErr != nil {
		logger.Warn("load ddns config from panelstate", "err", ddnsCfgErr, "fallback", "env")
		ddnsCfg = nil
	} else if ddnsCfg != nil {
		logger.Info("ddns config loaded from panelstate",
			"rr", ddnsCfg.RR,
			"enable_a", ddnsCfg.EnableA,
			"enable_aaaa", ddnsCfg.EnableAAAA,
			"period_seconds", ddnsCfg.PeriodSeconds,
		)
		// 用 panelstate 的值覆盖 env 启动值
		if ddnsCfg.RR != "" {
			cfg.AliyunRR = ddnsCfg.RR
		}
		// family 从 (EnableA, EnableAAAA) 反推回 v4/v6/dual 字符串(传给 ddns.NewSync)
		switch {
		case ddnsCfg.EnableA && ddnsCfg.EnableAAAA:
			cfg.DDNSFamily = "dual"
		case ddnsCfg.EnableA:
			cfg.DDNSFamily = "v4"
		case ddnsCfg.EnableAAAA:
			cfg.DDNSFamily = "v6"
		default:
			// 两个都关 → 保持 env 值
		}
	}
	ddnsPeriod := 60 * time.Second
	if ddnsCfg != nil && ddnsCfg.PeriodSeconds > 0 {
		ddnsPeriod = time.Duration(ddnsCfg.PeriodSeconds) * time.Second
	}

	var ddnsSync *ddns.Sync
	if cfg.DDNSEnabled {
		credStore := panelstate.NewStore()
		_, _ = credStore.LoadAliyun() // 启动时预热缓存(失败也不 fatal,getter 会重试)
		ddnsSync = ddns.NewSync(ddns.Config{
			Enabled:    cfg.DDNSEnabled,
			Family:     cfg.DDNSFamily, // v2-84:"v4" / "v6" / "dual"(pr23a 已根据 panelstate 反推)
			EnableA:    ddnsCfg != nil && ddnsCfg.EnableA,
			EnableAAAA: ddnsCfg != nil && ddnsCfg.EnableAAAA,
			// v2.86-pr23l:DetectTarget 字段废弃,IPv4 探测改用 internal/publicip API。
			// env 启动值作为 fallback,getter 优先
			AliyunAccessKeyID:     cfg.AliyunAccessKeyID,
			AliyunAccessKeySecret: cfg.AliyunAccessKeySecret,
			CredentialGetter: func() (string, string, bool) {
				c, err := credStore.ReadAliyun()
				if err != nil || c == nil {
					return "", "", false
				}
				return c.KeyID, c.KeySecret, true
			},
			Domain:         cfg.AliyunDomain,
			RR:             cfg.AliyunRR,
			Iface:          os.Getenv("IKEV2_OUT_IF"),
			Period:         ddnsPeriod,
			Throttle:       60 * time.Second,
			StateFile:      "/etc/ikev2/ddns.conf",
			LastFailedFile: "/data/le/LAST_DDNS_FAILED",
			Logger:         logger,
		})
		ddnsSync.SetDetectV6(swanctl.DetectGlobalV6)
		ddnsSync.SetDetectV4(swanctl.DetectGlobalV4) // v2-84
	}

	// ---------- Web server ----------
	installTokens := installtoken.New(10 * time.Minute)
	// P1-A：flash 存储（5 分钟 TTL,跟 install token 一致）
	flashStore := web.NewFlashStore(5 * time.Minute)
	// v2.86-PR13.3:读 swanctl.conf 当前 addrs 段,作为"清除面板配置"按钮的恢复值。
	// dev 模式下文件不存在 → 空,handler 走 "重启回 env" 路径。
	startupV4, startupV6 := scm.ReadCurrentPoolsFromFile()
	srv := &web.Server{
		Store:         st,
		Swanctl:       scm,
		Limiter:       lm,
		Templates:     tmpl,
		SessionTTL:    cfg.SessionTTL,
		Version:       panelVersion, // v2.86-PR18:登录页品牌区展示用
		Cert:          tlsCert,
		Secure:        cfg.CookieSecure,
		ServerAddr:    mcAddr,
		PanelHost:     panelHost,
		ServerCN:      cfg.ServerCN,
		CACertPEM:     caCertPEM,
		CertIncludeCA: includeCA,
		// v2.86-PR12.23:PayloadIdentifier 反向 DNS 前缀。空 → cert 内 fallback 默认值。
		// env IKEV2_PAYLOAD_ID_BASE 覆盖(给有自有反向域名的用户用)。
		PayloadIdentifierBase: os.Getenv("IKEV2_PAYLOAD_ID_BASE"),
		InstallTokens:         installTokens,
		FlashStore:            flashStore,
		// P1-C：M6 监控所需的服务端信息。
		//   - CertMode: "self-signed" / "letsencrypt"
		//   - DataDir:  持久化目录,LE 证书路径 = {DataDir}/le/fullchain.pem
		CertMode: cfg.CertMode,
		DataDir:  cfg.DataDir,
		// v2-82:DDNS 同步器(IPv4-only 模式时 nil)
		DDNSSync: ddnsSync,
		// v2-83:面板运行时状态(凭证卡用)+ 凭证来源
		PanelState: panelstate.NewStore(),
		// v2.86-PR12.5:证书配置运行时持久化(模式/域名/CN/邮箱)
		CertConfigStore: certCfgStore,
		// v2.86-PR13.2:客户端虚拟 IP 段运行时持久化(IPv4 pool + IPv6 ULA pool)
		SubnetConfigStore: subnetCfgStore,
		// v2.86-PR13.3:启动时生效的 IP 段(读 swanctl.conf 当前 addrs 段)。
		// 供 handlers_subnet.clear 用 — 用户点"清除面板配置"时立即改回这个值,
		// 不需要重启容器。dev 模式下文件不存在 → 空,handler 走 "重启回 env" 路径。
		StartupIPv4Subnet: startupV4,
		StartupIPv6Subnet: startupV6,
		// v2.86-PR12.22:管理员全局 mobileconfig 默认值,运行时热改无需重启。
		MobileConfigDefaults:  mcDefaultsStore,
		AliyunAccessKeySource: cfg.AliyunAccessKeySource,
		// v2.86-PR12.5:证书配置来源(启动日志用)
		CertConfigSource: cfg.CertConfigSource,
		// v2.85-PR3:显示时区(handler 渲染 DDNS / mobileconfig 时间字符串用)
		DisplayTimezone: cfg.DisplayTimezone,
		// v2.85-PR8(U04):Prometheus-style metrics registry。
		// middleware 计数 / handleMetrics 输出文本格式。
		Metrics: metrics.New(),
		// v2.86-PR9:登录 rate limit + 全局 POST 限速。
		// 内存 map + 持久化 /data/panel-state/ratelimit.json。
		RateLimiter: auth.NewRateLimiter(auth.DefaultRateLimitConfig(cfg.DataDir)),
		Logger:      logger,
	}
	handler := web.New(srv, staticDir)

	// v2-83:面板 HTTPS 证书热重载。
	// 用 atomic.Pointer 缓存当前 tls.Certificate,GetCertificate 回调里 atomic.Load
	// 拿最新值;**不在握手路径上做磁盘 I/O**(高频路径)。
	// SIGHUP goroutine 负责读 + 原子替换;只在续期(每天 1 次)触发,不在 handshake。
	var certCache atomic.Pointer[tls.Certificate]
	if tlsCert != nil {
		certCache.Store(tlsCert)
	}

	// panel-tls 路径(自签模式 cert.EnsurePanelCert 写、LE 模式 writePanelTLSCerts 写)
	panelCertPath := filepath.Join(cfg.DataDir, "panel-tls", "cert.pem")
	panelKeyPath := filepath.Join(cfg.DataDir, "panel-tls", "key.pem")

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		// v2-83: HTTPS 时预构造 TLSConfig(避免 ListenAndServeTLS 内部覆盖掉 GetCertificate)。
		// 注意:TLSConfig 非 nil 时 ListenAndServeTLS(cert,key) **会忽略** 这两个参数,
		// 改用 ListenAndServeTLS("","") 才能让 GetCertificate 生效。
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				// atomic.Load,无锁,O(1);失败时返回当前已知证书
				c := certCache.Load()
				if c == nil {
					return nil, fmt.Errorf("no certificate loaded")
				}
				return c, nil
			},
		},
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// v2-83:SIGHUP 热重载监听(独立 channel,buffered=1,跟 SIGINT/SIGTERM 不冲突)。
	// 触发场景:acme.sh --reloadcmd -> ikev2-reload.sh -> kill -HUP <pid>。
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)

	// (sighup startBG 在 startBG helper 定义之后追加 — 见下面"sighup reload"块)

	// ---------- 后台 goroutines ----------
	// v2.85-PR4 (Q2-01):6 个后台 goroutine 用 sync.WaitGroup 跟踪 + 命名,
	// SIGTERM 时 bgWg.Wait(5s) 等全部自然退出(避免 sqlite 写到一半 / swanctl reload 中途被强切)。
	// panic recover:defer 兜住,日志打印 stack,不退出整个进程。
	var (
		bgWg   sync.WaitGroup
		bgList []string
	)
	startBG := func(name string, fn func()) {
		bgWg.Add(1)
		bgList = append(bgList, name)
		go func() {
			defer bgWg.Done()
			defer func() {
				if r := recover(); r != nil {
					logger.Error("bg goroutine panic",
						"name", name,
						"recover", r,
						"stack", string(debug.Stack()))
				}
			}()
			fn()
			logger.Debug("bg goroutine exited", "name", name)
		}()
	}

	// v2.86-PR13.0:审查报告 C4。SIGHUP 监听改走 startBG 框架,获得 panic recover +
	// WaitGroup 跟踪 + 统一日志格式。原裸 go func() 无这层防御,SIGHUP reload
	// 路径 panic 会让整个进程挂掉。
	startBG("sighup", func() {
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-sighup:
				reloadPanelTLSCert(logger, panelCertPath, panelKeyPath, &certCache)
			}
		}
	})

	// v2.86-PR13.0:审查报告 C4。HTTPS / 明文 HTTP server 也走 startBG。
	// 之前是裸 go func(),ListenAndServe 阻塞 panic 会让进程挂掉。
	// startBG 的 panic recover 兜住,但因为 ListenAndServe 是阻塞的,这个 fn() 永远
	// 不返回 — SIGTERM 时 Shutdown 让它自然退出,startBG 的 bgWg.Done() 才会执行。
	startBG("http-listener", func() {
		if tlsCert != nil {
			logger.Info("https listening", "addr", cfg.ListenAddr, "tls_reload", "SIGHUP")
			if err := httpSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
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
	})

	// 1) 流量采集（每 5 分钟）
	startBG("collector", func() {
		c := limit.NewCollector(scm, st, logger)
		c.Run(rootCtx)
	})
	// 2) 过期扫描（每 60 秒）
	startBG("expiry", func() {
		c := expiry.NewChecker(scm, st, logger)
		c.Run(rootCtx)
	})
	// 2.5) sessions 表 GC（每 15 分钟清理已过期 session 记录,避免无限增长）
	// v2.86-PR13.0 审查项 C3:DeleteExpiredSessions 已存在但无调度器,
	// 现按 15min 周期调用,DELETE 量小且已建 idx_sessions_expires 索引,O(过期数)。
	startBG("session-gc", func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-ticker.C:
				n, err := st.DeleteExpiredSessions(rootCtx, time.Now())
				if err != nil {
					logger.Warn("session gc failed", "err", err)
				} else if n > 0 {
					logger.Info("session gc", "deleted", n)
				}
			}
		}
	})
	// 3) LE 续签健康检查（每 60 秒,仅 LE 模式启用）
	if cfg.CertMode == "letsencrypt" {
		startBG("le-certcheck", func() {
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
		})
	}
	// 4) IPv6 watch-dog:监控接口的 global IPv6,prefix 变化时自动更新 swanctl.conf
	// 适用场景:ISP 家宽 IPv6 prefix 动态下发(路由器重拨 / SLAAC 更新)
	// LE 模式 + IPv6_ONLY 才需要(IPv4 模式地址通常固定)
	if cfg.IPv6Only {
		startBG("ipv6watch", func() {
			iface := os.Getenv("IKEV2_OUT_IF")
			err := swanctl.RunIPv6Watch(rootCtx, swanctl.IPv6WatchConfig{
				ConfPath: "/etc/swanctl/swanctl.conf",
				Iface:    iface,
				Period:   60 * time.Second,
				Logger:   logger,
				// v2.86-PR16:让 ipv6watch 的 reload 走 Manager.ReloadAll,
				// 复用 reloadMu,避免和 web handler 并发时 charon 读到半截 conf.d（H2）。
				Reloader: scm,
			})
			if err != nil {
				logger.Warn("ipv6watch exited", "err", err)
			}
		})
	}
	// 5) DDNS 同步器(仅 DDNSEnabled=true 时启动)
	// Sync 对象已在 web.Server 装配前创建(见上方),这里只启动 Run。
	if ddnsSync != nil {
		startBG("ddns", func() {
			if err := ddnsSync.Run(rootCtx); err != nil {
				logger.Warn("ddns sync exited", "err", err)
			}
		})
	}

	// v2.86-PR15:SA lifecycle 监听(charon VICI ike-updown 事件)
	// 把"用户连接/断开"写进 audit_log,运维在 /audit 看到。
	// 设计:Subscribe("ike-updown") + NotifyEvents channel + 后台 retry。
	// dev / 测试模式(sc.Swanctl.SkipVici=true)跳过 — 测试不需要真 charon。
	if !scm.SkipVici {
		startBG("sa-lifecycle", func() {
			listener := swanctl.NewLifecycleListener(
				"",
				swanctl.NewSALifecycleAuditHandler(st, logger),
				logger,
			)
			listener.Run(rootCtx)
		})
	}

	// 6) install token + flash store 过期清扫(每 5 分钟,防止 map 无限增长)
	startBG("token-sweep", func() {
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
				// P1-A:flash store 同样需要清扫(虽然通常一次性消费,
				// 但如果用户关闭页面没渲染,flash 留在 store 里直到过期)
				if n := flashStore.Sweep(); n > 0 {
					logger.Debug("flash store sweep", "removed", n)
				}
			}
		}
	})

	logger.Info("background goroutines started",
		"count", len(bgList),
		"names", strings.Join(bgList, ","))

	// v2.86-PR17:明文 HTTP 监听(内网/调试用)。
	// - cfg.HTTPListenAddr == "" → 不起,行为跟之前完全一致
	// - cfg.HTTPListenAddr != "" → 起第二个 http.Server(handler 共用),
	//   自动关 cookie Secure,并打醒目 WARN 提醒 admin cookie 走明文
	if cfg.HTTPListenAddr != "" {
		if cfg.CookieSecure {
			// HTTPS 入口下浏览器会拒收 Secure cookie,等同登录态丢失。
			// 启 HTTP 入口时强制把 Secure 关掉,让 cookie 在明文入口能存能带。
			cfg.CookieSecure = false
			logger.Warn("cookie Secure auto-disabled because HTTP panel is enabled",
				"reason", "Secure cookies are dropped by browsers on plain http",
				"action", "set IKEV2_COOKIE_SECURE=true only if you do NOT use the HTTP entrypoint")
		}
		// 把 web.Server.Secure 同步刷新(handler 渲染 cookie 时会读)
		srv.Secure = cfg.CookieSecure
		logger.Warn("HTTP panel listening (plaintext); admin session cookie is now in cleartext",
			"addr", cfg.HTTPListenAddr,
			"safety", "bind to 127.0.0.1 or trusted LAN only; never expose to public internet",
			"note", "this listener is independent of HTTPS/IKEv2/strongSwan and does not affect tunnel certificates")
		httpPlainSrv := &http.Server{
			Addr:              cfg.HTTPListenAddr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			// 故意不带 TLSConfig → 走 ListenAndServe() 明文
		}
		// v2.86-PR13.0:明文 HTTP 监听走 startBG 框架,获得 panic recover。
		// 跟 https listener 同模式:fn() 阻塞在 ListenAndServe 直到 Shutdown。
		startBG("http-plain-listener", func() {
			if err := httpPlainSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("http (plaintext) server error", "err", err, "addr", cfg.HTTPListenAddr)
				stop()
			}
		})
		// 优雅关闭时也要 Shutdown 第二个 server
		defer func() {
			shutdownCtx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel2()
			if err := httpPlainSrv.Shutdown(shutdownCtx2); err != nil {
				logger.Error("http (plaintext) graceful shutdown failed", "err", err)
			}
		}()
	}

	// v2.86-PR13.0:HTTP server 监听已走 startBG("http-listener") 启动,这里不再裸 go func。
	// SIGTERM 时 rootCtx.Done() 触发后续 Shutdown 序列。

	<-rootCtx.Done()
	logger.Info("shutdown signal received, draining",
		"timeout", "5s", "bg_goroutines", len(bgList))

	// 1) 先 Shutdown HTTP — 停止接新请求,等在飞请求完成
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http graceful shutdown failed", "err", err)
	}
	cancel()

	// 2) 等后台 goroutine 自然退出
	// rootCtx 已 cancel,所有 watch rootCtx.Done() 的 goroutine **理论上**会立即 return。
	// bgWg.Wait() 给 5s 兜底:如果某个 goroutine 卡在系统调用(磁盘 IO / network),等它完成。
	// 超时只 log error,不再强制 kill(docker stop SIGKILL 在外层 10s 后兜底,本进程不重复)。
	//
	// 注意:此处刻意不用 startBG——startBG 内部会调 bgWg.Done(),
	// 而本 goroutine 自身就是 bgWg 的等待者,改用 startBG 会导致死锁
	// (bgWg.Wait() 永远等不到自身 Done)。这是协调器语义,不是普通后台任务,
	// 唯一职责是把 bgWg 收敛信号从 sync.WaitGroup 转换成 channel close。
	done := make(chan struct{})
	go func() {
		bgWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		logger.Info("all background goroutines exited cleanly")
	case <-time.After(5 * time.Second):
		logger.Error("background goroutines drain timeout (5s)",
			"hint", "some goroutines may have been killed mid-operation",
			"risk", "data inconsistency (sqlite write half / swanctl reload half)")
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

// reloadPanelTLSCert SIGHUP 处理器:重读 /data/panel-tls/{cert,key}.pem 并替换 atomic 缓存。
//
// v2-83 设计(评审 B 修正):
//   - 读 + 解析都在 SIGHUP 路径(每天最多 1 次,非热路径)
//   - atomic.Store 替换 tls.Certificate,无锁
//   - GetCertificate 回调 atomic.Load,无 I/O,无锁
//   - 任何失败保留旧证书(降级,不停服)
//
// 失败场景:
//   - 文件不存在(自签模式下未生成):保留旧 cert(理论上不可能,启动时写过)
//   - 解析失败(PEM 损坏):保留旧 cert,日志 WARN
//   - 文件权限错:保留旧 cert,日志 WARN
func reloadPanelTLSCert(logger *slog.Logger, certPath, keyPath string, cache *atomic.Pointer[tls.Certificate]) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		logger.Warn("SIGHUP reload: read cert failed, keeping previous", "err", err, "path", certPath)
		return
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		logger.Warn("SIGHUP reload: read key failed, keeping previous", "err", err, "path", keyPath)
		return
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		logger.Warn("SIGHUP reload: parse X509KeyPair failed, keeping previous", "err", err)
		return
	}
	cache.Store(&c)
	logger.Info("SIGHUP reload: panel TLS cert updated",
		"not_after", c.Leaf.NotAfter,
		"path", certPath,
	)
}

// installCertsToSwanctl 把 cert.EnsureCA/EnsureServerCert 生成的 PEM 复制到
// strongSwan 默认查找路径 /etc/swanctl/{x509,private, cacerts}。
// 这样 VICI load-creds 之后，swanctl --list-certs 才能看到 server.cert.pem，
// 用户 conf 里的 `certs = server.cert.pem` 才能匹配。
//
// 注意：/etc/swanctl/ 目录强 Sown 6.0.1 启动时已创建（Dockerfile 阶段建好）。
//
// P0-3 改进：用 swanctl.AtomicWriteFile（write tmp + rename 原子替换）替代
// 之前的 OpenFile + io.Copy。如果中间被中断（容器 OOM kill / 系统重启），
// 之前会留下半截 cert 文件 → charon 启动读半截 → SA 全挂。
// 现在：tmp 失败 → 原 cert 保留；rename 原子 → 观察者总看到完整文件。
func installCertsToSwanctl(dataDir string) error {
	// v2-79.1：按文件类型拆权限——cert 0644 可被其他进程读，key 必须 0600 只 root 可读
	// （之前所有 dst 都用 0o644，server.key.pem 私钥被同主机任何用户可读）
	type pair struct {
		src, dst string
		isKey    bool
	}
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
		data, err := os.ReadFile(p.src)
		if err != nil {
			return fmt.Errorf("read src %s: %w", p.src, err)
		}
		// AtomicWriteFile 在 dst 父目录缺失时自动 MkdirAll,
		// 但 /etc/swanctl/ 应该已经存在,所以这是双保险
		if err := swanctl.AtomicWriteFile(p.dst, data, mode); err != nil {
			return fmt.Errorf("atomic write %s: %w", p.dst, err)
		}
	}
	return nil
}
