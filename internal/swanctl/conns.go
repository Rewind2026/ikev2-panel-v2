// swanctl connection 状态查询(只读)。
//
// 背景(v2.93,192.168.50.63 实机):自签模式下 entrypoint §7 的
// `swanctl --load-all` 发生在证书装好之前 →
//
//	loading certs certificate '/etc/swanctl/x509/server.cert.pem' failed
//	loaded 0 of 1 connections, 1 failed to load
//
// 于是 connection 压根没注册进 charon。SA 数量同样是 0,面板光看 SA 数
// 分不出"没人连"和"conn 都没装上"—— 后者时客户端根本连不上。
//
// 本文件提供"charon 当前加载了哪些 connection"的查询,给 home 的 IKE_SA 卡
// 用:SA 为 0 时再查一次 conn,conn 也没加载 → 报 danger(故障)而不是
// off(只是没人连)。
//
// 为什么走 CLI 而不是 VICI:charon 的 VICI 没有 list-conns 之类的命令
// (VICI 侧只有 load-conn / load-creds / list-sas / terminate 等),
// `swanctl --list-conns` 是唯一的查询入口。它只列出**已加载**的 conn ——
// 没加载时输出为空,正好就是我们要的信号。
package swanctl

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ListConnsTimeout `swanctl --list-conns` 的超时。
//
// 只是读一次内存里的配置快照,正常 < 50ms。给 3s 足够,又不会让首页卡住。
const ListConnsTimeout = 3 * time.Second

// connHeaderRe 匹配 `swanctl --list-conns` 输出里的**顶层** connection 头。
//
// swanctl 输出是缩进树:
//
//	ikev2-rw: IKEv2, no reauthentication, rekeying every 86400s, dpd delay 30s
//	  local:  2408:832e:...
//	  remote: %any
//	  ikev2-rw: TUNNEL, rekeying every 3600s, dpd action is none     ← 子 SA,有缩进
//
// 顶层 conn 头在第 0 列,子段都有缩进 → 只取第 0 列的 "name:" 行。
var connHeaderRe = regexp.MustCompile(`^([A-Za-z0-9_.\-]+):\s`)

// ListLoadedConns 返回 charon 当前已加载的 connection 名列表。
//
// 返回空 slice(不是 error)表示"一个 conn 都没加载" —— 这是调用方要区分的
// 关键状态,不是错误。
//
// 错误只发生在"查不了"(swanctl 二进制不存在 / 执行超时 / 非零退出),
// 调用方应把这种情况当成"未知",不要当成"没加载"。
func (m *Manager) ListLoadedConns(ctx context.Context) ([]string, error) {
	if m.SkipVici {
		return nil, nil
	}
	ctx, cancel := withTimeout(ctx, ListConnsTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, m.swanctlBin(), "--list-conns")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("swanctl --list-conns timeout after %s", ListConnsTimeout)
		}
		return nil, fmt.Errorf("swanctl --list-conns failed: %w (out=%s)", err, strings.TrimSpace(string(out)))
	}

	names := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		mt := connHeaderRe.FindStringSubmatch(line)
		if mt == nil {
			continue
		}
		if seen[mt[1]] {
			continue
		}
		seen[mt[1]] = true
		names = append(names, mt[1])
	}
	return names, nil
}
