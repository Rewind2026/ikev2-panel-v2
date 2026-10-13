// v2.86-PR19(P2-9)：监听地址默认值的回归测试。
//
// 改动历史（两次）：
//  1. 曾把默认从 0.0.0.0:8443 收到 127.0.0.1:8443，并打 WARN —— **错的取舍，已回退**。
//     错在哪：本项目用 network_mode: host，容器与宿主共享网络命名空间，
//     没有 docker 端口映射这一层。绑 127.0.0.1 绑的是**宿主自己的 loopback**，
//     于是容器内通、宿主上通，但**其他机器一律连不上** —— 而靠管理 IP 直连
//     面板正是这个项目的常规访问方式。默认值不该让人"装完就失联"。
//  2. 回退后保留：非 loopback 时打一条**措辞准确**的提示（只讲公网暴露风险，
//     不再把"LAN 可访问"说成风险），且不 refuse。
//
// 本文件锁住：
//  1. 未设 env 时默认值是可用的 0.0.0.0:8443（不是 loopback）
//  2. 显式设成任何地址都照常生效，包括 loopback（用户有权自行收窄）
//  3. loopback 判定覆盖 IPv4 / IPv6 / localhost / 畸形输入
package config

import (
	"os"
	"testing"
)

// TestDefaultListenAddrIsReachable 默认必须"装完就能用"。
//
// 这条测试的存在意义是防止再次有人把默认值改成 loopback ——
// 那次改动通过了全部单测，却在真机上让管理 IP 直连彻底失效。
func TestDefaultListenAddrIsReachable(t *testing.T) {
	os.Unsetenv("IKEV2_LISTEN_ADDR")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ListenAddr != "0.0.0.0:8443" {
		t.Errorf("default ListenAddr = %q, want 0.0.0.0:8443", c.ListenAddr)
	}
	if listenIsLoopback(c.ListenAddr) {
		t.Errorf("default ListenAddr %q 是 loopback —— 管理 IP 直连会失效。"+
			"network_mode: host 下没有 port 映射层，绑 loopback 等于对所有外部主机关闭面板", c.ListenAddr)
	}
}

// TestExplicitListenAddrRespected 显式配置必须照常生效，两个方向都要。
//
// 这条是 P2-9 不被误伤的关键：**默认宽松 ≠ 禁止收紧**，反过来也一样。
// 部署者既可以主动放开到 0.0.0.0，也可以主动收窄到 loopback / 指定管理 IP。
func TestExplicitListenAddrRespected(t *testing.T) {
	cases := []string{
		// 放开方向
		"0.0.0.0:8443",
		"[::]:8443",
		"192.168.1.10:8443",
		// 收窄方向（回退默认值后同样必须可用）
		"127.0.0.1:8443",
		"127.0.0.1:9443",
		"localhost:8443",
		"[::1]:8443",
	}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("IKEV2_LISTEN_ADDR", addr)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.ListenAddr != addr {
				t.Errorf("ListenAddr = %q, want %q (explicit setting must be respected)", c.ListenAddr, addr)
			}
		})
	}
}

// TestListenIsLoopback listenIsLoopback 的判定矩阵。
func TestListenIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
		why  string
	}{
		{"127.0.0.1:8443", true, "IPv4 loopback"},
		{"127.0.0.53:8443", true, "IPv4 loopback 任意地址都算"},
		{"localhost:8443", true, "hostname localhost"},
		{"[::1]:8443", true, "IPv6 loopback"},
		{"[::]:8443", false, "IPv6 通配 = 暴露"},
		{"0.0.0.0:8443", false, "IPv4 通配 = 暴露"},
		{"192.168.1.10:8443", false, "内网 IP = 暴露"},
		{"10.0.0.5:8443", false, "内网 IP = 暴露"},
		{"example.com:8443", false, "hostname 不解析（刻意），按暴露处理"},
		{"", false, "空 = 交给后续报错"},
		{"no-port", false, "畸形输入按非 loopback 处理"},
		{"1.2.3.4", false, "缺端口"},
	}
	for _, c := range cases {
		t.Run(c.addr, func(t *testing.T) {
			if got := listenIsLoopback(c.addr); got != c.want {
				t.Errorf("listenIsLoopback(%q) = %v, want %v (%s)", c.addr, got, c.want, c.why)
			}
		})
	}
}

// TestNonLoopbackDoesNotBlockStartup 非 loopback 不能拦住启动。
//
// 这是刻意的设计取舍：只提示不 refuse。
// 拦下来等于让管理 IP 直连场景直接不可用，属于破坏性变更
// —— v2.86-PR19 那次把默认值改成 loopback，正是踩了这个坑。
func TestNonLoopbackDoesNotBlockStartup(t *testing.T) {
	t.Setenv("IKEV2_LISTEN_ADDR", "0.0.0.0:8443")
	if _, err := Load(); err != nil {
		t.Errorf("Load must not fail for 0.0.0.0 bind, got %v", err)
	}
}
