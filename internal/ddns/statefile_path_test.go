// v2.86-PR19(B6):DDNS 状态文件从容器可写层迁到持久化卷的回归测试。
//
// 背景:老默认路径是 /etc/ikev2/ddns.conf,而 compose 只把 /data 挂成命名卷
// → docker compose down / 重建镜像 / 换容器都会连带丢掉这个文件,面板上做的
// DDNS 设置(DDNS 总开关、RR、节流时间戳)重启后全部回到默认。
//
// 本文件验证:
//  1. 默认路径落在 /data 卷内,且不与 panelstate 的 ddns.conf 撞路径
//  2. 旧路径有配置 → 升级后第一次启动内容出现在新路径,旧文件被清掉
//  3. 新路径已有数据 → 不被旧文件覆盖(新优先)
//  4. 旧文件是空白 → 不搬(否则 parseStateFile 得到 Enabled=false,
//     反过来把 env 的 DDNSEnabled=true 覆盖掉)
//  5. 旧文件是 v2-83 裸 "true" → 搬迁 + INI 迁移一步到位,重启后开关仍为 true
//  6. 新旧同路径 / 参数为空 → 直接返回,不 panic
package ddns

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// legacyContent 造一个"老格式"状态文件,返回路径。
func legacyContent(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ddns.conf")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write legacy state file: %v", err)
	}
	return p
}

// TestDefaultStateFileLivesInVolume 默认路径必须在持久化卷内。
func TestDefaultStateFileLivesInVolume(t *testing.T) {
	if got, want := DefaultStateFile, "/data/panel-state/ddns-runtime.conf"; got != want {
		t.Fatalf("DefaultStateFile = %q, want %q", got, want)
	}
	if dir := filepath.Dir(DefaultStateFile); dir != "/data/panel-state" {
		t.Errorf("DefaultStateFile dir = %q, want /data/panel-state", dir)
	}
}

// TestDefaultStateFileDoesNotCollideWithPanelstate 是本次修复里最关键的一条:
// panelstate.DDNSConfigStore 已经占用 /data/panel-state/ddns.conf(JSON 格式),
// ddns 包写的是 INI-style key=value。同名 → 两边互相覆盖,直接把对方的文件写坏。
func TestDefaultStateFileDoesNotCollideWithPanelstate(t *testing.T) {
	if got := filepath.Base(DefaultStateFile); got == "ddns.conf" {
		t.Fatalf("DefaultStateFile basename %q collides with panelstate's ddns.conf", got)
	}
}

// TestMigrateLegacyStateFileCopiesContent 旧路径有 INI 配置 → 搬到新路径。
func TestMigrateLegacyStateFileCopiesContent(t *testing.T) {
	oldPath := legacyContent(t, "enabled=true\nfamily=v6\nrr=vpn\n")
	newPath := filepath.Join(t.TempDir(), "sub", "ddns-runtime.conf")

	migrateLegacyStateFile(newPath, oldPath, testLogger())

	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read migrated file: %v", err)
	}
	if string(data) != "enabled=true\nfamily=v6\nrr=vpn\n" {
		t.Errorf("migrated content = %q, want byte-identical copy", string(data))
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("legacy file should be removed after successful copy, stat err = %v", err)
	}
}

// TestMigrateLegacyStateFileNewWins 新路径已有数据时,旧文件不得覆盖它。
func TestMigrateLegacyStateFileNewWins(t *testing.T) {
	tmp := t.TempDir()
	newPath := filepath.Join(tmp, "ddns-runtime.conf")
	oldPath := filepath.Join(tmp, "legacy.conf")

	if err := os.WriteFile(newPath, []byte("enabled=false\nrr=new\n"), 0o600); err != nil {
		t.Fatalf("write new state file: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("enabled=true\nrr=old\n"), 0o644); err != nil {
		t.Fatalf("write legacy state file: %v", err)
	}

	migrateLegacyStateFile(newPath, oldPath, testLogger())

	data, _ := os.ReadFile(newPath)
	if string(data) != "enabled=false\nrr=new\n" {
		t.Errorf("new state file overwritten by legacy: %q", string(data))
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("legacy file must be left untouched when new file exists, stat err = %v", err)
	}
}

// TestMigrateLegacyStateFileSkipsBlank 旧文件是空白 → 不搬。
//
// 空白文件被搬过去后,parseStateFile 会返回 Enabled=false,把 env 里
// DDNSEnabled=true 的部署直接关掉 DDNS —— 这是迁移逻辑最容易踩的坑。
func TestMigrateLegacyStateFileSkipsBlank(t *testing.T) {
	for name, content := range map[string]string{
		"empty":       "",
		"whitespace":  "   \n\t\n",
		"commentless": "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			oldPath := legacyContent(t, content)
			newPath := filepath.Join(t.TempDir(), "ddns-runtime.conf")

			migrateLegacyStateFile(newPath, oldPath, testLogger())

			if _, err := os.Stat(newPath); !os.IsNotExist(err) {
				t.Errorf("blank legacy file must not be migrated, stat err = %v", err)
			}
			if _, err := os.Stat(oldPath); err != nil {
				t.Errorf("blank legacy file must be left in place, stat err = %v", err)
			}
		})
	}
}

// TestMigrateLegacyStateFileMissingOld 旧路径不存在(全新安装)→ 什么都不做。
func TestMigrateLegacyStateFileMissingOld(t *testing.T) {
	tmp := t.TempDir()
	newPath := filepath.Join(tmp, "ddns-runtime.conf")
	oldPath := filepath.Join(tmp, "not-there.conf")

	migrateLegacyStateFile(newPath, oldPath, testLogger())

	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("nothing should be created when legacy file is absent, stat err = %v", err)
	}
}

// TestMigrateLegacyStateFileNoOpArgs 参数退化路径不 panic、不产生副作用。
func TestMigrateLegacyStateFileNoOpArgs(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "same.conf")
	if err := os.WriteFile(p, []byte("enabled=true\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 同路径:不能把文件删了(否则自己删自己)
	migrateLegacyStateFile(p, p, testLogger())
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("same-path migration must be a no-op, stat err = %v", err)
	}
	// 空参数
	migrateLegacyStateFile("", p, testLogger())
	migrateLegacyStateFile(p, "", testLogger())
	migrateLegacyStateFile(p, p, nil) // logger nil 也不能 panic
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("degenerate args must not remove the file, stat err = %v", err)
	}
}

// TestNewSyncMigratesLegacyAndKeepsEnabled 端到端:老部署(v2-83 裸 "true")
// 升级后第一次 NewSync → 内容搬到新路径并转成 INI → IsEnabled 仍为 true。
//
// 这是 B6 的核心用户可见行为:升级不能把用户已开启的 DDNS 关掉。
func TestNewSyncMigratesLegacyAndKeepsEnabled(t *testing.T) {
	oldPath := legacyContent(t, "true")
	newPath := filepath.Join(t.TempDir(), "panel-state", "ddns-runtime.conf")

	s := NewSync(Config{
		Enabled:   false, // env 没开,老文件才是 source of truth
		Family:    "dual",
		StateFile: newPath,
		Logger:    testLogger(),
	})

	migrateLegacyStateFile(newPath, oldPath, s.cfg.Logger)
	migrateStateFileIfNeeded(newPath, "dual", s.cfg.Logger)

	s2 := NewSync(Config{
		Enabled:   false,
		Family:    "dual",
		StateFile: newPath,
		Logger:    testLogger(),
	})
	if !s2.IsEnabled() {
		t.Error("after migration, DDNS must stay enabled (legacy state file was \"true\")")
	}
	if s2.cfg.Family != "dual" {
		t.Errorf("Family = %q, want dual", s2.cfg.Family)
	}

	rec, err := parseStateFile(newPath, "dual")
	if err != nil {
		t.Fatalf("parseStateFile: %v", err)
	}
	if !rec.Enabled {
		t.Error("migrated file should record enabled=true")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("legacy file should be gone, stat err = %v", err)
	}
}

// TestNewSyncBlankLegacyKeepsEnvEnabled env 开着 + 旧文件空白 → 不被关掉。
func TestNewSyncBlankLegacyKeepsEnvEnabled(t *testing.T) {
	oldPath := legacyContent(t, "\n")
	newPath := filepath.Join(t.TempDir(), "ddns-runtime.conf")

	s := NewSync(Config{
		Enabled:   true, // env 开着
		Family:    "dual",
		StateFile: newPath,
		Logger:    testLogger(),
	})
	migrateLegacyStateFile(newPath, oldPath, s.cfg.Logger)

	s2 := NewSync(Config{
		Enabled:   true,
		Family:    "dual",
		StateFile: newPath,
		Logger:    testLogger(),
	})
	if !s2.IsEnabled() {
		t.Error("blank legacy file must not override env DDNSEnabled=true")
	}
}

// TestNewSyncPersistsThrottleAcrossRestart 节流时间戳落在新路径 → 模拟
// "容器重建"(内存全丢,只剩卷上的文件)后节流窗口仍在。
func TestNewSyncPersistsThrottleAcrossRestart(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "ddns-runtime.conf")

	// 第一次:写一个"刚刚同步过"的时间戳
	s := NewSync(Config{Enabled: true, Family: "v4", StateFile: stateFile, Logger: testLogger()})
	s.setLastSyncAndPersist(s.cfg, "A", time.Now())

	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("throttle timestamp should be persisted: %v", err)
	}

	// 第二次:全新实例(等价于容器重建,只有文件活下来)
	s2 := NewSync(Config{Enabled: true, Family: "v4", StateFile: stateFile, Logger: testLogger()})
	ts, ok := s2.lastSyncTime["A"]
	if !ok || ts.IsZero() {
		t.Fatalf("throttle timestamp lost across restart (state file is the only survivor)")
	}
	if time.Since(ts) > time.Minute {
		t.Errorf("restored throttle timestamp too old: %v", ts)
	}
}
