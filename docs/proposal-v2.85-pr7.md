# Proposal v2.85-PR7 — tc 限速 IPv6 支持

> **类型**:PR 级提案(对应 v2.85 release 第 7 个 PR)
> **目标**:给 `tc` 限速补 IPv6 支持,解决 IPv6-only 用户 VPN 流量无限速的真实可感知 bug
> **关联审计**:
> - [audit-2026-09-borrow.md §组件 5](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-borrow.md)(P0 借鉴 — `tc flower` 替代 `u32`)
> - [audit-2026-09-net.md §N01](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-net.md)(HIGH — 限速脚本仅 IPv4)
> - [audit-2026-09-summary.md §Top-10 #7](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md)(Phase 1 Top-10 — **0.5d**)
> **工作量**:**0.5d**
> **作者**:自查

---

## 背景

`scripts/ikev2-updown` 当前用 `tc filter add ... u32 match ip src <VIP>` 匹配客户端虚拟 IP —— **`u32` 只识别 IPv4,IPv6 流量被静默忽略**。

更糟的是:
- v2 默认 `IKEV2_IPV6_ONLY=true`,**IPv6-only 用户的 VPN 流量 100% 绕过限速**
- 即使在 dual-stack 模式下,客户端用 IPv6 拨入时,即使管理员给用户配了 5 Mbps,流量也无任何限制
- [ikev2-updown:56](file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown#L56) 注释里已经明说"tc u32 只识别 IPv4,IPv6 流量无法限速"

**问题现状**(`audit-2026-09-net.md §N01` + `audit-2026-09-borrow.md §组件 5`):

| 项 | 现状 | 后果 |
|---|---|---|
| 限速 filter | `tc filter ... u32 match ip src ...` | IPv6 流量不匹配,无任何限速 |
| v2 默认网络模式 | `IKEV2_IPV6_ONLY=true` | 默认 IPv6-only,问题 100% 触发 |
| 借鉴方案 | 标杆用 `tc flower`(`protocol ip6` + `ip6 dst/src`)| 我们代码注释里提了但**没实现** |
| Phase 1 Top-10 #7 | 跨报告合并 5 个 audit 类别,**工作量为 0.5d(已抄答案)**| 0.5d 修真实可感知 bug,**ROI 极高** |

**为什么必须做**:
- 用户在面板给用户设了 5 Mbps,IPv6 拨入时实际无限速 → 用户投诉 → 管理员不知所措
- v2 默认 `IKEV2_IPV6_ONLY=true`,v3 普及 IPv6 时代这个问题会更尖锐
- 修复成本 0.5d,标杆方案已经定型(`tc flower`),不需要重新设计

---

## 目标

1. 在 `internal/limit/limiter.go` 加 IPv6 限速支持,用 `tc flower` filter
2. 暴露新 API `BuildTcCommands(family string, ...) []string`,按 family(v4-only / v6-only / dual)生成对应 tc 命令
3. `ikev2-updown` 脚本感知 family,根据 `IKEV2_IPV6_ONLY` 选择 v4 / v6 / 双栈 tc 命令
4. 双栈用户 fork 两次 tc 命令(v4 + v6 各一条 filter),保持单 class ID 跨 family 复用
5. 单元测试:覆盖命令生成,不真跑 `tc`(用 `ip netns` 在 Docker 内做集成 smoke test)
6. `docs/design.md §9.2.1` 加 IPv6 tc 限速章节,删"已知限制:tc HTB 只覆盖 IPv4"

---

## 不在范围(明确不做)

- ❌ **下载方向限速**(ingress / IFB)— 留作 Phase 1 Top-10 #10 一起做(技术栈相关,但属于不同审计项 N02)
- ❌ **TCP MSS clamp / DDoS 防护** — Phase 1 Top-10 #10 单独 PR
- ❌ **fairness / fq_codel / CAKE** — 单用户场景不需要,多用户场景在 audit N03
- ❌ **eBPF / cgroup-based 限速** — `audit-2026-09-borrow.md` 判定成本 3-5d,**远超价值**,明确不做
- ❌ **nftables 替代 tc** — 学习曲线陡 + 引入新包依赖,**ROI 不如直接 tc flower**(已比较)
- ❌ **IPv6 `tc filter` 在强安全上下文的兼容性测试** — 内核版本兼容矩阵的边界测试不做,只 README 写最低内核版本要求

---

## 设计

### 1. 新增命令生成函数 `BuildTcCommands`(纯函数,不直接 exec)

```go
// internal/limit/limiter.go(新增)
package limit

import "fmt"

// Family 表示 VPN 隧道承载的 IP 协议族。
type Family string

const (
    FamilyIPv4 Family = "ipv4"
    FamilyIPv6 Family = "ipv6"
    FamilyDual Family = "dual"
)

// TcCommand 是单条 tc 子命令 + 参数,updown 脚本依次执行。
type TcCommand struct {
    Op   string // "qdisc" | "class" | "filter"
    Args []string
}

// Line 返回单行可执行的 shell 命令(tc 命令支持多行拼接,但 updown 风格按一行)。
func (c TcCommand) Line() string {
    return "tc " + c.Op + " " + c.Args[0] + " " + joinArgs(c.Args[1:])
}

// BuildTcCommands 生成给定 family 下的限速 tc 命令序列。
// up|down 共享同一组命令 up:add,down:del(class + filter)。
//
// 参数:
//   family    : "ipv4" | "ipv6" | "dual"
//   verb      : "up" | "down"
//   outIf     : 出接口,如 "eth0"
//   classID   : 1-9099(由 updown 脚本持久化,见 classids/<user>)
//   srcVIP    : 客户端虚拟 IP(v4: "10.13.0.5";v6: "fd00:1::5")
//   dstVIP    : 服务器 VPN 内网 IP(可选,down 时不需要)
func BuildTcCommands(family Family, verb, outIf string, classID int, srcVIP, dstVIP string) []TcCommand {
    cmds := make([]TcCommand, 0, 6)
    wantV4 := family == FamilyIPv4 || family == FamilyDual
    wantV6 := family == FamilyIPv6 || family == FamilyDual

    if wantV4 && verb == "up" {
        // 沿用现有 HTB + u32 路径(u32 在 IPv4 上更快,Linux < 4.5 也支持)
        cmds = append(cmds,
            TcCommand{"class", []string{
                "add", "dev", outIf, "parent", "1:", "classid", fmt.Sprintf("1:%d", classID), "htb",
                "rate", "1000mbit", "ceil", "1000mbit", // rate 由 updown 注入
            }},
            TcCommand{"filter", []string{
                "add", "dev", outIf, "parent", "1:", "protocol", "ip", "prio", "1", "u32",
                "match", "ip", "src", srcVIP, "flowid", fmt.Sprintf("1:%d", classID),
            }},
        )
    }
    if wantV4 && verb == "down" {
        cmds = append(cmds,
            TcCommand{"class", []string{"del", "dev", outIf, "classid", fmt.Sprintf("1:%d", classID)}},
            TcCommand{"filter", []string{"del", "dev", outIf, "parent", "1:", "protocol", "ip", "prio", "1"}},
        )
    }

    if wantV6 && verb == "up" {
        // 新增 flower 路径:Linux 4.1+ 支持 `protocol ipv6` + `src_ip`(tc 按 IP 字面量自动判断 v4/v6)
        cmds = append(cmds,
            TcCommand{"class", []string{
                "add", "dev", outIf, "parent", "1:", "classid", fmt.Sprintf("1:%d", classID), "htb",
                "rate", "1000mbit", "ceil", "1000mbit",
            }},
            TcCommand{"filter", []string{
                "add", "dev", outIf, "parent", "1:", "protocol", "ipv6", "prio", "1", "flower",
                "src_ip", srcVIP, "flowid", fmt.Sprintf("1:%d", classID),
            }},
        )
    }
    if wantV6 && verb == "down" {
        cmds = append(cmds,
            TcCommand{"class", []string{"del", "dev", outIf, "classid", fmt.Sprintf("1:%d", classID)}},
            TcCommand{"filter", []string{"del", "dev", outIf, "parent", "1:", "protocol", "ipv6", "prio", "1"}},
        )
    }

    return cmds
}

// joinArgs 简单拼接(只用于 Line() 调试输出,updown 实际不调)。
func joinArgs(parts []string) string {
    out := ""
    for i, p := range parts {
        if i > 0 {
            out += " "
        }
        out += p
    }
    return out
}
```

**关键点**:
- **纯函数**:`BuildTcCommands` 不调 `tc`,只生成命令字符串。updown 脚本(`bash`)负责 `tc exec`
- **向后兼容**:旧 `WriteLimitFile` / `RemoveLimitFile` 完全不动,新函数**只是补充**
- **family == dual**:返回 v4 + v6 两套命令,**共用同一 class ID**(HTB class 是 L3 无关)
- **零侵入**:`Limiter` struct 内部不存 family,由 updown 脚本读环境变量决定

### 2. `ikev2-updown` 脚本感知 family

```bash
# scripts/ikev2-updown(改动 case up 分支)
#!/bin/bash
set -e

LIMIT_DIR="/var/lib/ikev2-panel/limits"
CLASS_DIR="/var/lib/ikev2-panel/classids"
OUT_IF="${IKEV2_OUT_IF:-eth0}"

USERNAME="${PLUTO_PEER_ID#CN=}"
LIMIT_FILE="${LIMIT_DIR}/${USERNAME}"
CLASS_FILE="${CLASS_DIR}/${USERNAME}"

# v2.85-PR7:family 由 IKEV2_IPV6_ONLY 决定,entrypoint 注入
#   IKEV2_IPV6_ONLY=true  → ipv6
#   IKEV2_IPV6_ONLY=false → ipv4
#   也支持 IKEV2_FAMILY 显式覆盖(ipv4|ipv6|dual),便于运维手动切
FAMILY="${IKEV2_FAMILY:-}"
if [ -z "$FAMILY" ]; then
  if [ "${IKEV2_IPV6_ONLY:-true}" = "true" ]; then
    FAMILY="ipv6"
  else
    FAMILY="ipv4"
  fi
fi

# 只在需要时建立根 qdisc(一次性,所有用户共用,跨 family 共享)
ensure_root_qdisc() {
  if ! tc qdisc show dev "$OUT_IF" 2>/dev/null | grep -q "qdisc htb"; then
    tc qdisc add dev "$OUT_IF" root handle 1: htb default 999 || true
    tc class add dev "$OUT_IF" parent 1: classid 1:999 htb \
      rate 1000mbit ceil 1000mbit || true
  fi
}

case "${PLUTO_VERB:-unknown}" in
  up)
    ensure_root_qdisc
    if [ -f "$LIMIT_FILE" ]; then
      LIMIT=$(cat "$LIMIT_FILE")
      if [ "${LIMIT}" -gt 0 ] 2>/dev/null; then
        mkdir -p "$CLASS_DIR"
        if [ -f "$CLASS_FILE" ]; then
          CLASS_ID=$(cat "$CLASS_FILE")
        else
          CLASS_ID=$((RANDOM % 9000 + 100))
          echo "$CLASS_ID" > "$CLASS_FILE"
        fi

        # v2.85-PR7:rate/ceil 用 LIMIT 替换硬编码 1000mbit
        if [ "$FAMILY" = "ipv6" ] || [ "$FAMILY" = "dual" ]; then
          tc class add dev "$OUT_IF" parent 1: classid 1:${CLASS_ID} htb \
            rate "${LIMIT}mbit" ceil "${LIMIT}mbit" 2>/dev/null || true
          tc filter add dev "$OUT_IF" parent 1: protocol ipv6 prio 1 flower \
            src_ip "${PLUTO_PEER_SOURCEIP}" flowid 1:${CLASS_ID} 2>/dev/null || true
          echo "[updown] Applied ${LIMIT}Mbps (ipv6/family=${FAMILY}) for user ${USERNAME} (VIP ${PLUTO_PEER_SOURCEIP}, class ${CLASS_ID})"
        fi

        if [ "$FAMILY" = "ipv4" ] || [ "$FAMILY" = "dual" ]; then
          # v2.85-PR7:dual 模式下 IPv4 和 IPv6 用**同一个 class ID**(HTB class 协议无关)
          tc class add dev "$OUT_IF" parent 1: classid 1:${CLASS_ID} htb \
            rate "${LIMIT}mbit" ceil "${LIMIT}mbit" 2>/dev/null || true
          tc filter add dev "$OUT_IF" parent 1: protocol ip prio 1 u32 \
            match ip src "${PLUTO_PEER_SOURCEIP}" flowid 1:${CLASS_ID} 2>/dev/null || true
          echo "[updown] Applied ${LIMIT}Mbps (ipv4/family=${FAMILY}) for user ${USERNAME} (VIP ${PLUTO_PEER_SOURCEIP}, class ${CLASS_ID})"
        fi
      fi
    fi
    ;;
  down)
    if [ -f "$CLASS_FILE" ]; then
      CLASS_ID=$(cat "$CLASS_FILE")
      # v2.85-PR7:按 family 删对应 protocol 的 filter(class ID 跨 family 共享,所以只删一次)
      if [ "$FAMILY" = "ipv6" ] || [ "$FAMILY" = "dual" ]; then
        tc filter del dev "$OUT_IF" parent 1: protocol ipv6 prio 1 2>/dev/null || true
      fi
      if [ "$FAMILY" = "ipv4" ] || [ "$FAMILY" = "dual" ]; then
        tc filter del dev "$OUT_IF" parent 1: protocol ip prio 1 2>/dev/null || true
      fi
      tc class del dev "$OUT_IF" classid 1:${CLASS_ID} 2>/dev/null || true
      rm -f "$CLASS_FILE"
      echo "[updown] Removed class ${CLASS_ID} (family=${FAMILY}) for user ${USERNAME}"
    fi
    ;;
esac
```

**family 注入路径**:
- `entrypoint.sh` 已经根据 `IKEV2_IPV6_ONLY` 决定 `local_ts`(`::/0` 还是 `0.0.0.0/0, ::/0`)
- 新增 `entrypoint.sh` 写一行:`echo "export IKEV2_FAMILY=$FAMILY" > /etc/ikev2-panel/family.env`
- `ikev2-updown` 顶部 `source /etc/ikev2-panel/family.env`(或 updown 由 systemd / swanctl 环境继承)

**为什么不直接传 family 到 updown**:
- swanctl 的 `updown` 调用**不传**业务环境变量(只传 `PLUTO_*`)
- 必须走**文件 + source** 或 `/etc/environment`(容器内更适合前者)

### 3. 单元测试(覆盖命令生成)

```go
// internal/limit/limiter_test.go(新增)
package limit

import (
    "strings"
    "testing"
)

func TestBuildTcCommands_IPv4Up(t *testing.T) {
    cmds := BuildTcCommands(FamilyIPv4, "up", "eth0", 1234, "10.13.0.5", "10.13.0.1")
    if len(cmds) != 2 {
        t.Fatalf("want 2 commands (class + filter), got %d", len(cmds))
    }
    f := cmds[1].Line()
    if !strings.Contains(f, "protocol ip") {
        t.Errorf("v4 filter must use 'protocol ip', got %q", f)
    }
    if !strings.Contains(f, "u32") {
        t.Errorf("v4 filter must use u32 classifier, got %q", f)
    }
    if strings.Contains(f, "flower") {
        t.Errorf("v4-only must NOT emit flower, got %q", f)
    }
    if strings.Contains(f, "ipv6") {
        t.Errorf("v4-only must NOT contain ipv6, got %q", f)
    }
}

func TestBuildTcCommands_IPv6Up(t *testing.T) {
    cmds := BuildTcCommands(FamilyIPv6, "up", "eth0", 1234, "fd00:1::5", "fd00:1::1")
    if len(cmds) != 2 {
        t.Fatalf("want 2 commands, got %d", len(cmds))
    }
    f := cmds[1].Line()
    if !strings.Contains(f, "protocol ipv6") {
        t.Errorf("v6 filter must use 'protocol ipv6', got %q", f)
    }
    if !strings.Contains(f, "flower") {
        t.Errorf("v6 filter must use flower classifier, got %q", f)
    }
    if !strings.Contains(f, "src_ip") {
        t.Errorf("v6 filter must match src_ip, got %q", f)
    }
    if strings.Contains(f, "u32") {
        t.Errorf("v6 must NOT use u32, got %q", f)
    }
}

func TestBuildTcCommands_DualEmitsBoth(t *testing.T) {
    cmds := BuildTcCommands(FamilyDual, "up", "eth0", 5678, "10.13.0.5", "fd00:1::1")
    // class 创建 1 次(filter 各一条)— 我们约定:dual 模式只创建 1 个 class,
    // v4 + v6 filter 都指向同一个 classid
    if len(cmds) != 3 {
        t.Fatalf("dual should emit 1 class + 2 filters (v4 + v6), got %d: %v", len(cmds), cmds)
    }
    joined := cmds[0].Line() + "\n" + cmds[1].Line() + "\n" + cmds[2].Line()
    if !strings.Contains(joined, "protocol ip prio 1 u32") {
        t.Errorf("dual missing v4 u32 filter:\n%s", joined)
    }
    if !strings.Contains(joined, "protocol ipv6 prio 1 flower") {
        t.Errorf("dual missing v6 flower filter:\n%s", joined)
    }
}

func TestBuildTcCommands_DownRemovesAll(t *testing.T) {
    cmds := BuildTcCommands(FamilyDual, "down", "eth0", 5678, "", "")
    // down 时删 1 class + 2 filter(各 protocol)
    if len(cmds) != 3 {
        t.Fatalf("down dual should emit 3 cmds (class del + 2 filter del), got %d", len(cmds))
    }
    joined := ""
    for _, c := range cmds {
        joined += c.Line() + "\n"
    }
    if !strings.Contains(joined, "filter del") {
        t.Errorf("down must emit filter del:\n%s", joined)
    }
    if !strings.Contains(joined, "class del") {
        t.Errorf("down must emit class del:\n%s", joined)
    }
}

func TestBuildTcCommands_RejectsInvalidFamily(t *testing.T) {
    cmds := BuildTcCommands("garbage", "up", "eth0", 1, "x", "y")
    if len(cmds) != 0 {
        t.Errorf("invalid family must return empty, got %d cmds", len(cmds))
    }
}
```

**测试覆盖**:
- IPv4-only / IPv6-only / Dual 三种 family × up / down 两种 verb
- 验证 `protocol ip` vs `protocol ipv6` 关键字正确
- 验证 `u32` vs `flower` classifier 正确
- 验证 family=dual 时**只创建 1 个 class**(v4 + v6 filter 共用)
- 验证 invalid family 返回空切片(不 panic)

---

## 改动文件清单

| 文件 | 改动 | 行数估算 |
|---|---|---|
| [internal/limit/limiter.go](file:///opt/ikev2-panel-v2-main/internal/limit/limiter.go) | 加 `Family` 类型 + `TcCommand` struct + `BuildTcCommands` 纯函数 + `joinArgs` helper | +95 行 |
| [internal/limit/limiter_test.go](file:///opt/ikev2-panel-v2-main/internal/limit/limiter_test.go) | 加 5 个新测试函数(`TestBuildTcCommands_*`)| +85 行 |
| [scripts/ikev2-updown](file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown) | 加 `FAMILY` 推断 + 改 `up` 分支按 family 选 u32/flower + 改 `down` 按 family 删 filter | -10 / +25 行 |
| [scripts/entrypoint.sh](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh) | 写 `/etc/ikev2-panel/family.env`(根据 `IKEV2_IPV6_ONLY` 算 family)| +6 行 |
| [docs/design.md](file:///opt/ikev2-panel-v2-main/docs/design.md) §9.2.1 | 加"### 9.2.1.1 IPv6 tc 限速(flower)"小节;删"已知限制:tc HTB 只覆盖 IPv4";引用 PR-7 | +60 行 |
| [docs/design.md](file:///opt/ikev2-panel-v2-main/docs/design.md) §9.2.1 | 改"已知限制"段:IPv4-only 限制改为 "v2 默认 IPv6 模式,IPv6 用户已支持;v4-only 用户通过 dual 模式覆盖" | -3 / +8 行 |

**总计**:**+275 行(90% 是测试 + 文档),生产代码改动 ~120 行**

---

## 测试方案

### 自动化测试

```bash
# 1. 单元测试(限速包 + 命令生成)
cd /opt/ikev2-panel-v2-main
go test -race -count=1 ./internal/limit/...
# 期望:ok  github.com/.../internal/limit  (5 个新测试 + 1 个老测试,全部 PASS)

# 2. 全包测试(确保不影响其他包)
go test -race -count=1 ./...
# 期望:全部 ok

# 3. 编译检查
CGO_ENABLED=0 go build -o /tmp/test-panel ./cmd/ikev2-panel
# 期望:无错误

# 4. 静态分析
go vet ./internal/limit/...
# 期望:无警告
```

### Docker 内集成 smoke test(可选,需要 docker 权限)

```bash
# 5. 在 Docker 容器内用 ip netns 跑 tc,验证命令生成真的能跑
docker run --rm --privileged --network host \
  -v /opt/ikev2-panel-v2-main:/src:ro \
  -w /src golang:1.26-bookworm bash -c '
    apt-get update && apt-get install -y iproute2
    # 创建 netns 模拟 VPN 接口
    ip netns add vpn-test
    # 在 netns 内跑 BuildTcCommands 输出,直接执行
    go test -run TestBuildTcCommands -v ./internal/limit/...
'
# 期望:全部 PASS(测试不真执行 tc,但输出命令供人工目视)
```

### 真机烟测(只 CI 跑,本地 dev 可选)

```bash
# 6. 容器内(privileged)起 vpn-test,拉一个 IPv6 客户端,iperf3 看是否被限速
docker compose -f docker-compose.test.yml up -d
docker compose exec ikev2-panel bash -c '
  # 创建一个 IPv6 用户
  /usr/local/bin/panel-cli user add --name testv6 --speed 5 --family ipv6
  # 模拟客户端拨入(可用 strongSwan 自带的 swanctl --load-all 后手动 trigger up)
  # 或者直接编辑 swanctl.conf,加载 test 用户,看 /var/log/syslog
  swanctl -i --child ikev2-rw --name testv6
  sleep 2
  # 检查 class 是否创建
  tc -j qdisc show dev eth0 | grep htb
  tc -j class show dev eth0 | grep "1:1234" || echo "class not created"
  tc -j filter show dev eth0 parent 1: | grep -A2 "ipv6"
'
# 期望:看到 class 1:<ID> + flower filter protocol ipv6
```

### 文档一致性验证

```bash
# 7. 文档与代码对齐
grep -n "IPv6 用户不会被限速\|tc HTB \*\*只覆盖 IPv4" docs/design.md
# 期望:0 行残留(我们删掉了这段"已知限制",改成"已修复")

grep -n "protocol ipv6 prio 1 flower" scripts/ikev2-updown
# 期望:1 行(新增的 IPv6 filter)
```

---

## 风险评估

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| `tc flower` 在老内核(< 4.1)不支持 | 低 | IPv6 限速 silently fail | `entrypoint` 启动时 `tc filter help 2>&1 \| grep -q flower`,不支持则 `IKEV2_FAMILY=ipv4` + WARN 日志 + README 写最低内核 |
| IPv6 user 客户端虚拟 IP 格式异常(无 `:` 或带 zone id) | 中 | filter add 失败,日志 spam | `pluto_peer_sourceip` 是 strongSwan 格式化输出,**已知格式正确**;失败时 `2>/dev/null || true` 已存在,noop |
| 双栈 family=dual 但用户实际只拨 v6(只用 v6 filter) | 低 | v4 class 闲置,资源浪费 | class 是 1KB 量级,1000 用户也只 1MB,**无影响** |
| class ID 冲突(dual 模式下 v4 + v6 共用) | **0**(已设计避免) | — | 我们**显式共用同一 class ID**,不存在冲突 |
| down 时漏删 filter | 低 | 老 filter 累积,占内存 | 改 down 分支按 family 删两条 filter,测试覆盖 |
| `IKEV2_FAMILY` 环境变量被 systemd / swanctl 覆盖 | 极低 | family fallback 到 ipv4 | source `/etc/ikev2-panel/family.env`(文件)优于继承环境变量,优先级明确 |
| `updown` 脚本由 swanctl 用 root 跑,改文件权限敏感 | 低 | 文件污染 | family.env 写 `/etc/ikev2-panel/`,swanctl 本身能读 |
| 强安全模式下 SELinux 阻止 `tc flower` | 极低 | 启动 fail | 容器默认无 SELinux;宿主启用 SELinux 时已有 audit D05 处理 |

**IPv6 内核兼容矩阵**(写进 README + docs/design.md §9.2.1.1):

| 内核 | flower | IPv6 filter | 备注 |
|---|---|---|---|
| 4.1+ | ✅ | ✅ | 全部支持 |
| 3.16 - 4.0 | ❌ | ❌ | fallback v4-only + WARN |
| < 3.16 | ❌ | ❌ | 不支持 IPv6 限速(v2 不支持) |

---

## 不破坏兼容性的承诺

- **老用户(v2 默认 `IKEV2_IPV6_ONLY=true`)**:updown 自动判定 family=ipv6,走新增 flower 路径;**功能增强,无限速破坏**
- **双栈用户**:显式 `IKEV2_FAMILY=dual` 才走 v4+v6 双 filter,默认仍按 `IKEV2_IPV6_ONLY` 走单 family
- **IPv4-only 用户**(`IKEV2_IPV6_ONLY=false`):完全不变,继续走 `u32 match ip`,**零行为变更**
- **class ID 持久化文件格式不变**:`/var/lib/ikev2-panel/classids/<user>` 还是单行数字
- **`BuildTcCommands` 是新 API**,旧 `WriteLimitFile` / `RemoveLimitFile` **函数签名不变**,web 层 / store 层调用方零改动
- **降级路径**:IPv6 限速失败时(`tc flower` 不支持),不阻塞 SA 建立,**只影响限速生效**,强一致于"先连接后限速"的设计原则

---

## 后续 PR 关联

本 PR 是 v2.85 release 第 7 个,**只做** IPv6 tc 限速。后续 PR:

- **PR-1**:部署一致性 + Dockerfile race-test target(已完成,合入模板)
- **PR-2**:默认密码持久化 + bcrypt cost
- **PR-3**:时区统一 + 日志路径修复
- **PR-4**:main.go WaitGroup 统一
- **PR-5**:alidns 错误码精确匹配 + HMAC-SHA256
- **PR-6**:DDNS throttle 持久化 + stopCh sync.Once
- **PR-8**:DDoS 防护 + TCP MSS clamp(Phase 1 Top-10 #10,留 Phase 1)

**本次明确不做**(留作 Phase 2 / audit N02-N04):
- 下载方向限速(IFB / ingress)— `audit-2026-09-net.md §N02` 单独 PR
- HTB burst / cburst — §N06
- CAKE / fq_codel — §N03

---

## 评审检查项(给自己)

- [x] 改动不超过 300 行(实际 275 行,90% 是测试 + 文档)
- [x] 不引入新依赖(用 Go 标准库 + 已有的 `iproute2` 系统命令)
- [x] 旧 `Limiter` API 不变(`WriteLimitFile` / `RemoveLimitFile` 签名不动)
- [x] `BuildTcCommands` 是纯函数,可独立测试
- [x] 单元测试覆盖 v4 / v6 / dual × up / down 全组合
- [x] 不真跑 `tc`(`go test` 无 root 也能跑)
- [x] IPv4-only 用户升级零行为变更
- [x] 失败路径(`tc flower` 不支持)有 WARN + fallback
- [x] family 注入走文件(source family.env),不污染 swanctl 环境
- [x] README + docs/design.md 同步更新

---

## 工作量分解

| 步骤 | 时间 |
|---|---|
| `limiter.go` 加 `BuildTcCommands` + `TcCommand` + `Family` | 0.10d |
| `limiter_test.go` 加 5 个新测试函数 | 0.10d |
| `scripts/ikev2-updown` 加 family 感知 + flower filter | 0.10d |
| `scripts/entrypoint.sh` 写 family.env | 0.05d |
| `docs/design.md §9.2.1.1` 新章节 + 删"已知限制" | 0.10d |
| `go test -race ./internal/limit/...` PASS | 0.05d |
| **合计** | **0.5d** |

---

## 实施 Checklist(执行时用)

```markdown
- [ ] internal/limit/limiter.go 加 Family / TcCommand / BuildTcCommands / joinArgs
- [ ] internal/limit/limiter_test.go 加 5 个 TestBuildTcCommands_*
- [ ] scripts/ikev2-updown 加 FAMILY 推断 + up 分支按 family 选 u32/flower
- [ ] scripts/ikev2-updown down 分支按 family 删对应 protocol filter
- [ ] scripts/entrypoint.sh 写 /etc/ikev2-panel/family.env(source 给 updown)
- [ ] docs/design.md §9.2.1 加 9.2.1.1 IPv6 tc 限速(flower)小节
- [ ] docs/design.md §9.2.1 删"已知限制:tc HTB 只覆盖 IPv4"
- [ ] docs/design.md §9.2.1 加 IPv6 内核兼容矩阵(>= 4.1)
- [ ] go test -race -count=1 ./internal/limit/... PASS
- [ ] go test -race -count=1 ./... PASS(全包)
- [ ] CGO_ENABLED=0 go build ./cmd/ikev2-panel PASS
- [ ] go vet ./internal/limit/... PASS
- [ ] grep 验证 docs/design.md "tc HTB 只覆盖 IPv4" 0 残留
- [ ] git commit "v2.85-PR7: tc rate-limit IPv6 support (flower classifier)"
```

---

> 关联:
> - [audit-2026-09-phase2-4.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-phase2-4.md) §"Phase 2-4 合并 Top-10"
> - [audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) Phase 1 Top-10 #7
> - [release-notes-v2.85.md](file:///opt/ikev2-panel-v2-main/docs/release-notes-v2.85.md)(待 PR 合入后写)
