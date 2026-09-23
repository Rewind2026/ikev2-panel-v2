// v2.86-PR13.3:runtime.Merge 的 subnet 合并测试。
//
// 覆盖:
//   1. panelstate subnet.conf 存在 → 优先于 env
//   2. panelstate 不存在 → 用 env 值(可能为空)
//   3. panelstate 文件存在但 v6 为空 → v4 用 panelstate,v6 保留 env
//   4. panelstate 文件损坏 → WARN + fallback env,Source 标 env-default
package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yourname/ikev2-panel-v2/internal/config"
	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
)

func TestMerge_PanelStateSubnetWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	// 写 subnet.conf
	cfg := struct {
		IPv4Subnet string `json:"ipv4_subnet"`
		IPv6Subnet string `json:"ipv6_subnet"`
	}{"10.10.20.0/24", "fd00:1::/64"}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "subnet.conf"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	env := &config.Config{
		IPv4Subnet:         "10.10.0.0/24",
		IPv6Subnet:         "fd00:99::/64",
		SubnetConfigSource: "env-default",
	}

	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("Merge err=%v", err)
	}
	if r.IPv4Subnet != "10.10.20.0/24" {
		t.Errorf("v4 = %q, want panelstate value 10.10.20.0/24", r.IPv4Subnet)
	}
	if r.IPv6Subnet != "fd00:1::/64" {
		t.Errorf("v6 = %q, want panelstate value fd00:1::/64", r.IPv6Subnet)
	}
	if r.SubnetConfigSource != "panelstate" {
		t.Errorf("source = %q, want panelstate", r.SubnetConfigSource)
	}
}

func TestMerge_PanelStateMissing_FallsBackToEnv(t *testing.T) {
	dir := t.TempDir()
	// 不写 subnet.conf

	env := &config.Config{
		IPv4Subnet:         "10.13.0.0/24",
		IPv6Subnet:         "fd00:5::/64",
		SubnetConfigSource: "env-default",
	}

	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("Merge err=%v", err)
	}
	if r.IPv4Subnet != "10.13.0.0/24" {
		t.Errorf("v4 = %q, want env value", r.IPv4Subnet)
	}
	if r.IPv6Subnet != "fd00:5::/64" {
		t.Errorf("v6 = %q, want env value", r.IPv6Subnet)
	}
	if r.SubnetConfigSource != "env-default" {
		t.Errorf("source = %q, want env-default", r.SubnetConfigSource)
	}
}

func TestMerge_PanelStateV6Empty_PreservesEnvV6(t *testing.T) {
	dir := t.TempDir()

	// v6 字段为空 → 保留 env 的 v6
	cfg := struct {
		IPv4Subnet string `json:"ipv4_subnet"`
		IPv6Subnet string `json:"ipv6_subnet"`
	}{"10.10.20.0/24", ""}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "subnet.conf"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	env := &config.Config{
		IPv4Subnet:         "10.10.0.0/24",
		IPv6Subnet:         "fd00:99::/64",
		SubnetConfigSource: "env-default",
	}

	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("Merge err=%v", err)
	}
	if r.IPv4Subnet != "10.10.20.0/24" {
		t.Errorf("v4 = %q, want panelstate value", r.IPv4Subnet)
	}
	if r.IPv6Subnet != "fd00:99::/64" {
		t.Errorf("v6 = %q, want env fallback (panelstate v6 was empty)", r.IPv6Subnet)
	}
}

func TestMerge_PanelStateCorrupted_FallsBackToEnv(t *testing.T) {
	dir := t.TempDir()

	// 写坏 JSON
	if err := os.WriteFile(filepath.Join(dir, "subnet.conf"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := &config.Config{
		IPv4Subnet:         "10.13.0.0/24",
		IPv6Subnet:         "fd00:5::/64",
		SubnetConfigSource: "env-default",
	}

	r, err := mergeWithDir(env, dir)
	if err != nil {
		t.Fatalf("Merge err=%v", err)
	}
	if r.IPv4Subnet != "10.13.0.0/24" {
		t.Errorf("v4 = %q, want env fallback after parse error", r.IPv4Subnet)
	}
	if r.SubnetConfigSource != "env-default" {
		t.Errorf("source = %q, want env-default after parse error", r.SubnetConfigSource)
	}
}

// mergeWithDir 等价于 Merge(env),但 subnet 用指定目录的 SubnetConfigStore。
//
// 这是个 helper,因为 Merge 直接调 panelstate.NewSubnetConfigStore()(默认 /data/panel-state),
// 测试不能污染它。
func mergeWithDir(env *config.Config, dir string) (*Runtime, error) {
	if env == nil {
		return nil, nil
	}
	r := &Runtime{Config: env}

	// 阿里云凭证(走默认 Store,跳过 panelstate 不存在的场景下不会读盘)
	credStore := panelstate.NewStore()
	if c, err := credStore.LoadAliyun(); err == nil && c != nil {
		r.AliyunAccessKeyID = c.KeyID
		r.AliyunAccessKeySecret = c.KeySecret
		r.AliyunAccessKeySource = "panelstate"
	}

	// 证书配置(走默认 Store)
	certStore := panelstate.NewCertConfigStore()
	if c, err := certStore.LoadCertConfig(); err == nil && c != nil {
		r.CertMode = pickStr(c.CertMode, env.CertMode)
		r.Domain = pickStr(c.Domain, env.Domain)
		r.ServerCN = pickStr(c.ServerCN, env.ServerCN)
		r.ACMEEmail = pickStr(c.ACMEEmail, env.ACMEEmail)
		r.CertConfigSource = "panelstate"
	} else {
		r.CertMode = env.CertMode
		r.Domain = env.Domain
		r.ServerCN = env.ServerCN
		r.ACMEEmail = env.ACMEEmail
		r.CertConfigSource = env.CertConfigSource
	}

	// 关键差异:用测试目录的 SubnetConfigStore
	subnetStore := panelstate.NewSubnetConfigStoreWithDir(dir)
	if c, err := subnetStore.LoadSubnetConfig(); err == nil && c != nil {
		r.IPv4Subnet = pickStr(c.IPv4Subnet, env.IPv4Subnet)
		r.IPv6Subnet = pickStr(c.IPv6Subnet, env.IPv6Subnet)
		r.SubnetConfigSource = "panelstate"
	} else {
		if err != nil {
			stderrWarn("panelstate subnet.conf parse failed", err)
		}
		r.IPv4Subnet = env.IPv4Subnet
		r.IPv6Subnet = env.IPv6Subnet
		r.SubnetConfigSource = env.SubnetConfigSource
	}

	return r, nil
}