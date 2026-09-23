# 数据库 + 存储层审计报告 — 2026-09

> 维度:**正确性 + 借鉴优化**(审计框架第 3、5 阶段,聚焦存储层)
> 审计员:项目组自查
> 范围:`/opt/ikev2-panel-v2-main/` 持久化相关代码 + Go 生态最佳实践对标
> 时间:2026-09-19
> 状态:**草案,等待评审**

---

## 范围

| 路径 | 类别 |
|------|------|
| [file:///opt/ikev2-panel-v2-main/internal/store/store.go](../internal/store/store.go) | SQLite 打开 / DSN / migrate |
| [file:///opt/ikev2-panel-v2-main/internal/store/types.go](../internal/store/types.go) | 数据模型 |
| [file:///opt/ikev2-panel-v2-main/internal/store/users.go](../internal/store/users.go) | users 表 CRUD |
| [file:///opt/ikev2-panel-v2-main/internal/store/sessions.go](../internal/store/sessions.go) | sessions 表 CRUD |
| [file:///opt/ikev2-panel-v2-main/internal/store/admins.go](../internal/store/admins.go) | admins 表 CRUD |
| [file:///opt/ikev2-panel-v2-main/internal/store/store_test.go](../internal/store/store_test.go) | 集成测试覆盖 |
| [file:///opt/ikev2-panel-v2-main/internal/panelstate/state.go](../internal/panelstate/state.go) | JSON 凭证存储 |
| [file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go](../cmd/ikev2-panel/main.go) | DB 初始化 / 启动顺序 |
| [file:///opt/ikev2-panel-v2-main/internal/expiry/expiry.go](../internal/expiry/expiry.go) | 过期 goroutine |
| [file:///opt/ikev2-panel-v2-main/internal/limit/collector.go](../internal/limit/collector.go) | 流量采集 goroutine |
| [file:///opt/ikev2-panel-v2-main/internal/web/handlers_users.go](../internal/web/handlers_users.go) | 用户 CRUD 编排 |
| [file:///opt/ikev2-panel-v2-main/internal/auth/password.go](../internal/auth/password.go) | bcrypt / session id 生成 |

## 工具 / 参考

- [SQLite 文档](https://www.sqlite.org/docs.html)
- [SQLite Security](https://www.sqlite.org/security.html)
- [SQLite Encryption Extension (SEE)](https://www.sqlite.org/see/)
- [SQLite WAL Mode](https://www.sqlite.org/wal.html)
- [SQLite Query Language — Busy Timeout](https://www.sqlite.org/pragma.html#pragma_busy_timeout)
- [modernc.org/sqlite (pure-Go 驱动)](https://gitlab.com/cznic/sqlite)
- [SQLCipher](https://www.zetetic.net/sqlcipher/)
- [golang-migrate/migrate](https://github.com/golang-migrate/migrate)
- [pressly/goose](https://github.com/pressly/goose)
- [ent (Facebook ORM)](https://github.com/ent/ent)
- [dex schema 设计参考](https://github.com/dexidp/dex)
- [authentik schema 参考](https://github.com/goauthentik/authentik)
- [OWASP Password Storage Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)
- [Atomic file writes (Go)](https://pkg.go.dev/io/fs#Rename)
- [bcrypt cost (NIST SP 800-63B)](https://pages.nist.gov/800-63-3/sp800-63b.html)

---

## 关键观察 / 概览

| 子系统 | 实现 | 评价 |
|--------|------|------|
| 驱动 | modernc.org/sqlite 1.59.0 (pure-Go) | ✓ 避免 CGO,Go 生态首选 |
| DSN 模式 | `_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)` | ✓ 三件套齐全,符合 [SQLite WAL 最佳实践](https://www.sqlite.org/wal.html) |
| 迁移工具 | 手写 `migrate()` 函数 + 硬编码 stmts 切片 | 🟡 没有版本表,无迁移历史(对比 [golang-migrate](https://github.com/golang-migrate/migrate)) |
| 用户密码 | `users.password` **明文 TEXT 列** | 🔴 已记录为 ISSUE-S05,本报告延续 |
| 管理员密码 | `admins.password_hash` bcrypt (cost 10) | ✓ 符合 [OWASP 推荐](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html#bcrypt) |
| Session | `id TEXT PK` + `csrf_token` + `expires_at` + `user_agent`,只有 `idx_sessions_expires` | 🟡 缺 CSRF rotation / last_seen_at / 索引太多 |
| SQL 注入 | 全部 `?` 占位符 | ✓ |
| 并发 | `IncrementUserBytes` 用 SQL `SET col = col + ?` 避免 RMW 竞争 | ✓ 设计正确 |
| WAL 锁 | `busy_timeout(5000)` + WAL | ✓ 符合 SQLite 建议 |
| 备份脚本 | **无 backup.sh**(design.md L377 列出但实际不存在,见 [release-notes-v2.84.md L158](../release-notes-v2.84.md)) | 🔴 仅有证书备份,DB 无备份 |
| DB 加密 | 无 | 🔴 设计妥协,见 ISSUE-S05 |
| panelstate JSON | atomic rename + 0600 权限 + 内存缓存 + RWMutex | ✓ 实现规范 |

---

## 发现汇总

按严重度分组:

| 严重度 | 数量 | 关键问题 |
|--------|------|----------|
| **HIGH** | 4 | DB 无备份脚本 / 无 DB 加密(S05)/ 无迁移版本表 / 设计文档承诺未兑现(backup.sh) |
| **MED** | 6 | sessions 缺 last_seen_at / busy_timeout 5s 不够长 / 无 `_txlock=immediate` / 无连接池调优 / panelstate 与 sqlite 不一致风险 / IndexUsersEnabled 索引冗余 |
| **LOW** | 6 | `user_agent` 无长度上限 / username 无 CHECK 约束 / 删除用户不级联清 session / `note` 无长度限制 / 缺 `pragma_synchronous` 显式设置 / 缺 schema 版本探针 |

---

## [SEVERITY-HIGH] ISSUE

### ISSUE-D01:DB 无备份 / 恢复脚本,设计文档承诺未兑现

- **位置**:
  - [internal/store/store.go:22](../internal/store/store.go#L22) `dbPath = filepath.Join(dataDir, "panel.db")`
  - [docs/design.md:377](../docs/design.md#L377) 列出"一个 `backup.sh` 脚本(cron 跑)"
  - [docs/architecture.md:86](../docs/architecture.md#L86) 目录树里也有 `backup.sh`
  - [docs/release-notes-v2.84.md:158](../release-notes-v2.84.md#L158) **明确承认** "design §2.1 列入但实际不需要 → P2(设计文档收口)"
- **问题**:
  - 没有任何脚本/工具/Cron 备份 `/data/panel.db` + `/data/panel-state/` + `/data/le/`
  - 用户数据(users / sessions / admins / 配置 / 阿里云凭证 / LE 私钥) **全裸存,无冷备**
  - release-notes 自承"不需要" —— 但 release-notes 跟 design.md 自相矛盾;若用户按 design 部署,expectation 是有 backup
  - SQLite 用 WAL,即使"在线 cp"也不行(可能有未 checkpoint 的 WAL 文件 `panel.db-wal`)。需要 `.backup` 命令([SQLite Backup API](https://www.sqlite.org/c3ref/backup_finish.html))
- **影响**:
  - 容器崩溃 / 磁盘损坏 → **所有用户 + 凭证丢失**
  - 误删 admin 记录 → 没有"昨天"版本可回滚
  - LE 私钥丢失 → acme.sh rate-limit 要等 1 周才能重新签发
- **参考**:
  - [SQLite Backup API](https://www.sqlite.org/c3ref/backup_finish.html)
  - [SQLite WAL checkpoint](https://www.sqlite.org/wal.html#checkpointing)
  - [sqlite3 .backup CLI 命令](https://www.sqlite.org/cli.html#backup)
  - 标杆:[restic](https://github.com/restic/restic) / [borgbackup](https://www.borgbackup.org/)
- **修复方案**:
  1. 在 `cmd/ikev2-panel` 里加 `mode=backup` 子命令(类似 [pressly/goose 风格](https://github.com/pressly/goose)):
     ```go
     // 调 SQLite Online Backup API:用现代 c 驱动在 Go 里的等价物
     // modernc.org/sqlite 提供 Backup API 绑定
     dst, _ := os.Create(backupPath)
     defer dst.Close()
     bk, _ := sqlite3.NewBackup("file:"+dbPath, dst)
     bk.Step(-1) // copy all pages
     bk.Finish()
     ```
  2. 入口脚本加 cron:`0 4 * * * /etc/ikev2-panel/scripts/backup-db.sh`,把 `panel.db` + `/data/panel-state/` + `/data/le/privkey.pem` 打包到 `/data/backups/`,**保留 7 天滚动**
  3. **加密备份**:`tar -cz ... | gpg --symmetric --cipher-algo AES256 > backup.tgz.gpg`(参考 [HashiCorp Vault Transit](https://www.vaultproject.io/docs/secrets/transit))
  4. **演练恢复**:`docker run --rm -v backup:/backup ikev2-panel:latest /etc/ikev2-panel/scripts/restore.sh /backup/2026-09-19` → 必须每周 CI 跑一次
- **工作量**:1.5d(写脚本 + 测试 + 文档 + CI 演练)
- **优先级**:**本周修**(跟 S05 一起做)

---

### ISSUE-D02:DB 静态加密缺失,文件落盘明文

- **位置**:
  - [internal/store/store.go:11](../internal/store/store.go#L11) `_ "modernc.org/sqlite"`
  - [internal/store/store.go:29](../internal/store/store.go#L29) DSN 无 `_pragma=key(...)`
- **问题**:
  - 没有用 [SQLite Encryption Extension (SEE)](https://www.sqlite.org/see/)、[SQLCipher](https://www.zetetic.net/sqlcipher/) 或 [modernc.org/sqlite 加密扩展](https://gitlab.com/cznic/sqlite/-/issues/11)
  - 物理偷盘(物理访问 / 备份泄漏 / 误 chmod 0444)→ 直接读到所有 user 明文 VPN 密码 + admin bcrypt hash + session token
  - 跟 [ISSUE-S05(密码明文)](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md#issue-s05vpn-用户密码明文存-db泄露则全裸) 叠加 = 复合风险
- **影响**:
  - 主机旁路(LXC 邻居 / 物理访问 / 容器逃逸后)→ 全裸
  - 备份拷贝泄漏 → 全裸
  - cloud-init snapshot 共享 → 全裸
- **参考**:
  - [SQLite Encryption Extension](https://www.sqlite.org/see/)(SQLite 官方,商业 license)
  - [SQLCipher](https://www.zetetic.net/sqlcipher/)(开源,BSD-3)
  - [modernc.org/sqlite 加密 fork(社区)](https://gitlab.com/cznic/sqlite/-/issues/11)
  - [dex — storage encryption 设计](https://github.com/dexidp/dex/blob/master/docs/storage.md)
  - [authentik — crypto key management](https://goauthentik.io/docs/architecture/encryption)
- **修复方案**:
  1. **短方案(0.5d)**:DB 文件 chmod 0600(目前看 entrypoint 没显式 chmod,沿用 0644)+ 在 release-notes 明确"DB 文件应 chmod 0600"
  2. **中方案(1.0d)**:换驱动 → [github.com/mutecomm/go-sqlcipher/v4](https://github.com/mutecomm/go-sqlcipher/v4) 或 [modernc.org/sqlite 加密 fork](https://gitlab.com/cznic/sqlite),DSN 加 `_pragma=key("...")`
     - key 生成:`/data/panel-state/db.key` (0600),启动时读 → 注入 DSN
     - entrypoint 加 `chmod 0600 /data/panel-state/db.key`
  3. **长方案(2.0d)**:封装 `store.WithEncryptionKey(key []byte)` 选项 + KMS 集成(用户指定本地 KMS,例如 Vault Transit)
- **工作量**:0.5d(短)/ 1.0d(中)/ 2.0d(长)
- **优先级**:**本周修短方案 + 中方案**(跟 S05 + D01 一起做)

---

### ISSUE-D03:无 migration 版本表,schema 演进不可控

- **位置**:
  - [internal/store/store.go:56-98](../internal/store/store.go#L56-L98) `migrate()` 函数
  - 注释 L54-55 说"未来 schema 变更加 002_xxx、003_xxx 即可" —— **但没有实现机制**
- **问题**:
  - 没有 schema_migrations 表 → 不知道当前运行的 schema 是哪个版本
  - 没有事务保护每个 migration → 中途失败 → DB 半迁移
  - 没有 `up` / `down` 对偶 → 不能 rollback
  - 大量外部 SQL 串拼一个字符串切片 → 100 个 migration 时不可维护
  - 对比标杆:[pressly/goose](https://github.com/pressly/goose) / [golang-migrate/migrate](https://github.com/golang-migrate/migrate) 都内置版本表 + transactional + 文件命名版本号
- **影响**:
  - 当前只有一个 001_init,但只要再加一个 migration(加字段 / 加索引)→ 必须自己写"已迁移则跳过"逻辑
  - 测试场景:production 跑 002 → 本地测试 db 是新表 → migrate() 跑 002 → DDL 报错
  - 同事拉新代码 → 如果本地 db 是老的,启动时 DDL 失败 → 直接 panic
- **参考**:
  - [pressly/goose — schema_migrations 表](https://github.com/pressly/goose#goose-basics)
  - [golang-migrate/migrate — schema_migrations](https://github.com/golang-migrate/migrate/tree/master/source/file)
  - [dex — database migration](https://github.com/dexidp/dex/blob/master/storage/storage.go)
  - [SQLite ALTER TABLE — column rename / drop limitations](https://www.sqlite.org/lang_altertable.html)
- **修复方案**(二选一):
  1. **轻量(0.5d)**:在 `migrate()` 头部加 schema version 表:
     ```sql
     CREATE TABLE IF NOT EXISTS schema_version (
         version INTEGER PRIMARY KEY,
         applied_at INTEGER NOT NULL
     );
     ```
     + `migrate()` 改成 `for _, stmt := range migrations002 { tx, _ := db.Begin(); ... tx.Exec(stmt); tx.Commit(); recordVersion(i+1) }`
  2. **引入库(0.5d,推荐)**:加 `github.com/pressly/goose/v3` 依赖,把 stmts 拆成 `internal/store/migrations/002_add_last_seen.sql` 等独立文件,goose 自动事务 + 版本管理
- **工作量**:0.5d
- **优先级**:**P2**(等第一次需要 schema 演进时立即上,不要等到第 3 个 migration 才后悔)

---

### ISSUE-D04:设计文档 vs 实际代码不一致(`backup.sh` 永远缺位)

- **位置**:
  - [docs/design.md:377](../docs/design.md#L377) `一个 backup.sh 脚本(cron 跑),tar 整个数据目录`
  - [docs/architecture.md:86](../docs/architecture.md#L86) 目录树也含 `backup.sh`
  - 实际仓库:**无 backup.sh**(见 [release-notes-v2.84.md:158](../release-notes-v2.84.md#L158) `backup.sh / 多 stage / 登录限速文档收口 | 低 | ⏸️ design 标记"已确认不留"`)
- **问题**:
  - release-notes 标记"已确认不留",但 design.md / architecture.md 仍写着有 backup.sh
  - 新人按 design 部署 → expectation 是有 cron backup → 实际没 → 失职后背疼
- **影响**:
  - 文档失信(同步 ISSUE)
  - 跟 ISSUE-D01 直接叠加:因为没人补 backup.sh,数据实际上无备份
- **参考**:
  - [docs/audit-framework.md §借鉴工作流](../audit-framework.md)
- **修复方案**:
  1. **删文档**:从 design.md L377 删掉"一个 backup.sh 脚本",从 architecture.md L86 删 `backup.sh`
  2. **或兑现**:写一个 backup.sh(见 ISSUE-D01 修复方案 1)
  3. release-notes 加 `v2.85 backup.sh 已实现 / 已删除` 显式决策记录
- **工作量**:0.05d(删)/ 1.5d(写)
- **优先级**:**本周修**(跟 D01 一起)

---

## [SEVERITY-MED] ISSUE

### ISSUE-D05:`sessions` 表缺 `last_seen_at` / `ip` 字段,审计能力弱

- **位置**:
  - [internal/store/store.go:75-81](../internal/store/store.go#L75-L81) `sessions` 表 DDL
  - [internal/store/types.go:32-38](../internal/store/types.go#L32-L38) `Session` struct
- **问题**:
  - 只有 `id / csrf_token / created_at / expires_at / user_agent`,**没有**:
    - `last_seen_at`:无法检测"过期前最后一次活跃" → 不能做 idle timeout
    - `ip`:无法审计"哪个 IP 在用 admin 凭证" → security audit 弱
    - `user_id`:无法关联到 admins.id → 想做"列出某 admin 的所有登录设备"做不到
  - sessions 表跟 admins 表无 FK / 关联,纯 id 字符串 — 没办法做"admin 用户主动撤销所有 session"功能
  - 对比:[authentik session model](https://github.com/goauthentik/authentik/blob/main/authentik/core/models.py) 含 `last_ip / last_user_agent / expires`
- **影响**:
  - 被盗 session 无法定位攻击 IP
  - admin 改密后,旧 session 不会被自动失效(只有 expire_at 删 session,但 session id 跟 admin 无关联)
- **参考**:
  - [authentik — Session model](https://github.com/goauthentik/authentik/blob/main/authentik/core/models.py#L126)
  - [dex — Session storage](https://github.com/dexidp/dex/blob/master/storage/storage.go)
  - [OWASP Session Management Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)
- **修复方案**:
  1. DDL 加列:
     ```sql
     ALTER TABLE sessions ADD COLUMN user_id INTEGER NOT NULL DEFAULT 0;
     -- admin_id?单 admin 时永远 = 1,但保留扩展性
     ALTER TABLE sessions ADD COLUMN last_seen_at INTEGER NOT NULL DEFAULT 0;
     ALTER TABLE sessions ADD COLUMN ip TEXT NOT NULL DEFAULT '';
     ```
     配套 `idx_sessions_user_id ON sessions(user_id)`, `idx_sessions_ip ON sessions(ip)`
  2. `internal/web/server.go` L211 `GetSessionByID` 后调一次 `UPDATE sessions SET last_seen_at = ? WHERE id = ?`(或在 GetSessionByID 里 `RETURNING`)
  3. 面板 `/admin/sessions` 页面列出当前 admin 的活跃 session,提供"撤销"按钮
- **工作量**:1.0d(schema + handler + 模板)
- **优先级**:**P1**(跟 S01 登录限速联动,能定位攻击 IP)

---

### ISSUE-D06:`_txlock=immediate` 缺失,长事务可能写竞争失败

- **位置**:[internal/store/store.go:29](../internal/store/store.go#L29)
- **问题**:
  - DSN 只有 `foreign_keys(1) / busy_timeout(5000) / journal_mode(WAL)`,**没有** `_txlock=immediate`
  - SQLite 默认 `BEGIN` = `BEGIN DEFERRED`,**第一次读升级为读锁,第一次写升级为写锁**,升级过程可能因 busy 直接返回 `SQLITE_BUSY`
  - `busy_timeout(5000)` 5s 在流量高峰(collector + admin UI 并发)可能不够
  - 跟 modernc.org/sqlite 的官方建议对比:见 [sqlite locking](https://www.sqlite.org/lockingv3.html)
- **影响**:
  - 长事务(若未来加)容易 `SQLITE_BUSY`
  - 多 goroutine 并发 IncrementUserBytes + admin UI 写 user → 偶发 SQLITE_BUSY
- **参考**:
  - [SQLite Locking](https://www.sqlite.org/lockingv3.html)
  - [SQLite Transaction](https://www.sqlite.org/lang_transaction.html)
  - [modernc.org/sqlite — DSN options](https://pkg.go.dev/modernc.org/sqlite#Driver.Open)
- **修复方案**:
  1. DSN 加 `_pragma=txlock(2)`(= IMMEDIATE,值 1=DEFERRED,2=IMMEDIATE,3=EXCLUSIVE)
  2. 或写事务时显式 `BEGIN IMMEDIATE`:
     ```go
     tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
     ```
     (Go 的 LevelSerializable 在 SQLite 里就是 IMMEDIATE 行为)
  3. busy_timeout 可考虑上调到 10000ms(`_pragma=busy_timeout(10000)`)
- **工作量**:0.05d
- **优先级**:**本周修**

---

### ISSUE-D07:`sql.DB` 无连接池调优,默认 `SetMaxOpenConns=0`(无限)

- **位置**:[internal/store/store.go:31-34](../internal/store/store.go#L31-L34) `sql.Open` 后无 `db.SetMaxOpenConns(...)`
- **问题**:
  - Go `database/sql` 默认 `MaxOpenConns=0` 表示无上限 → 高并发时可能打开成百上千连接
  - modernc.org/sqlite 是单文件 SQLite,**SQLite 推荐同一进程最多 N 个连接(N ≤ writer 数,WAL 下 writer=1 + reader=多)**,见 [SQLite Limits](https://www.sqlite.org/limits.html)
  - WAL 模式下,多 reader 并行是安全的,但 writer 永远串行;开太多连接**会浪费内存** + **写竞争加剧**
- **影响**:
  - 内存占用上涨(每 conn 几 KB,~1000 个 conn ≈ 几 MB-几十 MB)
  - 写延迟升高(connection thrashing)
- **参考**:
  - [Go database/sql connection pool](https://go.dev/doc/database/manage-connections)
  - [SQLite WAL concurrent readers](https://www.sqlite.org/wal.html#concurrency)
- **修复方案**:
  ```go
  db.SetMaxOpenConns(10)       // 1 writer + 9 reader buffer
  db.SetMaxIdleConns(5)
  db.SetConnMaxLifetime(0)     // SQLite 无网络,不需要
  db.SetConnMaxIdleTime(5 * time.Minute)
  ```
- **工作量**:0.05d
- **优先级**:**本周修**(零风险)

---

### ISSUE-D08:`panelstate` JSON 与 `users` / `admins` DB 一致性风险

- **位置**:
  - [internal/panelstate/state.go:30](../internal/panelstate/state.go#L30) `Dir = "/data/panel-state"`
  - [internal/panelstate/state.go:55-204](../internal/panelstate/state.go#L55-L204) `Store` 内存缓存 + atomic rename
  - [cmd/ikev2-panel/main.go:309-336](../cmd/ikev2-panel/main.go#L309-L336) `panelstate.NewStore()` + `LoadAliyun()` + DDNS getter
- **问题**:
  - `panelstate.Store` 是**纯内存 cache + JSON file**,跟 sqlite DB **无任何事务关联**
  - 应用层"写 panelstate → 写 sqlite users/admins"是**两阶段提交**,中间进程崩溃 → 半成品
  - 当前只有 aliyun creds 用 panelstate(没跟 DB 共享数据),所以**实际风险低**
  - **但**:如果未来想加"用户自定义 settings"(比如 admin UI 上配 LE STAGING vs PRODUCTION)→ 可能搞"写 DB + 写 panelstate" — 风险立即出现
- **影响**:
  - 当前实现下:**无实际影响**
  - 未来扩展时:**数据不一致**风险
- **参考**:
  - [Two-phase commit pattern](https://en.wikipedia.org/wiki/Two-phase_commit_protocol)
  - [Vault — storage backend consistency](https://www.vaultproject.io/docs/internals/high-availability)
- **修复方案**:
  1. **当前**:加注释明确 panelstate 跟 sqlite 是独立 store,要求调用方自己保证一致性
  2. **未来**:把所有"运行时凭证"统一进 sqlite(建 `secrets` 表,value 列加密),panelstate 退役
- **工作量**:0.05d(注释)/ 2.0d(未来统一进 sqlite)
- **优先级**:**LOW** — 留 P3 观察

---

### ISSUE-D09:索引冗余 / 缺常用索引

- **位置**:
  - [internal/store/store.go:72-82](../internal/store/store.go#L72-L82) 仅 3 个索引
- **问题**:
  - `idx_users_enabled ON users(enabled)` — enabled 只有 0/1,选择性极差;ListUsers 不用 enabled,GetExpiredUsers 不用 enabled,几乎用不上
  - `idx_users_expires ON users(expires_at)` — ✓ `GetExpiredUsers` 用得上
  - `idx_sessions_expires ON sessions(expires_at)` — ✓ `DeleteExpiredSessions` 用得上
  - **缺**:`users.username` 已有 UNIQUE 约束(自动索引),OK
  - **缺**:`admins.username` 已有 UNIQUE,OK
  - **缺**:`users.id` 主键 OK
  - **缺**:`sessions.id` 主键 OK
- **影响**:
  - `idx_users_enabled` 浪费写入开销(每次 INSERT / UPDATE 都要更新)
  - ListUsers 表扫描就 100% 用得上主键顺序,**根本不需要 idx_users_enabled**
- **参考**:
  - [SQLite Query Optimizer Overview](https://www.sqlite.org/queryplanner.html)
  - [SQLite Indexes on Expressions](https://www.sqlite.org/expridx.html)
- **修复方案**:
  1. 删 `idx_users_enabled`(注释写明选择性差 + 不用)
  2. 加 `idx_users_username_password ON users(username, password)` — 不需要,username 已有 UNIQUE
  3. 真要加速 enabled 过滤:用 partial index `CREATE INDEX idx_users_enabled_enabled ON users(id) WHERE enabled = 1`,但场景不明确,不推荐
- **工作量**:0.05d
- **优先级**:**顺手改**

---

### ISSUE-D10:启动时缺 schema / DB 完整性自检

- **位置**:[cmd/ikev2-panel/main.go:57-62](../cmd/ikev2-panel/main.go#L57-L62)
- **问题**:
  - `store.Open` 只 ping,没跑 `PRAGMA integrity_check` / `PRAGMA quick_check`
  - 磁盘损坏 / 上次异常关机 → DB 损坏但应用照常启动 → 用户能登录但数据奇怪
  - `PRAGMA foreign_key_check` 也没跑(虽然 v2 没用外键,但留着能抓意外)
- **影响**:
  - 启动时不知道 DB 是好的;运维 / 监控也无从知道
- **参考**:
  - [SQLite PRAGMA integrity_check](https://www.sqlite.org/pragmatab.html#integrityck)
  - [SQLite PRAGMA quick_check](https://www.sqlite.org/pragma.html#pragma_quick_check)
  - [SQLite Application Defined Page Cache Failure](https://www.sqlite.org/c3ref/pcache_methods2.html)
- **修复方案**:
  ```go
  // store.Open 加:
  if err := quickCheck(ctx, db); err != nil {
      return nil, fmt.Errorf("DB integrity check failed: %w", err)
  }
  ```
  ```go
  func quickCheck(ctx context.Context, db *sql.DB) error {
      var ok string
      if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&ok); err != nil {
          return err
      }
      if ok != "ok" {
          return fmt.Errorf("quick_check returned %q", ok)
      }
      return nil
  }
  ```
- **工作量**:0.1d
- **优先级**:**本周修**(跟 D01 备份一起做)

---

## [SEVERITY-LOW] ISSUE

### ISSUE-D11:`sessions.user_agent` 无长度上限,DoS 风险

- **位置**:
  - [internal/store/store.go:80](../internal/store/store.go#L80) `user_agent TEXT NOT NULL DEFAULT ''`
- **问题**:
  - TEXT 类型 SQLite 没有原生长度上限 → 攻击者可塞 1MB user_agent
  - DB 体积涨大,索引效率下降
- **修复**:handler 层加 `len(userAgent) > 512` 截断;或在 DDL 加 `CHECK (length(user_agent) <= 512)`
- **工作量**:0.05d

---

### ISSUE-D12:`users.username` 无 CHECK 约束 / 长度上限

- **位置**:[internal/store/store.go:60](../internal/store/store.go#L60)
- **问题**:
  - `username TEXT NOT NULL UNIQUE` — UNIQUE 是,但空字符串 / 超长字符串 / 特殊字符没拦
  - handler 层在 [handlers_users.go:140](../internal/web/handlers_users.go#L140) 也没拦长度
- **修复**:`CHECK (length(username) BETWEEN 1 AND 64 AND username NOT LIKE '% %')`
- **工作量**:0.05d(跟 [audit-2026-09-security.md ISSUE-S14](../docs/audit-2026-09-security.md#issue-s14用户名--备注输入长度未限制) 一起)

---

### ISSUE-D13:删除用户不级联清 session / 配置

- **位置**:
  - [internal/store/users.go:141-150](../internal/store/users.go#L141-L150) `DeleteUser` 只 `DELETE FROM users WHERE id = ?`
  - [internal/web/handlers_users.go:344-380](../internal/web/handlers_users.go#L344-L380) 编排删除顺序:DB → swanctl → limit → terminate,**没碰 sessions**
- **问题**:
  - admin 删完用户后,该用户**无法登录面板**(用户都是 admin),所以无实际 session 风险(因为 sessions 只给 admin 用,user 没 session)
  - 但:如果未来支持"普通用户登录看自己流量",就出 bug
  - 对比 [dex cascade](https://github.com/dexidp/dex/blob/master/storage/storage.go)(每次关联都有 cascade)
- **修复**:加 `DELETE FROM sessions WHERE user_id = ?` (schema 改 D05)
- **工作量**:0.05d(等 D05 改完一起)

---

### ISSUE-D14:`users.note` / `password` 无长度上限

- **位置**:[internal/store/store.go:61-63](../internal/store/store.go#L61-L63)
- **问题**:
  - `note TEXT NOT NULL DEFAULT ''` 无长度上限
  - `password TEXT NOT NULL` 无长度上限(虽然 [GeneratePassword](../internal/auth/password.go#L26) 限制 [6, 64])
- **修复**:handler 层加 `len(note) > 256 → 截断`;DDL 加 `CHECK (length(note) <= 256)`
- **工作量**:0.05d

---

### ISSUE-D15:缺 `pragma_synchronous` 显式设置

- **位置**:[internal/store/store.go:29](../internal/store/store.go#L29) DSN
- **问题**:
  - SQLite 默认 `synchronous=FULL`,WAL 下通常是 OK 但慢
  - 对于应用数据(users / sessions),丢失 1 秒数据一般可接受 → `synchronous=NORMAL` 是 SQLite WAL 推荐值
  - 当前 implicit FULL → 写延迟升高一点(commit 时 fsync)
- **参考**:
  - [SQLite PRAGMA synchronous](https://www.sqlite.org/pragma.html#pragma_synchronous)
- **修复**:DSN 加 `_pragma=synchronous(NORMAL)`
- **工作量**:0.05d

---

### ISSUE-D16:测试未包含 store 边界场景

- **位置**:[internal/store/store_test.go](../internal/store/store_test.go)
- **问题**:
  - 没测:大 users 列表(10k+) ListUsers 性能 / WAL checkpoint 行为 / 外键启用 / PRAGMA 校验
  - 没测:IncrementUserBytes 并发安全(`go test -race` 没看到任何并发 test)
- **修复**:
  - 加 `TestIncrementUserBytes_Concurrent`(N 个 goroutine 并发累加同一用户,verify 总和正确)
  - 加 `TestListUsers_LargeSet`(插入 10k 行,测耗时 < 100ms)
  - 加 `TestOpen_PRAGMAs`(验证 foreign_keys / journal_mode / busy_timeout / synchronous 实际生效)
- **工作量**:0.5d
- **优先级**:**P2**(跟 Q1 并发审计一起)

---

## 借鉴参考(供方案决策用)

### 1. migration 工具

- **[pressly/goose](https://github.com/pressly/goose)**:Go 生态最流行,文件命名版本号(`00001_users.sql`),自动事务,支持 up/down,内置 `goose up` / `goose down` / `goose status`
- **[golang-migrate/migrate](https://github.com/golang-migrate/migrate)**:支持多 source(embed file / github / s3),多 database driver,跟 goose 功能重叠
- **[ariga/atlas](https://github.com/ariga/atlas)**:schema-as-code,适合复杂场景
- **轻量手写**:`schema_version` 表 + 单文件 stmts(我们当前已近似,只差版本表)

### 2. Schema 设计参考

- **[dex schema](https://github.com/dexidp/dex/blob/master/storage/storage.go)**:
  - 通用设计:每个 entity 一个表,主键 `id TEXT`(不是 INTEGER,便于分布式)
  - session 表带 `connector_id / subject / expires_at / last_seen` (我们可参考 D05)
- **[authentik schema](https://github.com/goauthentik/authentik/blob/main/authentik/core/models.py)**:
  - Django ORM 定义,但有 `Session.last_ip / last_user_agent / expires` (跟 D05 一致)
- **[authentik crypto](https://goauthentik.io/docs/architecture/encryption)**:
  - 用 Fernet 对称加密存敏感字段(我们用 AES-GCM 也行)
- **小型项目标杆**:
  - [Caddy admin storage](https://github.com/caddyserver/caddy/tree/master/caddyconfig)
  - [traefik pilot storage](https://doc.traefik.io/traefik/pilot/)

### 3. DB 加密

- **[SQLCipher](https://www.zetetic.net/sqlcipher/)**:开源,BGP 开源协议可用
- **[modernc.org/sqlite 加密 fork](https://gitlab.com/cznic/sqlite/-/issues/11)**:社区 patch,无 CGO
- **[HashiCorp Vault Transit](https://www.vaultproject.io/docs/secrets/transit)**:密钥管理外包,适合运维级

### 4. 备份策略

- **[restic](https://github.com/restic/restic)**:dedup + 加密 + 滚动 7 天
- **[borgbackup](https://www.borgbackup.org/)**:类似 restic
- **[sqlite3 .backup CLI](https://www.sqlite.org/cli.html#backup)**:SQLite 官方 API,无需停服

### 5. SQLite 性能 / 最佳实践

- [SQLite WAL](https://www.sqlite.org/wal.html):并发读 + 单写
- [SQLite Application-Defined Page Cache](https://www.sqlite.org/c3ref/pcache_methods2.html)
- [SQLite Recommended Settings](https://www.sqlite.org/pragma.html#pragma_application_id)
- [SQLite Tuning](https://wiki.postgresql.org/wiki/Tuning_Your_SQLite_Database_For_Better_Performance)

---

## 跟 [audit-2026-09-security.md](../docs/audit-2026-09-security.md) 重复的 issue

| 本报告 ID | 重复项 | 关联安全报告 ID |
|----------|--------|----------------|
| **D02 DB 静态加密** | 是 — 跟 S05 同源,都在"密码 + DB 文件 = 泄露"风险圈 | **ISSUE-S05 方案 C = D02 中方案**(加密驱动换 SQLCipher/SEE) |

**特别说明**:
- **D02 跟 S05 重复点**:两者都关心"DB 文件被偷走"这一场景。S05 的视角是"密码列明文"导致被偷就全裸;D02 的视角是"整个 DB 没有任何加密"导致被偷就全裸。
- **修复方案可合并**:S05 方案 D(chmod 0300)+ D02 短方案(chmod 0600 + 文档)= 同一个 PR
- **S05 方案 C(DB 加密)** = **D02 中方案**(SQLCipher/SEE)= 同一个 PR
- **不重复但相关**:
  - S01(登录 rate limit)= D05(sessions 加 last_seen_at + ip)联动 — 修复 S01 同时也要修 D05 才能定位攻击 IP
  - S06(Session ID 强度)— 当前用 `base64.RawURLEncoding.EncodeToString(crypto/rand 32 bytes)` = 256-bit entropy,**够强**,无重复 issue
  - S07(Cookie Secure)— 跟 panelstate JSON 文件权限(本报告 D08)同源但独立修复点

---

## 建议优先级

### 本周(W1,跟安全报告合并)

| 优先级 | ISSUE | 工作量 | 备注 |
|--------|-------|--------|------|
| 🔴 | **D01 DB 备份脚本** | 1.5d | 跟 S05 合并 |
| 🔴 | **D02 DB 加密(短+中)** | 1.0d | 跟 S05 合并 |
| 🔴 | **D04 设计文档收口** | 0.05d | 删/写 backup.sh |
| 🟡 | **D06 `_txlock=immediate` + busy_timeout 调高** | 0.05d | 一行 DSN 改 |
| 🟡 | **D07 连接池调优** | 0.05d | 4 行 SetXxx |
| 🟡 | **D10 启动完整性自检** | 0.1d | |
| ⚪ | **D09 删 `idx_users_enabled`** | 0.05d | |
| ⚪ | **D15 `pragma_synchronous=NORMAL`** | 0.05d | |
| **小计** | | **2.85d** | |

### 下周(W2,跟正确性报告合并)

- D05 sessions 扩 last_seen_at / ip / user_id(联动 S01 登录限速定位)
- D11 user_agent 长度限制
- D12 username CHECK 约束
- D14 note 长度限制
- D16 测试补全

### P2(后续版本)

- D03 migration 版本表 / 引入 goose
- D08 panelstate 统一进 sqlite(若需要扩 settings)
- D13 删除用户级联 session(等 D05 改完一起)

---

## 完成定义

- [ ] D01 / D02 / D04 / D06 / D07 / D10 修复(本周)
- [ ] D05 / D11-D16 修复(下周)
- [ ] D03 在第一次 schema 演进时立即上
- [ ] 每个修复有对应 test 覆盖
- [ ] release-notes 记录修复内容
- [ ] 跟 audit-2026-09-security.md(S05 方案 C)合并 PR