# 排错手册

先收集三样东西，再动手改：

```bash
systemctl status ts3panel --no-pager
journalctl -u ts3panel -n 200 --no-pager
curl -s http://127.0.0.1:8899/api/health
docker ps -a --filter label=ts3panel.managed=true \
  --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
ss -lntup | grep -E ':(8899|9987|10011|10022|30033)\b' || true
```

日志是 JSON；凭据、token、密码已打码，可以安全贴出（贴之前仍建议扫一眼域名与 IP）。

---

## 安装 / 启动

### `未发现 Docker`

```bash
systemctl start docker
docker info
```

仍失败说明 Docker 没装好或当前用户无 socket 权限。面板以 root 运行，正常不需要改 docker 组。
Debian / Ubuntu 安装步骤见 [部署文档](deploy.md#安装-dockerdebian--ubuntu)。

### `systemd 未在运行`

面板以 systemd 服务运行。容器 / WSL / chroot 里请手工执行
`ts3panel serve --config /etc/ts3panel/config.toml` 调试，但生产环境需要 systemd。

### `无法获取最新版本` / 下载超时

GitHub 在该网络不可达。改用离线安装：

```bash
sudo bash install.sh --from-file ./ts3panel_1.0.1_linux_amd64.tar.gz
```

或设置镜像：`TS3PANEL_GITHUB='https://<镜像>'`。

### 服务起不来：`config.toml is empty` / `parse config.toml`

配置文件损坏。先备份，再与 [`config.example.toml`](../config.example.toml) 对照修复。
注意 TOML 字符串必须用双引号，`[backup]` 等表头不要重复。

### 服务起不来：`master_key is missing but providers exist`

密钥被清空但配置里还有 Provider 密文。不要让它自动生成新密钥（旧凭据会永久无法解密）。
从备份恢复 `master_key`；确实要放弃旧凭据时，删除对应 `[[providers]]` 后重启，再重新添加平台。

### 服务起不来：`config.toml must be 0600`

```bash
chown root:root /etc/ts3panel/config.toml
chmod 0600 /etc/ts3panel/config.toml
chmod 0700 /etc/ts3panel
```

### `8899` 被占用

```bash
ss -lntp | grep 8899
```

改 `/etc/ts3panel/config.toml` 的 `listen`（例如 `127.0.0.1:8900`），
同步调整 SSH 隧道端口，然后 `systemctl restart ts3panel`。

### 页面打不开 / 连接被拒绝

面板默认只监听 `127.0.0.1`。请在**本机**建隧道：

```bash
ssh -L 8899:127.0.0.1:8899 root@<服务器 IP>
```

直接访问 `http://<服务器IP>:8899` 打不开是预期行为，不要为此把监听改成 `0.0.0.0`。

---

## 登录与账号

### 登录返回 `429`

同一 IP + 用户名 15 分钟内失败 5 次会锁 15 分钟。等待，或从另一 IP 登录。
不要用脚本反复试密码，服务器上的 fail2ban 可能同时封 SSH。

### 写接口返回 `403`（缺少 CSRF 头）

所有写接口要求：

```http
X-Requested-With: ts3panel
```

浏览器页面会自动带；用 curl 调试时必须手动加，否则返回 `E_CSRF`。

### 忘记密码 / 恢复码用完

```bash
ts3panel reset-password
```

生成 5 分钟有效的一次性链接。需要 root。重置会作废旧账号密码与全部恢复码，并踢掉所有会话。

---

## 实例

### `E_PORT_EXHAUSTED`

连续 50 组端口都不可用。先看占用：

```bash
ss -lntup | grep -E ':(99[0-9]{2}|100[0-9]{2}|300[0-9]{2})\b'
```

释放自己创建的测试容器，或检查是否有其他服务占着 9987 起的连续段。
不要停止机器上的现役服务（x-ui、xray、nginx、mysql 等）。

### `E_CONTAINER_TIMEOUT`

容器 60 秒内未进入 running。按顺序查：

```bash
docker ps -a --filter label=ts3panel.instance=<id>
docker logs --tail 200 ts3-<id>
journalctl -u docker -n 100 --no-pager
df -h /var/lib/docker /srv/ts3
```

常见原因：镜像拉取失败、磁盘满、内存不足（小内存机器一次只跑一个实例）、
端口被别的进程抢占、Docker daemon 异常。失败时面板会回滚容器与目录，状态置 `failed`。

### 状态显示 `missing`

容器被 `docker rm` 了，但 `state.json` 里还有记录（勘误 E5）。可在详情页删除实例，
或按「重建」流程重新创建。不要手工改 `state.json` 去伪造状态。

### 容器反复重启

```bash
docker logs --tail 200 ts3-<id>
docker inspect ts3-<id> --format '{{.State.ExitCode}} {{.State.Error}}'
```

TS3 常见原因：`ts3server.sqlitedb` 损坏、端口冲突、数据目录权限异常。
数据目录属主由镜像 entrypoint 负责，面板不做 chown；不要把宿主机用户强行 chown 成别的 uid。

### 抓不到管理员 token

token 只在首次启动日志里出现，且可能已被使用。面板会轮询 6 次 × 5 秒。
如果错过了：

```bash
docker logs ts3-<id> 2>&1 | grep -Ei 'token=|Privilege Key'
```

仍然没有就在 TS3 客户端里用 ServerQuery 登录后重新生成 Privilege Key。
从备份恢复的实例不会带回 token（备份不保存 token）。

### 删除实例时提示标签不匹配

面板只操作带 `ts3panel.managed=true` 的容器。如果容器被手工改过标签，面板会拒绝操作。
确认容器确实属于本项目后，用 `docker inspect` 核对标签；否则不要删。

---

## 域名解析

错误码都以 `E_DNS_` 开头，页面会同时给出手动解析清单。

| 错误码 | 含义 | 处理 |
| --- | --- | --- |
| `E_DNS_AUTH` | 凭据无效 / 过期 | 重新生成 Token，设置页覆盖保存（同平台 + 同主域名会覆盖，不是新增） |
| `E_DNS_PERM` | 权限不足 | Cloudflare 需要 `Zone:DNS:Edit` + `Zone:Zone:Read`；阿里云 / DNSPod 检查 RAM / API 密钥策略 |
| `E_DNS_ZONE` | Zone 不存在或不在账号下 | 核对主域名与账号；域名可能不在该平台 |
| `E_DNS_RATE` | 触发厂商限流 | 等待后重试，避免连续点绑定 |
| `E_DNS_UNKNOWN` | 其他厂商错误 | 看页面 message / hint，或改用手动模式 |

### 绑定失败后实例还在吗

在。**DNS 失败绝不回滚或删除实例**（硬约束）。实例仍可按 `IP:端口` 连接，页面会显示失败原因、
手动解析清单和重试按钮。

### Cloudflare 橙云

TS3 走 UDP，橙云不代理。面板强制 A 记录 `proxied=false`；SRV 的 target 也必须是灰云。
如果之前手工开过橙云，改回灰云后重新验证。

### SRV 记录读回后 `data.service/proto/name` 是 null

Cloudflare 读回 SRV 时这三个字段可能为 null，这是正常现象（勘误 E7）。
校验只看记录名 `_ts3._udp.<前缀>`、`data.port`、`data.target`，不要拿 GET 结果做全字段断言。

### 验证一直不通过（最多 180 秒）

面板用系统解析器真实查询，不信 API 返回 200：

```bash
dig +short SRV _ts3._udp.<子域名>
dig +short A <子域名>
```

可能原因：记录还没传播、TTL 缓存、权威 DNS 配错、子域名写错、公网 IP 填错（NAT 场景）。
页面提供「重新验证」；也可以先按 `IP:端口` 连接，稍后再验。

### 手动模式

手动模式不调用任何 API，面板只生成清单。记录由你自己在 DNS 控制台添加，
**面板不知道这些记录的 ID，删实例时不会删除它们**；从自动切手动时，面板会按旧 ID
删除之前自动创建的记录（勘误 E9）。切换和删除的确认框里都会写明这一点。

---

## 备份与恢复

### 备份直接失败：磁盘不足

面板要求剩余空间 ≥ 2 倍实例目录大小，否则不写半个包。清理旧备份或扩容后重试：

```bash
du -sh /srv/ts3/.backups/*
df -h /srv/ts3
```

### 恢复失败 / 解压后实例起不来

备份包必须包含 `ts3server.sqlitedb`。恢复前会先停止容器；解压失败会回滚原目录。
检查：

```bash
tar -tzf <备份包> | head -50
docker logs --tail 200 ts3-<id>
```

### 自动备份没有执行

- 确认 `[backup] enabled = true`，且 `mode` / `daily_at` / `interval_hours` 合法；
- 面板重启后调度器会按配置恢复，不会重复触发；
- 看 `journalctl -u ts3panel | grep -i backup`；
- 设置页会显示「上次成功时间 / 下次执行时间」。

### 手动备份为什么不自动删

按设计，手动备份不参与 `retention` 滚动清理，只有自动备份会被删。面板显示每份备份的占用空间，
请自行清理不需要的手动包。

---

## 安全与误操作

- 不要执行 `docker system prune`、`docker volume prune`。
- 不要 `docker rm -f` 未带 `ts3panel.managed=true` 标签的容器。
- 不要停止 / 修改机器上的现役服务（x-ui、xray、nginx、mysql、现役 TS3）。
- 怀疑凭据泄漏：立即在厂商控制台轮换 Token，并在设置页覆盖保存；同平台 + 同主域名是覆盖，不是新增。
- 需要把面板暴露公网时，先加 HTTPS 反向代理、强密码、限流，并确认备案要求。
