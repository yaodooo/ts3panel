# 各平台获取 DNS API 凭据

ts3panel v1 支持 **Cloudflare**、**阿里云 DNS**、**DNSPod / 腾讯云** 和 **手动模式**。
华为云等其余平台请选「手动解析」，由面板生成记录清单，你到控制台自行添加。

在初始化向导第 2 步，或登录后的「设置 → 解析平台」里填写。保存前可以点「测试连接」，
面板会调用厂商接口列出该账号可见的 Zone。

> **保存规则**：Provider 按「平台 + 主域名」唯一。同一平台、同一主域名再次保存是**覆盖**
> 现有凭据，不会新增第二个账号。要同时管理多个主域名，请为每个域名各建一条配置。

> **安全**：凭据在服务端用 AES-GCM 加密（`master_key`）后写入 `config.toml`，界面只显示
> 前 4 后 4 位。请使用最小权限的子账号 / Token，不要使用主账号全局密钥；怀疑泄漏时立即轮换。

---

## Cloudflare

面板使用 **API Token**（`Authorization: Bearer <token>`），不是 Global API Key。

### 1. 创建 Token

1. 登录 Cloudflare → 右上角头像 → **My Profile** → **API Tokens**；
2. **Create Token** → **Create Custom Token**；
3. 名称随意，例如 `ts3panel`；
4. Permissions 添加两条：
   - `Zone` → `DNS` → **Edit**
   - `Zone` → `Zone` → **Read**
5. Zone Resources：`Include` → `Specific zone` → 选择你的域名（例如 `example.com`）；
6. TTL 可留默认；Create Token；
7. **复制一次**显示的 Token（之后不能再看）。

### 2. 填写

在面板里选「Cloudflare」：

| 字段 | 值 |
| --- | --- |
| 主域名 / Zone | `example.com`（不带 `www`、不带 `https://`） |
| API Token | 上一步复制的 Token |

面板会自动查询并保存 Zone ID，不需要手填。点「测试连接」应能看到该域名。

### 3. 面板会写什么

- A 记录：`<前缀>.example.com` → 你填的公网 IP，**`proxied=false`（灰云）**，TTL 300；
- SRV 记录：`_ts3._udp.<前缀>.example.com` → `0 5 <语音端口> <前缀>.example.com`；
- 记录注释带 `ts3panel:<实例 id>`，便于识别；
- 重复绑定会先查询再 Update，不会产生重复记录。

### 常见失败

| 现象 | 原因 |
| --- | --- |
| `E_DNS_AUTH` | Token 复制不完整、已删除、或粘贴了 Global API Key |
| `E_DNS_PERM` | 少了 `Zone:DNS:Edit` 或 `Zone:Zone:Read`；或 Zone Resources 没包含该域名 |
| `E_DNS_ZONE` | 主域名拼错，或该域名不在这个 Cloudflare 账号下 |

橙云（Proxied）不代理 UDP，会导致语音连不上；面板写入时强制灰云。若你之前手工开过橙云，
请改回灰云后重新验证。

---

## 阿里云 DNS

面板使用 RAM 子账号的 AccessKey，自己实现 RPC 签名（不依赖阿里云 SDK），区域固定
`cn-hangzhou`。

### 1. 创建 RAM 子账号

1. 登录阿里云控制台 → **RAM 访问控制** → **用户** → **创建用户**；
2. 勾选「使用永久 AccessKey 访问」（或按企业要求使用 STS，但面板当前只支持 AccessKey）；
3. 创建完成后**立即保存 AccessKey ID 与 AccessKey Secret**（Secret 只显示一次）；
4. 给该用户授权。简单做法是附加 `AliyunDNSFullAccess`；更小的权限用自定义策略：

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "alidns:DescribeDomains",
        "alidns:DescribeDomainRecords",
        "alidns:AddDomainRecord",
        "alidns:UpdateDomainRecord",
        "alidns:DeleteDomainRecord"
      ],
      "Resource": "*"
    }
  ]
}
```

### 2. 填写

| 字段 | 值 |
| --- | --- |
| 主域名 / Zone | `example.com` |
| AccessKey ID | 子账号的 AccessKey ID |
| AccessKey Secret | 子账号的 AccessKey Secret |

该域名必须已经托管在阿里云云解析（在「云解析 DNS → 域名解析」列表里能看到）。
点「测试连接」会调用 `DescribeDomains` 列出可见域名。

### 3. 记录格式

- A：`Type=A`，`RR=<前缀>`，`Value=<公网 IP>`；
- SRV：`Type=SRV`，`RR=_ts3._udp.<前缀>`，`Value="0 5 <语音端口> <完整子域名>"`。

阿里云的 SRV 值是一个空格分隔的字符串，面板已按此格式生成并写了单元测试。

### 常见失败

- `E_DNS_AUTH`：AccessKey 错误或已禁用；
- `E_DNS_PERM`：RAM 策略缺少上面任一 `alidns:*` 动作；
- `E_DNS_ZONE`：域名不在该账号的云解析列表里，或主域名写成了子域名。

---

## DNSPod / 腾讯云

面板使用腾讯云 API 3.0（TC3-HMAC-SHA256，服务名 `dnspod`，版本 `2021-03-23`），
自己实现签名，不依赖腾讯云 SDK。

### 1. 创建 API 密钥

1. 登录腾讯云控制台 → **访问管理 CAM** → **API 密钥管理** → **新建密钥**；
2. 保存 **SecretId** 与 **SecretKey**（SecretKey 只显示一次）；
3. 授权：给该子用户附加 `QcloudDNSPodFullAccess`，或按最小权限自定义策略，至少包含：
   - `dnspod:DescribeDomainList`
   - `dnspod:DescribeRecordList`
   - `dnspod:CreateRecord`
   - `dnspod:ModifyRecord`
   - `dnspod:DeleteRecord`
4. 确认域名已经添加到 DNSPod（「DNS 解析 DNSPod → 我的域名」）。

### 2. 填写

| 字段 | 值 |
| --- | --- |
| 主域名 / Zone | `example.com` |
| SecretId | 上一步的 SecretId |
| SecretKey | 上一步的 SecretKey |

### 3. 记录格式

- A：`RecordType=A`，`SubDomain=<前缀>`，`Value=<公网 IP>`；
- SRV：`RecordType=SRV`，`SubDomain=_ts3._udp.<前缀>`，
  `Value="0 5 <语音端口> <完整子域名>"`。

> 腾讯云 SRV 的子域名写法与 Cloudflare 不同（Cloudflare 用完整 `_ts3._udp.<fqdn>` 作为记录名，
> 腾讯云用相对主域名的 `_ts3._udp.<前缀>`）。面板对每家都写了精确字符串的单测固定该格式。

### 常见失败

- `E_DNS_AUTH`：SecretId / SecretKey 错误，或密钥被禁用；
- `E_DNS_PERM`：CAM 策略缺少 `dnspod:*` 动作；
- `E_DNS_ZONE`：域名未添加到 DNSPod，或主域名写错；
- `E_DNS_RATE`：触发腾讯云限流，等几分钟再试。

---

## 手动模式（其他平台 / 不想给 API 权限）

在向导或设置里选「手动解析」，填主域名即可，不需要任何凭据。面板不会发任何请求，
只生成清单：

```text
类型   主机记录              记录值
A      ts1                  203.0.113.10
SRV    _ts3._udp.ts1        0 5 9987 ts1.example.com
```

到你的 DNS 控制台照抄，TTL 建议 300 秒。添加后回到面板点「我已添加，去验证」。

注意（勘误 E9）：

- 手动模式下这些记录**面板不知道 ID**，删除实例时不会删除它们，需要你自己去控制台删；
- 从自动模式切换到手动模式时，面板会按之前保存的 ID 删除它自动创建过的记录；
- 切换与删除的确认框里都会提示这一点。

---

## 公网 IP 与验证

- 面板允许手填 `public_ip`，**手填值优先于自动探测**；探测失败不会阻塞流程（勘误 E8）。
- NAT 云主机请填公网出口 IP，否则 A 记录会指向内网地址。
- 绑定成功不代表解析已生效：面板会用系统解析器真实查询 SRV / A，最多等 180 秒。
- 验证只看「记录名 + 端口 + target」，不看 Cloudflare GET 返回的 `data.service/proto/name`
  （读回时可能为 null，勘误 E7）。

验证命令：

```bash
dig +short SRV _ts3._udp.ts1.example.com
dig +short A ts1.example.com
```

如果长时间不生效，先确认记录已保存、主域名正确、TTL 未过长，再用 `dig @8.8.8.8` 对比本地 DNS。

---

## 通用安全建议

- 一个面板只填一个平台账号即可；多平台/多域名再逐条添加。
- 给 Token / 子账号限定到具体域名，不要给整个账号的全局权限。
- 不要把 Token、SecretKey 贴进 Issue、截图或日志；面板日志已对凭据打码。
- 轮换凭据后，在设置页覆盖保存（同一平台 + 同一主域名会覆盖旧值）。
- v1 不托管云厂商 AK/SK 去改安全组；端口仍需你手工放行，面板只生成清单。
