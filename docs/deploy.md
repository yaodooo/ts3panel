# 部署与升级

本文覆盖生产环境的安装、首次启动、升级与卸载。测试机准备脚本见 README 的安装说明。

## 1. 环境要求

| 项 | 要求 |
| --- | --- |
| 系统 | Debian 11+、Ubuntu 22.04（amd64 / arm64） |
| 权限 | root（读取 Docker socket、写 `/etc`、`/var/lib`、`/srv`） |
| Docker | Docker Engine 24+，`docker info` 正常 |
| systemd | 必须；`/run/systemd/system` 存在 |
| 网络 | 默认从 GitHub Releases 下载；内网用 `--from-file` |
| 磁盘 | 面板本身很小；每个 TS3 实例按实际文件量预留，备份约为实例目录大小 |
| 端口 | 面板 `127.0.0.1:8899`；TS3 语音 `9987+i/udp`、文件 `30033+i/tcp`、Query `10011+i`、SSH Query `10022+i`（后两者只绑回环） |

> 不支持 Windows 宿主机。CentOS 7 仅可作为备用测试机，不作为发布兼容性依据。
> 本次发布在 Debian 12 与 Debian 11 实测；Ubuntu 22.04 为目标平台，未在本次发布环境复测。

### 安装 Docker（Debian / Ubuntu）

如果机器还没有 Docker：

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl gnupg
sudo install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/debian/gpg | sudo tee /etc/apt/keyrings/docker.asc >/dev/null
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" |
  sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io
sudo systemctl enable --now docker
sudo docker run --rm hello-world
```

Ubuntu 把 `debian` 换成 `ubuntu`。

## 2. 安装

### 2.1 在线安装（推荐）

```bash
curl -fsSL https://raw.githubusercontent.com/yaodooo/ts3panel/main/scripts/install.sh | sudo bash
```

脚本是幂等的：重复执行会升级二进制、刷新 unit 并重启服务，**不会覆盖**已有的
`config.toml` 和 `state.json`。

如果需要固定版本或使用镜像：

```bash
sudo bash install.sh --version 1.0.0
sudo TS3PANEL_REPO='your-name/ts3panel' bash install.sh
sudo TS3PANEL_GITHUB='https://<GitHub 镜像>' bash install.sh
```

### 2.2 离线安装

在能联网的机器下载 Release 资产（`ts3panel_<version>_linux_<arch>.tar.gz`），连同
`scripts/install.sh` 一起上传：

```bash
sudo bash install.sh --from-file ./ts3panel_1.0.0_linux_amd64.tar.gz
```

`--from-file` 也接受裸二进制；可用 `--sha256 <hex>` 额外校验文件完整性。

### 2.3 手工安装（打包者 / 特殊环境）

```bash
# 1. 放置二进制
sudo install -m 0755 ts3panel /usr/local/bin/ts3panel

# 2. 目录与配置
sudo install -d -m 0700 -o root -g root /etc/ts3panel /var/lib/ts3panel
sudo install -d -m 0755 -o root -g root /srv/ts3
sudo install -m 0600 -o root -g root config.example.toml /etc/ts3panel/config.toml
# 编辑 config.toml，至少保留 listen / instance_root，可留空密钥让首次启动自动生成

# 3. systemd unit（见下）
sudo systemctl daemon-reload
sudo systemctl enable --now ts3panel
```

`/srv/ts3` 目录需要 `0755`：TS3 容器的 entrypoint 会把实例子目录 chown 给容器内用户，
容器内进程仍需要能够穿过父目录。实例子目录的属主由镜像负责，面板**不做 chown**（勘误 E3）。

### 2.4 systemd unit

把以下内容保存到 `/etc/systemd/system/ts3panel.service`（root:root，0644）：

```ini
[Unit]
Description=ts3panel - TeamSpeak 3 multi-instance panel
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
UMask=0077
ExecStart=/usr/local/bin/ts3panel serve --config /etc/ts3panel/config.toml
Restart=always
RestartSec=3
TimeoutStopSec=20

ProtectHome=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/srv/ts3 /var/lib/ts3panel /etc/ts3panel
RestrictSUIDSGID=true
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

- 面板以 root 运行：Docker socket 本身就是 root 等价权限，降权收益有限，且容器 chown 后的
  数据目录非 root 删不掉（勘误 E4）。加固依赖上面的 systemd 沙箱项。
- `ReadWritePaths` 必须覆盖 `instance_root`、状态目录和配置目录；改 `instance_root` 后同步修改。
- 不要把 `ReadWritePaths` 放宽到 `/` 或 `/var`，那会抵消 `ProtectSystem=strict`。

## 3. 首次启动

安装完成后终端会打印初始化入口。面板默认只绑回环，请在本机建立 SSH 隧道：

```bash
ssh -L 8899:127.0.0.1:8899 root@<服务器公网 IP>
# 浏览器打开 http://127.0.0.1:8899/setup
```

初始化向导会要求：

1. 自定义管理员用户名 / 密码（无默认账号）；
2. 选择解析平台或跳过；
3. 公网 IP（可手填，手填优先于探测）；
4. 保存 8 个一次性恢复码（只显示一次）。

初始化完成后 `/setup` 失效，再调用初始化接口返回 `E_SETUP_DONE`。

### 监听地址

默认 `127.0.0.1:8899`。不要把 8899 直接暴露到公网；如确需公网访问，请在前面加 HTTPS
反向代理，并自行承担备案、证书与爆破风险。改成 `0.0.0.0` 后，未初始化状态下的初始化请求
必须携带 `setup_token`（`ts3panel reset-password` 生成）。

## 4. 升级

```bash
# 在线
curl -fsSL https://raw.githubusercontent.com/yaodooo/ts3panel/main/scripts/install.sh | sudo bash

# 或离线
sudo bash install.sh --from-file ./ts3panel_1.0.0_linux_amd64.tar.gz
```

脚本会替换二进制、重写 unit、`systemctl restart ts3panel`。`state.json` 带版本号，启动时按序迁移。

升级前建议：

```bash
sudo cp -a /etc/ts3panel/config.toml /root/config.toml.bak
sudo cp -a /var/lib/ts3panel/state.json /root/state.json.bak
```

面板重启不会停止已运行的 TS3 容器；正在创建 / 备份的操作会被中断，重启后可继续或清理。

## 5. 迁移到新机器

1. 新机器安装同版本 ts3panel（先不要初始化）。
2. 停止旧面板服务（`systemctl stop ts3panel`），但**不要**删除实例容器。
3. 拷贝 `/etc/ts3panel/config.toml`、`/var/lib/ts3panel/state.json`、`/srv/ts3/` 到新机器相同路径。
4. 在新机器上用备份包逐个还原实例（见 [备份与恢复](backup-restore.md)），或直接迁移数据目录。
5. 启动新面板，确认容器标签、端口与 DNS 记录。

注意：实例身份在数据目录里（`ts3server.sqlitedb` 等），迁移时不要只拷 `state.json`。

## 6. 卸载

```bash
sudo systemctl disable --now ts3panel
sudo rm -f /etc/systemd/system/ts3panel.service
sudo systemctl daemon-reload
sudo rm -f /usr/local/bin/ts3panel
sudo rm -rf /etc/ts3panel /var/lib/ts3panel
# 实例数据目录 /srv/ts3 与容器请先在面板里删除，确认后再手动清理
```

卸载脚本不会自动删除容器和数据：请先在面板里删除实例（按需勾选删除数据），
再执行 `docker ps -a --filter label=ts3panel.managed=true` 确认没有残留。
**不要**使用 `docker system prune` 或删除未带标签的容器。

## 7. 日志与排错入口

```bash
systemctl status ts3panel
journalctl -u ts3panel -n 200 --no-pager
journalctl -u ts3panel -f
curl -s http://127.0.0.1:8899/api/health
```

结构化日志输出 JSON，凭据、token、密码均已打码。审计日志在 `/var/lib/ts3panel/audit.log`，
滚动保留 5000 条。

更多问题见 [排错手册](troubleshooting.md)。
