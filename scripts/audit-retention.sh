#!/bin/bash
# Phase 5 A:audit 日志 retention。
#
# 每天凌晨 04:00 跑,删除 N 天前的 audit_log 记录。
# 默认 90 天,可通过 IKEV2_AUDIT_RETENTION_DAYS env 覆盖。
#
# 设计:
#   - 用 sqlite3 直接 DELETE + VACUUM(可选)
#   - 保留天数日志输出到 /var/log/ikev2-audit-retention.log
#   - VACUUM 用 INCREMENTAL(避免长锁表),如不可用则省略
#   - 失败不报错,只 WARN(审计清理不影响业务)
#
# 用法:
#   /etc/ikev2-panel/scripts/audit-retention.sh
#   或 IKEV2_AUDIT_RETENTION_DAYS=180 /etc/ikev2-panel/scripts/audit-retention.sh

set -uo pipefail

LOG_PREFIX="[audit-retention]"
LOG_FILE="${IKEV2_AUDIT_RETENTION_LOG:-/var/log/ikev2-audit-retention.log}"
DB_PATH="${IKEV2_PANEL_DB:-/data/panel.db}"
RETENTION_DAYS="${IKEV2_AUDIT_RETENTION_DAYS:-90}"

log() {
  local msg="$1"
  echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') ${LOG_PREFIX} ${msg}" | tee -a "${LOG_FILE}" >&2
}

# 1) 检查 DB 存在
if [ ! -f "${DB_PATH}" ]; then
  log "WARN: panel.db not found at ${DB_PATH}, skipping"
  exit 0
fi

# 2) 检查 sqlite3 可用
if ! command -v sqlite3 >/dev/null 2>&1; then
  log "WARN: sqlite3 not found in PATH, skipping (image may lack it)"
  exit 0
fi

# 3) 检查 audit_log 表存在(老 v2.84 升级用户可能没建)
HAS_TABLE=$(sqlite3 "${DB_PATH}" "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='audit_log';" 2>/dev/null)
if [ "${HAS_TABLE:-0}" = "0" ]; then
  log "WARN: audit_log table not present, skipping (Phase 5 在 v2.85+ 才有)"
  exit 0
fi

# 4) 计算 cutoff unix timestamp(秒)
CUTOFF_TS=$(date -u -d "-${RETENTION_DAYS} days" '+%s' 2>/dev/null || \
            date -u -v-"${RETENTION_DAYS}"d '+%s' 2>/dev/null || \
            echo 0)
if [ "${CUTOFF_TS}" = "0" ]; then
  log "FATAL: cannot compute cutoff timestamp"
  exit 1
fi

# 5) 统计 + 删
BEFORE=$(sqlite3 "${DB_PATH}" "SELECT count(*) FROM audit_log;" 2>/dev/null || echo "?")
DELETED=$(sqlite3 "${DB_PATH}" \
  "DELETE FROM audit_log WHERE timestamp < ${CUTOFF_TS} * 1000000000;" 2>&1 \
  | tee -a "${LOG_FILE}" \
  | head -1 || echo "0")
# 注意:audit_log.timestamp 是 unix nano,所以乘 1e9

# DELETED 实际是 sqlite3 的"被改的行数",但 sqlite3 DELETE 默认不输出 rowcount
# 用变通:先 SELECT 再 DELETE 拿准确删除数
DELETED=$(sqlite3 "${DB_PATH}" \
  "SELECT changes() FROM (DELETE FROM audit_log WHERE timestamp < ${CUTOFF_TS} * 1000000000);" 2>/dev/null \
  || echo "0")

# 6) VACUUM(可选,只 INCREMENTAL)
if sqlite3 "${DB_PATH}" "PRAGMA auto_vacuum = INCREMENTAL; VACUUM;" 2>/dev/null; then
  VACUUM_STATUS="incremental_vacuum_ok"
else
  VACUUM_STATUS="vacuum_skipped_or_unsupported"
fi

AFTER=$(sqlite3 "${DB_PATH}" "SELECT count(*) FROM audit_log;" 2>/dev/null || echo "?")

log "OK: retention=${RETENTION_DAYS}d cutoff_ts=${CUTOFF_TS} before=${BEFORE} deleted=${DELETED} after=${AFTER} ${VACUUM_STATUS}"

# 7) 限制 log 文件大小(最多 1MB,保留最近)
if [ -f "${LOG_FILE}" ]; then
  SIZE=$(stat -c%s "${LOG_FILE}" 2>/dev/null || stat -f%z "${LOG_FILE}" 2>/dev/null || echo 0)
  if [ "${SIZE}" -gt 1048576 ]; then
    mv "${LOG_FILE}" "${LOG_FILE}.old"
    log "rotated ${LOG_FILE} (was ${SIZE} bytes)"
  fi
fi

exit 0
