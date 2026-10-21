#!/usr/bin/env bash
#
# ts3panel installer.
#
#   curl -fsSL https://raw.githubusercontent.com/yaodooo/ts3panel/main/scripts/install.sh | sudo bash
#   sudo bash scripts/install.sh --from-file ./ts3panel_1.0.0_linux_amd64.tar.gz
#
# The installer is idempotent: running it again upgrades the binary, refreshes
# the systemd unit and restarts the service without touching config.toml or
# state.json. It never creates a ts3panel service account: the panel runs as
# root because it owns the Docker socket anyway (see errata E4), and restrictive
# systemd sandboxing limits what it can write.

set -Eeuo pipefail
IFS=$'\n\t'

REPO="${TS3PANEL_REPO:-yaodooo/ts3panel}"
VERSION="${TS3PANEL_VERSION:-latest}"
FROM_FILE=""
SHA256_EXPECTED=""
BIN_DIR="/usr/local/bin"
START_SERVICE=1
CONFIG_DIR="/etc/ts3panel"
CONFIG_FILE="${CONFIG_DIR}/config.toml"
STATE_DIR="/var/lib/ts3panel"
UNIT_FILE="/etc/systemd/system/ts3panel.service"
DEFAULT_INSTANCE_ROOT="/srv/ts3"

TMP_DIR=""
BIN_SRC=""

if [[ -t 1 ]]; then
	COLOR_RED=$'\033[31m'
	COLOR_GREEN=$'\033[32m'
	COLOR_YELLOW=$'\033[33m'
	COLOR_RESET=$'\033[0m'
else
	COLOR_RED=""
	COLOR_GREEN=""
	COLOR_YELLOW=""
	COLOR_RESET=""
fi

info() { printf '%s[ts3panel]%s %s\n' "$COLOR_GREEN" "$COLOR_RESET" "$*"; }
warn() { printf '%s[ts3panel]%s %s\n' "$COLOR_YELLOW" "$COLOR_RESET" "$*" >&2; }
die() { printf '%s[ts3panel] 错误:%s %s\n' "$COLOR_RED" "$COLOR_RESET" "$*" >&2; exit 1; }

cleanup() {
	if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
		rm -rf -- "$TMP_DIR"
	fi
}
trap cleanup EXIT

usage() {
	cat <<'EOF'
用法: install.sh [选项]

选项:
  --from-file PATH    从本地 tar.gz（或裸二进制）安装，离线/内网环境使用
  --sha256 HEX        配合 --from-file 校验本地包
  --version VERSION   指定版本（默认 latest；也可写 v1.0.0）
  --repo OWNER/REPO   GitHub 仓库（默认 yaodooo/ts3panel）
  --bin-dir DIR       二进制安装目录（默认 /usr/local/bin）
  --no-start          只安装文件与 systemd unit，不启动服务（供打包/测试）
  -h, --help          显示本帮助

环境变量:
  TS3PANEL_REPO       同 --repo
  TS3PANEL_VERSION    同 --version
  TS3PANEL_GITHUB     可选 GitHub 镜像前缀，例如 https://ghproxy.example.com/https://github.com

示例:
  sudo bash install.sh
  sudo bash install.sh --version 1.0.0
  sudo bash install.sh --from-file ./ts3panel_1.0.0_linux_amd64.tar.gz
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--from-file)
		[[ $# -ge 2 ]] || die "--from-file 需要一个路径"
		FROM_FILE="$2"
		shift 2
		;;
	--sha256)
		[[ $# -ge 2 ]] || die "--sha256 需要一个十六进制摘要"
		SHA256_EXPECTED="$2"
		shift 2
		;;
	--version)
		[[ $# -ge 2 ]] || die "--version 需要一个版本号"
		VERSION="$2"
		shift 2
		;;
	--repo)
		[[ $# -ge 2 ]] || die "--repo 需要 OWNER/REPO 形式"
		REPO="$2"
		shift 2
		;;
	--bin-dir)
		[[ $# -ge 2 ]] || die "--bin-dir 需要一个目录"
		BIN_DIR="$2"
		shift 2
		;;
	--no-start)
		START_SERVICE=0
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		die "未知参数: $1（使用 --help 查看用法）"
		;;
	esac
done

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------

[[ "$(uname -s)" == "Linux" ]] || die "ts3panel 只支持 Linux 宿主机。"
[[ "${EUID}" -eq 0 ]] || die "请以 root 运行（sudo bash install.sh）。"
[[ "$BIN_DIR" != *[[:space:]]* ]] || die "--bin-dir 不能包含空白字符。"
command -v systemctl >/dev/null 2>&1 || die "缺少 systemd（systemctl 不存在）。"
[[ -d /run/systemd/system ]] || die "systemd 未在运行，无法安装为系统服务。"

if ! command -v docker >/dev/null 2>&1; then
	die "未发现 Docker。请先安装 Docker Engine 24+（Debian/Ubuntu 可参考 docs/deploy.md），再重新运行本脚本。"
fi

docker_ready() {
	if command -v timeout >/dev/null 2>&1; then
		timeout 10 docker info >/dev/null 2>&1
	else
		docker info >/dev/null 2>&1
	fi
}

if ! docker_ready; then
	warn "Docker 守护进程暂不可用，等待 10 秒后重试…"
	ok=0
	for _ in $(seq 1 10); do
		sleep 1
		if docker_ready; then ok=1; break; fi
	done
	[[ "$ok" -eq 1 ]] || die "Docker 守护进程不可用。请执行 systemctl start docker 后重试。"
fi

case "$(uname -m)" in
x86_64 | amd64) ARCH="amd64" ;;
aarch64 | arm64) ARCH="arm64" ;;
*) die "不支持的 CPU 架构: $(uname -m)（仅支持 amd64 / arm64）。" ;;
esac

TMP_DIR="$(mktemp -d /tmp/ts3panel-install.XXXXXX)"
mkdir -p "${TMP_DIR}/extract"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

http_get() {
	local url="$1" out="$2"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 2 --connect-timeout 15 -o "$out" "$url"
	else
		wget -q -O "$out" "$url"
	fi
}

rand_hex() {
	local bytes="$1" out=""
	if command -v openssl >/dev/null 2>&1; then
		out="$(openssl rand -hex "$bytes")"
	else
		out="$(head -c "$bytes" /dev/urandom | od -An -tx1 | tr -d ' \n')"
	fi
	[[ ${#out} -eq $((bytes * 2)) ]] || die "生成随机密钥失败。"
	printf '%s' "$out"
}

toml_string() {
	local key="$1" file="$2"
	sed -n "s/^[[:space:]]*${key}[[:space:]]*=[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$file" | head -n 1
}

normalize_version() {
	local v="${1#v}"
	v="${v#V}"
	printf '%s' "$v"
}

resolve_latest_version() {
	local effective tag=""
	if command -v curl >/dev/null 2>&1; then
		effective="$(curl -fsSLI -o /dev/null -w '%{url_effective}' --connect-timeout 15 \
			"https://github.com/${REPO}/releases/latest" 2>/dev/null || true)"
		case "$effective" in
		*/tag/v*) tag="${effective##*/tag/}" ;;
		esac
	fi
	if [[ -z "$tag" ]]; then
		local api="${TMP_DIR}/release.json"
		if http_get "https://api.github.com/repos/${REPO}/releases/latest" "$api" 2>/dev/null; then
			tag="$(sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "$api" | head -n 1)"
		fi
	fi
	[[ -n "$tag" ]] || die "无法获取最新版本（GitHub 不可达或仓库 ${REPO} 不存在）。请用 --version 或 --from-file。"
	printf '%s' "$(normalize_version "$tag")"
}

select_archive_binary() {
	local root="$1"
	local found=""
	found="$(find "$root" -type f -name ts3panel -print -quit 2>/dev/null || true)"
	[[ -n "$found" ]] || die "安装包中没有找到 ts3panel 二进制。"
	printf '%s' "$found"
}

download_release() {
	local version="$1"
	local asset="ts3panel_${version}_linux_${ARCH}.tar.gz"
	local base="${TS3PANEL_GITHUB:-https://github.com}/${REPO}/releases/download/v${version}"

	info "下载 ts3panel ${version} (${ARCH})…"
	http_get "${base}/${asset}" "${TMP_DIR}/pkg.tar.gz" ||
		die "下载失败: ${base}/${asset}
可改用 --from-file 从本地包安装，或检查服务器到 GitHub 的网络。"

	if http_get "${base}/ts3panel_${version}_checksums.txt" "${TMP_DIR}/checksums.txt"; then
		local line
		line="$(grep -F "  ${asset}" "${TMP_DIR}/checksums.txt" || true)"
		if [[ -n "$line" ]]; then
			(cd "$TMP_DIR" && printf '%s\n' "$line" | sha256sum -c - >/dev/null 2>&1) ||
				die "校验和验证失败，安装包可能损坏。"
			info "校验和验证通过。"
		else
			warn "校验和文件中没有 ${asset}，跳过校验。"
		fi
	else
		warn "未能下载校验和，跳过校验（可自行比对 Release 页面）。"
	fi

	tar -xzf "${TMP_DIR}/pkg.tar.gz" -C "${TMP_DIR}/extract"
	BIN_SRC="$(select_archive_binary "${TMP_DIR}/extract")"
}

prepare_from_file() {
	local path="$1"
	[[ -f "$path" ]] || die "找不到本地安装包: $path"

	if [[ -n "$SHA256_EXPECTED" ]]; then
		command -v sha256sum >/dev/null 2>&1 || die "缺少 sha256sum，无法校验。"
		local actual
		actual="$(sha256sum "$path" | awk '{print $1}')"
		[[ "${actual,,}" == "${SHA256_EXPECTED,,}" ]] || die "SHA256 不匹配（实际 ${actual}）。"
		info "SHA256 校验通过。"
	fi

	case "$path" in
	*.tar.gz | *.tgz)
		local member=""
		member="$(tar -tzf "$path" | awk '$0 == "ts3panel" || $0 ~ /(^|\/)ts3panel$/ { print; exit }')"
		[[ -n "$member" ]] || die "安装包中没有 ts3panel 二进制。"
		case "$member" in
		/* | ../* | */../*) die "安装包包含不安全路径: $member" ;;
		esac
		tar -xzf "$path" -C "${TMP_DIR}/extract" -- "$member"
		BIN_SRC="$(select_archive_binary "${TMP_DIR}/extract")"
		;;
	*)
		BIN_SRC="$path"
		;;
	esac
}

install_binary() {
	local src="$1"
	local target="${BIN_DIR}/ts3panel"
	install -d -m 0755 "$BIN_DIR"
	install -m 0755 "$src" "${target}.new.$$"
	mv -f "${target}.new.$$" "$target"
	info "已安装二进制: ${target}"
}

# ---------------------------------------------------------------------------
# Fetch
# ---------------------------------------------------------------------------

if [[ -n "$FROM_FILE" ]]; then
	[[ "$VERSION" == "latest" ]] || warn "--from-file 下忽略 --version（以包内二进制为准）。"
	prepare_from_file "$FROM_FILE"
else
	if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
		die "在线安装需要 curl 或 wget；离线环境请用 --from-file。"
	fi
	[[ "$VERSION" == "latest" ]] && VERSION="$(resolve_latest_version)"
	VERSION="$(normalize_version "$VERSION")"
	[[ "$VERSION" =~ ^[0-9A-Za-z._-]+$ ]] || die "版本号格式不正确: $VERSION"
	download_release "$VERSION"
fi

install_binary "$BIN_SRC"
installed_version="$("${BIN_DIR}/ts3panel" version 2>/dev/null | head -n 1 || true)"
[[ -n "$installed_version" ]] && info "二进制自检: ${installed_version}"

# ---------------------------------------------------------------------------
# Directories and configuration
# ---------------------------------------------------------------------------

install -d -m 0700 -o root -g root "$CONFIG_DIR"
install -d -m 0700 -o root -g root "$STATE_DIR"

if [[ ! -f "$CONFIG_FILE" ]]; then
	info "生成 ${CONFIG_FILE}（root:root 0600）…"
	session_secret="$(rand_hex 64)"
	master_key="$(rand_hex 32)"
	umask 077
	cat >"$CONFIG_FILE" <<EOF
# ts3panel configuration
# Generated by install.sh. Owned by root:root, mode 0600.
# It contains the session secret and the AES-GCM key for provider credentials.

listen = "127.0.0.1:8899"
instance_root = "${DEFAULT_INSTANCE_ROOT}"
session_secret = "${session_secret}"
setup_token = ""
master_key = "${master_key}"

[cloudflare]
public_ip = ""

[docker]
socket = "unix:///var/run/docker.sock"

[limits]
max_instances = 20
ts3_memory = "512m"
ts3_cpus = "1.0"
record_ttl = 300

[backup]
enabled = false
mode = "daily"
daily_at = "04:30"
interval_hours = 24
retention = 7

[ports]
external_check = false
check_cache_seconds = 60
EOF
else
	info "保留现有 ${CONFIG_FILE}（未覆盖）。"
fi

chown root:root "$CONFIG_FILE"
chmod 0600 "$CONFIG_FILE"

instance_root="$(toml_string "instance_root" "$CONFIG_FILE")"
[[ -n "$instance_root" ]] || instance_root="$DEFAULT_INSTANCE_ROOT"
case "$instance_root" in
/*) ;;
*) die "config.toml 中的 instance_root 必须是绝对路径: ${instance_root}" ;;
esac
[[ "$instance_root" != *[[:space:]]* ]] ||
	die "instance_root 不能包含空白字符: ${instance_root}"
# World-executable so the in-container ts3server user can traverse to its
# bind mount; the instance directories themselves are chowned by the image's
# entrypoint (errata E3), not by the panel.
install -d -m 0755 -o root -g root "$instance_root"
info "实例根目录: ${instance_root}"

# ---------------------------------------------------------------------------
# systemd unit (errata E4: User=root + sandboxing)
# ---------------------------------------------------------------------------

info "写入 systemd unit: ${UNIT_FILE}"
cat >"$UNIT_FILE" <<EOF
[Unit]
Description=ts3panel - TeamSpeak 3 multi-instance panel
Documentation=https://github.com/${REPO}
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
UMask=0077
ExecStart=${BIN_DIR}/ts3panel serve --config ${CONFIG_FILE}
Restart=always
RestartSec=3
TimeoutStopSec=20

# Sandbox: the panel only needs its own state/config, instance data and the
# Docker socket (a socket connect is not blocked by ProtectSystem=strict).
ProtectHome=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=${instance_root} ${STATE_DIR} ${CONFIG_DIR}
RestrictSUIDSGID=true
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF

chmod 0644 "$UNIT_FILE"
chown root:root "$UNIT_FILE"
systemctl daemon-reload
if [[ "$START_SERVICE" -eq 1 ]]; then
	if systemctl is-active --quiet ts3panel; then
		info "重启 ts3panel 服务…"
		systemctl restart ts3panel
	else
		info "启用并启动 ts3panel 服务…"
		systemctl enable --now ts3panel
	fi
else
	warn "已按 --no-start 跳过服务启动。"
fi

# ---------------------------------------------------------------------------
# Health check and summary
# ---------------------------------------------------------------------------

listen="$(toml_string "listen" "$CONFIG_FILE")"
[[ -n "$listen" ]] || listen="127.0.0.1:8899"
listen_host="${listen%:*}"
listen_port="${listen##*:}"
listen_is_loopback=0
case "$listen_host" in
"127.0.0.1" | "localhost" | "::1" | "[::1]" | 127.*) listen_is_loopback=1 ;;
esac
probe_host="$listen_host"
case "$listen_host" in
"" | "0.0.0.0" | "::" | "[::]") probe_host="127.0.0.1" ;;
esac
setup_url="http://${probe_host}:${listen_port}/setup"

if [[ "$START_SERVICE" -eq 1 ]]; then
	healthy=0
	for _ in $(seq 1 30); do
		if command -v curl >/dev/null 2>&1; then
			if curl -fsS --max-time 2 "http://${probe_host}:${listen_port}/api/health" >/dev/null 2>&1; then
				healthy=1
				break
			fi
		elif wget -q -O /dev/null --timeout=2 "http://${probe_host}:${listen_port}/api/health" 2>/dev/null; then
			healthy=1
			break
		fi
		sleep 1
	done
	if [[ "$healthy" -ne 1 ]]; then
		warn "服务未在 30 秒内就绪。请查看: journalctl -u ts3panel -n 80 --no-pager"
		exit 1
	fi
	info "服务已就绪: http://${probe_host}:${listen_port}/api/health"
fi

# A non-loopback listener requires the one-time setup token (M1 gate). Mint one
# after the service created state.json, otherwise the printed entry point would
# be unusable. The token is only printed, never written to a log file.
if [[ "$START_SERVICE" -eq 1 && "$listen_is_loopback" -eq 0 ]]; then
	if reset_output="$("${BIN_DIR}/ts3panel" reset-password --config "$CONFIG_FILE" 2>&1)"; then
		reset_token="$(printf '%s\n' "$reset_output" | grep -o 'setup?token=[A-Za-z0-9._~-]*' |
			head -n 1 | cut -d= -f2 || true)"
		if [[ -n "$reset_token" ]]; then
			setup_url="http://${probe_host}:${listen_port}/setup?token=${reset_token}"
		else
			warn "未能解析 setup token，请在服务器执行: ts3panel reset-password"
		fi
	else
		warn "非回环监听需要 setup_token；自动生成失败，请执行: ts3panel reset-password"
	fi
fi

cat <<EOF

$(printf '%s' "$COLOR_GREEN")ts3panel 安装完成$(printf '%s' "$COLOR_RESET")

  配置:     ${CONFIG_FILE}  (root:root 0600)
  状态:     ${STATE_DIR}/state.json
  实例目录: ${instance_root}
  服务:     systemctl status ts3panel

一次性初始化入口（仅在面板尚未初始化时有效）:
  ${setup_url}

若面板在远程服务器上，请先建立 SSH 隧道，再从本机浏览器打开:
  ssh -L ${listen_port}:127.0.0.1:${listen_port} root@<服务器公网IP>
  http://127.0.0.1:${listen_port}/setup

初始化完成后该入口会返回 E_SETUP_DONE，不会再次开放。
忘记密码时在服务器执行: ts3panel reset-password
EOF
