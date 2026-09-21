#!/usr/bin/env bash
#
# Forward2Any 一键安装 / 升级 / 卸载（Linux + systemd）。
#
#   curl -fsSL https://raw.githubusercontent.com/loarland/Forward2Any/main/scripts/install.sh | sudo bash
#
# 或者先下载再跑（推荐，能看到内容）：
#
#   curl -fsSLO https://raw.githubusercontent.com/loarland/Forward2Any/main/scripts/install.sh
#   sudo bash install.sh
#
# 子命令：
#   install（默认）  装二进制 + 建用户和数据目录 + 写 systemd 单元 + 启动
#   upgrade          只换二进制和单元文件，环境变量文件与数据一概不动
#   status           看服务状态、版本和健康检查
#   uninstall        停服务、删单元和二进制，**保留数据**
#   uninstall --purge  连数据目录、环境变量文件和系统用户一起删（不可恢复）
#
# 首次安装可以覆盖的环境变量（都会写进 /etc/forward2any.env）：
#   F2A_PORT            监听端口，默认 16000
#   F2A_BASE_URL        回调基址，默认 http://<本机 IP>:<端口>
#                      **套了反向代理就一定要显式指定外部地址**，否则回调 URL 是错的
#   F2A_ADMIN_USER      管理员用户名，默认 admin
#   F2A_ADMIN_PASSWORD  管理员密码，默认随机生成并打印出来
#   F2A_LOG_LEVEL       debug / info / warn / error，默认 info
#   F2A_DATA_DIR        数据目录，默认 /var/lib/forward2any
#
# 其它：
#   F2A_VERSION         指定版本号（如 1.0.0），默认取最新 release
#   F2A_REPO            改成你自己的 owner/repo（自建镜像源时用）
#   F2A_SKIP_CHECKSUM   设 1 跳过 sha256 校验（不建议）
#
# 这个脚本只做四件事：下载发布包、装二进制、写单元文件、启服务。
# 落地的四个路径是约定死的，forward2any.service 和 README 都按它来：
#   二进制 /usr/local/bin/f2a，数据 /var/lib/forward2any，
#   环境变量 /etc/forward2any.env，运行用户 f2a。

set -euo pipefail

REPO="${F2A_REPO:-loarland/Forward2Any}"
SERVICE="forward2any"
BIN="/usr/local/bin/f2a"
UNIT="/etc/systemd/system/${SERVICE}.service"
ENV_FILE="/etc/forward2any.env"
DATA_DIR="${F2A_DATA_DIR:-/var/lib/forward2any}"
RUN_USER="f2a"
PORT="${F2A_PORT:-16000}"
BASE_URL="${F2A_BASE_URL:-}"
ADMIN_USER="${F2A_ADMIN_USER:-admin}"
ADMIN_PASS="${F2A_ADMIN_PASSWORD:-}"
LOG_LEVEL="${F2A_LOG_LEVEL:-info}"
VERSION="${F2A_VERSION:-}"
SKIP_CHECKSUM="${F2A_SKIP_CHECKSUM:-0}"

TMP_DIR=""
cleanup() { [ -n "$TMP_DIR" ] && rm -rf "$TMP_DIR"; }
trap cleanup EXIT

say()  { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf '⚠️  %s\n' "$*" >&2; }
die()  { printf '❌ %s\n' "$*" >&2; exit 1; }

need_root() {
  [ "$(id -u)" = "0" ] || die "需要 root 权限：sudo bash $SELF ${1:-install}"
}

need_systemd() {
  command -v systemctl >/dev/null 2>&1 || die "这台机器上没有 systemctl。容器/非 systemd 的发行版请改用 Docker 部署（见 README「快速开始」）"
  [ -d /run/systemd/system ] || die "systemd 没在跑（比如这是个容器）。请改用 Docker 部署，或在宿主机上安装"
}

need_curl() {
  command -v curl >/dev/null 2>&1 || die "需要 curl：apt install curl / yum install curl"
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "不支持的架构 $(uname -m)：发布包只有 amd64 和 arm64。其它架构请用 Docker，或自己 go build" ;;
  esac
}

# 从 /releases/latest 的重定向里拿到版本号，这样不用调 GitHub API（也就没有速率限制）
resolve_version() {
  if [ -n "$VERSION" ]; then
    printf '%s' "${VERSION#v}"
    return
  fi
  local url
  url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest")" \
    || die "拿不到最新版本号（网络不通？）。也可以显式指定：F2A_VERSION=1.0.0"
  case "$url" in
    */releases/tag/*) printf '%s' "${url##*/}" | sed 's/^v//' ;;
    *) die "解析最新版本号失败：$url" ;;
  esac
}

download() { # $1=url $2=目标文件
  curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1" \
    || die "下载失败：$1"
}

sha256_of() { # 优先 sha256sum，其次 shasum，最后 openssl
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    openssl dgst -sha256 "$1" | awk '{print $NF}'
  fi
}

# 从环境变量文件里取值（文件不存在或没这一项就打印空）
env_value() {
  [ -f "$ENV_FILE" ] || return 0
  sed -n "s/^$1=//p" "$ENV_FILE" | head -1
}

ensure_user() {
  if id -u "$RUN_USER" >/dev/null 2>&1; then
    return
  fi
  local nologin
  nologin="$(command -v nologin || echo /bin/false)"
  useradd --system --no-create-home --home-dir "$DATA_DIR" --shell "$nologin" "$RUN_USER" \
    || die "创建系统用户 $RUN_USER 失败"
  say "    建了系统用户 $RUN_USER"
}

ensure_data_dir() {
  install -d -m 0700 -o "$RUN_USER" -g "$RUN_USER" "$DATA_DIR"
  # 目录已存在时 install -d 不会改属主，这里再纠正一次，重装才幂等
  chown "$RUN_USER:$RUN_USER" "$DATA_DIR"
  chmod 0700 "$DATA_DIR"
}

check_port() {
  case "$PORT" in
    ''|*[!0-9]*) die "F2A_PORT 不是数字：$PORT" ;;
  esac
  if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
    die "F2A_PORT 超出范围（1-65535）：$PORT"
  fi
}

# 首次安装写环境变量文件；已经有了就原样保留（升级不该动用户的配置）
write_env_file() {
  if [ -f "$ENV_FILE" ]; then
    say "    环境变量文件已存在，保持不动：$ENV_FILE"
    return
  fi
  if [ -z "$BASE_URL" ]; then
    local ip
    ip="$(ip route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}')" || true
    [ -n "${ip:-}" ] || ip="$(hostname -I 2>/dev/null | awk '{print $1}')" || true
    [ -n "${ip:-}" ] || ip="localhost"
    BASE_URL="http://${ip}:${PORT}"
    AUTO_BASE_URL=1
  fi
  if [ -z "$ADMIN_PASS" ]; then
    # 只用来生成初始密码；openssl 不一定有，用 /dev/urandom 更保险
    ADMIN_PASS="$(head -c 18 /dev/urandom | base64 | tr -d '/+=' | cut -c1-20)"
    GENERATED_PASS=1
  fi
  cat > "$ENV_FILE" <<EOF
# Forward2Any 的启动参数（systemd 通过 EnvironmentFile 读它）。
#
# 注意：这些只在**首次启动**时作为引导值写进数据库，之后一律以后台「设置」页里的值为准 ——
# 在这里改了端口或密码，重启服务不会覆盖你在界面上改过的值。
#
# 由 scripts/install.sh 生成。改完执行：systemctl restart $SERVICE
F2A_DATA_DIR=$DATA_DIR
F2A_PORT=$PORT
F2A_ADMIN_USER=$ADMIN_USER
F2A_BASE_URL=$BASE_URL
F2A_LOG_LEVEL=$LOG_LEVEL
F2A_ADMIN_PASSWORD=$ADMIN_PASS
EOF
  chmod 0600 "$ENV_FILE"
  say "    写了环境变量文件 $ENV_FILE（0600）"
}

install_unit() { # $1=解包出来的目录
  [ -f "$1/forward2any.service" ] || die "发布包里没有 forward2any.service，装不了"
  install -m 0644 "$1/forward2any.service" "$UNIT"
  # 数据目录是可以换的，单元里的 ReadWritePaths 得跟着走，不然 ProtectSystem=strict 下写不进去
  if [ "$DATA_DIR" != "/var/lib/forward2any" ]; then
    sed -i "s|/var/lib/forward2any|$DATA_DIR|g" "$UNIT"
  fi
  # 端口小于 1024 时需要这一个能力才能绑定；默认 16000 不需要，所以默认不给。
  # 以环境变量文件里的端口为准 —— 已经有文件时（升级/重装）脚本上给的 F2A_PORT 不生效。
  local port
  port="$(env_value F2A_PORT)"
  [ -n "$port" ] || port="$PORT"
  if [ "$port" -lt 1024 ]; then
    sed -i "s|^\[Service\]$|[Service]\n# F2A_PORT=$port 小于 1024，绑定低端口需要这个能力\nAmbientCapabilities=CAP_NET_BIND_SERVICE\nCapabilityBoundingSet=CAP_NET_BIND_SERVICE|" "$UNIT"
    say "    端口 $port < 1024，单元里加了 CAP_NET_BIND_SERVICE"
  fi
  grep -qx "ExecStart=$BIN" "$UNIT" \
    || die "单元文件里的 ExecStart 不是 $BIN，跟本脚本的约定对不上，请检查 $UNIT"
  say "    装了 systemd 单元 $UNIT"
}

stop_service() {
  if systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
    say "    停掉正在跑的服务"
    systemctl stop "$SERVICE"
  fi
}

start_service() {
  systemctl daemon-reload
  systemctl enable "$SERVICE" >/dev/null 2>&1 || true
  systemctl restart "$SERVICE"
}

# 以**服务用户**的身份跑 healthcheck。
#
# 两件事都得注意：数据目录要显式给（脚本自己的环境里没有 F2A_DATA_DIR，不给就退回默认端口），
# 身份要切成 f2a —— 这个子命令会碰库和 -wal/-shm，用 root 跑会把它们建成 root 的，
# 之后服务自己反而打不开（第一次写这个脚本就这么翻的车）。
healthcheck_now() {
  if command -v runuser >/dev/null 2>&1; then
    runuser -u "$RUN_USER" -- env F2A_DATA_DIR="$DATA_DIR" "$BIN" healthcheck
  elif command -v su >/dev/null 2>&1; then
    su -s /bin/sh "$RUN_USER" -c "F2A_DATA_DIR='$DATA_DIR' '$BIN' healthcheck"
  else
    F2A_DATA_DIR="$DATA_DIR" "$BIN" healthcheck
  fi
}

wait_healthy() {
  sleep 1   # 让服务先把库建出来（首次启动时库还不存在）
  for _ in $(seq 40); do
    # 不带 --url：它会按数据库里记着的实际端口来探，设置页里改过端口也不会误判
    if healthcheck_now >/dev/null 2>&1; then
      return 0
    fi
    if ! systemctl is-active --quiet "$SERVICE"; then
      warn "服务没起来，最近日志："
      journalctl -u "$SERVICE" -n 20 --no-pager >&2 || true
      return 1
    fi
    sleep 1
  done
  warn "等了 40 秒还没通过健康检查，最近日志："
  journalctl -u "$SERVICE" -n 20 --no-pager >&2 || true
  return 1
}

print_summary() {
  local ip port pass
  ip="$(ip route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}')" || true
  [ -n "${ip:-}" ] || ip="<服务器地址>"
  port="$(grep -E '^F2A_PORT=' "$ENV_FILE" 2>/dev/null | cut -d= -f2)" || true
  [ -n "${port:-}" ] || port="$PORT"
  pass="$(grep -E '^F2A_ADMIN_PASSWORD=' "$ENV_FILE" 2>/dev/null | cut -d= -f2-)" || true

  say ""
  say "──────────────────────────────────────────────"
  say " Forward2Any 已装好并启动"
  say ""
  say "   后台地址   http://${ip}:${port}/"
  say "   管理员     ${ADMIN_USER} / ${pass:-（见 $ENV_FILE）}"
  say "   数据目录   ${DATA_DIR}"
  say "   环境变量   ${ENV_FILE}"
  say "   服务日志   journalctl -u ${SERVICE} -f"
  say "   看状态     sudo bash install.sh status"
  say "   升级       sudo bash install.sh upgrade"
  say "   卸载       sudo bash install.sh uninstall（加 --purge 连数据一起删）"
  say "──────────────────────────────────────────────"
  if [ "${GENERATED_PASS:-0}" = "1" ]; then
    say " 密码是随机生成的，只在这里和 $ENV_FILE 里出现过：现在记下来。"
  fi
  if [ "${AUTO_BASE_URL:-0}" = "1" ]; then
    say " 回调基址是自动猜的（$BASE_URL）。如果前面还套了反向代理或者要用域名，"
    say " 请到后台「设置 → 回调基址」改成外部真正能访问到的地址 —— 回调 URL 和 curl 示例都按它生成。"
  fi
  say ""
}

cmd_install() { # $1=upgrade 时为 1，表示"已有安装，只换二进制"
  local upgrading="${1:-0}"
  need_root
  need_systemd
  need_curl
  detect_arch
  check_port

  step "准备"
  if [ "$upgrading" = "1" ] && [ ! -f "$UNIT" ]; then
    say "    还没有装过，那就当首次安装"
    upgrading=0
  fi
  local ver tarball pkg
  ver="$(resolve_version)"
  pkg="forward2any_${ver}_linux_${ARCH}"
  tarball="${pkg}.tar.gz"
  say "    版本 ${ver}，平台 linux/${ARCH}"

  TMP_DIR="$(mktemp -d)"
  local base_url
  if [ -n "$VERSION" ]; then
    base_url="https://github.com/${REPO}/releases/download/v${ver}"
  else
    base_url="https://github.com/${REPO}/releases/latest/download"
  fi

  step "下载并校验"
  download "${base_url}/${tarball}" "$TMP_DIR/$tarball"
  if [ "$SKIP_CHECKSUM" = "1" ]; then
    warn "按 F2A_SKIP_CHECKSUM=1 跳过了 sha256 校验"
  else
    download "${base_url}/checksums.txt" "$TMP_DIR/checksums.txt"
    local want got
    want="$(awk -v f="$tarball" '$2 == f || $2 == "./" f {print $1}' "$TMP_DIR/checksums.txt")"
    [ -n "$want" ] || die "checksums.txt 里没有 $tarball 这一行"
    got="$(sha256_of "$TMP_DIR/$tarball")"
    [ "$want" = "$got" ] || die "sha256 校验不一致！期望 $want，实际 $got"
    say "    sha256 校验通过"
  fi
  tar -xzf "$TMP_DIR/$tarball" -C "$TMP_DIR"
  [ -x "$TMP_DIR/$pkg/f2a" ] || die "发布包里没有可执行的 f2a"
  say "    包内版本：$("$TMP_DIR/$pkg/f2a" version)"

  step "装文件"
  stop_service
  ensure_user
  ensure_data_dir
  install -m 0755 "$TMP_DIR/$pkg/f2a" "$BIN.new"
  mv -f "$BIN.new" "$BIN"
  say "    二进制 → $BIN"
  write_env_file
  # 环境变量文件是权威来源：里面的端口和数据目录决定单元文件怎么写
  DATA_DIR="$(env_value F2A_DATA_DIR)"; DATA_DIR="${DATA_DIR:-$DATA_DIR}"
  install_unit "$TMP_DIR/$pkg"

  step "启动"
  start_service
  wait_healthy || die "服务起来了但没通过健康检查，看上面日志"
  say "    健康检查通过"
  print_summary
}

cmd_status() {
  need_root
  say "== 版本 =="
  if [ -x "$BIN" ]; then
    "$BIN" version
  else
    say "（还没装）"
  fi
  say ""
  say "== systemd =="
  systemctl status "$SERVICE" --no-pager --lines=5 || true
  say ""
  say "== 健康检查 =="
  if [ -x "$BIN" ] && healthcheck_now; then
    say "✅ 通过"
  else
    say "❌ 没通过"
  fi
}

cmd_uninstall() {
  local purge="${1:-0}"
  need_root
  step "停服务"
  systemctl disable --now "$SERVICE" 2>/dev/null || true
  rm -f "$UNIT"
  systemctl daemon-reload 2>/dev/null || true
  say "    已停用并删掉单元文件"
  rm -f "$BIN"
  say "    已删掉 $BIN"

  if [ "$purge" = "1" ]; then
    step "清理数据（--purge）"
    rm -rf "$DATA_DIR" "$ENV_FILE"
    if id -u "$RUN_USER" >/dev/null 2>&1; then
      userdel "$RUN_USER" 2>/dev/null || true
      say "    已删掉系统用户 $RUN_USER"
    fi
    say "    已删掉 $DATA_DIR 和 $ENV_FILE"
  else
    say ""
    say "数据还在：$DATA_DIR（库里存着源、规则和投递日志）"
    say "连数据一起删：sudo bash install.sh uninstall --purge"
  fi
}

usage() {
  cat <<'EOF'
Forward2Any 一键安装 / 升级 / 卸载（Linux + systemd）

用法：
  sudo bash install.sh [子命令]

子命令：
  install（默认）     装二进制 + 建用户和数据目录 + 写 systemd 单元 + 启动
  upgrade             只换二进制和单元文件，环境变量文件与数据一概不动
  status              看服务状态、版本和健康检查
  uninstall           停服务、删单元和二进制，保留数据
  uninstall --purge   连数据目录、环境变量文件和系统用户一起删（不可恢复）

首次安装可覆盖的环境变量：F2A_PORT、F2A_BASE_URL、F2A_ADMIN_USER、
F2A_ADMIN_PASSWORD、F2A_LOG_LEVEL、F2A_DATA_DIR；另外还有 F2A_VERSION
（指定版本，默认最新）、F2A_REPO（自建镜像源）、F2A_SKIP_CHECKSUM=1（跳过校验）。

完整说明见脚本开头的注释，或 https://github.com/loarland/Forward2Any
EOF
}

SELF="${BASH_SOURCE[0]:-install.sh}"
case "${1:-install}" in
  install)   cmd_install 0 ;;
  upgrade)   cmd_install 1 ;;
  status)    cmd_status ;;
  uninstall)
    if [ "${2:-}" = "--purge" ]; then cmd_uninstall 1; else cmd_uninstall 0; fi
    ;;
  help|-h|--help) usage ;;
  *) die "不认识的子命令：$1（可用：install / upgrade / status / uninstall [--purge]）" ;;
esac
