# 部署

三种放法，按需求叠加：

| 场景 | 用哪些文件 |
|---|---|
| 内网 / 直连端口 / 前面已有反代 | `install.sh` + `agentshare.service` |
| 公网域名，Cloudflare 橙云在前 | 上面 + `nginx/*`（代理地址） |
| **还要传大文件**（超过橙云的上限） | 上面 + `nginx/dl.*.conf`（直连地址，见第三节） |

---

## 一、装成 systemd 服务

```bash
make build-linux                     # 出 amd64 + arm64 静态二进制
scp bin/agentshare-linux-amd64 deploy/install.sh deploy/agentshare.service root@host:/tmp/ash/
ssh root@host 'bash /tmp/ash/install.sh'
```

`install.sh` 是**幂等**的：重跑只换二进制、重启服务，**不会动**已有的 root token。
第一次跑会生成 root token 写进 `/etc/agentshare.env`（0600）并打印一次。

部署参数从环境读，所以一台机器的设置活在**你敲的那条命令**里，而不是活在服务器上
一个被手改过的文件里。**已在运行的主机上重跑也会生效** —— 脚本是逐个键就地更新的：

```bash
ASH_BASE_URL=https://files.example.com \
ASH_BEHIND_PROXY=true \
ASH_MAX_UPLOAD=0 \
ASH_PROXY_MAX_UPLOAD=100M \
ASH_DIRECT_URL=https://dl.files.example.com \
ASH_DEFAULT_TTL=30d \
  bash install.sh
```

| 变量 | 写进 env 文件 | 说明 |
|---|---|---|
| `ASH_BASE_URL` | `AGENTSHARE_BASE_URL` | 响应里 URL 的前缀。**反代后面必须设**，否则回 `127.0.0.1:8080` |
| `ASH_BEHIND_PROXY` | `AGENTSHARE_BEHIND_PROXY` | `true` 才读 `X-Forwarded-Proto/Host` |
| `ASH_MAX_UPLOAD` | `AGENTSHARE_MAX_UPLOAD` | 本进程的上限，`0` = 不限（只受磁盘限制） |
| `ASH_PROXY_MAX_UPLOAD` | `AGENTSHARE_PROXY_MAX_UPLOAD` | **前面那层**（CDN）的上限。只写进自述、不在这里执行，见第四节 |
| `ASH_DIRECT_URL` | `AGENTSHARE_DIRECT_URL` | 另一条不经 CDN 的地址，只写进自述，让 agent 知道大文件该往哪传 |
| `ASH_DEFAULT_TTL` | `AGENTSHARE_DEFAULT_TTL` | 没指定寿命时的默认值，不设 = 永不过期 |
| `ASH_TRUSTED_PROXIES` | `AGENTSHARE_TRUSTED_PROXIES` | 默认 `127.0.0.1/32,::1/128` |

### ⚠️ 两条踩过的坑（都写进脚本了，别改回去）

- **必须 `restart` 而不是 `enable --now`**。`--now` 只*启动*未运行的单元；升级时服务本来就在跑，
  于是新二进制装进去了、旧进程还在服务它 —— **一次"报告成功但什么都没变"的部署**。
- **`DynamicUser=` 的单元不要在脚本里预建 StateDirectory**。systemd 会自己建并持有它；
  先建一个"公开的"目录会让 systemd 把它迁移到 `/var/lib/private/` 并留下一条看着像出错的日志。

---

## 二、代理地址（Cloudflare 橙云）

需要五个文件（三个 snippet、一个 vhost、一个生成脚本）：

```bash
install -d -m 0755 /etc/nginx/snippets /var/www/certbot
install -m 0644 nginx/agentshare-proxy.conf   /etc/nginx/snippets/agentshare-proxy.conf
install -m 0644 nginx/acme-challenge.conf     /etc/nginx/snippets/acme-challenge.conf
install -m 0644 nginx/cloudflare-guard.conf   /etc/nginx/snippets/cloudflare-guard.conf

# 必须早于引用它的 vhost：里面的 log_format 要在 sites-enabled 被解析之前存在
install -m 0644 nginx/hardening.conf          /etc/nginx/conf.d/00-agentshare-hardening.conf
# 版本号：改这一条，不是加一条（加了会报 duplicate directive）
sed -i 's/^\tserver_tokens build;.*/\tserver_tokens off;/' /etc/nginx/nginx.conf

install -m 0644 nginx/tempsnap.cc.conf        /etc/nginx/sites-available/<你的域名>.conf   # 改成自己的 server_name
bash nginx/update-cloudflare-ips.sh           # 生成 realip + geo 两个 conf.d 文件
ln -sf /etc/nginx/sites-available/<域名>.conf /etc/nginx/sites-enabled/
nginx -t && systemctl reload nginx
```

`hardening.conf` 做三件事：关掉版本号（改 nginx.conf 那一行）、给所有 vhost 发 HSTS、
以及让 access log **不再记录 `/s/<token>` 的明文**。三个都是"源站这一层自己该管、
反代不会替你做"的事。

⚠️ **为什么分享 token 不该进日志**：分享链接是**能力凭据** —— 谁拿到 token 谁就能下载，
不需要任何别的凭据。把它写进 access log，等于让每一个**后来**能读到日志的人
（日志收集器、备份、某次排障会话）都成为服务历史上发过的**每一条链接**的持有者。
agentshare 自己的访问日志早就把它脱敏成 `/s/<redacted>`，nginx 这一层现在做同一件事。

⚠️ **改 nginx 配置的手法**：先备份 → 装文件 → `nginx -t` → **验过了才 `reload`**。
nginx 在 reload 成功之前一直用内存里的旧配置服务，所以这个顺序本身是安全的 ——
但**备份必须按目录分开**：如果 vhost 和 conf.d 的备份混在同一个目录，
还原时 `cp 备份/* /etc/nginx/conf.d/` 会把 vhost 一起拷进 conf.d，
变成「http 上下文里出现 server 块」的另一个错（这个我踩过，靠 `nginx -t` 挡住了）。

### 为什么是这几条

- **`client_max_body_size 0`** —— nginx 默认 1 MiB，会把几乎每个文件都挡在 nginx 这一层，
  而且返的是 nginx 自己的错误页，调用方根本不知道撞的是哪个限制。
- **`proxy_request_buffering off`** —— 不把请求体先落盘到 nginx 的临时文件再转发，
  大文件上传少一次写放大；被拒的上传也是立刻失败，而不是等客户端把整个文件发完。
- **`proxy_set_header X-Forwarded-For $remote_addr`** —— 不是 `$proxy_add_x_forwarded_for`。
  Cloudflare 在后面时，realip 模块已经把 `$remote_addr` 改写成真实客户端了，
  再往后追加会让同一个客户端被记两遍。
- **`acme-challenge.conf` 要 include 进每一个 server 块**，不只是 80 的那个。
  任何从 HTTPS 进来的挑战都会落到 `location /` 拿到 API 的 404 ——
  这种失败难查，正因为 80 那条路是通的。
- **自签证书** —— zone 的 SSL 模式是 `Full`（不是 `Full (strict)`），
  CF 到源站这一跳不校验证书。**若改成 `Full (strict)`，必须换真证书，否则全站 526。**

### Cloudflare 的 real_ip

橙云在前时每个请求都从 CF 的地址到源站 —— 不处理的话 nginx 会给每个客户端都记一个 CF 的 IP，
access log 直接废掉。`update-cloudflare-ips.sh` 生成两个文件：

- `cloudflare-realip.conf` —— `set_real_ip_from` × N + `real_ip_header CF-Connecting-IP`，
  让 nginx 知道**客户端**是谁。
- `cloudflare-geo.conf` —— 同样那些网段，做成 `geo` 变量，让 nginx 知道**对端**是谁。
  直连地址存在之后这个是必需的，见第三节。

⚠️ CF 的 `ips-v4` **末尾没有换行**。两份列表拼成一个流时，最后一条 v4 和第一条 v6 会粘成
`131.0.72.0/222400:cb00::/32` —— nginx 报 `host not found in set_real_ip_from`，
**看着像 DNS 问题，实际是少了个换行**。脚本已经把两份列表分开取。
（同一个坑还会让 `wc -l` 少算一行。）

---

## 三、大文件：直连地址

Cloudflare 橙云会对**请求体**设上限（实测 100 MiB，见第四节）。要传更大的文件，
就再挂一个**灰云（DNS only）**的子域，让它直连源站，绕过 CDN。

```bash
# DNS：把 dl.<域名> 建成 A 记录、指向源站 IP、**关掉橙云**（这一步没有脚本，因为改 DNS 需要 CF 凭据）

# 真证书：直连时 TLS 由源站终止，自签会被每个客户端拒绝
apt-get install -y certbot
install -d -m 0755 /var/www/certbot
install -m 0644 nginx/acme-challenge.conf /etc/nginx/snippets/acme-challenge.conf
install -m 0644 nginx/dl.tempsnap.cc.conf /etc/nginx/sites-available/dl.<域名>.conf   # 改 server_name + 证书路径
ln -sf /etc/nginx/sites-available/dl.<域名>.conf /etc/nginx/sites-enabled/
certbot certonly --webroot -w /var/www/certbot -d dl.<域名> \
  --non-interactive --agree-tos --register-unsafely-without-email
# 证书换新后 nginx 不会自己重载，必须挂钩子，否则"续期成功"但线上仍是旧的、到点就过期
install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy
printf '#!/bin/sh\nsystemctl reload nginx\n' > /etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh
chmod 0755 /etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh
systemctl enable --now certbot.timer
```

⚠️ **证书必须在 443 块之前拿到**：那份 vhost 引用了
`/etc/letsencrypt/live/dl.<域名>/fullchain.pem`，文件不存在时 `nginx -t` 直接失败。
先只装 80 块、签发、再补 443 块（或者让首次签发走默认 server 的 ACME location）。

### ⚠️ 为什么必须给代理地址加 guard

直连地址一旦存在，**443 就得对全网开放**（客户端不再是 CF 了）。于是任何知道源站 IP 的人
都能 `Host: <代理域名>` 直接打过来，**整条绕过 CDN** —— 它的 WAF、bot 规则、限速、
以及直连地址存在的理由（请求体上限）全部失效。

所以「只有 Cloudflare 能访问代理域名」这条防线从 **ufw 挪到了 nginx**：
`cloudflare-guard.conf` 在代理域名的 :80 和 :443 两个 server 块里各 include 一次，
对端不在 CF 网段就 `return 403`。

**关键细节：判的是 `$realip_remote_addr`，不是 `$remote_addr`。**
realip 模块跑在 post-read 阶段，早于 access 阶段 —— 等 `allow`/`deny` 之类求值时，
`$remote_addr` 已经**被改写成来访客户端**了，而客户端永远不是 CF 的地址。
拿 CF 网段去比 `$remote_addr` 会把所有流量都拒掉。
`$realip_remote_addr` 才是"连接实际来自哪个对端"。

**失败信号**：代理域名如果开始回 **403**（而不是 522 超时），说明 CF 加了新网段 →
重跑 `update-cloudflare-ips.sh`。跟 ufw 时代是同一个修法，只是错误码不同
（`DROP` 让 CF 回 522，`403` 是我们自己回的）。

⚠️ **别再退回"443 只放行 CF 网段"的 ufw 规则**了：那样直连地址会被丢包，
而症状是 `Connection timed out`，看起来像"服务没起来"。

### 自述里要同时说清两个地址

`ASH_PROXY_MAX_UPLOAD` + `ASH_DIRECT_URL` 让 `GET /` 能回答"这条地址的上限是多少、
大的该往哪传"——只写"请用直连地址"而不说是哪个，读者只能猜主机名：

```json
"limits": {
  "max_upload_bytes": 0,
  "max_upload_note": "0 means unlimited: the only limit is free space on the server",
  "proxied": {
    "url": "https://<代理域名>",
    "max_upload_bytes": 104857600,
    "direct_url": "https://dl.<域名>",
    "direct_max_upload_bytes": 0,
    "note": "uploads to this address above this size are rejected by the proxy in front of it, …"
  }
}
```

---

## 四、Cloudflare 橙云下的实测限制

### 请求体：100 MiB（**超出就完全绕过我们**）

| 上传 | 结果 | 谁答的 |
|---|---|---|
| 99 MiB | **201**，有 `x-request-id` | agentshare |
| 300 MiB 经代理地址 | **413**，`content-type: text/html`、**无** `x-request-id`、0.06s | **Cloudflare** |
| 300 MiB 经直连地址 | **201**，1.57s，200 MB/s | agentshare |

→ **超限时调用方拿到的是 CF 的 HTML 错误页，不是 API 的 JSON** —— 没有 `hint` 可读。
判别"是谁拒的"两条硬判据：**有没有 `x-request-id`**、**`content-type` 是不是 HTML**。

### 响应体：**没有**同等限制（这点容易搞反）

| 下载 300 MiB | 结果 |
|---|---|
| 经直连地址 | 200，1.33s，236 MB/s |
| **经代理地址（CF）** | **200，完整 314572800 字节，3.33s，94.6 MB/s** |

→ CF 的 100 MiB 只管**请求体**。所以**分享链接照旧放在代理域名上没问题**，
大文件也能从 CF 下回来（只是比直连慢一半）。

### 100 秒的超时

CF 对源站响应有 100 秒上限（524）。**这条没有实测**，是文档口径。
慢链路上传大文件即使没到 100 MiB 也可能撞上它 —— 真遇到了就改走直连地址。

---

## 五、日常运维

```bash
systemctl status agentshare
journalctl -u agentshare -f
curl -sS https://<域名>/healthz

# root token 从这里读（0600，别贴到聊天里）
grep AGENTSHARE_ADMIN_TOKEN /etc/agentshare.env

# 数据在哪
#   /var/lib/agentshare -> private/agentshare   （DynamicUser 的原因，是软链）
#   agentshare.db / blobs/ab/cd/<sha256> / tmp/

# 升级
HOST=root@host ./deploy/deploy.sh

# 端到端自检（30 项断言，会自己清理干净）
AGENTSHARE_ADMIN_TOKEN=$(grep -oP '(?<=^AGENTSHARE_ADMIN_TOKEN=).*' /etc/agentshare.env) \
  bash scripts/smoke.sh https://<域名>

# 打直连地址时要指明"期望哪个域名出现在 URL 里"，否则分享链接那条会假失败
EXPECT_BASE=https://<代理域名> bash scripts/smoke.sh https://dl.<域名>
```

### 几个容易踩的

- **改完 nginx 立刻打第一个请求，可能拿到旧配置的答复。** reload 起来的新 worker 生效之前，
  旧 worker 还在处理连接 —— 我第一次测直连地址的 301 就撞上了，看起来像"vhost 没生效"。
  **首轮异常先原样复测一次再下结论**（同族的坑：CF 规则变更、部署收敛窗口）。
- **`systemctl is-active` ≠ "服务已经能应答"**。systemd 在进程 exec 之后就报 active，
  那时它还没 bind 端口。`install.sh` 现在是**轮询 `/healthz`** 才判定成功。
- **`RestrictAddressFamilies` 必须带 `AF_NETLINK`**（单元里已经带了）。
  Go 在 Linux 上靠 netlink 枚举网卡，少了它 `net.Interfaces()` 直接报错，
  而症状是"本机没有 IPv6"这种**看起来像环境问题**的错 —— 静默降级。
- **别在管道后面接 heredoc**：`tar -czf - . | ssh host 'tar -xzf - && bash -s' <<EOF…`
  里的 heredoc 会**抢走 stdin**，tar 收到的是脚本文本。
  同理，**`ssh host "…" <<EOF`（不带引号）会在本地展开 heredoc 里的 `$(…)` 和 `$VAR`** ——
  远程那段命令会在你本机先被求值一次。要展开就显式用 `VAR=v ssh host 'bash -s' <<'EOF'`。
- **打包加 `COPYFILE_DISABLE=1`** 可少传 macOS 的 `._*` 同伴文件。
- **公网仓库里别写源站 IP**（这份文档用 `<域名>` / `<代理域名>` 占位就是这个原因）——
  否则等于把 CDN 隐藏源站的效果从仓库里抵消掉。
