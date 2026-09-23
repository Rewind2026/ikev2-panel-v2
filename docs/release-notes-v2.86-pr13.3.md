# Release Notes — v2.86-PR13.3

**PanelState subnet 对齐 cert.conf:重启后保留面板用户的 IP 段配置**

## 背景

v2.86-PR13.2 引入了 `/data/panel-state/subnet.conf`,允许用户在面板运行时修改客户端虚拟 IP 段(IPv4 pool + IPv6 ULA pool)。但**只解决了运行期改**:重启容器后 entrypoint §0.6 的 auto-detect 不知道 panelstate 写过什么,会用候选列表 `10.10.0.0/24 → ...` 选第一段,把面板用户设置的 `10.10.20.0/24` 覆盖回 `10.10.0.0/24`,导致 iOS 客户端重拨后拿到错的 IP。

类似的 panelstate 优先级问题在 `cert.conf` 和 `aliyun.creds` 都已修复,subnet 一直没跟上。

## 改动

### 1. `scripts/entrypoint.sh` §-0.5 新增(对称 cert.conf)

启动期优先读 panelstate subnet.conf:

```bash
SUBNET_CONF="/data/panel-state/subnet.conf"
if [ -f "$SUBNET_CONF" ]; then
  subnet_v4_conf=$(grep ... '"ipv4_subnet"' ... $SUBNET_CONF | head -1)
  subnet_v6_conf=$(grep ... '"ipv6_subnet"' ... $SUBNET_CONF | head -1)
  if [ -n "$subnet_v4_conf" ]; then
    IKEV2_VPN_SUBNET="$subnet_v4_conf"
    [ -n "$subnet_v6_conf" ] && IKEV2_VPN_SUBNET_V6="$subnet_v6_conf"
    SUBNET_CONF_USED=1
    echo "${LOG_PREFIX} [subnet.conf] applied: ipv4=... ipv6=..."
  fi
fi
```

§0.6 / §0.6b 加守卫:`SUBNET_CONF_USED=1` 时跳过 auto-detect。

### 2. `internal/runtime/runtime.go` §3 subnet 合并(对称 cert / aliyun)

`Runtime.SubnetConfigSource` 新增,`runtime.Merge` 启动期 panelstate → env 合并。

### 3. `internal/config/config.go` 新增 env 字段

```go
IPv4Subnet         string // IKEV2_VPN_SUBNET
IPv6Subnet         string // IKEV2_VPN_SUBNET_V6
SubnetConfigSource string // 来源标记
```

`config.Load()` 读取 env,`SubnetConfigSource="env-default"`。

### 4. `internal/panelstate/subnet.go` Validate 允许 IPv6Subnet 为空

面板用户可能只改 v4(IPv6 ULA 跟 v4 独立,不值得每次都填两个)。
空时 Validate 通过;runtime.Merge 保留 env 的 v6;handler save 时用 `StartupIPv6Subnet` 兜底,不把当前 v6 覆盖成空串。

### 5. `internal/web/handlers_subnet.go` clear 按钮联动 swanctl

旧:清除只删 panelstate 文件,需要重启容器才能让 swanctl.conf 改回 startup 池段。
新:清除时立即调 `Manager.UpdatePoolsAndReload(StartupIPv4Subnet, StartupIPv6Subnet)`,无需重启容器。对称 cert.conf / aliyun.creds 的 clear("清掉 panelstate + 立即生效")。

`Server.StartupIPv4Subnet` / `StartupIPv6Subnet` 由 main.go 启动期调 `Manager.ReadCurrentPoolsFromFile()` 注入。

### 6. `cmd/ikev2-panel/main.go` 注入 StartupIPv4/6

```go
startupV4, startupV6 := scm.ReadCurrentPoolsFromFile()
srv := &web.Server{
    ...
    StartupIPv4Subnet: startupV4,
    StartupIPv6Subnet: startupV6,
}
```

dev 模式文件不存在 → 空,handler clear 走 "重启回 env" 老路径。

## 改动文件

| 文件 | 改动 |
|---|---|
| `scripts/entrypoint.sh` | §-0.5 新增 panelstate subnet 读取;§0.6/§0.6b 加 SUBNET_CONF_USED 守卫 |
| `internal/config/config.go` | `Config.IPv4Subnet/IPv6Subnet/SubnetConfigSource` 字段 + `Load()` 读 env |
| `internal/runtime/runtime.go` | `Runtime.SubnetConfigSource` 字段 + `Merge()` §3 subnet 合并 |
| `internal/panelstate/subnet.go` | `Validate()` IPv6Subnet 允许空 |
| `internal/web/server.go` | `Server.StartupIPv4Subnet/IPv6Subnet` 字段 |
| `internal/web/handlers_subnet.go` | `handleSubnetClear` 立即改 swanctl;`handleSubnetSave` v6 空时用 StartupIPv6Subnet 兜底 |
| `cmd/ikev2-panel/main.go` | 注入 StartupIPv4/6 |
| `internal/runtime/runtime_test.go` | 新增 4 个 subnet 合并单元测试 |
| `internal/panelstate/subnet_test.go` | 改 v6 空测试用例为 OK(原断言失败) |
| `docs/design.md` §16.2 表格下方 | PR13.3 优先级链说明 |
| `docs/release-notes-v2.86-pr13.3.md` | 本文件 |

## 验证

```
$ go vet ./...                          # 0 warnings
$ go test ./...                         # ok (含 4 个新增 runtime.Merge subnet 测试)
$ bash -n scripts/entrypoint.sh          # OK
$ docker build -t ikev2-panel:v2.86-pr13.3 .
$ ssh 192.168.50.63 docker compose up -d
```

启动期 entrypoint 日志:
```
[subnet.conf] applied: ipv4=10.10.20.0/24 ipv6=fd00:1::/64
[subnet.conf] skip auto-detect for v4 subnet (panel state wins)
[subnet.conf] skip auto-detect for v6 subnet (panel state wins)
```

iOS 重拨 → 拿到 `10.10.20.1`。

## 不在范围

- 没改 entrypoint `auto-detect` 候选列表(10.10.0.0/24 等 5 段),纯 panelstate 优先级修复。
- 没动 `configs/swanctl-ipv6-only.conf` 模板(它本来是 `%IKEV2_VPN_SUBNET%` 占位符,entrypoint 注入)。
- 没碰 `installtoken`(纯运行时,无 panelstate 持久化)。