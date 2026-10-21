// / 首页（M6 完整版）：欢迎 + 用户数 + 在线 SA 列表 + LE 续签告警 + IPv6 变化提示。
// 见 docs/architecture.md §3.1
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/cert"
	"github.com/yourname/ikev2-panel-v2/internal/limit"
	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
	"github.com/yourname/ikev2-panel-v2/internal/store"
	"github.com/yourname/ikev2-panel-v2/internal/swanctl"
)

type homeData struct {
	PageMeta   // P1-B
	TotalUsers int

	// v2.86-pr23g:P1-A flash 一次性提示(DDNS 保存 / 同步 / 查询 / 创建 反馈)
	Flash string
	// v2.86-pr23h:flash 类别("" / "info" / "success" / "error")。
	// 模板按 Kind 切 banner-info / banner-success / banner-danger。
	FlashKind string

	// v2.86-pr23l:Aliyun 凭证卡专用 flash slot,跟 DDNS 默认 slot 分开,
	// 模板端 aliyun-card 表单按钮组下方就近展示 .aliyun-action-flash。
	// 这样 aliyun save/clear 的反馈不会跑去 ddns 卡片区域,避免视觉跳跃。
	AliyunFlash     string
	AliyunFlashKind string

	// P1-C M6 监控数据
	ActiveSAs   []swanctl.SA // 当前活跃 SA(ESTABLISHED 状态)
	TotalActive int          // ESTABLISHED SA 总数
	LEWarning   string       // LE 续签告警文本（空=无告警）
	ServerAddr  string       // 当前 mobileconfig RemoteAddress

	// v2.85-PR8(Q1-04):swanctl.ListSAs 超过 3s 时设 true,模板渲染 fallback
	SAsUnavailable bool

	// v2.93:charon 里**一个 connection 都没加载**。
	//
	// 为什么单独一个字段:SA 数为 0 有两种完全不同的含义 ——
	//   a) "当前没人连"        → 正常,off 态
	//   b) "conn 压根没注册进 charon" → 客户端连都连不上,是故障,danger 态
	// 光看 SA 数分不出来。63 实机就踩过 b):
	//   自签模式 entrypoint §7 的 load-all 发生在证书装好之前 →
	//   "loaded 0 of 1 connections, 1 failed to load" → conn 一直是空的。
	// 查询失败(没 swanctl 二进制 / 超时)时保持 false,不误报故障。
	ConnsUnloaded bool

	// v2-82 DDNS 状态,v2-84 加 family / per-family IP,v2.86-pr23a 改独立 A/AAAA bool
	DDNSConfigured      bool   // 同步器是否配置
	DDNSEnabled         bool   // 当前是否启用(总开关)
	DDNSFamily          string // v2-84:当前 family 配置(v4 / v6 / dual)— v2.86-pr23a:deprecated,从 EnableA/AAAA 反推
	DDNSEnableA         bool   // v2.86-pr23a:是否同步 A 记录(独立 bool,UI checkbox)
	DDNSEnableAAAA      bool   // v2.86-pr23a:是否同步 AAAA 记录(独立 bool,UI checkbox)
	DDNSFailed          bool   // 最近一次是否失败
	DDNSLastError       string // 失败原因(成功时空;dual 模式下是 v4+v6 拼接)
	DDNSLastTime        string // 最近同步时间(ISO8601,空=从未同步)
	DDNSCurrentIP       string // 同步到的目标 IP(最近成功的 NewIP;v2-84 dual 下填 V6IP)
	DDNSV4IP            string // v2-84:dual 模式下独立显示 v4 IP
	DDNSV4Error         string // v2-84:dual 模式下独立显示 v4 error
	DDNSV4Attempted     bool   // v2.86-pr23g:v4 这轮 tick 是否真的探测过(区分未启用 / 等待首次)
	DDNSV6IP            string // v2-84:dual 模式下独立显示 v6 IP
	DDNSV6Error         string // v2-84:dual 模式下独立显示 v6 error
	DDNSV6Attempted     bool   // v2.86-pr23g:v6 这轮 tick 是否真的探测过
	DDNSAttempted       bool   // v2.86-pr23g:整轮 tick 是否跑过探测(配 Runing vs Idle 显示)
	DDNSRemoteA         string // v2.86-pr23a:阿里云当前 A 记录(fetch-remote 后填)
	DDNSRemoteAAAA      string // v2.86-pr23a:阿里云当前 AAAA 记录(fetch-remote 后填)
	DDNSRemoteDomain    string // v2.86-pr23a:fetch-remote 时查询的域名
	DDNSRemoteFetchedAt string // v2.86-pr23a:fetch-remote 时间("已查询"标签用)
	DDNSRemoteError     string // v2.86-pr23a:fetch-remote 错误信息

	// v2-83 阿里云凭证状态(给 home 模板渲染用)
	AliyunConfigured  bool   // 凭证文件是否存在
	AliyunKeyIDMasked string // 掩码后的 KeyID
	AliyunKeySource   string // 来源: panelstate / env-new / env-legacy-ddns / env-legacy-acme.sh

	// v2.86-PR12.5:证书配置状态(给 home 模板渲染用)
	CertConfigured bool   // cert.conf 文件是否存在
	CertMode       string // self-signed / letsencrypt
	CertDomain     string // LE 模式域名
	CertServerCN   string // mobileconfig RemoteIdentifier
	CertACMEEmail  string // LE 注册邮箱
	CertSource     string // 来源: panelstate / env-default / ""

	// v2.93:证书「实际状态」—— 给 lane 卡的「服务端证书」「LE 续签」用。
	//
	// 为什么不能复用上面两个字段:
	//   - CertConfigured 的语义是「cert.conf 文件是否存在」= 用户在面板里
	//     **显式配过**证书。自签模式(默认)下 cert.conf 根本不存在,
	//     但自签证书一直都在用(/data/server/server.cert.pem + /data/panel-tls/)。
	//     拿它当「有没有证书在用」→ 卡片误报「未配置」,而实际 HTTPS 正常握手。
	//   - CertMode 的语义是「cert.conf 里写的模式」,改完要重启容器才生效。
	//     未重启的那段时间它跟**实际跑的**模式可能不一样。
	//
	// lane 卡是「服务就绪链路」,必须反映**当前实际**的状态,否则用户看到的是假象。
	CertInPlace  bool   // 服务端证书文件真的在位(LE: le/fullchain.pem;自签: server/server.cert.pem)
	CertLiveMode string // 运行中实际生效的证书模式(self-signed / letsencrypt)

	// v2.93:面板里的证书配置与运行中实际生效的配置是否不一致
	// (= 改完 cert.conf 还没 docker compose restart)。
	// 只有 panelstate 来源的改动才需要重启;env 改动不在面板管辖范围内。
	CertRestartRequired bool

	// v2.86-PR13.2:客户端虚拟 IP 段配置状态(给 home 模板渲染用)
	SubnetConfigured bool   // subnet.conf 文件是否存在
	SubnetIPv4       string // panelstate 里的 IPv4 pool CIDR
	SubnetIPv6       string // panelstate 里的 IPv6 ULA pool
	SubnetSource     string // panelstate / runtime-default / dev-unknown
	SubnetUpdatedAt  int64  // unix seconds,面板最后修改时间

	// v2.86-pr22:证书有效期(LE 模式才有值)
	CertDaysLeft        int    // 距过期天数(自签模式 = -1 表示不适用)
	CertExpiresAt       string // "YYYY-MM-DD" 格式;自签模式 = ""
	CertLastRenewFailed bool   // true = 续签失败标志存在

	// v2.86-pr22:DDNS 同步参数(给面板显示域名 / RR / cron 间隔)。
	// v2.86-pr23l:DDNSDetectTarget 字段废弃,IPv4 探测改用公网 IP API。
	DDNSDomain        string // 完整域名(vpn.example.com)
	DDNSBaseDomain    string // 裸域名(example.com)
	DDNSRR            string // 主机记录(vpn / @)
	DDNSPeriod        string // 人类可读周期(60s / 5m)
	DDNSPeriodSeconds int    // v2.86-pr23a:周期秒数(给 input number 默认值用)
	DDNSIface         string // 监听接口(空 = any)

	// v2.86-pr23d:细粒度"是否真的会跑"派生字段,给顶部 metric + 右侧 HUD 用。
	// 比 DDNSEnabled 更严:还要看 BaseDomain / AccessKey / 至少一个 checkbox。
	DDNSHasDomain bool // BaseDomain != ""(env ALIYUN_DOMAIN 或 panelstate)
	DDNSHasCreds  bool // 阿里云 AccessKey 已配(panelstate / env 至少一处)
	DDNSReady     bool // "真的能跑":Enabled && HasDomain && HasCreds && (EnableA || EnableAAAA)
	// 当前 swanctl.conf 正在生效的值(独立于 panelstate,看运行态)
	SubnetCurrentIPv4 string
	SubnetCurrentIPv6 string

	// v2.93:IP 池「运行态漂移」—— 面板配的段与 swanctl 实际在用的段不一致。
	//
	// 成因:面板保存 subnet.conf 后要执行 swanctl --load-all,charon 正忙 /
	// swanctl 异常时 reload 失败,但 panelstate 已经写进去了 → 两边对不上
	// (handlers_subnet.go: reload 失败返回 500,panelstate 不回滚)。
	//
	// 漂移时卡片必须报「未生效」(warn)而不是绿 —— 绿的意思是"配置已落地"。
	SubnetPoolDrift bool

	// v2.86 配置统一批次 D:面板设置状态(给 home 模板渲染「面板设置」卡)
	Settings settingsStatusData

	// v2.86 配置统一批次 D:面板设置卡的专属 flash slot(跟 aliyun / ddns 分开,
	// 这样保存反馈就近显示在设置卡内部,不跟其它卡的 banner 抢位置)。
	SettingsFlash     string
	SettingsFlashKind string

	// v2.85-PR2:默认密码横幅(/data/panel-state/INITIAL_ADMIN_PASSWORD.txt 存在 → 提示改密码)
	IsDefaultPassword bool

	// v2.86-PR12.21:Visual-first topology dashboard
	TopologyClients []TopologyClient   // 渲染服务器↔用户拓扑图
	RecentEvents    []store.AuditEvent // 首页底部 timeline (最近 8 条)
	NowUnix         int64              // 用于用户详情页过期判断
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	admin, ok := AdminFrom(r.Context())
	if !ok {
		http.Error(w, "内部错误，请查看服务日志", http.StatusInternalServerError)
		return
	}
	sess, _ := SessionFrom(r.Context())

	total, err := s.Store.CountUsers(r.Context())
	if err != nil {
		s.Logger.Error("count users", "err", err)
		http.Error(w, "内部错误，请查看服务日志", http.StatusInternalServerError)
		return
	}

	// P1-C: 获取在线 SA 列表(用 swanctl.ListSAs,不抛错;nil SA 表示没有活跃连接或 VICI 不可用)
	// v2.85-PR8(Q1-04):totalActive == -1 表示 swanctl 阻塞超过 3s,模板渲染 fallback
	activeSAs, totalActive := loadActiveSAs(r.Context(), s)
	sasUnavailable := totalActive == -1
	if sasUnavailable {
		totalActive = 0
	}

	// v2.86-pr23g:P1-A flash 消费(给顶部 banner — DDNS 操作反馈显示)
	var flashMsg, flashKind string
	if sess != nil && s.FlashStore != nil {
		if f, err := s.FlashStore.Consume(sess.ID); err == nil {
			flashMsg = f.Message
			flashKind = f.Kind
		}
	}

	// v2.86-pr23l:Aliyun 专属 flash slot — 跟默认 slot 并行消费,互不影响。
	// 默认 slot → .Flash(顶部 / DDNS),Aliyun slot → .AliyunFlash(aliyun-card 内联)。
	var aliyunFlashMsg, aliyunFlashKind string
	if sess != nil && s.FlashStore != nil {
		if f, err := s.FlashStore.ConsumeFor(sess.ID, "aliyun"); err == nil {
			aliyunFlashMsg = f.Message
			aliyunFlashKind = f.Kind
			s.Logger.Info("aliyun flash consumed", "kind", aliyunFlashKind, "msg", aliyunFlashMsg)
		}
	}

	// v2.86 批次 D:面板设置卡 flash slot(第三个 slot,互不覆盖)。
	var settingsFlashMsg, settingsFlashKind string
	if sess != nil && s.FlashStore != nil {
		if f, err := s.FlashStore.ConsumeFor(sess.ID, "settings"); err == nil {
			settingsFlashMsg = f.Message
			settingsFlashKind = f.Kind
		}
	}

	// P1-C: LE 续签告警
	leWarning := loadLEWarning(s)

	// v2-82:DDNS 状态(v2-84 加 family / per-family)
	ddnsStatus := loadDDNSStatus(s)

	// v2-83:阿里云凭证状态
	aliyunConfigured, aliyunKeyIDMasked := loadAliyunStatus(s)

	// v2.86-PR12.5:证书配置状态
	certCfg := loadCertConfigStatus(s)

	// v2.93:运行中**实际生效**的证书状态(lane 卡必须反映这个,不是配置值)
	certLive := loadCertLiveStatus(s)

	// v2.86-PR13.2:客户端虚拟 IP 段配置状态
	subnetCfg := loadSubnetConfigStatus(s)

	// v2.86 配置统一批次 D:面板设置状态(「面板设置」卡)
	settingsCfg := loadSettingsStatus(s)

	// v2.85-PR2:检测是否仍用启动时生成的默认密码
	isDefaultPassword := loadIsDefaultPassword(s)

	data := homeData{
		PageMeta:            PageMeta{Page: "home", Title: "首页", PageKey: "home", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
		Flash:               flashMsg,
		FlashKind:           flashKind,
		AliyunFlash:         aliyunFlashMsg,
		AliyunFlashKind:     aliyunFlashKind,
		TotalUsers:          total,
		ActiveSAs:           activeSAs,
		TotalActive:         totalActive,
		SAsUnavailable:      sasUnavailable,
		// v2.93:SA 为 0 时再查一次 conn(有 SA 就没必要 —— 有 SA 必然 conn 已加载)
		ConnsUnloaded: !sasUnavailable && loadConnHealth(r.Context(), s, totalActive),
		LEWarning:           leWarning,
		ServerAddr:          s.ServerAddr,
		DDNSConfigured:      ddnsStatus.configured,
		DDNSEnabled:         ddnsStatus.enabled,
		DDNSFamily:          ddnsStatus.family,
		DDNSEnableA:         ddnsStatus.enableA,
		DDNSEnableAAAA:      ddnsStatus.enableAAAA,
		DDNSFailed:          ddnsStatus.failed,
		DDNSLastError:       ddnsStatus.lastErr,
		DDNSLastTime:        ddnsStatus.lastTime,
		DDNSCurrentIP:       ddnsStatus.currentIP,
		DDNSV4IP:            ddnsStatus.v4IP,
		DDNSV4Error:         ddnsStatus.v4Err,
		DDNSV4Attempted:     ddnsStatus.v4Attempted,
		DDNSV6IP:            ddnsStatus.v6IP,
		DDNSV6Error:         ddnsStatus.v6Err,
		DDNSV6Attempted:     ddnsStatus.v6Attempted,
		DDNSAttempted:       ddnsStatus.attempted,
		DDNSRemoteA:         ddnsStatus.remoteA,
		DDNSRemoteAAAA:      ddnsStatus.remoteAAAA,
		DDNSRemoteDomain:    ddnsStatus.remoteDomain,
		DDNSRemoteError:     ddnsStatus.remoteError,
		DDNSRemoteFetchedAt: ddnsStatus.remoteFetchedAt,
		AliyunConfigured:    aliyunConfigured,
		AliyunKeyIDMasked:   aliyunKeyIDMasked,
		AliyunKeySource:     s.AliyunAccessKeySource,
		CertConfigured:      certCfg.configured,
		CertMode:            certCfg.mode,
		CertDomain:          certCfg.domain,
		CertServerCN:        certCfg.serverCN,
		CertACMEEmail:       certCfg.acmeEmail,
		CertSource:          certCfg.source,
		// v2.93:运行中实际生效的证书状态
		CertInPlace:         certLive.inPlace,
		CertLiveMode:        certLive.mode,
		CertRestartRequired: certRestartRequired(s, certCfg, certLive.mode),
		// v2.86-PR13.2:subnet 状态
		SubnetConfigured:  subnetCfg.configured,
		SubnetIPv4:        subnetCfg.ipv4,
		SubnetIPv6:        subnetCfg.ipv6,
		SubnetSource:      subnetCfg.source,
		SubnetUpdatedAt:   subnetCfg.updatedAt,
		SubnetCurrentIPv4: subnetCfg.currentIPv4,
		SubnetCurrentIPv6: subnetCfg.currentIPv6,
		// v2.93:面板配的段 vs swanctl 实际在用的段 —— 不一致就是没生效
		SubnetPoolDrift: subnetPoolDrift(subnetCfg),
		// v2.86 配置统一批次 D:面板设置
		Settings:          settingsCfg,
		SettingsFlash:     settingsFlashMsg,
		SettingsFlashKind: settingsFlashKind,
		IsDefaultPassword: isDefaultPassword,
		// v2.86-PR12.21:Visual-first topology
		TopologyClients: buildHomeTopology(r.Context(), s, activeSAs),
		RecentEvents:    loadRecentAudit(r.Context(), s, 8),
		NowUnix:         time.Now().Unix(),
	}

	// v2.86-pr22:证书有效期(LE 模式从 acme 包拿)
	//
	// v2.93:判据从 s.CertMode 改成 data.CertLiveMode —— 两者目前相等
	// (CertLiveMode 就是从 s.CertMode 推出来的),但用「运行中实际生效的
	// 模式」做判据语义更准:卡片上的天数必须对应**正在用的那张证书**。
	if data.CertLiveMode == "letsencrypt" {
		certPath := filepath.Join(s.DataDir, "le", "fullchain.pem")
		st := cert.CheckLERenewStatus(certPath)
		data.CertDaysLeft = st.DaysLeft
		data.CertLastRenewFailed = st.LastRenewFailed
		if !st.CertExpires.IsZero() {
			data.CertExpiresAt = st.CertExpires.Format("2006-01-02")
		}
	} else {
		// 自签模式:证书永不"过期",DaysLeft = -1 标记不适用
		data.CertDaysLeft = -1
	}

	// v2.86-pr22:DDNS 同步参数(给面板显示探测目标 / cron 间隔 / 域名)
	if s.DDNSSync != nil {
		data.DDNSDomain = s.DDNSSync.Domain()
		data.DDNSBaseDomain = s.DDNSSync.BaseDomain()
		data.DDNSRR = s.DDNSSync.RR()
		// v2.86-pr23l:DDNSDetectTarget 字段废弃,不再读取
		data.DDNSPeriod = s.DDNSSync.Period().String()
		data.DDNSPeriodSeconds = s.DDNSSync.PeriodSeconds()
		data.DDNSIface = s.DDNSSync.Iface()
		// v2.86-pr23d:细粒度"是否会真正 tick"派生字段。
		// BaseDomain 来自 env ALIYUN_DOMAIN(必要时由 panelstate 覆盖);空字符串就是没配。
		data.DDNSHasDomain = data.DDNSBaseDomain != ""
		// AccessKey 已配就等价于 aliyunConfigured(panelstate 文件 / env 任一处)。
		data.DDNSHasCreds = aliyunConfigured
		// "真的会跑"= 总开关 + 至少一个 checkbox + 主域名 + AccessKey。
		data.DDNSReady = data.DDNSEnabled &&
			(data.DDNSEnableA || data.DDNSEnableAAAA) &&
			data.DDNSHasDomain && data.DDNSHasCreds
	}

	s.RenderPage(w, r, http.StatusOK, "home", data)
}

// loadConnHealth 检查 charon 里有没有已加载的 connection。
//
// 返回 true = 查到了,而且**一个 conn 都没加载** → 客户端根本连不上,
//         是故障(面板上要报 danger,而不是"0 在线"这种看着像正常的文案)。
// 返回 false = conn 在 / 查不了(没 swanctl 二进制、超时、SA 非 0)。
//
// 只在"没有活跃 SA"时才查:有 SA 说明 conn 必然已加载,再查是白花钱
// (这个检查要 spawn 一个 swanctl 进程)。
func loadConnHealth(ctx context.Context, s *Server, totalActive int) bool {
	if s.Swanctl == nil || totalActive > 0 {
		return false
	}
	conns, err := s.Swanctl.ListLoadedConns(ctx)
	if err != nil {
		// 查不了 ≠ 没加载。查不了就保持"正常"语义,不误报故障。
		s.Logger.Debug("home: list loaded conns failed", "err", err)
		return false
	}
	return len(conns) == 0
}

// loadActiveSAs 从 swanctl 拿活跃 SA 列表。
//
// VICI 不可用（dev 模式 / container 没启动 charon）时返回 nil,不阻塞首页。
//
// v2.85-PR8(Q1-04):加 3s context timeout,避免 swanctl 阻塞 10s+ 让首页 hang。
// 返回值: (saList, totalActive)
//   - totalActive = -1 表示"暂不可用"(timeout),模板渲染 fallback 文案
//   - totalActive = 0 表示"无活跃连接" 或 swanctl 不可用
//   - totalActive > 0 表示活跃 SA 数量
func loadActiveSAs(ctx context.Context, s *Server) ([]swanctl.SA, int) {
	if s.Swanctl == nil {
		return nil, 0
	}
	// v2.85-PR8(Q1-04):3s timeout,超过就放弃并标记 "暂不可用"
	saCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	sas, err := s.Swanctl.ListSAs(saCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			s.Logger.Warn("home: list SAs timeout (3s)", "err", err)
			return nil, -1
		}
		s.Logger.Debug("home: list SAs failed", "err", err)
		return nil, 0
	}
	// 仅返回 ESTABLISHED,过滤掉 CONNECTING 等中间态
	out := make([]swanctl.SA, 0, len(sas))
	for _, sa := range sas {
		if sa.IkeState == "ESTABLISHED" {
			out = append(out, sa)
		}
	}
	return out, len(out)
}

// certStatusData v2.86-PR12.5:homeData 用的证书配置状态集合。
type certStatusData struct {
	configured bool   // cert.conf 文件是否存在
	mode       string // self-signed / letsencrypt
	domain     string // LE 模式域名
	serverCN   string // mobileconfig RemoteIdentifier
	acmeEmail  string // LE 注册邮箱
	source     string // panelstate / env-default / ""
}

// loadCertConfigStatus 加载证书配置状态(给 home 模板用)。
//
// 行为:
//   - panelstate 优先:CertConfigStore.ReadCertConfig()
//   - panelstate 没设 → fallback 到 Server 字段(CertMode + ServerCN 启动时已注入)
func loadCertConfigStatus(s *Server) certStatusData {
	if s.CertConfigStore != nil && s.CertConfigStore.CertConfigExists() {
		if c, err := s.CertConfigStore.ReadCertConfig(); err == nil && c != nil {
			return certStatusData{
				configured: true,
				mode:       c.CertMode,
				domain:     c.Domain,
				serverCN:   c.ServerCN,
				acmeEmail:  c.ACMEEmail,
				source:     "panelstate",
			}
		}
	}
	// env fallback
	return certStatusData{
		configured: false,
		mode:       s.CertMode,
		serverCN:   s.ServerCN,
		source:     s.CertConfigSource,
	}
}

// certLiveStatus v2.93:运行中**实际生效**的证书状态。
//
// 与 certStatusData(配置状态)是两回事:
//   - certStatusData 说「配置文件里写着什么」
//   - certLiveStatus 说「这个进程现在用的是哪张证书、它在不在」
type certLiveStatus struct {
	mode    string // self-signed / letsencrypt —— 当前进程真正在用的模式
	inPlace bool   // 该模式的证书文件真的在位
}

// loadCertLiveStatus 探测运行中实际生效的证书模式。
//
// 为什么不能直接用 homeData.CertMode:
//   homeData.CertMode 来自 loadCertConfigStatus,语义是「**配置里写的**模式」
//   (cert.conf 优先,其次 env)。用户在面板里保存 cert.conf 之后必须重启容器
//   才生效 —— 这段时间配置说 LE、实际跑的还是自签,卡片显示"LE"就是假的。
//
// 判定方式:按 main.go 的启动分支反查证书文件在不在:
//   - LE 分支:  读 {DataDir}/le/fullchain.pem,读不到直接 os.Exit(1)
//   - 自签分支: 生成 {DataDir}/server/server.cert.pem
//   文件在位 = 该模式的证书确实被这个进程用着。
//
// s.CertMode 是**启动时**读进来的值(runtime.Merge 合并了当时的 env +
// cert.conf),正是 main.go 选哪个分支的依据,所以用它来选要查哪个文件。
func loadCertLiveStatus(s *Server) certLiveStatus {
	out := certLiveStatus{mode: s.CertMode}
	if out.mode != "letsencrypt" {
		out.mode = "self-signed"
	}
	var p string
	if out.mode == "letsencrypt" {
		p = filepath.Join(s.DataDir, "le", "fullchain.pem")
	} else {
		p = filepath.Join(s.DataDir, "server", "server.cert.pem")
	}
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		out.inPlace = true
	}
	return out
}

// certRestartRequired 判断「面板里的证书配置」是否还没在运行中生效
// (= 需要 docker compose restart)。
//
// 只有 panelstate 来源的改动才归面板管:env 改动要改 compose 文件再重启,
// 面板看不到也管不了,不该在这里报"待重启"。
//
// LE 模式还要看域名:main.go 强制 cfg.ServerCN = cfg.Domain(LE 模式
// 无条件),所以 s.ServerCN 就是启动时用的域名 —— 域名改了同样要重启。
func certRestartRequired(s *Server, cfg certStatusData, liveMode string) bool {
	if cfg.source != "panelstate" {
		return false
	}
	if cfg.mode != liveMode {
		return true
	}
	if liveMode == "letsencrypt" && cfg.domain != "" && cfg.domain != s.ServerCN {
		return true
	}
	return false
}

// subnetPoolDrift 判断「面板配的 IP 段」与「swanctl 运行态在用的段」是否不一致。
//
// 典型场景:面板保存 subnet.conf → 写文件成功 → swanctl --load-all 失败
// (charon 正忙)→ 接口返回 500,但 panelstate 不会回滚,于是两边对不上。
// 这种情况下 IP 池卡片必须报「未生效」,不能报绿 —— 绿代表"配置已落地"。
//
// 有一侧为空不算漂移(那是"根本没读到运行态",由模板单独处理成 warn)。
func subnetPoolDrift(cfg subnetStatusData) bool {
	if !cfg.configured {
		return false
	}
	if cfg.ipv4 != "" && cfg.currentIPv4 != "" && cfg.ipv4 != cfg.currentIPv4 {
		return true
	}
	if cfg.ipv6 != "" && cfg.currentIPv6 != "" && cfg.ipv6 != cfg.currentIPv6 {
		return true
	}
	return false
}

// subnetStatusData v2.86-PR13.2:homeData 用的 subnet 配置状态集合。
//
// 与 certStatusData 类似,但多了 currentIPv4/IPv6 字段(从 swanctl.conf
// 读当前实际生效的 pool 段,跟 panelstate 字段可以不一致 — 比如刚 clear
// 了 panelstate 但 swanctl.conf 仍是上一次的配置)。
type subnetStatusData struct {
	configured  bool   // subnet.conf 文件是否存在
	ipv4        string // panelstate IPv4 pool CIDR
	ipv6        string // panelstate IPv6 ULA pool
	updatedAt   int64  // panelstate UpdatedAt
	source      string // panelstate / runtime-default / dev-unknown
	currentIPv4 string // swanctl.conf 当前生效的 IPv4(独立于 panelstate)
	currentIPv6 string // swanctl.conf 当前生效的 IPv6
}

// loadSubnetConfigStatus 加载 subnet 配置状态(给 home 模板用)。
//
// 行为:
//   - panelstate 优先:SubnetConfigStore.ReadSubnetConfig()
//   - panelstate 没设 → 仍从 swanctl.conf 读 currentIPv4/IPv6(给 UI 显示"运行态")
//   - dev 模式(swanctl.conf 不存在)→ source="dev-unknown",currentIPv4/IPv6 都为空
func loadSubnetConfigStatus(s *Server) subnetStatusData {
	out := subnetStatusData{}

	// 1) 当前 swanctl.conf 正在生效的值(给 UI 看"实际跑的是哪个段")
	if s.Swanctl != nil {
		v4, v6 := s.Swanctl.ReadCurrentPoolsFromFile()
		out.currentIPv4 = v4
		out.currentIPv6 = v6
	} else {
		out.source = "dev-unknown"
	}

	// 2) panelstate 优先
	if s.SubnetConfigStore != nil && s.SubnetConfigStore.SubnetConfigExists() {
		if c, err := s.SubnetConfigStore.ReadSubnetConfig(); err == nil && c != nil {
			out.configured = true
			out.ipv4 = c.IPv4Subnet
			out.ipv6 = c.IPv6Subnet
			out.updatedAt = c.UpdatedAt
			out.source = "panelstate"
			return out
		}
	}

	// 3) 没设 panelstate → source = runtime-default(如果能读到 swanctl.conf)
	if out.source == "" && (out.currentIPv4 != "" || out.currentIPv6 != "") {
		out.source = "runtime-default"
	}
	return out
}

// loadLEWarning 检查 LE 模式证书状态,返回告警文本(无告警返回空字符串)。
//
// 设计：仅在 CertMode == "letsencrypt" 时检查,自签模式永远返回空。
// 告警分两个级别:
//   - 红色：续签失败（LAST_RENEW_FAILED 标志存在）
//   - 黄色：证书将在 14 天内过期
func loadLEWarning(s *Server) string {
	if s.CertMode != "letsencrypt" {
		return ""
	}
	// cert 路径由 cmd/ikev2-panel/main.go 安装到 /etc/swanctl/x509/$DOMAIN.pem
	// 但 health check 用 /data/le/fullchain.pem 更直接(acme.sh 写入位置)
	certPath := filepath.Join(s.DataDir, "le", "fullchain.pem")
	st := cert.CheckLERenewStatus(certPath)

	// v2-83 顺手修:CheckLERenewStatus 内部已经检查 /data/le/LAST_RENEW_FAILED
	// 文件存在 → LastRenewFailed=true → 显示红色告警。
	// 之前的 v2-82 实现里,这里漏了 file 检查,只有 DaysLeft<14 才告警,
	// 导致 renew-cert.sh 回退到旧证书后面板要等 ~30 天才报"将过期"。
	// (CheckLERenewStatus 内部 cert/acme.go L41-44 已经做了 os.Stat 这件事。)
	if st.LastRenewFailed {
		// v2.85-PR3 U07 修复:同时提示两个真实日志路径(acme-renew.log 是 acme.sh 输出,
		// ikev2-renew.log 是 entrypoint 调我们的 renew-cert.sh 时的输出)。
		// 之前只说 ikev2-renew.log → 用户 grep 找不到 → 误以为问题不存在。
		return "⚠️ Let's Encrypt 续签失败！最后一次续签失败，面板仍用旧证书运行。" +
			"请检查 /var/log/acme-renew.log（acme.sh 输出）和 /var/log/ikev2-renew.log（renew-cert.sh 输出）"
	}
	if cert.ShouldWarn(st.DaysLeft) {
		return "⚠️ Let's Encrypt 证书将在 " + itoa(st.DaysLeft) + " 天后过期,请关注。"
	}
	return ""
}

// ddnsStatusData homeData 用的 DDNS 状态集合(v2-84 + v2.86-pr23a 扩展)。
//
// 为什么用 struct 而不是多返回值:
//   - v2-82 6 个返回值已经难读,v2-84 加 5 个字段(v4IP/V4Err/V6IP/V6Err/family)后
//     11 个返回值根本没法读。改 struct 后 homeData 字段也清爽。
//   - v2.86-pr23a 加 EnableA/EnableAAAA / RemoteSnapshot 字段,继续走 struct。
type ddnsStatusData struct {
	configured bool
	enabled    bool
	family     string
	// v2.86-pr23a:EnableA/EnableAAAA 替代 family 枚举(独立 bool,UI checkbox)
	enableA     bool
	enableAAAA  bool
	failed      bool
	lastErr     string
	lastTime    string
	currentIP   string
	v4IP        string
	v4Err       string
	v4Attempted bool // v2.86-pr23g
	v6IP        string
	v6Err       string
	v6Attempted bool // v2.86-pr23g
	attempted   bool // v2.86-pr23g:整轮 tick 是否跑过
	// v2.86-pr23a:RemoteSnapshot(用户点了"查询阿里云记录值"按钮后的结果)
	remoteA         string
	remoteAAAA      string
	remoteDomain    string
	remoteError     string
	remoteFetchedAt string // ISO8601 in display timezone
}

// loadDDNSStatus 收集 DDNS 状态给 home 模板。
//
// 设计:
//   - Sync 为 nil → configured=false,模板跳过整个卡片
//   - 启用时填充最近一次同步信息,失败时填充 failed + lastError
//   - v2-84:dual 模式下 V4IP/V6IP/V4Err/V6Err 独立显示;单 family 模式下填 NewIP 兼容字段
//   - v2.86-pr23a:额外填 EnableA/EnableAAAA(给 UI checkbox 用)+ RemoteSnapshot(fetch-remote 结果)
func loadDDNSStatus(s *Server) ddnsStatusData {
	if s.DDNSSync == nil {
		return ddnsStatusData{}
	}
	enabled := s.DDNSSync.IsEnabled()
	last := s.DDNSSync.LastSyncSnapshot()
	family := s.DDNSSync.Family()

	out := ddnsStatusData{
		configured: true,
		enabled:    enabled,
		family:     family,
		// v2.86-pr23a:独立 bool(给 UI checkbox)
		enableA:    s.DDNSSync.EnableA(),
		enableAAAA: s.DDNSSync.EnableAAAA(),
	}

	// v2.86-pr23a:RemoteSnapshot(fetch-remote 结果)
	rs := s.DDNSSync.RemoteSnapshot()
	if !rs.FetchedAt.IsZero() {
		loc, err := time.LoadLocation(s.DisplayTimezone)
		if err != nil || loc == nil {
			loc = time.UTC
		}
		out.remoteFetchedAt = rs.FetchedAt.In(loc).Format("2006-01-02 15:04:05 MST")
		out.remoteA = rs.A
		out.remoteAAAA = rs.AAAA
		out.remoteDomain = rs.Domain
		out.remoteError = rs.Error
	}

	if !last.Time.IsZero() {
		// v2.85-PR3:用 s.DisplayTimezone 渲染(跟面板表格列格式一致)
		loc, err := time.LoadLocation(s.DisplayTimezone)
		if err != nil || loc == nil {
			loc = time.UTC
		}
		out.lastTime = last.Time.In(loc).Format("2006-01-02 15:04:05 MST")
		// currentIP:dual 模式优先显示 v6(向后兼容老模板),单 family 模式用 NewIP
		if family == "dual" {
			out.v4IP = last.V4IP
			out.v4Err = last.V4Error
			out.v4Attempted = last.V4Attempted
			out.v6IP = last.V6IP
			out.v6Err = last.V6Error
			out.v6Attempted = last.V6Attempted
			out.currentIP = last.V6IP
			// dual 模式下错误信息合并显示
			if last.V4Error != "" && last.V6Error != "" {
				out.lastErr = fmt.Sprintf("v4: %s; v6: %s", last.V4Error, last.V6Error)
				out.failed = true
			} else if last.V4Error != "" || last.V6Error != "" {
				out.lastErr = firstNonEmpty(last.V4Error, last.V6Error)
				out.failed = true
			}
		} else {
			out.currentIP = last.NewIP
			if !last.Success {
				out.failed = true
				out.lastErr = last.Error
			}
			// 单 family 也填 v4/v6 字段,方便模板按 family 渲染
			if family == "v4" {
				out.v4IP = last.NewIP
				out.v4Err = last.Error
				out.v4Attempted = last.Attempted
			} else { // "v6"
				out.v6IP = last.NewIP
				out.v6Err = last.Error
				out.v6Attempted = last.Attempted
			}
		}
		// v2.86-pr23g:整轮 Attempted 透传(只有 dual 走完 loop / 单 family 真正跑过 才 true)
		out.attempted = last.Attempted
	} else if exists := lastDDNSFailedExists(); exists {
		// 兜底分支:本次进程还没跑过一轮(last.Time 为 zero),
		// 但上次运行留下的失败标志文件还在 —— 于是知道"失败过",
		// 却拿不到失败原因和时间。
		//
		// v2.88 修订文案:原来这里写的是"详见 /var/log/charon.log",
		// 指错日志了 —— charon.log 是 strongSwan IKE 进程的日志,
		// DDNS 同步是面板自己的 goroutine 跑的,原因只可能在面板日志里。
		// 同时不再说"最近一次同步失败":本次进程压根没同步过,
		// 真实语义是"上次运行失败,且本次尚未重试"。
		out.failed = true
		out.lastErr = "上次运行同步失败，本次进程尚未开始重试；具体原因见面板日志（docker compose logs ikev2-panel）"
	}

	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// loadAliyunStatus 收集阿里云凭证状态(v2-83)。
//
// 返回值:
//   - configured: 凭证文件是否存在
//   - keyIDMasked: 掩码后的 KeyID(空 = 未配置)
//
// 错误(MaskKeyID 失败等)被静默吞掉 → 返回空字符串即可,模板有默认值。
func loadAliyunStatus(s *Server) (bool, string) {
	if s.PanelState == nil {
		return false, ""
	}
	if !s.PanelState.AliyunExists() {
		return false, ""
	}
	c, err := s.PanelState.ReadAliyun()
	if err != nil || c == nil {
		return true, ""
	}
	return true, panelstate.MaskKeyID(c.KeyID)
}

// v2.85-PR2:检测 admin 是否仍在用启动时生成的默认密码。
//
// 触发条件:panel-state/INITIAL_ADMIN_PASSWORD.txt 存在。
// 启动时 main.go 在创建 admin 时写入这个文件;admin 通过面板修改密码后
// 我们需要在密码变更路径里把这个文件删除(本 PR 不做,留 v3.0 admin 改密页)。
// 所以现阶段**只有首次启动后的横幅**,admin 改密后横幅不会自动消失 —
// 这是已知妥协,详见 audit-2026-09-usability.md §U02。
//
// dev 模式(cfg.DataDir 非 /data)→ 检测 <DataDir>/panel-state/INITIAL_ADMIN_PASSWORD.txt,
// 这样 dev 本地开发也能验证横幅效果。
func loadIsDefaultPassword(s *Server) bool {
	pwPath := initialPasswordPath(s)
	_, err := os.Stat(pwPath)
	return err == nil
}

// initialPasswordPath 返回 INITIAL_ADMIN_PASSWORD.txt 的绝对路径。
//
// dev 模式(cfg.DataDir 非 /data)→ <DataDir>/panel-state/INITIAL_ADMIN_PASSWORD.txt,
// 这样 dev 本地开发也能验证横幅效果。
//
// 生产 → panelstate.Dir(= /data/panel-state)。
func initialPasswordPath(s *Server) string {
	pwDir := panelstate.Dir
	if s.DataDir != "/data" {
		pwDir = filepath.Join(s.DataDir, "panel-state")
	}
	return filepath.Join(pwDir, "INITIAL_ADMIN_PASSWORD.txt")
}

// clearInitialPasswordFile 删除 INITIAL_ADMIN_PASSWORD.txt。
//
// 调用时机：admin 改密成功后（handleAccountChangePassword）。
// 作用：让首页「管理口令」卡片从「默认」翻成「已改」，
// 同时让顶部默认密码横幅消失 —— 两者都靠这个文件是否存在来判断。
//
// 失败只记 warn 不中断：密码已经落库，改密不可能因为删一个文件失败而回滚。
func (s *Server) clearInitialPasswordFile() {
	pwPath := initialPasswordPath(s)
	if err := os.Remove(pwPath); err != nil {
		if !os.IsNotExist(err) {
			s.Logger.Warn("account change password: remove INITIAL_ADMIN_PASSWORD.txt failed",
				"path", pwPath, "err", err)
		}
		return
	}
	s.Logger.Info("account change password: removed INITIAL_ADMIN_PASSWORD.txt",
		"path", pwPath)
}

// 仅用于 LE 告警场景,负数返回 "未知"(对应"证书过期/无效")。
// 0 返回 "0"(而不是 "未知")。
func itoa(n int) string {
	if n < 0 {
		return "未知"
	}
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ensure limit/swanctl/cert are referenced even if unused in some configs
var _ = limit.New

// buildHomeTopology v2.86-PR12.21: 把 DB 用户列表 + swanctl 活跃 SA 合并成 topology 客户端节点。
//
// 行为:
//   - 列出所有 enabled 用户 (limit 50 个, 超过会渲染 "+N 更多" 占位)
//   - 活跃 SA (activeSAs) 按 RemoteID 匹配 → online=true, 填 BytesIn/BytesOut
//   - 离线用户 (last_used=0 或 不在 activeSAs) → online=false
//
// 失败 (DB 不可用) → 返回 nil (模板渲染空 topology)。
func buildHomeTopology(ctx context.Context, s *Server, activeSAs []swanctl.SA) []TopologyClient {
	if s.Store == nil {
		return nil
	}
	users, err := s.Store.ListUsers(ctx)
	if err != nil {
		return nil
	}

	// 按 username → SA bytes 索引
	saBytes := make(map[string]struct {
		in, out int64
	}, len(activeSAs))
	for _, sa := range activeSAs {
		if sa.RemoteID == "" {
			continue
		}
		var totalIn, totalOut int64
		for _, ch := range sa.Children {
			totalIn += ch.BytesIn
			totalOut += ch.BytesOut
		}
		saBytes[sa.RemoteID] = struct {
			in, out int64
		}{totalIn, totalOut}
	}

	out := make([]TopologyClient, 0, len(users))
	for _, u := range users {
		if !u.Enabled {
			continue
		}
		c := TopologyClient{
			ID:   u.ID,
			Name: u.Username,
			Href: fmt.Sprintf("/users/%d", u.ID),
		}
		if sa, ok := saBytes[u.Username]; ok {
			c.Online = true
			c.BytesIn = sa.in
			c.BytesOut = sa.out
		}
		out = append(out, c)
		if len(out) >= 50 {
			break
		}
	}
	return out
}

// loadRecentAudit v2.86-PR12.21: 取最近 N 条 audit 用于首页 timeline。
//
// 失败 → 返回 nil (timeline 自动隐藏)。
func loadRecentAudit(ctx context.Context, s *Server, n int) []store.AuditEvent {
	if s.Store == nil || n <= 0 {
		return nil
	}
	events, err := s.Store.ListAudit(ctx, n)
	if err != nil {
		return nil
	}
	return events
}
