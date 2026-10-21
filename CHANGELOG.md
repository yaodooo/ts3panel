# Changelog

本文件记录 ts3panel 的版本变化。格式参考 Keep a Changelog，版本号遵循语义化版本。

## [1.0.0] - 2026-10-22

首个正式版本。单机自部署、单管理员账号；发布前在 Debian 12 完成安装、初始化、实例创建与
备份恢复，在 Debian 11 完成离线安装、跨机器还原与整机重启恢复验证；目标兼容 Debian 11+ /
Ubuntu 22.04。

### 新增

- **实例管理**：创建 / 启动 / 停止 / 重启 / 删除；按 `9987+i`、`30033+i`、`10011+i`、`10022+i`
  自动分配端口；数据目录与容器完全隔离；实例上限默认 20。
- **容器编排**：只操作带 `ts3panel.managed=true` 标签的容器；`network_mode=host`；
  端口与绑定地址通过环境变量注入（镜像会重建 ini，见勘误 E1）；
  Query 与 SSH Query 固定绑定 `127.0.0.1`。
- **域名解析**：Cloudflare / 阿里云 DNS / DNSPod / 手动模式；A + SRV 幂等写入；
  Cloudflare 强制 `proxied=false`；写入后用系统解析器真实校验；
  绑定失败保留实例并给出可复制的记录清单。
- **管理员账号**：初始化向导、自定义用户名密码、bcrypt cost 12、8 个一次性恢复码、
  恢复码找回密码、`ts3panel reset-password` CLI 兜底、内存会话与「退出所有设备」。
- **实例详情**：管理员 token 自动捕获与持久化、最近 200 行脱敏日志、
  CPU / 内存快照、数据目录大小、危险操作区。
- **端口看板**：按协议合并区间、一键复制、可选的第三方连通性探测（默认关闭）、
  云厂商安全组开启指引。
- **备份恢复**：手动备份 / 恢复、跨机器还原、自动备份计划（daily / interval + retention）、
  备份下载流式返回、磁盘余量检查与 `.tmp` 原子落盘。
- **运维**：`scripts/install.sh`（支持 `--from-file` 离线安装、幂等升级、systemd 加固）、
  `scripts/test-setup.sh`（测试机准备）、systemd unit、`/api/health`、审计日志。

### 文档与发布

- README、部署 / 排错 / DNS 取 Token / 备份恢复文档。
- MIT LICENSE、`config.example.toml`、`.gitignore`、GitHub Actions CI 与 GoReleaser
  双架构（amd64 / arm64）发布配置。

### 已知限制

- 不做在线人数与频道管理、不做公网 HTTPS、不做实例克隆、不代管云厂商 AK/SK。
- 华为云等其余解析平台在 v1 走手动模式。
- 从备份恢复后管理员 token 不会恢复（备份不保存 token）。
- 第三方端口探测依赖 check-host.net；该服务不可达时只显示「未知」，不会误报「不通」。

### 勘误吸收

本版本已按真机实测吸收 E1–E9：环境变量注入端口、SSH Query 需显式开启、
容器自行 chown、面板以 root 运行、容器状态含 `missing`、
Provider 按平台 + 主域名唯一、Cloudflare SRV 读回形态差异、
公网 IP 手填优先、手动模式记录不由面板管理。
