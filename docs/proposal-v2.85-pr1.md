# Proposal v2.85-PR1 — 部署一致性 + Dockerfile race-test target

> **类型**:PR 级提案(对应 v2.85 release 第 1 个 PR)
> **目标**:消除 README ↔ docker-compose ↔ Dockerfile 三处的不一致 + 给 `go test -race` 铺路
> **关联审计**:
> - [audit-2026-09-usability.md §U01](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md)(HIGH — image tag 不一致)
> - [audit-2026-09-correctness.md §Q1-05](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)(MED — Dockerfile 缺 race-test target)
> **工作量**:**0.4d**
> **作者**:自查

---

## 背景

Phase 1-4 审计发现 3 个部署相关问题,都是"文档/构建产物跟代码脱节"类:

| 症状 | 现状 | 后果 |
|------|------|------|
| README 写 `v2-80`,compose 写 `v2-84` | [README.md:179](file:///opt/ikev2-panel-v2-main/README.md#L179) vs [docker-compose.yml:38](file:///opt/ikev2-panel-v2-main/docker-compose.yml#L38) | 首次部署 `docker compose up` 找不到镜像;要么走 `build: .` fallback 出无名 tag |
| 启动 banner 不显示真实版本号 | [main.go:100-107](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L100-L107) `fmt.Println` 默认密码但**无版本号** | 排错时无法快速确认"我跑的是不是 v2.85" |
| Dockerfile 无 `runner-test` stage | 当前 `Dockerfile` 只 build runtime,无 gcc | 沙箱/本地永远跑不了 `go test -race`,CI 阶段才发现 race |

**为什么先做这个 PR**:
- U01 是真实新人卡壳点,影响所有新用户
- Dockerfile race-test 是后续 Correctness issue(Q1-01 panic recover / Q1-04 timeout)验证手段,**先铺路**
- 这两项都**不涉及行为变更**,只动文档 + 构建参数 + 一个新 target,风险极低

---

## 目标

1. README / docker-compose / Dockerfile 三处 tag 一致,且**跟 release version 联动**
2. 启动日志打印构建期注入的版本号
3. Dockerfile 加 `runner-test` target(含 gcc),支持 `docker build --target runner-test` 跑 race detector

---

## 不在范围(明确不做)

- ❌ 不改任何 Go 代码逻辑(只改 main.go 的版本号读取)
- ❌ 不引入 semver / git tag 自动同步(简单 ARG 传即可)
- ❌ 不改 entrypoint.sh / renew-cert.sh
- ❌ 不动 strongSwan 编译参数
- ❌ 不动 cap / network / volume(都是功能性问题,其他 PR)

---

## 设计

### 1. Dockerfile 加 `PANEL_VERSION` ARG + 写入版本文件

```dockerfile
# Dockerfile(改动在 runtime stage 顶部)
FROM debian:bookworm-slim AS runtime

# 版本注入:build 时 --build-arg PANEL_VERSION=v2.85,默认 dev
ARG PANEL_VERSION=dev
# 写入 /etc/ikev2-panel-version,启动时读取并打到日志
RUN echo "${PANEL_VERSION}" > /etc/ikev2-panel-version
```

**build 命令**:
```bash
docker build --build-arg PANEL_VERSION=v2.85 -t ikev2-panel:v2.85 .
```

### 2. main.go 启动时读版本号 + 打到日志

```go
// cmd/ikev2-panel/main.go(新增,放在 logger.Info("starting ikev2-panel") 之前)
versionBytes, err := os.ReadFile("/etc/ikev2-panel-version")
if err != nil || len(versionBytes) == 0 {
    versionBytes = []byte("dev")
}
panelVersion := strings.TrimSpace(string(versionBytes))
logger.Info("starting ikev2-panel", "version", panelVersion, "go_version", runtime.Version())
```

**dev 模式(本地 go build)**:没有 `/etc/ikev2-panel-version` 文件 → fallback `"dev"`,**不影响 dev 体验**。

### 3. docker-compose.yml 用 env 拼 image tag

```yaml
# docker-compose.yml 改 image 行为:用变量 + 默认值
services:
  ikev2-panel:
    image: ikev2-panel:${PANEL_TAG:-v2.85}    # 默认 v2.85,build 时 PANEL_TAG=v2.85 同步
    build:
      context: .
      args:
        PANEL_VERSION: ${PANEL_TAG:-v2.85}     # 注入到 Dockerfile ARG
```

**这样保证**:
- 镜像 tag 跟 Dockerfile `PANEL_VERSION` 一致(都从 `PANEL_TAG` env 读)
- 默认值改一处,两处都跟着变
- CI 可以在 build 前 `export PANEL_TAG=v2.85-rc1` 一次性传入

### 4. README 同步:统一 v2.85 + 加"验证部署"小节

改动点:
- 第 179 行 `docker build ... v2-80` → `v2-85`
- 第 159-160 行 image tag → `v2-85`
- 第 187 行 `./scripts/up.sh` 说明保留(不依赖 tag)
- 加 "**验证部署正确性**" 小节:

```markdown
### 验证部署

启动后确认版本:
\`\`\`bash
docker compose logs ikev2-panel 2>&1 | grep "version="
# 应看到: msg="starting ikev2-panel" version=v2.85 ...

docker compose exec ikev2-panel cat /etc/ikev2-panel-version
# 应输出: v2.85
\`\`\`
```

### 5. Dockerfile `runner-test` target

```dockerfile
# 加在 go-builder stage 之后,runtime stage 之前
# ============================================================================
# Stage 4: runner-test  跑 go test -race / go vet
# ============================================================================
FROM golang:1.26-bookworm AS runner-test

ENV CGO_ENABLED=1
# race detector 必须 cgo,装 gcc
RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc libc6-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./

# 默认跑全部包的 race 检测
CMD ["go", "test", "-race", "-count=1", "./..."]
```

**用法**:
```bash
# CI 跑 race
docker build --target runner-test -t ikev2-panel-race:test .
docker run --rm ikev2-panel-race:test   # 跑 go test -race ./...

# 也可以交互式进容器跑单包
docker build --target runner-test -t ikev2-panel-race:test .
docker run --rm -it ikev2-panel-race:test bash
# 进容器后: go test -race ./internal/ddns/...
```

**生产镜像不受影响**:runtime stage 不变,**runtime 镜像不包含 gcc**,体积不变。

---

## 改动文件清单

| 文件 | 改动 | 行数估算 |
|------|------|---------|
| [Dockerfile](file:///opt/ikev2-panel-v2-main/Dockerfile) | +1 ARG(runtime)+ 写入版本文件 + 新增 runner-test stage | +30 行 |
| [docker-compose.yml](file:///opt/ikev2-panel-v2-main/docker-compose.yml) | `image:` 改用 `${PANEL_TAG}` + build.args 同步 | -3 / +5 行 |
| [cmd/ikev2-panel/main.go](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go) | 启动时读版本文件 + logger.Info 加 version 字段 | +10 行 |
| [README.md](file:///opt/ikev2-panel-v2-main/README.md) | 同步 v2-80 → v2-85(7 处)+ 加 "验证部署" 小节 | -10 / +25 行 |
| [.env.example](file:///opt/ikev2-panel-v2-main/.env.example) | 加 `PANEL_TAG=v2.85` 说明 | +3 行 |

**总计**:**+60 行,改动小,review 简单**。

---

## 测试方案

### 自动化测试

```bash
# 1. 单元测试(确保 main.go 改动不破坏)
go test ./...

# 2. 编译检查(确保新 main.go 编译通过)
CGO_ENABLED=0 go build -o /tmp/test-panel ./cmd/ikev2-panel
# (沙箱无 gcc 但 CGO_ENABLED=0 仍可编)

# 3. dev 模式启动,确认 fallback 到 "dev"
mkdir -p /tmp/panel-test
IKEV2_DATA_DIR=/tmp/panel-test IKEV2_LISTEN_ADDR=127.0.0.1:8443 \
    IKEV2_LOG_LEVEL=debug IKEV2_SERVER_CN=test.local \
    IKEV2_COOKIE_SECURE=false /tmp/test-panel &
sleep 2
docker logs / grep "version="
# 期望:version=dev(因为本地无 /etc/ikev2-panel-version)
kill %1
```

### Dockerfile runner-test target 验证(本地有 docker 时)

```bash
# 跑完整 race detector(可能 1-2 分钟)
docker build --target runner-test -t ikev2-panel-race:test .
docker run --rm ikev2-panel-race:test
# 期望:ok  github.com/.../internal/ddns
#      ok  github.com/.../internal/swanctl
#      ... 全 PASS
```

### 部署一致性验证(模拟新用户)

```bash
# 1. README 步骤逐条跑通
docker build --network=host --build-arg PANEL_VERSION=v2.85 -t ikev2-panel:v2.85 .
docker compose up -d
docker compose logs ikev2-panel 2>&1 | head -20
# 期望:看到 version=v2.85,看到默认密码

# 2. 验证版本文件
docker compose exec ikev2-panel cat /etc/ikev2-panel-version
# 期望:v2.85
```

### README 链接 + 锚点验证

- README 所有提到 `v2-80` 的位置都改为 `v2-85`(grep 确认 0 处残留)
- README 提到的字段名跟 docker-compose.yml 的 env 名称一致(`IKEV2_DOMAIN` / `Ali_Key` / `Ali_Secret` 等)

---

## 风险评估

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| `/etc/ikev2-panel-version` 写入失败(runtime stage) | 极低 | 启动 fallback "dev" 不影响功能 | `RUN echo ...` 是基础 shell 命令 |
| main.go 读文件失败 → 启动崩 | 极低 | 容器起不来 | 已加 `if err != nil` fallback |
| 用户没改 PANEL_TAG → compose 默认 v2.85 跟镜像名不匹配 | 低 | 用户没 build 就 compose up,找不到镜像 | README 明确 `docker build --build-arg PANEL_VERSION=v2.85 ...`,README 验证小节给完整命令链 |
| runner-test target 增加 build 时间 | 中 | CI 多 1 分钟 | 只在 CI 跑,本地默认 build 不受影响 |

---

## 不破坏兼容性的承诺

- **现有 v2.84 用户升级**:`PANEL_TAG` 默认 `v2.85`,但用户**只跑 `docker compose up -d`** 会触发 `build: .`,看到 ARG 默认 `dev` → 启动日志 `version=dev`。**不影响功能**。如果要看到 `v2.85`,用户需 `export PANEL_TAG=v2.85` 后 build。
- **PANEL_TAG 缺失**:`${PANEL_TAG:-v2.85}` 默认值兜底。
- **runtime stage 不变**:已部署用户的 volume / 网络 / 端口 100% 不受影响。
- **/etc/ikev2-panel-version 不存在**(老用户从 v2.84 升级 image 后第一次起):main.go 读不到 → fallback "dev"。

---

## 后续 PR 关联

本 PR 是 v2.85 release 第 1 个,**只做**部署一致 + race-test 铺路。后续 PR:
- **PR-2**:默认密码持久化到文件 + bcrypt cost → 12
- **PR-3**:时区统一 + 日志路径修复
- **PR-4**:main.go 统一 WaitGroup
- **PR-5**:alidns 错误码精确匹配 + HMAC-SHA256
- **PR-6**:DDNS throttle 持久化 + stopCh sync.Once + collector REKEYING
- **PR-7**:tc IPv6 限速

每个 PR 独立,可单独 revert。

---

## 评审检查项(给自己)

- [x] 改动不超过 60 行
- [x] 不引入新依赖
- [x] 不改任何 Go 函数签名 / 接口
- [x] runtime 镜像体积不变(gcc 在 runner-test target,runtime stage 不装)
- [x] dev 模式(本地 go build)不受影响
- [x] 用户升级路径清晰(README 验证小节)
- [x] 测试方案具体可执行(三条命令)

---

## 工作量分解

| 步骤 | 时间 |
|------|------|
| Dockerfile 改(runtime ARG + runner-test target) | 0.1d |
| main.go 改(读版本文件 + logger) | 0.05d |
| docker-compose.yml 改(image + build.args) | 0.05d |
| README 改(7 处 v2-80 → v2-85 + 验证小节) | 0.1d |
| .env.example 加 PANEL_TAG | 0.02d |
| 跑测试(go test + docker build runner-test) | 0.08d |
| **合计** | **0.4d** |

---

## 实施 Checklist(执行时用)

```markdown
- [ ] Dockerfile runtime stage 加 ARG PANEL_VERSION + 写入版本文件
- [ ] Dockerfile 加 runner-test target
- [ ] docker-compose.yml image + build.args 改 ${PANEL_TAG}
- [ ] main.go 启动时读版本文件 + logger.Info version 字段
- [ ] README 7 处 v2-80 → v2-85
- [ ] README 加 "验证部署正确性" 小节
- [ ] .env.example 加 PANEL_TAG=v2.85
- [ ] go test ./... PASS
- [ ] CGO_ENABLED=0 go build ./cmd/ikev2-panel PASS
- [ ] docker build --target runner-test PASS(本地或 CI)
- [ ] git commit "v2.85-PR1: deployment consistency + Dockerfile runner-test target"
```

---

> 关联:
> - [audit-2026-09-phase2-4.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-phase2-4.md) §"Phase 2-4 合并 Top-10"
> - [audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) Phase 1 Top-10
> - [release-notes-v2.85.md](file:///opt/ikev2-panel-v2-main/docs/release-notes-v2.85.md)(待 PR 合入后写)
