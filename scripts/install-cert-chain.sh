#!/bin/sh
# install-cert-chain.sh — fullchain 拆分安装到 strongSwan 证书目录
# 设计说明见 docs/architecture.md §4.6
#
# v2.86-PR21 重大改动:替换 v2.86-PR19(A11) 的"剥根 + 整份 fullchain 单文件拷贝"。
#
# 背景(2026-10-29,130 实机 Win11 排障):
#   strongSwan 每个 .pem 文件只加载**第一张**证书,PEM 里多张合一份时后续
#   证书被静默忽略 —— 旧逻辑把 3 张 fullchain 原样拷进 x509/ 后,charon 只认得
#   叶子,服务器 IKE_AUTH 从不下发中间 CA。iOS / strongSwan-app 客户端会自己
#   做 AIA 补链所以能过;Windows IKEEXT 用系统根库构建链、不做 AIA 补链,
#   "中间 CA 缺失"必然验证失败。
#
# 本脚本的正确姿势(与 130 实机运行时验证过的目标状态一致:
# 叶子入 x509/ + 链 CA 逐张入 x509ca/ + send_cert=always → Win11 与 iPhone 双通过):
#   1. fullchain 按 PEM 块拆分(awk,源文件只读不改)
#   2. 第 1 张(叶子) → /etc/swanctl/x509/<domain>.pem
#   3. 其余每张(中间 CA / 交叉签发根)逐张单独 → /etc/swanctl/x509ca/<domain>-chain-<i>.pem
#      注:x509ca/ 里的自签根只作信任锚不会下发给客户端;非自签链成员
#      (含 LE 2025+ 的交叉签发根 ISRG Root YR ← X1)在 send_cert=always 时
#      作为链一环发送 —— 这正是 Windows 需要的
#   4. AIA 向上补链:链内最深一张若非自签,读其 CA Issuers URI 下载 issuer
#      (curl 优先,wget 兜底),指纹去重防循环,直到自签根 / 无 AIA / 超过
#      MAX_AIA_DEPTH。fullchain 里已有全部 CA 时此步自然空转,兜底给
#      "只有叶子"的非标 fullchain
#
# 用法: install-cert-chain.sh <domain> <fullchain.pem>
#   - POSIX sh(busybox 兼容),镜像内由 entrypoint.sh §7.5 与 ikev2-reload.sh 调用
#   - 幂等:安装前先清理本域名的旧 chain-*/issuer-* 文件
#   - 致命错误(文件缺失 / 无证书 / 叶子安装失败)exit 非 0,调用方自行回退旧拷贝;
#     AIA 下载失败只 WARN 不算失败(链内 CA 已装好,不影响主要修复)
set -eu

LOG_PREFIX="[cert-chain]"
SWAN_X509_DIR="/etc/swanctl/x509"
SWAN_X509CA_DIR="/etc/swanctl/x509ca"
MAX_AIA_DEPTH=4

DOMAIN="${1:?usage: install-cert-chain.sh <domain> <fullchain.pem>}"
FULLCHAIN="${2:?usage: install-cert-chain.sh <domain> <fullchain.pem>}"

fatal() {
  echo "${LOG_PREFIX} FATAL: $1" >&2
  exit 2
}

if [ ! -f "${FULLCHAIN}" ]; then
  fatal "fullchain not found: ${FULLCHAIN}"
fi

CERT_COUNT=$(grep -c "BEGIN CERTIFICATE" "${FULLCHAIN}" 2>/dev/null || true)
case "${CERT_COUNT}" in
  ''|*[!0-9]*) CERT_COUNT=0 ;;
esac
if [ "${CERT_COUNT}" -lt 1 ]; then
  fatal "no certificate found in ${FULLCHAIN}"
fi

# 取 sha256 指纹(openssl 失败时输出空,调用方判空)
fp_of() {
  openssl x509 -in "$1" -noout -fingerprint -sha256 2>/dev/null \
    | sed 's/^.*=//' | head -1
}

# ---------- 1. 拆分(临时目录,源 fullchain 不动)----------
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

# awk 按证书边界计数:第 1 张 → leaf.pem,第 n>=2 张 → chain-<n>.pem。
# 同一文件名 awk 只 open 一次、后续 print 追加,多行 PEM 不会互相覆盖
awk -v d="${TMP_DIR}" '
  /-----BEGIN CERTIFICATE-----/ { n++ }
  n == 1 { print > d "/leaf.pem" }
  n >= 2 { print >> d "/chain-" n ".pem" }
' "${FULLCHAIN}"

if [ ! -f "${TMP_DIR}/leaf.pem" ]; then
  fatal "split produced no leaf cert"
fi
if ! openssl x509 -in "${TMP_DIR}/leaf.pem" -noout >/dev/null 2>&1; then
  fatal "leaf cert parse failed (corrupt fullchain?)"
fi

# ---------- 2. 指纹预置(防 AIA 又拉回链内已有证书造成循环)----------
SEEN_FP="${TMP_DIR}/seen.fp"
_fp=$(fp_of "${TMP_DIR}/leaf.pem")
[ -n "${_fp}" ] && echo "${_fp}" >> "${SEEN_FP}"
_split_n=2
while [ -f "${TMP_DIR}/chain-${_split_n}.pem" ]; do
  _fp=$(fp_of "${TMP_DIR}/chain-${_split_n}.pem")
  [ -n "${_fp}" ] && echo "${_fp}" >> "${SEEN_FP}"
  _split_n=$((_split_n + 1))
done

# ---------- 3. 幂等清理 + 链 CA 逐张安装(每文件一张)----------
mkdir -p "${SWAN_X509_DIR}" "${SWAN_X509CA_DIR}"
rm -f "${SWAN_X509CA_DIR}/${DOMAIN}"-chain-*.pem \
      "${SWAN_X509CA_DIR}/${DOMAIN}"-issuer-*.pem

_ca_idx=0
_n=2
while [ -f "${TMP_DIR}/chain-${_n}.pem" ]; do
  _ca_idx=$((_ca_idx + 1))
  _dst="${SWAN_X509CA_DIR}/${DOMAIN}-chain-${_ca_idx}.pem"
  install -m 644 "${TMP_DIR}/chain-${_n}.pem" "${_dst}.tmp.$$"
  mv -f "${_dst}.tmp.$$" "${_dst}"
  _subj=$(openssl x509 -in "${TMP_DIR}/chain-${_n}.pem" -noout -subject 2>/dev/null \
            | sed 's/^subject=//' | head -1)
  echo "${LOG_PREFIX} chain CA #${_ca_idx} -> ${_dst} (${_subj})"
  _n=$((_n + 1))
done

# ---------- 4. AIA 向上补链(从链内最深一张开始)----------
# _deepest:链内最深证书序号(>=2);fullchain 只有叶子时从叶子开始补
_deepest=1
_n=2
while [ -f "${TMP_DIR}/chain-${_n}.pem" ]; do
  _deepest=${_n}
  _n=$((_n + 1))
done
_cur="${TMP_DIR}/leaf.pem"
if [ "${_deepest}" -ge 2 ]; then
  _cur="${TMP_DIR}/chain-${_deepest}.pem"
fi

_issuer_idx=0
_depth=0
while [ "${_depth}" -lt "${MAX_AIA_DEPTH}" ]; do
  _subj=$(openssl x509 -in "${_cur}" -noout -subject 2>/dev/null | sed 's/^subject=//' | head -1)
  _iss=$(openssl x509 -in "${_cur}" -noout -issuer 2>/dev/null | sed 's/^issuer=//' | head -1)
  if [ -n "${_subj}" ] && [ "${_subj}" = "${_iss}" ]; then
    echo "${LOG_PREFIX} reached self-signed root (trust anchor), AIA walk done"
    break
  fi
  _aia_uri=$(openssl x509 -in "${_cur}" -noout -text 2>/dev/null \
    | sed -n 's/.*CA Issuers - URI:\(http:\/\/[^ ]*\).*/\1/p' | head -1)
  if [ -z "${_aia_uri}" ]; then
    echo "${LOG_PREFIX} WARN: no AIA CA Issuers URI on (${_subj}), stop upward walk" >&2
    break
  fi
  _dl="${TMP_DIR}/aia-${_depth}.bin"
  if ! curl -fsS --max-time 15 -o "${_dl}" "${_aia_uri}" 2>/dev/null; then
    if command -v wget >/dev/null 2>&1; then
      wget -q -T 15 -O "${_dl}" "${_aia_uri}" 2>/dev/null || true
    fi
    if [ ! -s "${_dl}" ]; then
      echo "${LOG_PREFIX} WARN: AIA download failed: ${_aia_uri}" >&2
      break
    fi
  fi
  _issuer_file="${TMP_DIR}/aia-${_depth}.pem"
  # LE 的 CA Issuers 返回 DER;失败则当 PEM 原样试
  if ! openssl x509 -inform DER -in "${_dl}" -out "${_issuer_file}" 2>/dev/null; then
    cp "${_dl}" "${_issuer_file}"
  fi
  if ! openssl x509 -in "${_issuer_file}" -noout >/dev/null 2>&1; then
    echo "${LOG_PREFIX} WARN: AIA payload is not a certificate: ${_aia_uri}" >&2
    break
  fi
  _fp=$(fp_of "${_issuer_file}")
  if [ -n "${_fp}" ] && [ -f "${SEEN_FP}" ] && grep -Fxq "${_fp}" "${SEEN_FP}"; then
    echo "${LOG_PREFIX} issuer already known (fingerprint dedup), AIA walk done"
    break
  fi
  [ -n "${_fp}" ] && echo "${_fp}" >> "${SEEN_FP}"
  _issuer_idx=$((_issuer_idx + 1))
  _dst="${SWAN_X509CA_DIR}/${DOMAIN}-issuer-${_issuer_idx}.pem"
  install -m 644 "${_issuer_file}" "${_dst}.tmp.$$"
  mv -f "${_dst}.tmp.$$" "${_dst}"
  echo "${LOG_PREFIX} AIA issuer #${_issuer_idx} -> ${_dst} (from ${_aia_uri})"
  _cur=${_issuer_file}
  _depth=$((_depth + 1))
done

# ---------- 5. 叶子最后安装(先有 CA 再有叶子,方便人读日志)----------
_dst="${SWAN_X509_DIR}/${DOMAIN}.pem"
install -m 644 "${TMP_DIR}/leaf.pem" "${_dst}.tmp.$$"
mv -f "${_dst}.tmp.$$" "${_dst}"
echo "${LOG_PREFIX} leaf -> ${_dst} (total: leaf + ${_ca_idx} chain CA + ${_issuer_idx} AIA issuer)"
