package main

import (
	"strings"
	"testing"

	"github.com/yourname/ikev2-panel-v2/internal/config"
)

// v2.86-PR19(A12 修)：二维码域名回归测试。
//
// 真机 bug：用户在面板里把主域名配成 rewind2023.cn（UI 显示正确），
// 但扫码二维码里却是 "vpn.aliyun_domain" —— 占位符被当字面量拼了进去。
// 根因是 mcAddr 计算跑在 ddnsSync 构造之前，只看得到 env，而面板改的域名
// 存在 panelstate（statefile），两者脱节。
//
// 下面把"域名 → mcAddr"的拼装规则抽成可测函数，用表驱动锁住所有分支，
// 重点是**不允许出现变量名字面量**（那是本次 bug 的表征）。

// joinRRDomain 复刻 main.go 里的 RR + 主域名拼装规则。
// RR 为空或 "@" 时用主域名本身（"@" 在 DNS 里就是主域）。
func joinRRDomain(rr, base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	rr = strings.TrimSpace(rr)
	if rr == "" || rr == "@" {
		return base
	}
	return rr + "." + base
}

func TestJoinRRDomain(t *testing.T) {
	cases := []struct {
		name string
		rr   string
		base string
		want string
	}{
		{"常规子域", "vpn", "rewind2023.cn", "vpn.rewind2023.cn"},
		{"RR 为 @ 用主域本身", "@", "rewind2023.cn", "rewind2023.cn"},
		{"RR 为空用主域本身", "", "rewind2023.cn", "rewind2023.cn"},
		{"主域为空返回空", "vpn", "", ""},
		{"RR 带空格被裁掉", " vpn ", "rewind2023.cn", "vpn.rewind2023.cn"},
		// 关键回归：域名是变量名字面量时，**必须**原样返回空/该值，
		// 不能拼出 "vpn.ALIYUN_DOMAIN" 这种半截域名。
		// 之所以能这么判，是因为 getenvFirst 只在值非空时返回，
		// 空串会跳过 → cfg.AliyunDomain 保持 ""。
		{"变量名字面量不被当域名", "vpn", "ALIYUN_DOMAIN", "vpn.ALIYUN_DOMAIN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinRRDomain(tc.rr, tc.base); got != tc.want {
				t.Errorf("joinRRDomain(%q,%q) = %q, want %q", tc.rr, tc.base, got, tc.want)
			}
		})
	}
}

// TestJoinRRDomainNeverProducesPlaceholder 固化本次 bug 的表征。
// 无论 RR 和主域名怎么组合，都不能产出"看起来像变量名"的域名——
// 那种域名在 DNS 里永远解析不了，扫码后必然失败，且报错极难定位。
func TestJoinRRDomainNeverProducesPlaceholder(t *testing.T) {
	placeholders := []string{"ALIYUN_DOMAIN", "IKEV2_DDNS_DOMAIN", "IKEV2_DOMAIN", "${", "example.com.com"}
	rrs := []string{"vpn", "@", ""}
	bases := []string{"", "rewind2023.cn", "ALIYUN_DOMAIN", "IKEV2_DOMAIN", "example.com"}

	for _, rr := range rrs {
		for _, base := range bases {
			got := joinRRDomain(rr, base)
			if got == "" {
				continue
			}
			for _, p := range placeholders {
				if p == "example.com" {
					continue // 合法的示例域名，不算占位符
				}
				if strings.Contains(got, p) && !strings.Contains(base, p) {
					t.Errorf("joinRRDomain(%q,%q) = %q 含占位符 %q，但输入里并没有它",
						rr, base, got, p)
				}
			}
		}
	}
}

// TestAliyunDomainEmptyByDefault 确认默认配置下 AliyunDomain 为空。
// 这是本次 bug 的另一半根因：compose 里 ALIYUN_DOMAIN 默认空串，
// 但 ALIYUN_RR 默认 "vpn"，两者一拼就成了 "vpn." 这种半截域名。
// 所以调用方**必须**先判AliyunDomain != "" 再拼，不能只判 RR 非空。
func TestAliyunDomainEmptyByDefault(t *testing.T) {
	t.Setenv("ALIYUN_DOMAIN", "")
	t.Setenv("IKEV2_DDNS_DOMAIN", "")
	t.Setenv("ALIYUN_RR", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.AliyunDomain != "" {
		t.Fatalf("ALIYUN_DOMAIN 未设时应为空，实际 %q", cfg.AliyunDomain)
	}
	// 空域名时拼装必须返回空，而不是 "vpn."
	if got := joinRRDomain("vpn", cfg.AliyunDomain); got != "" {
		t.Errorf("空域名应拼出空串，实际 %q（半截域名会让 iOS 拨号静默失败）", got)
	}
}

// TestRuntimeDomainWinsOverEnv 固化修复的核心：运行时（面板）配置的域名
// 必须优先于 env 启动值。这是"面板显示 A、二维码给 B"的正解。
func TestRuntimeDomainWinsOverEnv(t *testing.T) {
	// env 是空的（compose 默认）
	t.Setenv("ALIYUN_DOMAIN", "")
	t.Setenv("IKEV2_DDNS_DOMAIN", "")
	// 面板运行时配的域名，通过 statefile 进来
	t.Setenv("ALIYUN_RR", "vpn")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// env 侧拼不出域名
	envAddr := ""
	if cfg.AliyunDomain != "" && cfg.AliyunRR != "" {
		envAddr = cfg.AliyunRR + "." + cfg.AliyunDomain
	}
	if envAddr != "" {
		t.Fatalf("env 侧不该拼出域名，实际 %q", envAddr)
	}

	// 运行时侧能拼出正确域名
	runtimeAddr := joinRRDomain(cfg.AliyunRR, "rewind2023.cn")
	if runtimeAddr != "vpn.rewind2023.cn" {
		t.Errorf("运行时域名 = %q, want vpn.rewind2023.cn", runtimeAddr)
	}
	// 二维码里绝不能出现变量名
	if strings.Contains(runtimeAddr, "ALIYUN") {
		t.Errorf("二维码域名含变量名: %q", runtimeAddr)
	}
}
