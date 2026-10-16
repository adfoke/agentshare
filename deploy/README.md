# 部署

三种放法，按需求叠加：

| 场景 | 用哪些文件 |
|---|---|
| 内网 / 直连端口 / 前面已有反代 | `install.sh` + `agentshare.service` |
| 公网域名，Cloudflare 橙云在前 | 上面 + `nginx/*`（代理地址） |
| **还要传大文件**（超过橙云的上限） | 上面 + 一条**灰云直连地址**（见第三节；没有成品文件，那节给了骨架） |

## 〇、参考实例

下面几节是**通用配方**（`<域名>` / `<代理域名>` / `root@<主机>` 都是占位符，别照抄）。
真正跑过的实例有三个，其中只有一个还在跑 —— **`nginx/share.tocli.cc.conf` 就是它那份**，
也是本目录**唯一**留着的 vhost（另外两个已随各自的实例删掉，那两种形态改用第二节的骨架）：

| 实例 | 主机 | 回源口 | 状态 |
|---|---|---|---|
| **`share.tocli.cc`** | LA（Vultr） | **8443** | **在跑**（CF 橙云）—— 即 `nginx/share.tocli.cc.conf` |
| `tempsnap.cc` | LA 2（DMIT） | **443** | 2026-10-16 已下线并删除（域名解析也摘了）|
| `dl.tempsnap.cc` | LA 2 | 443（灰云直连） | 随 `tempsnap.cc` 一并下线 |

→ **新机器只需要先定一件事：源站的 443 空着没有。**
  空着就直接听 443 —— 最省事，连 CF 的 Origin Rule 都不需要（本节末尾给了最小示例）；
  被别的服务占着就挑一个别的端口（`share.tocli.cc` 用的是 8443），
  再让 CF 的 Origin Rule 把 https 访客的回源口指过去 —— 见第二节「回源口」。
→ 第三节的直连地址（`dl.*`）**现在没有在跑的实例**，那节的骨架是唯一的一份。

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

# ⚠️ 必须在装 vhost 之前：cloudflare-guard.conf 用的 $agentshare_cf_peer 就定义在这里
#    生成的 conf.d/cloudflare-geo.conf 里。反了的话 nginx -t 报未知变量、reload 被卡住，
#    从那一刻起整台机器的配置都改不动（这是真踩过的坑，见第六节）
bash nginx/update-cloudflare-ips.sh           # 生成 realip + geo 两个 conf.d 文件

# 本目录留着的 vhost 只有这一份（443 被占着 → 听 8443）；443 空着的那种形态见下面的小例。
install -m 0644 nginx/share.tocli.cc.conf     /etc/nginx/sites-available/<域名>.conf
ln -sf /etc/nginx/sites-available/<域名>.conf /etc/nginx/sites-enabled/
nginx -t && systemctl reload nginx
```

照抄后要改：`server_name`、证书路径、日志文件名、以及要不要 `default_server`
（那份里连 8443 + Origin Rule 一起带着说明，443 空着时按下面这个骨架改）。

**源站 443 空着**（最省事的一种，连 CF 的 Origin Rule 都不用配）：

```nginx
server {
    listen      80;
    listen      [::]:80;
    listen      443 ssl;
    listen      [::]:443 ssl;
    http2       on;
    server_name <你的域名>;

    ssl_certificate     /etc/nginx/ssl/<你的域名>.crt;   # 自签即可：Full 模式不校验源站证书
    ssl_certificate_key /etc/nginx/ssl/<你的域名>.key;

    # 别用默认的 combined —— 它会把 /s/<token> 明文写进日志（见上面那段）
    access_log /var/log/nginx/<域名>.access.log agentshare;
    error_log  /var/log/nginx/<域名>.error.log;

    include /etc/nginx/snippets/acme-challenge.conf;

    location / {
        include /etc/nginx/snippets/cloudflare-guard.conf;   # 见第三节：这个端口谁都能连
        proxy_pass http://127.0.0.1:8080;
        include /etc/nginx/snippets/agentshare-proxy.conf;
    }
}
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

### 回源口：源站的 443 空着，还是被占着

CF 的回源口**默认跟随访客的 scheme**（http 访客走 80、https 访客走 443），
跟 zone 的 SSL 模式无关。所以先看一眼源站的 443：

- **空着** → 什么都不用配。`tempsnap.cc` 就是这个形态：自签证书 + `listen 443 ssl`，
  Full 模式下 CF 不校验它，一条 CF 规则都不需要。
- **被别的服务占着** → nginx 只能听一个别的端口（`share.tocli.cc` 那台用的是 8443，
  因为 443 是 xray REALITY 的），再用 **Origin Rule**（phase `http_request_origin`）
  把 https 访客的回源口指过去。

⚠️ **443 被占着却不配这条规则**：CF 照旧打 443 → 落到那个别的服务上 →
REALITY 会把未认证握手**回落给它的 `dest`**（现在是 `www.amazon.com`，早先是
`www.yahoo.co.jp`）→ **访客看到的是别人站点的 404 页**。
判别指纹：`server: cloudflare` + CF 的 ray 头 + **body 根本不是你的站点**。
（`dest` 是会变的 —— 拿不准就去读那台机器的 xray 配置，别背域名。）

| 表达式 | 回源口 | |
|---|---|---|
| `(http.host eq "<域名>" and ssl)` | `8443` | **必需，只要这一条** |
| `(http.host eq "<域名>" and not ssl)` | `80` | 冗余（默认就是 80）；带着 `and not ssl` 所以无害 |

★ **为什么"只要一条"**：http 访客的回源口本来就是 80，多写一条属于重复；
而**没带 `not ssl` 的「一律 → 80」是有害的** —— 它会把 **https 访客**也送回明文口，
症状是 CF 的 `400 Invalid URL` 或打回 443 的那个服务，和"规则没生效"长得几乎一样
（2026-10-11 在 tomoon 上现场证明过）。
★ 同理**别写成"一律 → 8443"**：往回源口发明文请求会被拒
（`The plain HTTP request was sent to HTTPS port`）。
★ **CF 规则有传播窗口**：改完立刻测可能拿到旧边缘的答复（实测中间夹过一次 400，
等 60 秒 → 10/10 全 200）。**首轮异常先原样复测再下结论。**

**怎么改** —— 要 zone 级写权限（CF 面板点，或一个带 Zone → Origin Rules 写权限的 token）。
`cf` CLI 的 beta 版子命令全改名了，直接打 API 更可控：

```bash
ZID=<zone id>
# 1) 先备份 —— 这份 JSON 的 result.rules 就是待会儿 PUT 的 body
curl -sS -H "Authorization: Bearer $CF_TOKEN" \
  "https://api.cloudflare.com/client/v4/zones/$ZID/rulesets/phases/http_request_origin/entrypoint" \
  > origin-rules-backup.json
# 2) 在每条规则 expression 的主机名括号里补上你的域名（`or http.host eq "…"`），其余字段原样照抄
# 3) PUT 同一个 URL，body = {"rules": [...]}
```

⚠️ **phase entrypoint 不支持 DELETE**（回 405）：清空要用 `PUT {"rules": []}`，
而更新是**整体替换** —— 所以第 1 步的备份不是客套话。
★ 回填时我只留了 `action` / `action_parameters` / `description` / `enabled` / `expression`
这五个字段（把 `id`/`ref`/`version`/`last_updated` 剥掉），一次就过 ——
**至于原样带回去会不会被拒，没测过，别当结论。**

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
# vhost 照下面那份骨架写成 /etc/nginx/sites-available/dl.<域名>.conf
# （成品文件已随其实例一起移除；443 被占着的机器得另挑端口，见第二节「回源口」）
ln -sf /etc/nginx/sites-available/dl.<域名>.conf /etc/nginx/sites-enabled/
certbot certonly --webroot -w /var/www/certbot -d dl.<域名> \
  --non-interactive --agree-tos --register-unsafely-without-email
# 证书换新后 nginx 不会自己重载，必须挂钩子，否则"续期成功"但线上仍是旧的、到点就过期
install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy
printf '#!/bin/sh\nsystemctl reload nginx\n' > /etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh
chmod 0755 /etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh
systemctl enable --now certbot.timer
```

★ **这份 vhost 的实例（`dl.tempsnap.cc`）已经随 `tempsnap.cc` 一起下线，成品文件也移除了**
—— 下面是最小的等价骨架（上面那三条"为什么"就是它的注释版）：

```nginx
# :80 只做两件事：伺候 ACME 挑战，其余全部 301 到 https ——
# 直连没有 CDN 那一跳，token 走明文会被链路上的人读走。
server {
    listen      80;
    listen      [::]:80;
    server_name dl.<域名>;
    access_log /var/log/nginx/access.log agentshare;

    include /etc/nginx/snippets/acme-challenge.conf;

    location / {
        return 301 https://$host$request_uri;
    }
}

# :443 才是真正的直连口。**这里没有 cloudflare-guard，也不该有** ——
# 灰云意味着访客本来就不是 CF，挂了就把真实访客全挡在门外。
server {
    listen      443 ssl;
    listen      [::]:443 ssl;
    server_name dl.<域名>;
    access_log /var/log/nginx/access.log agentshare;

    ssl_certificate     /etc/letsencrypt/live/dl.<域名>/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/dl.<域名>/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;

    # 刻意不做 default_server：裸 IP / 没配置过的名字都落回代理域名那个 vhost，
    # 在那里被 guard 挡掉。要够到这个口，就得指名道姓。
    location / {
        proxy_pass http://127.0.0.1:8080;
        include /etc/nginx/snippets/agentshare-proxy.conf;
    }
}
```

⚠️ **证书必须在 443 块之前拿到**：这份 vhost 引用了
`/etc/letsencrypt/live/dl.<域名>/fullchain.pem`，文件不存在时 `nginx -t` 直接失败。
先只装 80 块、签发、再补 443 块（或者让首次签发走默认 server 的 ACME location）。

### ⚠️ 为什么必须给代理地址加 guard

**只要端口不是"只有 CF 能连"，就得挂。** 触发它的情形有两种，而且经常同时出现：

- **有直连地址**（第三节）→ 那个端口必须对全网开放（客户端不再是 CF 了）；
- **防火墙没按 CF 网段收口** → 端口本来就对全网开着（`tempsnap.cc` 那台的 80 就是这样，
  `share.tocli.cc` 那台的 80 也是）。

不管哪种，任何知道源站地址的人都能 `Host: <代理域名>` 直接打过来，
**整条绕过 CDN** —— 它的 WAF、bot 规则、限速、以及直连地址存在的理由（请求体上限）全部失效。

所以「只有 Cloudflare 能访问代理域名」这条防线落在 **nginx** 上：
`cloudflare-guard.conf` include 进**每个** `location /`（或 :443 的 server 级），
对端不在 CF 网段就 `return 403`。
⚠️ **它对"这个端口"生效，不是对"这个域名"** —— 所以有几个 server 块/端口就要挂几次，
少挂一处就是少一处（下面那条 `tempsnap.cc` 的 :80 就是漏出来的）。

⚠️ **两处的位置不一样，这不是笔误**：

| 端口 | 位置 | 为什么 |
|---|---|---|
| :443 | server 块顶层（`location` 之前） | 那一块没有哪个路径需要"先放行再判断"，非 CF 的请求哪个 path 都不该到 |
| :80 | **`location /` 里面** | 上面紧挨着 ACME 的 `location ^~`。server 级的 `if` 跑在 **server-rewrite 阶段、早于 location 匹配**，会把挑战一起 403 掉 —— 而 http-01 挑战只走 :80、且来自 Let's Encrypt 而不是 CF，挡了就等于断掉续期。放进 `location /` 后检查发生在 location 选定之后，`^~` 那个 location 仍然优先命中 |

（实测过：把同样的检查放在 :443 的 **server 级**，从非 CF 地址直连
`/.well-known/acme-challenge/` 拿到的是 **403** —— 那个 location 其实够不着。）

★ **两个端口写在同一个 server 块里时**（`share.tocli.cc` 就是：一块听 80 + 8443），
`location /` 是唯一的位置，"服务器级"这个选项直接不存在 —— 而 ACME 的 `location ^~`
照样优先命中（实测：非 CF 对端打 `/.well-known/acme-challenge/<探针>` 得到 **404** 而不是 403，
说明它没被 guard 吃掉；同一个对端打 `/healthz` 是 403）。

★ **别漏掉任何一条进来的路。** `tempsnap.cc` 那台最早只挂了 :443 —— 因为它的 :80 那个块是
:80 的 `default_server`，`http://<源站IP>/`（连 Host 头都不用）就直接被转发给 agentshare，
绕开了 CF 的 WAF / bot 规则 / 限速。（`:443` 那次靠 `default_server` 落到 guard 上挡住了，
`:80` 没有 —— 而 `deploy/README` 里本来就写着"两个 server 块各 include 一次"，是配置漏了。）
`share.tocli.cc` 那台没有这个问题：它不是 `default_server`，
裸 IP / 别的 Host 会落回 clash-sub 的 404。

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

# 端到端自检（41 项断言，跑完自己清理干净）
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

---

## 六、`share.tocli.cc`（LA）这台的具体情况

同一个程序套进一台**已经跑着别的服务**的机器，所以下面四处选择值得单独记一笔
（它们和 vhost 里的注释是一件事）：

| 决定 | 为什么 |
|---|---|
| 听 **80 + 8443**，不听 443 | 443 被同机的 xray REALITY 占着 —— 只能让位，再靠 CF 的 Origin Rule 把 https 访客指过来（见第二节「回源口」） |
| **共用** `/etc/nginx/ssl/tocli.cc-selfsigned.{crt,key}` | 那台本来就有 hyper / clash / mdview / tomoon 四个站共用一张自签证书；加个 SAN 名字比再签一张省事，Full 模式不校验、自签够用 |
| 不做 `default_server` | 那台的 default_server 是 clash-sub（它原来就是）→ 裸 IP / 别的 Host 仍旧落回它的 404，这个 vhost 只应答自己的域名 |
| guard 放在 **`location /` 里**（两个端口都放） | 一个 server 块同时听 80 和 8443，而 ACME 的 `location ^~` 要在两个端口上都够得着 —— 原因见第三节「为什么必须给代理地址加 guard」 |

另外两点：这个 vhost 的日志用 `agentshare` 格式（`/s/<token>` 会写成 `/s/<redacted>`，
理由见第二节）；它**不需要** `dl.*` 那种直连地址（大文件上限直接设成 100 MiB，
跟橙云的真实上限对齐，见第四节）。

**装法：顺序有讲究**（2026-10-16 实测跑通的就是这个顺序）。

```bash
scp -r nginx/{agentshare-proxy,acme-challenge,cloudflare-guard,hardening}.conf \
       nginx/share.tocli.cc.conf nginx/update-cloudflare-ips.sh root@<LA>:/tmp/ash-nginx/

ssh root@<LA> 'set -e
  install -d -m 0755 /etc/nginx/snippets /var/www/certbot
  install -m 0644 /tmp/ash-nginx/agentshare-proxy.conf /etc/nginx/snippets/
  install -m 0644 /tmp/ash-nginx/acme-challenge.conf   /etc/nginx/snippets/
  install -m 0644 /tmp/ash-nginx/cloudflare-guard.conf /etc/nginx/snippets/
  install -m 0644 /tmp/ash-nginx/hardening.conf        /etc/nginx/conf.d/00-agentshare-hardening.conf
  bash /tmp/ash-nginx/update-cloudflare-ips.sh
  install -m 0644 /tmp/ash-nginx/share.tocli.cc.conf   /etc/nginx/sites-available/share
  ln -sf /etc/nginx/sites-available/share /etc/nginx/sites-enabled/share
  nginx -t && systemctl reload nginx'
```

1. 先装 snippet + `hardening.conf`：**此刻没有任何 vhost 引用它们**，坏也坏不到线上。
2. 再跑 `update-cloudflare-ips.sh`：它生成 `cloudflare-{realip,geo}.conf`，
   **guard 依赖的 `$agentshare_cf_peer` 就在 geo 里**。少了它 `nginx -t` 直接报未知变量，
   reload 被卡住 → **整台机器的配置从此改不动**（这正是「先放坏文件再测」的坑）。
3. 然后装 vhost + 软链。
4. **最后换证书** —— 共用证书一换就影响那四个站，所以放最后，且 `nginx -t` 过了才 reload。
   生成命令（EC P-256、十年、SAN 六个名字，别的参数照原样）：

```bash
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
  -subj "/CN=tocli.cc" \
  -addext "subjectAltName=DNS:hyper.tocli.cc,DNS:clash.tocli.cc,DNS:mdview.tocli.cc,DNS:tomoon.space,DNS:www.tomoon.space,DNS:share.tocli.cc" \
  -keyout /tmp/n.key -out /tmp/n.crt
install -m 0644 /tmp/n.crt /etc/nginx/ssl/tocli.cc-selfsigned.crt
install -m 0600 /tmp/n.key /etc/nginx/ssl/tocli.cc-selfsigned.key   # 私钥 600，别用 644
```

### CF 侧

已配好（2026-10-16）：把 `share.tocli.cc` 加进 `tocli.cc` zone 原有的那两条 Origin Rule
（`… and ssl` → 8443、`… and not ssl` → 80，即 ruleset version 6）。
**配法和坑都在第二节「回源口」**，这里只留一条教训：

⚠️ **"我手上没有这个能力"和"这件事做不了"是两回事。** 当天装机时本机确实没有任何
CF 的 zone 级写通道（`cf` 已卸载、wrangler 的 OAuth 只有 `zone (read)`、拿它打
`/zones/<id>/rulesets` 回 `Authentication error`），于是我把"没有写通道"当成结论，
差点让 https 就这么搁着。实际上那只是**当时那组凭据的状态** ——
装回 `cf` 并登录之后，十分钟就补上了。
→ 写文档/写结论时把这两件事分开：**「当前这个工具链做不到」** ≠ **「这件事做不到」**。
→ 真正的收获是当时**没有因此偷懒**：应用和 nginx 全部装好验好（41 项自检在源站侧全过），
  规则一加立刻全绿。

### ⚠️ 副作用：`hardening.conf` 里那条 HSTS 是**全机**的

它写在 http 上下文，所以 LA 上**每个** vhost 都会带上
`Strict-Transport-Security: max-age=15552000` —— 除非该 location 自己也有 `add_header`
（`add_header` 的规矩是"子层有就完全不继承父层"）。实测那台五个站：

| 站点 | 改前 | 改后 |
|---|---|---|
| hyper `/` | 无 | **无**（它自己的 security-headers 覆盖掉了） |
| clash `/` | 无 | **有**（那个 location 只有 `return 404`，没有 add_header） |
| clash `/s/` | 无 | **无**（订阅路径有自己的 add_header，不受影响） |
| mdview `/`、tomoon `/` | 有 | 有 |
| share `/` | — | 有（本意） |

不想要这条就把它注释掉再装 —— 但那会让**仓库与线上差一行**，所以要么改仓库、要么留着。

---

## 七、下线一个实例（收尾清单）

2026-10-16 把 `tempsnap.cc`（LA 2）那套整个删掉时走的就是这些步骤。
域名换成自己的，**顺序别改：先备份，再从外往里拆，最后收防火墙。**

```bash
# 1) 先看数据 —— 决定要不要留备份（那次是 7 文件 / 4 分享 / 0 token / 1.6 MB，全是测试件，
#    但仍然整包留了一份：数据 + env 里的 root token + nginx 件 + 单元 + 证书副本）
D=/var/lib/private/agentshare          # DynamicUser 的真实目录，不是那个软链
python3 -c "import sqlite3;print(sqlite3.connect('file:$D/agentshare.db?mode=ro',uri=True)\
  .execute('select count(*) from files').fetchone())"
du -sh "$D"

# 2) 停服务 → 删单元 + 二进制 + 凭据文件
systemctl disable --now agentshare.service
rm -f /etc/systemd/system/agentshare.service /usr/local/bin/agentshare /etc/agentshare.env
systemctl daemon-reload
rm -f /var/lib/agentshare && rm -rf "$D"          # ⚠️ 两个都要删，见下面第 1 条

# 3) 撤 nginx：vhost（enabled + available 两处）、snippet、conf.d、ACME webroot
rm -f /etc/nginx/sites-enabled/<域名> /etc/nginx/sites-available/<域名>
rm -f /etc/nginx/snippets/{agentshare-proxy,acme-challenge,cloudflare-guard}.conf
rm -f /etc/nginx/conf.d/{00-agentshare-hardening,cloudflare-realip,cloudflare-geo}.conf
rm -rf /var/www/certbot
nginx -t && systemctl reload nginx

# 4) 证书用 certbot 自己删，不要手删目录
certbot delete --cert-name <域名> --non-interactive

# 5) 没有站点要服务了 → 停掉 nginx 本体（可选）；防火墙收回最小
systemctl stop nginx && systemctl disable nginx
ufw delete allow 80/tcp && ufw delete allow 443/tcp

# 6) 复核三层
find / -xdev -iname '*agentshare*'    # 期望 0 命中（排除自己的备份目录）
grep -rl agentshare /etc              # 期望 0 —— 「配置里真的不残留引用」的硬判据
ss -lntp                              # 期望只剩 22（+ 本地 53 stub）
# 再从外部探一次：22 open，80 / 443 / 8080 全部 closed/filtered
```

★ **三条实测事实**（这轮现场发现的，都不是推理）：

1. **`DynamicUser=` 的状态目录有**两个**：`/var/lib/agentshare`（软链）**和**
   `/var/lib/private/agentshare`（真实数据）。**少删一个就是残留** ——
   而且 `find` 会被那个软链误导，别只看它。
2. **`nginx -t` 在 `sites-enabled` 全空（零个 server 块）时仍然 successful。**
   所以"没有站点"时不必为了让它能启动而留一个 stock `default` 站 ——
   直接 stop + disable 就干净，配置树也不会变成一颗地雷。
3. **证书要交给 `certbot delete`，不要手删 `/etc/letsencrypt/live/<域名>`**：
   前者会把 renewal 配置一起收走。手删的话，域名都没了，
   但续期任务每天还会报一次失败 —— 顺手把两条路都清掉
   （Debian/Ubuntu 上是 `certbot.timer` + `/etc/cron.d/certbot`）。
