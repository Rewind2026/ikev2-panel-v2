#!/bin/bash
# ============================================================
# ikev2-panel-v2 — 数据备份 / 恢复 / 回滚工具
# 基线: d04ce38 (baseline-20261008)
#
# 用法:
#   ./scripts/backup-restore.sh backup              # 全量备份(代码 tag + 运行时数据)
#   ./scripts/backup-restore.sh backup-data         # 仅备份运行时数据
#   ./scripts/backup-restore.sh list                # 列出可用备份
#   ./scripts/backup-restore.sh verify <dir>        # 校验备份完整性
#   ./scripts/backup-restore.sh restore <dir>       # 恢复运行时数据
#   ./scripts/backup-restore.sh rollback <ref>      # 代码回滚到指定 commit/tag
#   ./scripts/backup-restore.sh snapshot <名称>      # 改代码前打本地快照(等价 git tag)
# ============================================================
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$REPO_ROOT" || { echo "无法进入仓库目录: $REPO_ROOT"; exit 1; }
BACKUP_ROOT="${BACKUP_ROOT:-/opt/ikev2-backups}"
BASELINE_TAG="baseline-20261008"
TS="$(date +%Y%m%d-%H%M%S)"

RED=$'\033[31m'; GRN=$'\033[32m'; YLW=$'\033[33m'; CYN=$'\033[36m'; RST=$'\033[0m'
ok()   { echo "${GRN}[ OK ]${RST} $*"; }
err()  { echo "${RED}[ERR ]${RST} $*"; }
warn() { echo "${YLW}[WARN]${RST} $*"; }
info() { echo "${CYN}[INFO]${RST} $*"; }

# 运行时数据位置(容器内路径 -> 宿主可选路径)
DATA_SOURCES=(
  "${DATA_DIR:-/data}/panel.db"
  "${DATA_DIR:-/data}/panel-state"
  "${DATA_DIR:-/data}/le"
)

usage() { sed -n '2,20p' "$0" | sed 's/^# \?//'; exit 1; }

cmd_backup_data() {
  local dest="$BACKUP_ROOT/data-$TS"
  mkdir -p "$dest" || { err "无法创建 $dest"; return 1; }
  info "备份运行时数据 -> $dest"
  local n=0
  for src in "${DATA_SOURCES[@]}"; do
    if [ -e "$src" ]; then
      cp -a "$src" "$dest/" 2>/dev/null && ok "已备份 $src" && n=$((n+1))
      warn "跳过 $src(权限或不存在)"
    else
      info "不存在,跳过: $src"
    fi
  done
  if [ "$n" -eq 0 ]; then
    warn "无任何运行时数据可备份(尚未部署?DATA_DIR 默认 /data)"
    info "提示: 本地开发数据在 $REPO_ROOT/dev-data,可用 DATA_DIR=$REPO_ROOT/dev-data 重试"
  fi
  # 元信息
  {
    echo "created_at: $(date -Iseconds)"
    echo "git_commit: $(git rev-parse HEAD 2>/dev/null)"
    echo "git_branch: $(git rev-parse --abbrev-ref HEAD 2>/dev/null)"
    echo "data_dir:   ${DATA_DIR:-/data}"
    echo "file_count: $n"
  } > "$dest/MANIFEST.txt"
  ok "备份完成: $dest (含 MANIFEST.txt)"
}

cmd_backup() {
  cmd_backup_data || return 1
  local dest="$BACKUP_ROOT/data-$TS"
  # 代码快照:用 git bundle 保存完整历史,即使 .git 被破坏也能恢复
  if git bundle create "$dest/repo.bundle" --all >/dev/null 2>&1; then
    ok "代码 bundle 已保存: $dest/repo.bundle ($(du -h "$dest/repo.bundle" | cut -f1))"
  else
    warn "git bundle 创建失败(不影响数据备份)"
  fi
  info "回滚基线 tag: $BASELINE_TAG -> $(git rev-parse --short "$BASELINE_TAG" 2>/dev/null || echo 'N/A')"
}

cmd_list() {
  if [ ! -d "$BACKUP_ROOT" ]; then warn "无备份目录: $BACKUP_ROOT"; return 0; fi
  info "备份列表 ($BACKUP_ROOT):"
  local d
  for d in "$BACKUP_ROOT"/data-*; do
    [ -d "$d" ] || continue
    local size files
    size="$(du -sh "$d" 2>/dev/null | cut -f1)"
    files="$(ls -1 "$d" 2>/dev/null | tr '\n' ' ')"
    echo "  ${CYN}$(basename "$d")${RST}  [$size]  $files"
  done
  echo ""
  info "当前代码位置: $(git rev-parse --short HEAD) ($(git rev-parse --abbrev-ref HEAD))"
  info "回滚基线: $BASELINE_TAG"
}

cmd_verify() {
  local d="${1:-}"
  [ -z "$d" ] && { err "用法: verify <备份目录>"; return 1; }
  [ -d "$d" ] || { err "目录不存在: $d"; return 1; }
  local bad=0
  [ -f "$d/MANIFEST.txt" ] && ok "MANIFEST.txt 存在" || { warn "缺 MANIFEST.txt"; bad=1; }
  if [ -f "$d/panel.db" ]; then
    if command -v sqlite3 >/dev/null 2>&1; then
      if sqlite3 "$d/panel.db" "PRAGMA integrity_check;" 2>/dev/null | grep -q '^ok$'; then
        ok "panel.db SQLite 完整性检查通过"
      else
        err "panel.db 损坏!"; bad=1
      fi
    else
      info "sqlite3 未安装,跳过 DB 完整性校验"
      # 至少检查文件头
      if [ "$(head -c 15 "$d/panel.db")" = "SQLite format 3" ]; then
        ok "panel.db 文件头有效"
      else
        err "panel.db 文件头异常"; bad=1
      fi
    fi
  fi
  [ -d "$d/panel-state" ] && ok "panel-state 存在 ($(ls -1 "$d/panel-state" 2>/dev/null | wc -l) 个条目)" || warn "缺 panel-state"
  if [ -f "$d/repo.bundle" ]; then
    if git bundle verify "$d/repo.bundle" >/dev/null 2>&1; then ok "repo.bundle 有效"
    else err "repo.bundle 损坏"; bad=1; fi
  fi
  [ "$bad" -eq 0 ] && ok "备份校验通过" || err "备份存在问题"
  return $bad
}

cmd_restore() {
  local d="${1:-}"
  [ -z "$d" ] && { err "用法: restore <备份目录>"; return 1; }
  [ -d "$d" ] || { err "目录不存在: $d"; return 1; }
  warn "即将用 $d 覆盖当前运行时数据。此操作不可撤销。"
  echo -n "确认继续? 输入 'yes' 继续: "
  read -r ans
  [ "$ans" = "yes" ] || { info "已取消"; return 0; }
  local target="${DATA_DIR:-/data}"
  mkdir -p "$target" || return 1
  for item in panel.db panel.db-wal panel.db-shm panel-state le; do
    if [ -e "$d/$item" ]; then
      rm -rf "${target:?}/$item"
      cp -a "$d/$item" "$target/" && ok "已恢复 $target/$item"
    fi
  done
  # 权限修复:panel.db 必须 0600(含全部 VPN 明文密码)
  if [ -f "$target/panel.db" ]; then
    chmod 600 "$target/panel.db" && ok "panel.db 权限已设为 0600"
  fi
  ok "恢复完成。请重启服务: docker compose restart ikev2-panel"
}

cmd_rollback() {
  local ref="${1:-$BASELINE_TAG}"
  warn "代码回滚到: $ref"
  info "当前 HEAD: $(git rev-parse --short HEAD)"
  echo ""
  echo "  提示: 本脚本不自动执行 reset。安全回滚步骤:"
  echo "    1) 查看差异:  git diff $ref..HEAD --stat"
  echo "    2) 放弃本地改动: git checkout -- ."
  echo "    3) 另建回滚分支(不破坏历史):"
  echo "       git checkout -B rollback/$TS $ref"
  echo "    4) 验证后合并回 main,或直接在此分支构建部署"
  echo ""
  if git rev-parse --verify "$ref" >/dev/null 2>&1; then
    ok "目标 ref 存在: $ref ($(git rev-parse --short "$ref"))"
  else
    err "目标 ref 不存在: $ref"; return 1
  fi
}

cmd_snapshot() {
  local name="${1:-snap-$TS}"
  git tag -a "$name" -m "修改前快照 $TS" 2>/dev/null \
    && ok "已创建快照 tag: $name" \
    || { err "创建失败(tag 已存在?)"; return 1; }
  info "回滚命令: git checkout -B rollback $name"
}

case "${1:-}" in
  backup)         cmd_backup ;;
  backup-data)    cmd_backup_data ;;
  list)           cmd_list ;;
  verify)         shift; cmd_verify "$@" ;;
  restore)        shift; cmd_restore "$@" ;;
  rollback)       shift; cmd_rollback "$@" ;;
  snapshot)       shift; cmd_snapshot "$@" ;;
  *)              usage ;;
esac
