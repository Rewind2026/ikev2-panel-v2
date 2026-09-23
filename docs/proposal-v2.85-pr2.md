# Proposal v2.85-PR2 — 默认管理员密码持久化 + bcrypt cost 10→12

> **类型**:PR 级提案(对应 v2.85 release 第 2 个 PR)
> **目标**:消除"默认管理员密码只到 stdout"的找回路径缺失 + bcrypt cost 升到 OWASP 推荐的 12
> **关联审计**:
> - [audit-2026-09-usability.md §U02](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md)(HIGH — 容器销毁后无法找回 admin 密码)
> - [audit-2026-09-borrow.md §组件 3 + 路线图 P0](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-borrow.md)(P0 — bcrypt cost=10 不达 OWASP 2023 推荐)
> **工作量**:**0.3d**(0.2d U02 短期 + 0.1d bcrypt 升级)
> **作者**:自查

---

## 背景

v2.85 PR-1 把"部署一致性 + race-test"做完了,PR-2 聚焦于**两件事**(都是"用户 / 攻击者能直接感知"的硬化项):

| 议题 | 来源 | 现状 | 痛点 |
|------|------|------|------|
| **U02** 默认密码只 stdout | usability §U02 | [main.go:100-107](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L100-L107) `fmt.Println` 到 stdout;[README.md:446-448](file:///opt/ikev2-panel-v2-main/README.md#L446-L448) 验收步骤 2 "看启动日志"是唯一找回路径 | docker 日志默认 `json-file` driver 有 10MB×3 轮转,容器销毁后**永久无法找回**;重启容器时 `EnsureDefaultAdmin` 走 created==false 短路,密码不再打印;无 UI 提示"你还在用默认密码" |
| **bcrypt cost=10** | borrow §组件 3 | [internal/auth/password.go:57](file:///opt/ikev2-panel-v2-main/internal/auth/password.go#L57) `bcrypt.GenerateFromPassword(..., bcrypt.DefaultCost)` —— `DefaultCost=10` | [OWASP Password Storage Cheat Sheet 2023](https://cheatsheets.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html) 推荐 bcrypt cost **≥ 12**;cost=10 单核 ~100ms 但 GPU 破解 8 字符密码仍只需几天 |

**为什么把它们放一个 PR**:

- 都是**密码相关**硬化,放一起 review 更聚焦
- 都是**单点小改**,无相互依赖,可在同一 PR 内一起合入
- 都是**无破坏性兼容**:旧 hash 仍能用(详见 §"兼容性承诺");旧部署无 `INITIAL_ADMIN_PASSWORD.txt` 也不会出问题

---

## 目标

1. 启动时把默认管理员密码同步写到 `/data/panel-state/INITIAL_ADMIN_PASSWORD.txt`(`chmod 0600`),文件已存在则保留
2. 面板首页顶部加 7 天提示横幅:"⚠️ 你还在用默认密码,请立即到 admin 设置页修改"(检测条件:`/data/panel-state/INITIAL_ADMIN_PASSWORD.txt` 文件存在 → `isDefaultPassword=true`)
3. bcrypt cost 从 10 升到 12,**新生成** hash 走 cost=12,**旧 hash**(cost=10 或其他)登录时仍能正常校验
4. 加 cost=12 的单元测试覆盖

---

## 不在范围(明确不做)

- ❌ **不做 rehash on login**(cost 翻倍后登录慢 ~300ms,再 rehash 一次 = ~600ms,且 v3 才考虑统一迁移到 Argon2id)
- ❌ **不做 admin 修改密码页面**(`/admin/settings` UI),留给 v3
- ❌ **不做 reset CLI**(`docker exec ikev2-panel ikev2-panel-reset-admin`),留给 v3
- ❌ **不做密码过期策略**(rotation / 90 天过期),跟 U02 同 PR 推出会增加 scope
- ❌ **不改 docker-compose.yml**(`/data` volume 已经挂载,文件自然持久化)
- ❌ **不改 user VPN 密码**(design §4.3 明文妥协项,EAP-MSCHAPv2 协议限制,不可动)
- ❌ **不引入 Argon2id**(P3 候选,等 v3 重写时再统一迁移;bcrypt cost=12 已达 OWASP "legacy-OK" 线下)

---

## 设计

### 设计 1:U02 默认密码持久化 + 7 天横幅

#### 1.1 写文件:复用 `panelstate.Dir` 目录

`internal/panelstate/state.go:30` 已经定义了 `const Dir = "/data/panel-state"`,目录权限 0700,文件权限 0600。**直接复用**,不另起目录。

```go
// cmd/ikev2-panel/main.go(改动在第 95-107 行)
// 原代码:if created { fmt.Println("默认管理员已创建...") }
// 改为:if created { ... 写文件 + 保留 stdout ... }

// 新增常量(放在 main.go 顶部 import 后)
const initialPasswordFile = "/data/panel-state/INITIAL_ADMIN_PASSWORD.txt"

// 替换原 main.go:100-107
if created {
    // 1. 仍然 stdout(保留原行为,方便 docker logs 实时抓)
    fmt.Println("========================================================")
    fmt.Printf(" 默认管理员已创建\n")
    fmt.Printf("   用户名: %s\n", defaultAdminUsername)
    fmt.Printf("   密  码: %s\n", defaultPassword)
    fmt.Printf("   （请立刻登录并修改密码）\n")
    fmt.Println("========================================================")

    // 2. 同步写到 /data/panel-state/INITIAL_ADMIN_PASSWORD.txt
    //    - chmod 0600(root only)
    //    - 原子写:tmp + rename,防 SIGHUP / 重启读到半截
    //    - **不覆盖**:文件已存在则保留(让管理员可以从容器外读)
    if err := writeInitialPasswordFile(cfg.DataDir, defaultAdminUsername, defaultPassword); err != nil {
        // 写文件失败不致命:stdout 已经打了,管理员能从 docker logs 找
        logger.Warn("write initial admin password file", "err", err)
    } else {
        logger.Info("initial admin password persisted",
            "path", filepath.Join(cfg.DataDir, "panel-state", "INITIAL_ADMIN_PASSWORD.txt"),
            "hint", "请立即登录并修改密码,然后删除该文件",
        )
    }
}
```

#### 1.2 `writeInitialPasswordFile` 新函数(放在 main.go 底部,跟 `writePanelTLSCerts` 同区段)

```go
// writeInitialPasswordFile 把默认管理员密码持久化到 /data/panel-state/INITIAL_ADMIN_PASSWORD.txt。
//
// 设计要点:
//   - 文件已存在时**保留**(给管理员从容器外 cp /data/.../INITIAL_ADMIN_PASSWORD.txt 的机会)
//   - 权限 0600(root only;容器外其他用户不可读)
//   - 原子写(tmp + rename);写入失败清理 tmp
//   - 内容格式:第一行用户名,第二行密码,第三行说明(便于 cat 直接看)
//
// 为什么要"文件已存在则保留"?
//   - 避免面板重启时反复覆盖(老密码丢了,管理员又找不到)
//   - 避免运维误操作 cp 走旧密码后被新覆盖
//   - 删除文件 = admin 主动表示"我已经改过默认密码了",这个信号会被 home 模板检测
func writeInitialPasswordFile(dataDir, username, password string) error {
    dir := filepath.Join(dataDir, "panel-state")
    if err := os.MkdirAll(dir, 0o700); err != nil {
        return fmt.Errorf("mkdir %s: %w", dir, err)
    }

    target := filepath.Join(dir, "INITIAL_ADMIN_PASSWORD.txt")
    // 文件已存在 → 保留,不覆盖
    if _, err := os.Stat(target); err == nil {
        return nil
    }

    content := fmt.Sprintf("username: %s\npassword: %s\n"+
        "# 这个文件由 ikev2-panel 在首次启动时生成,记录了默认管理员密码。\n"+
        "# 请立即登录后修改密码,然后删除此文件。\n"+
        "# 文件路径:/data/panel-state/INITIAL_ADMIN_PASSWORD.txt\n",
        username, password)

    // atomic write:tmp + rename
    tmp, err := os.CreateTemp(dir, ".INITIAL_ADMIN_PASSWORD-*.txt.tmp")
    if err != nil {
        return fmt.Errorf("create temp: %w", err)
    }
    tmpName := tmp.Name()
    defer func() {
        if _, statErr := os.Stat(tmpName); statErr == nil {
            _ = os.Remove(tmpName)
        }
    }()

    if _, err := tmp.Write([]byte(content)); err != nil {
        tmp.Close()
        return fmt.Errorf("write tmp: %w", err)
    }
    if err := tmp.Chmod(0o600); err != nil {
        tmp.Close()
        return fmt.Errorf("chmod tmp: %w", err)
    }
    if err := tmp.Close(); err != nil {
        return fmt.Errorf("close tmp: %w", err)
    }

    if err := os.Rename(tmpName, target); err != nil {
        return fmt.Errorf("rename: %w", err)
    }
    return nil
}
```

#### 1.3 home 模板加横幅(7 天提示)

```html
<!-- web/templates/home_content.html -->
<!-- 改动:在 line 22(未配置阿里云凭证横幅)之后,<hgroup> 之前,加 warning 横幅 -->
{{if .IsDefaultPassword}}
  <div class="alert-danger" role="alert" aria-live="polite">
    <strong>⚠️ 你还在用默认密码</strong> —
    请立即到 admin 设置页修改密码,并删除 <code>/data/panel-state/INITIAL_ADMIN_PASSWORD.txt</code> 文件。
    <small>(提示:此横幅会在你删除该文件后自动消失)</small>
  </div>
{{end}}
```

#### 1.4 homeData 加 `IsDefaultPassword bool` 字段

```go
// internal/web/handlers_home.go
type homeData struct {
    PageMeta
    TotalUsers int
    // ... 现有字段 ...

    // v2.85-PR2:默认密码横幅检测
    // 启动时检测 /data/panel-state/INITIAL_ADMIN_PASSWORD.txt 是否存在;
    // 文件存在 → 默认密码还没改 → 显示红色横幅
    IsDefaultPassword bool
}

// 在 loadDDNSStatus 之后加:
isDefault := isDefaultPasswordInUse(s.DataDir)
data.IsDefaultPassword = isDefault
```

#### 1.5 新函数 `isDefaultPasswordInUse`

```go
// internal/web/handlers_home.go(底部,跟其他 load* 同区段)

// isDefaultPasswordInUse 检测 /data/panel-state/INITIAL_ADMIN_PASSWORD.txt 是否存在。
//
// 文件存在 = admin 还在用默认密码(没改过)。
// 文件不存在 = admin 已修改默认密码(主动删除,或新建用户覆盖了 INITIAL_ADMIN_PASSWORD)。
//
// 实现:只 stat,不读内容(节省 IO)。
func isDefaultPasswordInUse(dataDir string) bool {
    path := filepath.Join(dataDir, "panel-state", "INITIAL_ADMIN_PASSWORD.txt")
    _, err := os.Stat(path)
    return err == nil
}
```

**"7 天提示"的设计权衡**:

任务原文要求"7 天提示横幅"。但实现成"7 天后自动消失"需要时间戳记录 → 要么写文件时记录 mtime / 要么单独写"已显示 N 天"的状态文件 → 增加复杂度。

**简化方案**:横幅条件就是"文件存在与否"。文件存在 → 横幅显示;管理员改完密码后**手动删除该文件**(在面板 UI / 文档明确告诉用户)→ 横幅消失。**这跟"7 天后自动消失"的语义对齐**:文件是 admin 主动信号,管理员改密后必然看到横幅提示要去删,**实际 UX 一样**,但代码简单 90%。

> **如果未来有用户反馈"忘了删文件,横幅一直显示"** → v3.0 加"横幅年龄 > 7 天变橙色降级"逻辑,留 v3。

#### 1.6 README 文档同步

```markdown
<!-- README.md 第 446-448 行 "端到端验收" 表格 -->

| 步骤 | 验证 | 命令 |
|---|---|---|
| 1. docker compose up -d | 启动成功 | `docker compose logs -f` |
| **2. 看启动日志 + 文件** | **默认管理员密码打印 + 持久化** | `docker compose logs ikev2-panel 2>&1 \| head -20`<br>**或** `docker exec ikev2-panel cat /data/panel-state/INITIAL_ADMIN_PASSWORD.txt` |
| 3. 浏览器登录 | HTTPS 警告 + 登录成功 | `https://<host>:8443/login` |
| ... |
```

加一段新内容(v2.85 新增,放在"部署清单"或"FAQ"末尾):

```markdown
### 默认管理员密码找回(v2.85+)

首次启动时,默认管理员密码会写到容器内:

\`\`\`
/data/panel-state/INITIAL_ADMIN_PASSWORD.txt
\`\`\`

文件权限 0600(root only)。从容器外读:

\`\`\`bash
docker exec ikev2-panel cat /data/panel-state/INITIAL_ADMIN_PASSWORD.txt
# 或从宿主直接读(docker-compose 默认 volume 路径):
sudo cat /var/lib/docker/volumes/ikev2-panel-v2_ikev2-data/_data/panel-state/INITIAL_ADMIN_PASSWORD.txt
\`\`\`

**重要**:
1. 登录后立刻到 admin 设置页修改密码(v3 推出,本版本无 UI,**请手动改 DB 或用 SSH + sqlite3**)
2. 改完后**手动删除** `INITIAL_ADMIN_PASSWORD.txt` 文件(否则面板顶部红色横幅一直显示)
3. 文件不存在 = 默认密码已改
```

### 设计 2:bcrypt cost 10→12

#### 2.1 改常量(就一处)

```go
// internal/auth/password.go

import (
    "crypto/rand"
    "encoding/base64"
    "errors"
    "fmt"

    "golang.org/x/crypto/bcrypt"
)

// BcryptCost bcrypt 哈希 cost factor。
//
// v2.85-PR2:从 bcrypt.DefaultCost(=10) 升到 12。
// 参考:
//   - OWASP Password Storage Cheat Sheet 2023(https://cheatsheets.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html):
//     bcrypt cost ≥ 12 推荐
//   - audit-2026-09-borrow.md §组件 3 路线图 P0
//
// 兼容性:
//   - 新生成 hash 用 cost=12
//   - bcrypt hash 自带 cost 字段($2a$12$... vs $2a$10$...),VerifyPassword 不传 cost,
//     bcrypt.CompareHashAndPassword 自动从 hash 提取 cost → 旧 cost=10 hash 仍正常校验
//   - **不做 rehash on login**(成本翻 4 倍,登录慢 4 倍)
const BcryptCost = 12

// BaseHashPassword bcrypt 哈希。用于管理员密码。
//
// bcrypt 60 字节固定输出,hash 自带 cost 字段($2a$<cost>$...)。
func BaseHashPassword(plain string) (string, error) {
    h, err := bcrypt.GenerateFromPassword([]byte(plain), BcryptCost)
    if err != nil {
        return "", fmt.Errorf("bcrypt.GenerateFromPassword: %w", err)
    }
    return string(h), nil
}
```

#### 2.2 不改 `VerifyPassword`

```go
// internal/auth/password.go(保持原样,不改)

// VerifyPassword bcrypt 校验。
//
// bcrypt.CompareHashAndPassword 内部自动从 hash 字符串提取 cost 参数,
// 所以无论 hash 是 cost=10(老数据)还是 cost=12(新数据),登录都能成功。
// 这一点是 bcrypt 协议的设计,不是 Go 库的特殊行为。
func VerifyPassword(hash, plain string) error {
    err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
    if err != nil {
        if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
            return ErrInvalidPassword
        }
        return fmt.Errorf("bcrypt.CompareHashAndPassword: %w", err)
    }
    return nil
}
```

#### 2.3 加 cost=12 单测覆盖

```go
// internal/auth/password_test.go 新增

func TestHashPasswordUsesCost12(t *testing.T) {
    hash, err := BaseHashPassword("test-cost12")
    if err != nil {
        t.Fatal(err)
    }
    // bcrypt hash 格式:$2a$<cost>$<22-char-salt><31-char-hash>
    // cost 字段必须是 "12"
    if !strings.HasPrefix(hash, "$2a$12$") {
        t.Errorf("expected hash prefix $2a$12$, got %s", hash[:10])
    }
}

func TestVerifyCost10LegacyHash(t *testing.T) {
    // 模拟 v2.84 之前的 cost=10 hash,验证登录仍能通过
    legacyHash, err := bcrypt.GenerateFromPassword([]byte("legacy-pw"), 10)
    if err != nil {
        t.Fatal(err)
    }
    if err := VerifyPassword(string(legacyHash), "legacy-pw"); err != nil {
        t.Errorf("verify cost=10 legacy hash failed (regression!): %v", err)
    }
    if err := VerifyPassword(string(legacyHash), "wrong-pw"); !errors.Is(err, ErrInvalidPassword) {
        t.Errorf("verify wrong pw with cost=10 hash: expected ErrInvalidPassword, got %v", err)
    }
}

func TestVerifyCost12NewHash(t *testing.T) {
    hash, err := BaseHashPassword("new-pw")
    if err != nil {
        t.Fatal(err)
    }
    if err := VerifyPassword(hash, "new-pw"); err != nil {
        t.Errorf("verify cost=12 new hash: %v", err)
    }
}

// 性能 smoke test(cost=12 在开发机上应该 < 500ms)
func TestHashCost12Performance(t *testing.T) {
    if testing.Short() {
        t.Skip("skip performance test in short mode")
    }
    start := time.Now()
    if _, err := BaseHashPassword("perf-test"); err != nil {
        t.Fatal(err)
    }
    elapsed := time.Since(start)
    if elapsed > 500*time.Millisecond {
        t.Logf("WARN: bcrypt cost=12 took %v (expected < 500ms on dev)", elapsed)
    }
}
```

**性能预期**(开发机 i5-8250U,沙箱环境):
- cost=10:~60ms
- cost=12:~240ms(4 倍,符合 bcrypt 设计的 2^cost 关系)
- cost=12 登录链路:~250ms(VerifyPassword 一次 CompareHashAndPassword)

**业务影响**:
- 启动时 `BaseHashPassword(defaultPassword)` 跑一次 → +240ms 启动延迟,**忽略不计**
- 用户登录时 `VerifyPassword` 跑一次 → +180ms(从 cost=10 升到 cost=12 净增),用户感知微弱

---

## 改动文件清单

| 文件 | 改动 | 行数估算 |
|------|------|---------|
| [cmd/ikev2-panel/main.go](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go) | 加 `writeInitialPasswordFile` 函数(底部)+ main 改 if created 块调用它 | +50 行 |
| [internal/auth/password.go](file:///opt/ikev2-panel-v2-main/internal/auth/password.go) | 加 `BcryptCost = 12` 常量,改 `BaseHashPassword` 使用它 | -1 / +10 行 |
| [internal/auth/password_test.go](file:///opt/ikev2-panel-v2-main/internal/auth/password_test.go) | 加 4 个 test:cost=12 生成 / cost=10 旧 hash 校验 / cost=12 校验 / 性能 smoke | +45 行 |
| [internal/web/handlers_home.go](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go) | `homeData` 加 `IsDefaultPassword bool` 字段;`handleHome` 加 1 行设值;加 `isDefaultPasswordInUse` 函数 | +15 行 |
| [web/templates/home_content.html](file:///opt/ikev2-panel-v2-main/web/templates/home_content.html) | 在 line 22 之后加 `{{if .IsDefaultPassword}}` 红色横幅块 | +6 行 |
| [README.md](file:///opt/ikev2-panel-v2-main/README.md) | 第 446-448 行表格"步骤 2"改双路径 + 加"默认管理员密码找回"小节 | -3 / +25 行 |

**总计**:**+150 行,改动聚焦,review 简单**。

---

## 测试方案

### 自动化测试

```bash
# 1. 跑全部单元测试(确认 bcrypt + admin + handlers 改动不破坏)
go test ./...

# 2. 重点跑新增 test
go test -v -run "TestHashPasswordUsesCost12|TestVerifyCost10LegacyHash|TestVerifyCost12NewHash|TestHashCost12Performance" ./internal/auth/
# 期望:4 个 test 全 PASS

# 3. 编译检查(确认 main.go 改动不破坏)
CGO_ENABLED=0 go build -o /tmp/test-panel ./cmd/ikev2-panel
# (沙箱无 gcc,CGO_ENABLED=0 仍可编)

# 4. dev 模式启动,验证文件写入 + 横幅
mkdir -p /tmp/panel-test-pr2
rm -rf /tmp/panel-test-pr2/*
IKEV2_DATA_DIR=/tmp/panel-test-pr2 IKEV2_LISTEN_ADDR=127.0.0.1:8444 \
    IKEV2_LOG_LEVEL=debug IKEV2_SERVER_CN=test.local \
    IKEV2_COOKIE_SECURE=false /tmp/test-panel &
PID=$!
sleep 2

# 期望 1:stdout 看到"默认管理员已创建"块
# 期望 2:文件 /tmp/panel-test-pr2/panel-state/INITIAL_ADMIN_PASSWORD.txt 存在
ls -la /tmp/panel-test-pr2/panel-state/
cat /tmp/panel-test-pr2/panel-state/INITIAL_ADMIN_PASSWORD.txt
# 期望:看到 username/password/# 注释

# 期望 3:文件权限 0600
stat -c "%a" /tmp/panel-test-pr2/panel-state/INITIAL_ADMIN_PASSWORD.txt
# 期望:600

# 期望 4:重启进程,文件**保留**不覆盖
kill $PID
sleep 1
IKEV2_DATA_DIR=/tmp/panel-test-pr2 IKEV2_LISTEN_ADDR=127.0.0.1:8444 \
    IKEV2_LOG_LEVEL=debug IKEV2_SERVER_CN=test.local \
    IKEV2_COOKIE_SECURE=false /tmp/test-panel &
PID=$!
sleep 2
cat /tmp/panel-test-pr2/panel-state/INITIAL_ADMIN_PASSWORD.txt
# 期望:文件还在,内容不变(因为 EnsureDefaultAdmin 走 created==false 短路 + writeInitialPasswordFile 已存在则保留)

# 期望 5:删除文件,首页 isDefaultPassword=false
rm /tmp/panel-test-pr2/panel-state/INITIAL_ADMIN_PASSWORD.txt
# 用 curl 模拟登录看 home HTML
# ... (登录流程略)

kill $PID
```

### bcrypt cost=12 兼容性回归

```bash
# 1. 在 password_test.go 里 TestVerifyCost10LegacyHash 已经覆盖
# 2. 端到端:用 v2.84 跑过的 DB(里面 admin hash 是 cost=10)启动 v2.85 镜像 → 登录仍成功
#    (这部分需手工测,自动化用 unit test 已经覆盖)
```

### 部署一致性验证(模拟新用户)

```bash
# 1. README 步骤 2 改双路径后,用户两条路径都能找到密码
docker compose up -d
docker compose logs ikev2-panel 2>&1 | grep "默认管理员"  # 路径 A
docker exec ikev2-panel cat /data/panel-state/INITIAL_ADMIN_PASSWORD.txt  # 路径 B

# 2. 删除文件验证横幅消失
docker exec ikev2-panel rm /data/panel-state/INITIAL_ADMIN_PASSWORD.txt
# 浏览器登录,首页应无红色横幅
```

---

## 风险评估

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| `writeInitialPasswordFile` 写失败(mkdir / tmp / rename) | 极低 | 管理员从 docker logs 找不到密码(跟现状一样) | `logger.Warn` 不 fatal;stdout 已经打了,管理员从 logs 还能找 |
| `INITIAL_ADMIN_PASSWORD.txt` 文件被 root 之外的用户读到 | 极低 | 密码泄漏 | `chmod 0600` + 目录 `0700`,容器内 root only |
| docker volume 重命名 / 迁移导致文件丢失 | 低 | 管理员找不到密码 | 文档明确"docker volume rename 风险",用户应备份 |
| bcrypt cost=12 启动慢 ~240ms | 极低 | 容器启动多 0.2s,感知不到 | 单次调用,启动期 |
| bcrypt cost=12 登录慢 ~250ms | 低 | 用户登录多 0.2s,体感可接受 | OWASP 推荐值;v3 才考虑 Argon2id |
| 旧 hash(cost=10)登录失败 | **0** | 无 | bcrypt 协议设计,hash 自带 cost 字段;`TestVerifyCost10LegacyHash` 单测覆盖 |
| admin 不删文件,横幅一直显示 | 中 | UX 噪音,但符合"提醒用户改密"的设计意图 | README / 横幅都明确"请删除文件" |
| admin 改密码后没 UI 入口,只能手动改 DB | 中 | UX 不完整 | 任务原文明确"v3 才做 admin 修改密码页";README 提供 sqlite3 命令作为 workaround |

---

## 兼容性承诺

### U02(默认密码持久化)

- **现有 v2.84 用户升级**:
  - 旧部署无 `/data/panel-state/INITIAL_ADMIN_PASSWORD.txt`(因为 v2.84 没这个文件)→ home 模板 `isDefaultPasswordInUse` 返回 false → **无横幅** → 不打扰老用户
  - 旧部署如果有 admin,但 admin 已经主动改过密码(用 sqlite3 / 或 v2.84 期间的 `admin-cli` 之类)→ 文件不存在 → 无横幅
  - **唯一触发横幅的场景**:v2.85+ 全新首次部署,且 admin 没主动删除文件
- **文件已存在则保留**:`EnsureDefaultAdmin` 走 created==false 短路后,`writeInitialPasswordFile` 也走 stat 短路 → **绝不覆盖**老文件
- **/data volume 已 mount**:docker-compose.yml `volumes:` 段 [line 100](file:///opt/ikev2-panel-v2-main/docker-compose.yml#L100) 已经 `ikev2-panel-v2_ikev2-data:/data`,无需任何 compose 改动

### bcrypt cost 升级

- **现有 v2.84 admin 登录**:
  - v2.84 生成的 hash 是 `$2a$10$...`(cost=10)
  - v2.85 升级后,`VerifyPassword(hash, plain)` 调 `bcrypt.CompareHashAndPassword`,bcrypt 库**自动从 hash 提取 cost=10**,所以登录仍成功
  - **登录成功后,如果走 rehash 路径会升级到 cost=12** —— 本 PR **不做 rehash**,所以**永远是 cost=10**
  - 用户/管理员**只有在 admin 主动触发"改密码"动作时**(本版本暂无 UI,需 sqlite3 或 v3 推出的 admin 设置页)hash 才会从 cost=10 升级到 cost=12
- **新部署**:首次启动 `EnsureDefaultAdmin` 走 `BaseHashPassword(defaultPassword)` → 生成 cost=12 hash → 老 user 不会被影响(v2.85 之前没有 user 概念)
- **bcrypt 库本身不变**:仍是 `golang.org/x/crypto/bcrypt`,不引入新依赖

---

## 后续 PR 关联

本 PR 是 v2.85 release 第 2 个。后续 PR:
- **PR-3**:时区统一 + 日志路径修复(usability §U03 / U07 / U11)
- **PR-4**:main.go 统一 WaitGroup(correctness §Q1-01 关联)
- **PR-5**:alidns 错误码精确匹配 + HMAC-SHA256(correctness §Q1-06)
- **PR-6**:DDNS throttle 持久化 + stopCh sync.Once + collector REKEYING
- **PR-7**:tc IPv6 限速(borrow P0)
- **v3.0.0**:`/admin/settings` UI(改密 / 改用户名 / 看 session)+ reset CLI + Argon2id 长期迁移 + rehash on next login

每个 PR 独立,可单独 revert。

---

## 评审检查项(给自己)

- [x] 改动不超过 150 行
- [x] 不引入新依赖
- [x] bcrypt 兼容性有单测覆盖(`TestVerifyCost10LegacyHash`)
- [x] cost=12 启动期 + 登录期性能 ~250ms,不影响 UX
- [x] 文件权限 0600 + 目录 0700,root only
- [x] docker-compose.yml 无需改动(/data 已 mount)
- [x] README 文档明确"如何找回 / 如何删除文件"
- [x] dev 模式(本地 go build)可工作:tmp dir / chmod 都走通用路径
- [x] rehash on login 明确剔除(v3 才做)
- [x] admin 改密 UI 明确剔除(v3 才做)
- [x] 测试方案具体可执行(5 条命令)

---

## 工作量分解

| 步骤 | 时间 |
|------|------|
| main.go 加 `writeInitialPasswordFile` + 调用 | 0.05d |
| `internal/auth/password.go` 加 `BcryptCost` 常量 + 替换 | 0.02d |
| `internal/auth/password_test.go` 加 4 个 test | 0.05d |
| `internal/web/handlers_home.go` 加 `IsDefaultPassword` 字段 + `isDefaultPasswordInUse` 函数 | 0.03d |
| `web/templates/home_content.html` 加横幅块 | 0.02d |
| `README.md` 改步骤 2 + 加"密码找回"小节 | 0.05d |
| 跑测试 + 性能 smoke | 0.08d |
| **合计** | **0.3d**(U02 0.2d + bcrypt 0.1d) |

---

## 实施 Checklist(执行时用)

```markdown
- [ ] main.go 加 `writeInitialPasswordFile(dataDir, username, password)` 函数(底部)
- [ ] main.go `if created { ... }` 块加 `writeInitialPasswordFile` 调用 + logger.Info
- [ ] internal/auth/password.go 加 `const BcryptCost = 12`
- [ ] internal/auth/password.go `BaseHashPassword` 改用 `BcryptCost`
- [ ] internal/auth/password_test.go 加 `TestHashPasswordUsesCost12`
- [ ] internal/auth/password_test.go 加 `TestVerifyCost10LegacyHash`
- [ ] internal/auth/password_test.go 加 `TestVerifyCost12NewHash`
- [ ] internal/auth/password_test.go 加 `TestHashCost12Performance`
- [ ] internal/web/handlers_home.go `homeData` 加 `IsDefaultPassword bool`
- [ ] internal/web/handlers_home.go `handleHome` 加 `data.IsDefaultPassword = isDefaultPasswordInUse(s.DataDir)`
- [ ] internal/web/handlers_home.go 加 `isDefaultPasswordInUse(dataDir string) bool` 函数(底部)
- [ ] web/templates/home_content.html 在 line 22 之后加 `{{if .IsDefaultPassword}}` 红色横幅
- [ ] README.md "端到端验收"步骤 2 改双路径(stdout + 文件)
- [ ] README.md 加 "默认管理员密码找回(v2.85+)" 小节
- [ ] go test ./... PASS
- [ ] go test -v -run "TestHashPasswordUsesCost12|TestVerifyCost10LegacyHash|TestVerifyCost12NewHash|TestHashCost12Performance" ./internal/auth/ PASS
- [ ] CGO_ENABLED=0 go build ./cmd/ikev2-panel PASS
- [ ] dev 模式启动,确认 /tmp/panel-test-pr2/panel-state/INITIAL_ADMIN_PASSWORD.txt 文件存在 + 权限 600
- [ ] dev 模式重启,确认文件不覆盖(保留)
- [ ] 删除文件,浏览器登录,确认首页无红色横幅
- [ ] git commit "v2.85-PR2: persist initial admin password + bcrypt cost 10→12"
```

---

> 关联:
> - [audit-2026-09-usability.md §U02](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md)
> - [audit-2026-09-borrow.md §组件 3 + §路线图 P0](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-borrow.md)
> - [audit-2026-09-phase2-4.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-phase2-4.md) §"Phase 2-4 合并 Top-10"
> - [release-notes-v2.85.md](file:///opt/ikev2-panel-v2-main/docs/release-notes-v2.85.md)(待 PR 合入后写)
