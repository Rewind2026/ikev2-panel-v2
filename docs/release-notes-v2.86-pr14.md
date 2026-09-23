# Release Notes — v2.86-PR14 — 网络模式收敛到 host 唯一

**日期**: 2026-09-23
**作者**: panel-maintainer
**类型**: cleanup(代码精简 + 文档收敛)
**前序 PR**: v2.86-PR13.4(日志持久化)

---

## 背景

v2-79 引入的"IKEv2 panel 自动网络模式选择"在历史上一共支持 3 种:

| 模式 | 探测分支 | 实机验证 |
|---|---|---|
| `network_mode: host` | 公网 IPv6 → 首选 | ✅ 生产稳定 |
| `bridge` + libipsec | 仅公网 IPv4 时 fallback | ❌ 多用户并发丢包严重 |
| `bridge` + ipvlan | 无公网 IP 时 fallback | ❌ 需手工建网络 + docker daemon 实验配置 |

实际上**几乎所有生产用户都用 host**(自 v2-79 推出公网 IPv6 推荐后),bridge/ipvlan 模式只是"理论上能跑",从未在生产中真正发挥作用。

v2-85 决定收敛到 host 唯一,本 PR 把代码 / 文档 / 探测脚本全部砍掉。

---

## 改动

### 删除

| 文件 | 说明 |
|---|---|
| `docker-compose.override.yml` | 不再需要 override(只有 host 一种) |
| `scripts/auto-network.sh` | 探测脚本,300 行 → 0 |

### 简化

| 文件 | 改动 |
|---|---|
| `docker-compose.yml` | 文件头注释收敛到"host 唯一";删 bridge/ipvlan ports/networks 模板;删 sysctls bridge 注释段;env 默认值 `IKEV2_NETWORK_MODE=host`(原来 `auto`) |
| `scripts/up.sh` | 不再调 `auto-network.sh`,只做 `./logs` 准备 + `docker compose up -d`;75 行 → 75 行(去掉了探测调用块) |
| `scripts/entrypoint.sh` | §0.7 `detect_network_mode` 整段删除(替换为 case 校验:非 host → FATAL 拒绝);§3 forwarding 删 bridge 跳过分支;env 默认 `host` |
| `.env.example` | 删 `IKEV2_NETWORK_MODE=auto` 默认,改注释"v2.86-PR14 收敛:只支持 host" |
| `README.md` | M10 改标题"v2-82 阿里云 DDNS"(去掉自动网络选择字眼);§3 网络模式对比表收敛(5 列 → 3 列);§Q1/Q3/Q4 删除,新增 Q1 "怎么从 bridge 迁到 host";后续 Q 编号统一收编 |
| `docs/design.md` | §18 整章改写为"网络模式收敛到 host 唯一";§1.5.4 改写"网络模式(host 唯一)";其他章节残留 bridge/ipvlan 引用清零 |
| `web/templates/login_content.html` | 修 bug:`{{.Version}}` 渲染 `v2.86-PR13.4`,模板字面量 `v{{.Version}}` 导致 `vv2.86-PR13.4` 双 v 显示 |

### 行为变化

| 场景 | v2.86-PR14 之前 | v2.86-PR14 之后 |
|---|---|---|
| `docker compose up -d`(默认) | host 网络(default),OK | host 网络,OK(不变) |
| `./scripts/up.sh` | 探测网络模式 → 写 override → docker compose up | `./logs` 准备 → docker compose up(更简单) |
| `.env` 设 `IKEV2_NETWORK_MODE=bridge` | 探测 fallback 到 bridge 模式(可能跑不起来) | **FATAL 退出** + 明确报错(v2.86-PR14 §0.7) |
| `.env` 设 `IKEV2_NETWORK_MODE=ipvlan` | 探测 fallback 到 ipvlan 模式 | **FATAL 退出** + 明确报错 |

**关键保证**:老用户(从 v2.86-PR13 之前升上来的)默认 `docker compose up -d` 行为 100% 不变(host 网络是 yml 默认)。

---

## 验证步骤

### 1. 语法检查

```bash
bash -n scripts/up.sh
bash -n scripts/entrypoint.sh
```

### 2. YAML 结构(无 docker compose 时)

```bash
grep -E '^\s*network_mode:' docker-compose.yml
# 期望: services.ikev2-panel.network_mode: host

grep -E '^\s*IKEV2_NETWORK_MODE:' docker-compose.yml
# 期望: 1 行 IKEV2_NETWORK_MODE: "${IKEV2_NETWORK_MODE:-host}"
```

### 3. 老 override 文件处理(如有)

```bash
# 老用户可能有残留 docker-compose.override.yml,删掉:
rm -f docker-compose.override.yml

# 老用户 .env 可能设了 NETWORK_MODE=bridge|ipvlan:
grep -E '^IKEV2_NETWORK_MODE=' .env
# 如果有:从 .env 删掉那一行,或改成 host
```

### 4. 启动验证

```bash
./scripts/up.sh
docker logs ikev2-panel 2>&1 | grep -E 'network-mode|FATAL'
# 期望: [entrypoint] [network-mode] host (only supported mode)
```

### 5. login 页验证

```bash
docker exec ikev2-panel cat /etc/ikev2-panel-version
# 期望: v2.86-PR14
# 浏览器访问 /login → 品牌区 "版本 v2.86-PR14" 应该是单 v(不是 vV...)
```

---

## 回滚方案

```bash
git revert <this-commit>
# 重新生成 override:
./scripts/up.sh   # 老 up.sh 会调 auto-network.sh 重建 override
```

---

## 风险评估

- **生产用户损失**:几乎为零。所有生产用户都用 host(2025 实机验证数据)。
- **bridge/ipvlan 残留用户**:极少数(从 v2-79~v2-85 实验性用过 bridge/ipvlan 的人)。entrypoint.sh §0.7 FATAL 退出 + 报错信息明确告诉他们怎么改,不会静默失败。
- **代码减量**:删 1 个文件(300 行) + 多个文件改注释 + 改小逻辑,纯删除 + 文档收敛。

---

## 设计决策记录(为什么这么做)

- **不做兼容层**:不引入 "IKEV2_NETWORK_MODE_LEGACY_BRIDGE" 这种兼容 env。bridge 模式本来就是实验性功能,不是稳定 API,直接砍。
- **entrypoint.sh §0.7 拒绝而非 WARN**:WARN 容易被用户忽略 → 用 bridge 模式 → 跑不起来 → 投诉链长。直接 FATAL + 明确报错反而更友好。
- **保留 .env / env 字段**:虽然不读 `auto`,但保留 `IKEV2_NETWORK_MODE` env(默认 `host`)。用户老 .env 文件无需改,设 `bridge|ipvlan` 才会被 FATAL。
- **不动 Dockerfile / Go 代码**:Go 主程序本来就不读 `IKEV2_NETWORK_MODE`,Dockerfile 跟网络模式解耦,无需改动。