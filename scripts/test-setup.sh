#!/usr/bin/env bash
#
# Test-machine preparation for ts3panel (NOT for production).
#
#   sudo bash scripts/test-setup.sh
#
# What it does, idempotently:
#   1. Installs Docker Engine from the official apt repository when missing.
#   2. Adds a 2 GiB swap file when the machine has less than 2 GiB RAM.
#   3. Creates /srv/ts3panel-test and /srv/ts3-test.
#   4. Prints a self-check of ports, memory and disk.
#
# It never touches existing services, firewalls or containers. Production
# deployments must use scripts/install.sh instead.

set -Eeuo pipefail
IFS=$'\n\t'

INSTALL_DOCKER=1
ADD_SWAP=1
SWAP_FILE="/swapfile"
SWAP_SIZE="2G"
MEM_THRESHOLD_KB=$((2 * 1024 * 1024))

if [[ -t 1 ]]; then
	COLOR_GREEN=$'\033[32m'
	COLOR_YELLOW=$'\033[33m'
	COLOR_RED=$'\033[31m'
	COLOR_RESET=$'\033[0m'
else
	COLOR_GREEN=""
	COLOR_YELLOW=""
	COLOR_RED=""
	COLOR_RESET=""
fi

info() { printf '%s[test-setup]%s %s\n' "$COLOR_GREEN" "$COLOR_RESET" "$*"; }
warn() { printf '%s[test-setup]%s %s\n' "$COLOR_YELLOW" "$COLOR_RESET" "$*" >&2; }
die() { printf '%s[test-setup] 错误:%s %s\n' "$COLOR_RED" "$COLOR_RESET" "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
用法: sudo bash scripts/test-setup.sh [选项]

仅用于测试机，生产环境请使用 scripts/install.sh。

选项:
  --no-docker   跳过 Docker 安装
  --no-swap     跳过 swap 创建
  -h, --help    显示帮助
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--no-docker) INSTALL_DOCKER=0; shift ;;
	--no-swap) ADD_SWAP=0; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "未知参数: $1" ;;
	esac
done

[[ "$(uname -s)" == "Linux" ]] || die "只支持 Linux。"
[[ "${EUID}" -eq 0 ]] || die "请以 root 运行。"
[[ -r /etc/os-release ]] || die "无法识别系统（缺少 /etc/os-release）。"
# shellcheck disable=SC1091
. /etc/os-release

case "${ID:-}" in
debian | ubuntu) ;;
*)
	die "本脚本只覆盖 Debian 11+ / Ubuntu 22.04（当前: ${PRETTY_NAME:-unknown}）。
CentOS 7 仅作为备用测试机，请按部署文档手工准备。"
	;;
esac

install_docker_repo() {
	local codename="${VERSION_CODENAME:-}"
	[[ -n "$codename" ]] || die "无法确定发行版代号（VERSION_CODENAME）。"

	info "安装 Docker 官方 apt 源（${ID} ${codename}）…"
	apt-get update -qq
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates curl gnupg >/dev/null

	install -m 0755 -d /etc/apt/keyrings
	curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc
	chmod a+r /etc/apt/keyrings/docker.asc

	cat >/etc/apt/sources.list.d/docker.list <<EOF
deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${ID} ${codename} stable
EOF

	apt-get update -qq
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
		docker-ce docker-ce-cli containerd.io >/dev/null
}

ensure_docker() {
	if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
		info "Docker 已就绪: $(docker --version)"
		return
	fi
	if ! command -v docker >/dev/null 2>&1; then
		if [[ "$INSTALL_DOCKER" -ne 1 ]]; then
			warn "未安装 Docker，且已指定 --no-docker。"
			return
		fi
		if ! install_docker_repo; then
			warn "Docker 官方源安装失败，回退到发行版自带的 docker.io 包。"
			apt-get update -qq
			DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io >/dev/null
		fi
	fi

	systemctl enable --now docker
	local ok=0
	for _ in $(seq 1 30); do
		if docker info >/dev/null 2>&1; then ok=1; break; fi
		sleep 1
	done
	[[ "$ok" -eq 1 ]] || die "Docker 守护进程启动失败，请查看 journalctl -u docker。"
	info "Docker 已安装: $(docker --version)"
}

ensure_swap() {
	local mem_kb swap_kb avail_kb
	mem_kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
	swap_kb="$(awk '/^SwapTotal:/ {print $2}' /proc/meminfo)"

	if [[ "$mem_kb" -ge "$MEM_THRESHOLD_KB" ]]; then
		info "内存 ${mem_kb} KiB，无需额外 swap。"
		return
	fi
	if [[ "$swap_kb" -ge $((512 * 1024)) ]]; then
		info "已有 swap ${swap_kb} KiB，跳过。"
		return
	fi
	if [[ "$ADD_SWAP" -ne 1 ]]; then
		warn "内存仅 ${mem_kb} KiB 且未开启 swap（--no-swap）。"
		return
	fi

	avail_kb="$(df -Pk / | awk 'NR == 2 {print $4}')"
	[[ "$avail_kb" -ge $((3 * 1024 * 1024)) ]] ||
		die "根分区剩余空间不足 3 GiB，无法创建 ${SWAP_SIZE} swap。"

	if [[ ! -f "$SWAP_FILE" ]]; then
		info "创建 ${SWAP_SIZE} swap: ${SWAP_FILE}"
		if ! fallocate -l "$SWAP_SIZE" "$SWAP_FILE" 2>/dev/null; then
			warn "fallocate 不可用，改用 dd（较慢）。"
			dd if=/dev/zero of="$SWAP_FILE" bs=1M count=2048 status=none
		fi
		chmod 0600 "$SWAP_FILE"
		mkswap "$SWAP_FILE" >/dev/null
	fi

	if ! swapon --show=NAME --noheadings | grep -qx "$SWAP_FILE"; then
		swapon "$SWAP_FILE"
	fi
	if ! grep -qE "^${SWAP_FILE}[[:space:]]" /etc/fstab; then
		printf '%s none swap sw 0 0\n' "$SWAP_FILE" >>/etc/fstab
	fi
	info "swap 已启用: $(swapon --show=SIZE --noheadings --bytes | head -n 1) bytes"
}

ensure_dirs() {
	install -d -m 0755 /srv/ts3panel-test
	install -d -m 0755 /srv/ts3-test
	info "目录已就绪: /srv/ts3panel-test /srv/ts3-test"
}

self_check() {
	echo
	info "自检"
	echo "  系统:   ${PRETTY_NAME:-unknown} $(uname -r)"
	echo "  架构:   $(uname -m)"
	echo "  内存:   $(awk '/^MemTotal:/ {printf "%.0f MiB", $2/1024}' /proc/meminfo)"
	echo "  Swap:   $(awk '/^SwapTotal:/ {printf "%.0f MiB", $2/1024}' /proc/meminfo)"
	echo "  磁盘:   $(df -Ph / | awk 'NR == 2 {print $4 " 可用 / " $2 " 总"}')"
	if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
		echo "  Docker: $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo OK)"
	else
		echo "  Docker: 不可用"
	fi

	local occupied=""
	for port in 9987 10011 10022 30033; do
		if ss -H -lntu "sport = :${port}" 2>/dev/null | grep -q .; then
			occupied="${occupied} ${port}"
		fi
	done
	if [[ -n "$occupied" ]]; then
		warn "以下端口已被占用（面板会自动跳过，请勿停止现有服务）:${occupied}"
	else
		echo "  TS3 端口: 9987/10011/10022/30033 均空闲"
	fi

	if ss -H -lnt "sport = :8899" 2>/dev/null | grep -q .; then
		warn "8899 已被占用；面板启动前请修改 config.toml 的 listen。"
	fi
}

ensure_docker
ensure_swap
ensure_dirs
self_check

cat <<'EOF'

测试机准备完成。下一步:

  1. 上传并安装面板（示例）:
       sudo bash scripts/install.sh --from-file ./ts3panel_1.0.0_linux_amd64.tar.gz
  2. 在本机建立 SSH 隧道:
       ssh -L 8899:127.0.0.1:8899 root@<测试机IP>
  3. 浏览器打开 http://127.0.0.1:8899/setup

提醒: 这台机器如果还跑着其他业务，只操作带 ts3panel.managed=true 标签的容器。
EOF
