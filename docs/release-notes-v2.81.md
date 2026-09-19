# v2.81 (2026-09-18)

## 改进

### 自动网络模式切换 (`§0.7` 自动选 host/bridge/ipvlan,详见 design.md §18)

之前 `IKEV2_NETWORK_MODE=auto` 只是 `docker-compose.yml` 里的占位变量,entrypoint 无法热切换 network_mode (docker compose 启动前就要决定)。
现在新增 `scripts/up.sh` 包装脚本,跑 `./scripts/up.sh` 即可在 docker compose 启动**前**自动探测宿主环境,
生成 `docker-compose.override.yml`,业界标准做法。

**核心实现**:

| 文件 | 作用 |
|---|---|
| `scripts/auto-network.sh` (新增) | 探测 OUT_IF / 公网 IPv6 / 公网 IPv4 → 按规则选模式 → 写 `docker-compose.override.yml` |
| `scripts/up.sh` (新增) | 包一层:`./scripts/up.sh` = `auto-network.sh` + `docker compose up -d` |
| `scripts/entrypoint.sh` (§0.7 新增) | 容器内校验实际模式 vs 声明模式,不匹配时 WARN 提示用户跑 `up.sh` |
| `docker-compose.yml` (改注释) | 头部加 v2-80 自动模式说明,默认仍是 host (向后兼容) |

**探测规则** (跟 design.md §18.2 完全一致):

```
OUT_IF 有 2000::/3 公网 IPv6  →  host          (直绑宿主网卡,推荐)
OUT_IF 有公网 IPv4            →  bridge         (端口映射 + libipsec)
都没有                        →  bridge+ipvlan  (自动建 ipvlan 网络,失败时退回 bridge)
```

**用户使用方式**:

```bash
# 新用户推荐:
./scripts/up.sh

# 强制指定模式(用户优先级最高):
IKEV2_NETWORK_MODE=bridge ./scripts/up.sh
IKEV2_NETWORK_MODE=ipvlan ./scripts/up.sh

# 跳过探测(老用户习惯):
./scripts/up.sh --no-detect    # 等价于直接 docker compose up -d

# 老用户继续:
docker compose up -d           # 100% 兼容,默认 host 网络
```

**ipvlan 网络自动建** (幂等):
- 已存在 → 跳过
- 创建失败 → 退回 bridge+libipsec + WARN 提示

## 升级

```bash
cd /home/TEST/ikev2-panel-v2
git pull
./scripts/up.sh    # 推荐,自动选模式
# 或者:
docker compose up -d  # 老习惯,默认 host
```

## 镜像

- ghcr.io: `ghcr.io/rewind2026/ikev2-panel-v2:2.81`
- 多架构: `linux/amd64`、`linux/arm64`

## 升级提示

- **新增文件**:`scripts/up.sh`、`scripts/auto-network.sh`(已加执行权限)
- **生成文件**:`docker-compose.override.yml` (每次 `./scripts/up.sh` 自动重生成)
- **老用户零迁移成本**:`docker compose up -d` 100% 行为不变

## 测试覆盖

- ✅ `bash -n scripts/auto-network.sh` 语法检查
- ✅ `bash -n scripts/up.sh` 语法检查
- ✅ `bash -n scripts/entrypoint.sh` 语法检查
- ✅ 实测:auto 模式(检测到公网 IPv6)→ host override 正确
- ✅ 实测:`IKEV2_NETWORK_MODE=host` 用户指定 → host override
- ✅ 实测:`IKEV2_NETWORK_MODE=bridge` 用户指定 → bridge + 端口映射 override
- ✅ 实测:`IKEV2_NETWORK_MODE=ipvlan` (无 docker) → 优雅退回 bridge + WARN
- ✅ `go build ./...` 通过
- ✅ `go test ./...` 9 包全绿
