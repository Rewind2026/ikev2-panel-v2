// v2.86-PR12.5:CertConfig + Store 单元测试
package panelstate

import (
	"os"
	"strings"
	"testing"
)

func TestCertConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     CertConfig
		wantErr bool
		errSub  string // 期望错误包含的子串
	}{
		{
			name:    "self-signed,空 domain OK",
			cfg:     CertConfig{CertMode: "self-signed"},
			wantErr: false,
		},
		{
			name:    "letsencrypt + 合法 domain",
			cfg:     CertConfig{CertMode: "letsencrypt", Domain: "vpn.example.com"},
			wantErr: false,
		},
		{
			name:    "letsencrypt + 子域名",
			cfg:     CertConfig{CertMode: "letsencrypt", Domain: "v.example.co.uk"},
			wantErr: false,
		},
		{
			name:    "letsencrypt + 空 domain 失败",
			cfg:     CertConfig{CertMode: "letsencrypt"},
			wantErr: true,
			errSub:  "域名",
		},
		{
			name:    "letsencrypt + IP 失败",
			cfg:     CertConfig{CertMode: "letsencrypt", Domain: "1.2.3.4"},
			wantErr: true,
			errSub:  "域名不合法",
		},
		{
			name:    "letsencrypt + 含下划线失败",
			cfg:     CertConfig{CertMode: "letsencrypt", Domain: "bad_domain.example.com"},
			wantErr: true,
			errSub:  "域名不合法",
		},
		{
			name:    "letsencrypt + 含 _ 失败(LE 不签)",
			cfg:     CertConfig{CertMode: "letsencrypt", Domain: "v_pn.example.com"},
			wantErr: true,
			errSub:  "域名不合法",
		},
		{
			name:    "未知模式失败",
			cfg:     CertConfig{CertMode: "custom"},
			wantErr: true,
			errSub:  "cert_mode",
		},
		{
			name:    "空模式失败",
			cfg:     CertConfig{CertMode: ""},
			wantErr: true,
			errSub:  "cert_mode",
		},
		{
			name:    "ServerCN 合法",
			cfg:     CertConfig{CertMode: "self-signed", ServerCN: "vpn.example.com"},
			wantErr: false,
		},
		{
			name:    "ServerCN 不合法",
			cfg:     CertConfig{CertMode: "self-signed", ServerCN: "not a domain"},
			wantErr: true,
			errSub:  "server_cn",
		},
		{
			name:    "邮箱合法",
			cfg:     CertConfig{CertMode: "self-signed", ACMEEmail: "admin@example.com"},
			wantErr: false,
		},
		{
			name:    "邮箱不合法",
			cfg:     CertConfig{CertMode: "self-signed", ACMEEmail: "not-an-email"},
			wantErr: true,
			errSub:  "acme_email",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.errSub)
			}
		})
	}
}

func TestCertConfigStoreWriteRead(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewCertConfigStoreWithDir(tmpDir)

	// 1) Load on empty dir → nil, nil
	cfg, err := store.LoadCertConfig()
	if err != nil {
		t.Fatalf("LoadCertConfig on empty: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil cfg on empty dir, got %+v", cfg)
	}

	// 2) Write + Reload
	want := CertConfig{
		CertMode:  "letsencrypt",
		Domain:    "vpn.example.com",
		ServerCN:  "vpn.example.com",
		ACMEEmail: "admin@example.com",
	}
	if err := store.WriteCertConfig(want); err != nil {
		t.Fatalf("WriteCertConfig: %v", err)
	}

	// 文件权限校验
	info, err := os.Stat(store.certConfPath())
	if err != nil {
		t.Fatalf("stat cert.conf: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cert.conf perm = %o, want 0600", perm)
	}

	// 3) Reload
	cfg2, err := store.LoadCertConfig()
	if err != nil {
		t.Fatalf("LoadCertConfig: %v", err)
	}
	if cfg2 == nil {
		t.Fatalf("expected cfg after write")
	}
	if cfg2.CertMode != want.CertMode || cfg2.Domain != want.Domain {
		t.Errorf("reload mismatch: got %+v want %+v", cfg2, want)
	}
	if cfg2.UpdatedAt == 0 {
		t.Errorf("UpdatedAt should be set after Write")
	}

	// 4) v2.86-pr23o:JSON 必须包含 server_cn/acme_email/domain 字段(即使空值),
	// 否则 entrypoint.sh 的 grep 找不到 → `set -euo pipefail` → 容器秒死 → 无限重启。
	data, err := os.ReadFile(store.certConfPath())
	if err != nil {
		t.Fatalf("read cert.conf: %v", err)
	}
	for _, key := range []string{`"cert_mode"`, `"domain"`, `"server_cn"`, `"acme_email"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("cert.conf missing %s key, content:\n%s", key, data)
		}
	}

	// 4) Read 缓存命中
	cfg3, err := store.ReadCertConfig()
	if err != nil || cfg3 == nil {
		t.Fatalf("ReadCertConfig: %v cfg=%v", err, cfg3)
	}
	if cfg3.Domain != want.Domain {
		t.Errorf("ReadCertConfig domain mismatch")
	}

	// 5) CertConfigExists
	if !store.CertConfigExists() {
		t.Error("CertConfigExists should be true after write")
	}

	// 6) ClearCertConfig
	if err := store.ClearCertConfig(); err != nil {
		t.Fatalf("ClearCertConfig: %v", err)
	}
	if store.CertConfigExists() {
		t.Error("CertConfigExists should be false after clear")
	}
	if _, err := os.Stat(store.certConfPath()); !os.IsNotExist(err) {
		t.Errorf("cert.conf should be removed, stat err = %v", err)
	}
}

func TestCertConfigStoreValidateBeforeWrite(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewCertConfigStoreWithDir(tmpDir)

	// 无效配置不能写入
	bad := CertConfig{CertMode: "invalid-mode"}
	if err := store.WriteCertConfig(bad); err == nil {
		t.Fatal("expected error for invalid cert_mode")
	}
	if store.CertConfigExists() {
		t.Error("invalid config should not have written file")
	}
}

func TestCertConfigStoreReloadValidates(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewCertConfigStoreWithDir(tmpDir)

	// 直接写一个坏 JSON 到磁盘,Load 应该报错
	if err := os.WriteFile(store.certConfPath(), []byte(`{"cert_mode":"bogus"}`), 0o600); err != nil {
		t.Fatalf("seed bad config: %v", err)
	}
	if _, err := store.LoadCertConfig(); err == nil {
		t.Fatal("expected LoadCertConfig to fail on invalid mode")
	}
}