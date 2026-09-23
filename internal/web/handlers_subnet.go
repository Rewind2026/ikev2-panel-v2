// 客户端虚拟 IP 段子网配置卡 API (v2.86-PR13.2 新增)
//
// 路由:
//   - GET  /api/subnet/status   返回当前 subnet 配置(IPv4 + IPv6 + 来源 + 启动时的初始值)
//   - POST /api/subnet/save     保存 subnet(form: ipv4_subnet, ipv6_subnet) + 立即 swanctl reload
//   - POST /api/subnet/clear    清除 subnet 配置(回退到 env / auto;**只清 panelstate, 不改 swanctl.conf 当前值**)
//
// 设计要点:
//   - 跟 cert.conf 一样持久化,但**不需要重启容器** —— Go 进程写完 panelstate
//     后立即调 Manager.UpdatePoolsAndReload,运行期生效
//   - 副作用:swanctl --load-all 会中断活跃 SA(~500ms),客户端需要重新拨号
//   - 不需要 audit + mask + 二次确认(IP 段不是凭证)
//   - clear() 不动 swanctl.conf —— 因为"清除 panelstate"≠"清空 swanctl.conf 段",
//     要清空 swanctl.conf 必须重新给一对合法值。clear 等价于"重启容器时不再用 panelstate 覆盖"
//
// 设计见 docs/design.md §19.6(v2-83 panelstate 基础)+ v2.86-PR13.2 扩展。
package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
)

// SubnetStatusResp GET /api/subnet/status 返回。
type SubnetStatusResp struct {
	// Configured panelstate 文件是否存在
	Configured bool `json:"configured"`
	// IPv4Subnet panelstate 里的 IPv4 pool CIDR(为空 = 未配置)
	IPv4Subnet string `json:"ipv4_subnet,omitempty"`
	// IPv6Subnet panelstate 里的 IPv6 ULA pool 前缀(为空 = 未配置)
	IPv6Subnet string `json:"ipv6_subnet,omitempty"`
	// UpdatedAt unix seconds,面板最后修改时间
	UpdatedAt int64 `json:"updated_at,omitempty"`
	// CurrentIPv4 / CurrentIPv6 当前 swanctl.conf 正在生效的 pool 段。
	// 用户改完后,handler 直接调 ReloadAll,这里会反映"实际生效值"。
	// 跟 panelstate 字段不同:panelstate 是"用户配置",current 是"运行态"。
	// 例如:用户刚清掉 panelstate → panelstate 字段为空,但 current 还是上一次的 10.13.0.0/24。
	CurrentIPv4 string `json:"current_ipv4,omitempty"`
	CurrentIPv6 string `json:"current_ipv6,omitempty"`
	// Source 来源描述 panelstate / runtime-default / dev-unknown
	//   - panelstate: 面板 UI / API 填的,运行期热生效
	//   - runtime-default: 当前 swanctl.conf 正在生效的值(可能是 entrypoint 启动时探测 / env 注入)
	//   - dev-unknown: dev 模式 / 无法读 swanctl.conf
	Source string `json:"source"`
}

// handleSubnetStatus GET /api/subnet/status
func (s *Server) handleSubnetStatus(w http.ResponseWriter, r *http.Request) {
	resp := SubnetStatusResp{}

	// 1) 尝试读当前 swanctl.conf 的 addrs 段(给 UI 显示"运行态")
	//    dev 模式下文件不存在时静默忽略,Source = dev-unknown
	if s.Swanctl != nil {
		v4, v6 := s.Swanctl.ReadCurrentPoolsFromFile()
		if v4 != "" || v6 != "" {
			resp.CurrentIPv4 = v4
			resp.CurrentIPv6 = v6
			if resp.Source == "" {
				resp.Source = "runtime-default"
			}
		}
	} else {
		resp.Source = "dev-unknown"
	}

	// 2) panelstate 优先
	if s.SubnetConfigStore != nil && s.SubnetConfigStore.SubnetConfigExists() {
		cfg, err := s.SubnetConfigStore.ReadSubnetConfig()
		if err == nil && cfg != nil {
			resp.Configured = true
			resp.IPv4Subnet = cfg.IPv4Subnet
			resp.IPv6Subnet = cfg.IPv6Subnet
			resp.UpdatedAt = cfg.UpdatedAt
			resp.Source = "panelstate"
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleSubnetSave POST /api/subnet/save
//
// Form 参数:
//   - ipv4_subnet   必填,合法 IPv4 CIDR (/24)
//   - ipv6_subnet   必填,合法 IPv6 ULA CIDR (/64, fd00::/8)
//
// 副作用:
//   - 写 /data/panel-state/subnet.conf (atomic rename, 0600)
//   - 立即调 Manager.UpdatePoolsAndReload 改 swanctl.conf pool 段 + swanctl --load-all
//   - 副作用:活跃 SA 会被卸载,客户端需要重新拨号才能拿到新 IP
//
// 错误返回:
//   - Validate 失败 → 400 + 中文错误
//   - swanctl 写入/Reload 失败 → 500 + 错误(此时 panelstate 已写,swanctl 未生效,需要重试)
func (s *Server) handleSubnetSave(w http.ResponseWriter, r *http.Request) {
	if s.SubnetConfigStore == nil {
		http.Error(w, "subnet config store not initialized", http.StatusInternalServerError)
		return
	}
	if s.Swanctl == nil {
		http.Error(w, "swanctl manager not initialized", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	cfg := panelstate.SubnetConfig{
		IPv4Subnet: r.PostFormValue("ipv4_subnet"),
		IPv6Subnet: r.PostFormValue("ipv6_subnet"),
	}

	// 1. 校验 + 写 panelstate
	if err := s.SubnetConfigStore.WriteSubnetConfig(cfg); err != nil {
		s.Logger.Warn("subnet config save failed", "err", err)
		http.Error(w, "保存失败: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 2. 立即改 swanctl.conf + reload
	if err := s.Swanctl.UpdatePoolsAndReload(r.Context(), cfg.IPv4Subnet, cfg.IPv6Subnet); err != nil {
		s.Logger.Error("swanctl pool update failed (panelstate saved but charon not reloaded)",
			"err", err,
			"ipv4", cfg.IPv4Subnet,
			"ipv6", cfg.IPv6Subnet,
		)
		http.Error(w, "panelstate 已保存,但 swanctl reload 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	masked := fmt.Sprintf("ipv4=%s ipv6=%s", cfg.IPv4Subnet, cfg.IPv6Subnet)
	s.Logger.Info("subnet config saved + reloaded via panel", "masked", masked)
	s.writeAudit(r, "subnet.save", masked)

	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		// 不需要 restart_required:运行期已生效
		_, _ = w.Write([]byte(`{"ok":true,"restart_required":false,"msg":"客户端 IP 段已更新,客户端需重新拨号获取新 IP"}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleSubnetClear POST /api/subnet/clear
//
// 副作用:
//   - 删除 /data/panel-state/subnet.conf
//   - 重启后 Go 进程从 env 读 IKEV2_VPN_SUBNET / IKEV2_VPN_SUBNET_V6(老用户兼容)
//   - **不**改 swanctl.conf 当前值(已生效的值保留)
//   - 不需要二次确认(用户主动点"清除"按钮,意图明确)
//
// 注意:clear 不会把 swanctl.conf 改回 entrypoint 启动时的初始值。
// entrypoint 启动时已经写入并 reload 过一次;那时的 pool 段是当时决定的。
// 想要"回到 entrypoint 启动值"需要重启容器(或者再次调 save 用之前那段)。
func (s *Server) handleSubnetClear(w http.ResponseWriter, r *http.Request) {
	if s.SubnetConfigStore == nil {
		http.Error(w, "subnet config store not initialized", http.StatusInternalServerError)
		return
	}
	if err := s.SubnetConfigStore.ClearSubnetConfig(); err != nil {
		s.Logger.Error("subnet config clear failed", "err", err)
		http.Error(w, "清除失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Logger.Info("subnet config cleared via panel (swanctl.conf current pool unchanged)")
	s.writeAudit(r, "subnet.clear", "")

	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"restart_required":false,"msg":"已清除,重启容器后回到 env / auto 默认段(当前 swanctl.conf 值不变)"}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}
