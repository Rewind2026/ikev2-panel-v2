// Command dnsq 直接向阿里云 DNS API 查询记录，绕开本地递归 DNS。
//
// 为什么需要这个工具：
//
//	判断"某条 DNS 记录是否存在"时，dig / nslookup / ping 都**不可信** ——
//	它们只反映"本机的递归链路能不能拿到答案"。递归链路坏掉、被劫持、
//	被防火墙黑洞时它们返回空，而你只能推出"我拿不到"，**推不出"记录不存在"**。
//
//	真机上就因此误判过三次：
//	  1. "A/AAAA 记录不存在"        → 实际一直存在
//	  2. "DDNS 修复没生效"          → 实际已生效
//	  3. "LE 与 DDNS 循环依赖死锁"   → 不存在
//
//	同刻用阿里云权威 API 查，记录一直好着 —— 而那台机的递归 DNS 是坏的
//	（连主域都解析不了）。判断云上资源存在性，一律用云自己的 API。
//
// 用法（凭证在 /data，所以要在容器内跑）：
//
//	docker compose exec ikev2-panel /usr/local/bin/dnsq example.com vpn
//	docker compose exec ikev2-panel /usr/local/bin/dnsq -creds /tmp/c.json example.com @ A
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/dns"
)

func main() {
	var (
		credsPath = flag.String("creds", "/data/panel-state/aliyun.creds",
			"阿里云凭证 JSON 路径（字段 key_id / key_secret）")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: %s [flags] <domain> [rr] [type]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  例: %s example.com vpn\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "  rr 省略则查 @ 与 vpn 两组；type 省略则 A 与 AAAA 都查")
		fmt.Fprintln(os.Stderr)
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		flag.Usage()
		os.Exit(2)
	}
	domain := args[0]

	rrs := []string{"@", "vpn"}
	if len(args) > 1 {
		rrs = []string{args[1]}
	}
	types := []string{dns.RecordTypeA, dns.RecordTypeAAAA}
	if len(args) > 2 {
		types = []string{args[2]}
	}

	creds := loadCreds(*credsPath)
	fmt.Printf("凭证 %s...\n\n", mask(creds.KeyID))

	c := dns.NewAliyunClient(creds.KeyID, creds.KeySecret)

	fmt.Println("=== 阿里云权威 API（ground truth）===")
	for _, rr := range rrs {
		for _, typ := range types {
			rec, err := c.FindRecord(context.Background(), domain, rr, typ)
			switch {
			case err != nil:
				fmt.Printf("  %-8s %-5s -> ERROR %v\n", rr, typ, err)
			case rec == nil:
				fmt.Printf("  %-8s %-5s -> 不存在\n", rr, typ)
			default:
				fmt.Printf("  %-8s %-5s -> RecordId=%s Value=%s\n", rr, typ, rec.RecordID, rec.Value)
			}
		}
	}

	fmt.Println("\n=== 本机递归 DNS（仅供对照，可能因网络环境失真）===")
	for _, name := range namesFor(domain, rrs) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		addrs, err := net.DefaultResolver.LookupHost(ctx, name)
		cancel()
		if err != nil {
			fmt.Printf("  %-32s -> 本机拿不到（%v）\n", name, err)
			continue
		}
		fmt.Printf("  %-32s -> %v\n", name, addrs)
	}

	fmt.Println("\n结论：以上面 API 的结果为准。下面这组只反映本机递归链路的状态。")
}

// creds 是 /data/panel-state/aliyun.creds 的结构（字段与 panelstate 写入方一致）。
type creds struct {
	KeyID     string `json:"key_id"`
	KeySecret string `json:"key_secret"`
}

// loadCreds 读并校验凭证文件。
func loadCreds(path string) creds {
	var c creds
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读凭证失败 %s: %v\n", path, err)
		fmt.Fprintln(os.Stderr, "（凭证在 /data，需在容器内跑：docker compose exec ikev2-panel ...）")
		os.Exit(1)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		fmt.Fprintf(os.Stderr, "解析凭证失败: %v\n", err)
		os.Exit(1)
	}
	if c.KeyID == "" || c.KeySecret == "" {
		fmt.Fprintln(os.Stderr, "凭证为空（key_id 或 key_secret 缺失）")
		os.Exit(1)
	}
	return c
}

// mask 返回凭证前 4 位，其余打码。
func mask(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[:4] + strings.Repeat("*", len(s)-4)
}

// namesFor 拼出要对照解析的域名列表。
func namesFor(domain string, rrs []string) []string {
	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		if rr == "@" || rr == "" {
			out = append(out, domain)
		} else {
			out = append(out, rr+"."+domain)
		}
	}
	return out
}
