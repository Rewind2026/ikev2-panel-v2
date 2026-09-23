# Release Notes — v2.86-PR13.4 — 容器日志持久化 + 轮转

**日期**: 2026-09-23
**作者**: panel-maintainer
**类型**: hotfix(运维可观测性 / 日志治理)
**前序 PR**: v2.86-PR13.3(panelstate subnet 对齐 cert.conf)

---

## 背景

之前 `/var/log` 在 docker-compose.yml 里是 `tmpfs` 128m,有两个痛点:

1. **重启即丢** — 容器重启后 `swanctl.log` / `charon.log` / `audit.log` 全部消失,排查"重启前最后发生了什么"无据可查
2. **128m 写满即写不进** — strongSwan / Go 进程写 `/var/log/*.log` 失败 → 500 错误 / charon 异常退出 / cron 续签失败但不告警

用户项目 `ddnsv6` 的范式已经做好(`./ddns_logs:/var/log/ddns` + `logging.driver=json-file` 轮转),**咱们也按这个来**。

## 改造

### 1. 容器内 `/var/log` 改 bind mount 到宿主 `./logs`

- `docker-compose.yml` volumes 段加 `./logs:/var/log:rw`
- 删 tmpfs 里的 `/var/log:size=128m`
- `scripts/up.sh` 启动前 `mkdir -p ./logs && chmod 1777 ./logs`(sticky bit)
- `.gitignore` 加 `logs/` + `/logs/`(防日志入库)

### 2. docker daemon 层日志轮转

- `docker-compose.yml` 加 `logging.driver=json-file` + `max-size=20m` + `max-file=5`
- 这是**双保险**:即使 `./logs` bind mount 写满了(宿主磁盘满),docker 也会按 max-size 滚动 stdout/stderr 日志,不让容器把宿主磁盘吃满

### 3. README + design.md 文档同步

- README 部署章节补"日志在 `./logs/`"段落
- docs/design.md §16.2 PR13.3 段后加 PR13.4 持久化策略段

## 为什么 `./logs` 用 bind mount 不用 named volume

- **跟 ddnsv6 项目对齐**(`./ddns_logs:/var/log/ddns`),运维直觉一致
- 用户能直接 `ls ./logs/` / `tail -f`,不用 `docker volume inspect` 找路径
- named volume 适合"用户应该知道看什么、不该知道看什么"的数据(LE 私钥 / SQLite DB)

## 为什么 `./data` 保留 named volume

`/data` 里面有 SQLite DB + LE 私钥 + acme.sh account.json,bin mount 到源码目录会让数据跟 `.git` 距离太近,误删 / 误加入风险高。named volume 由 docker 管理,可备份:

```bash
docker run --rm \
  -v ikev2-panel-v2_ikev2-data:/data:ro \
  -v $(pwd):/backup \
  alpine tar czf /backup/data-$(date +%Y%m%d).tgz -C /data .
```

## 改动文件清单

| 文件 | 改动 |
|---|---|
| `docker-compose.yml` | volumes 加 `./logs:/var/log:rw` + 加 `logging:` 段 + 删 tmpfs `/var/log` |
| `.gitignore` | 加 `logs/` + `/logs/` |
| `scripts/up.sh` | 加 §0.5 `./logs` 启动前准备(mkdir + chmod 1777) |
| `README.md` | 部署章节加"日志落盘"段落 |
| `docs/design.md` | §16.2 加 PR13.4 持久化策略表 |
| `docs/release-notes-v2.86-pr13.4.md` | (本文件) |

## 验证步骤

### 部署前检查

```bash
# 1. yml 语法(在装 docker 的机器上)
docker compose config | grep -E '(\./logs|logging|max-size)'

# 2. bash 语法
bash -n scripts/up.sh
```

### 部署

```bash
./scripts/up.sh
# 期望看到:
#   [up.sh] creating ./logs bind mount target (chmod 1777)...
#   [up.sh] ✓ ./logs created
# 或(目录已存在):
#   (无日志,直接进入 docker compose up)
```

### 部署后验证

```bash
# 1. ./logs 是否在容器内 mount 进去
docker exec ikev2-panel ls -la /var/log | head
# 期望看到 charon / swanctl / audit / cron 等日志文件

# 2. 容器内写日志,宿主能看到
docker exec ikev2-panel bash -c 'echo "test $(date)" >> /var/log/test.log'
cat ./logs/test.log
# 期望: 看到 test.log 内容,证明 bind mount 打通

# 3. daemon 层日志轮转
docker inspect ikev2-panel | grep -A 5 LogConfig
# 期望:
#   "LogConfig": {
#       "Type": "json-file",
#       "Config": {
#           "max-size": "20m",
#           "max-file": "5"
#       }
#   }

# 4. 重启容器,验证日志持久
docker compose restart ikev2-panel
ls -la ./logs/
# 期望: ./logs/ 下的日志还在(对比之前重启即丢的 tmpfs 行为)

# 5. 清理测试文件
rm -f ./logs/test.log
```

## 风险评估

- **宿主 `./logs` 权限**:chmod 1777 → sticky bit 防误删。如果用户已有 `./logs`(从别的项目),权限可能不对 — `up.sh` 已加 WARN 提示。
- **磁盘空间**:sticky bit + json-file 轮转双保险,正常情况下不会写满宿主磁盘。
- **dev 模式无影响**:`make dev` 不走 docker,./logs 不会被创建。日志仍在 `/tmp/ikev2-panel-dev.log`(Makefile 已有)。

## 回滚方案

```bash
git revert <this-commit>
docker compose up -d
# 容器内 /var/log 恢复为 tmpfs 128m
```