# 部署

两种放法，按"要不要挂域名"选：

| 场景 | 用哪些文件 |
|---|---|
| 内网 / 直连端口 / 前面已有反代 | `install.sh` + `agentshare.service` |
| 公网域名，Cloudflare 橙云在前 | 上面两个 + `nginx/` |

---

## 一、装成 systemd 服务

```bash
make build-linux                     # 出 amd64 + arm64 静态二进制
scp bin/agentshare-linux-amd64 deploy/install.sh deploy/agentshare.service root@host:/tmp/ash/
ssh root@host 'bash /tmp/ash/install.sh'
```

`install.sh` 是**幂等**的：重跑只换二进制、重启服务，**不会动**已有的 root token 和数据目录。
第一次跑会生成 root token 写进 `/etc/agentshare.env`（0600）并打印一次。

部署参数从环境读，所以一台机器的设置活在**你敲的那条命令**里，而不是活在服务器上
一个被手改过的文件里：

```bash
ASH_BASE_URL=https://files.example.com \
ASH_BEHIND_PROXY=true \
ASH_MAX_UPLOAD=100M \
ASH_DEFAULT_TTL=30d \
  bash install.sh
```

| 变量 | 写进 env 文件 | 说明 |
|---|---|---|
| `ASH_BASE_URL` | `AGENTSHARE_BASE_URL` | 响应里 URL 的前缀。**反代后面必须设**，否则回 `127.0.0.1:8080` |
| `ASH_BEHIND_PROXY` | `AGENTSHARE_BEHIND_PROXY` | `true` 才读 `X-Forwarded-Proto/Host` |
| `ASH_MAX_UPLOAD` | `AGENTSHARE_MAX_UPLOAD` | 单次上传上限 |
| `ASH_DEFAULT_TTL` | `AGENTSHARE_DEFAULT_TTL` | 没指定寿命时的默认值，不设 = 永不过期 |
| `ASH_TRUSTED_PROXIES` | `AGENTSHARE_TRUSTED_PROXIES` | 默认 `127.0.0.1/32,::1/128` |

⚠️ **`ASH_BASE_URL` 只在首次安装时写入**。已经装过的机器上改，要直接编辑 `/etc/agentshare.env`
再 `systemctl restart agentshare`。

## 二、nginx + Cloudflare

需要三个文件：

```bash
install -m 0644 nginx/agentshare-proxy.conf   /etc/nginx/snippets/agentshare-proxy.conf
install -m 0644 nginx/tempsnap.cc.conf        /etc/nginx/sites-available/<你的域名>.conf   # 记得改成自己的 server_name
bash nginx/update-cloudflare-ips.sh           # 生成 /etc/nginx/conf.d/cloudflare-realip.conf
ln -sf /etc/nginx/sites-available/<域名>.conf /etc/nginx/sites-enabled/
nginx -t && systemctl reload nginx
```

### 为什么 nginx 那份配置里是这几条

- **`client_max_body_size 0`** —— 这是**必须**的。nginx 默认 1 MiB，会把几乎每个文件都挡在
  nginx 这一层，而且返的是 nginx 自己的错误页，调用方根本不知道撞的是哪个限制。
- **`proxy_request_buffering off`** —— 不把请求体先落盘到 nginx 的临时文件再转发，
  大文件上传少一次写放大；被拒的上传也是立刻失败，而不是等客户端把整个文件发完。
- **`proxy_set_header X-Forwarded-For $remote_addr`** —— 不是 `$proxy_add_x_forwarded_for`。
  Cloudflare 在后面时，realip 模块已经把 `$remote_addr` 改写成真实客户端了，
  再往后追加会让同一个客户端被记两遍。
- **自签证书** —— zone 的 SSL 模式是 `Full`（不是 `Full (strict)`），
  CF 到源站这一跳不校验证书，自签够用。**如果哪天改成 `Full (strict)`，必须换 CF Origin 证书或
  Let's Encrypt，否则全站 526。**

### Cloudflare 的 real_ip

橙云在前时，**每个请求都从 CF 的地址到源站** —— 不处理的话 nginx 会给每个客户端都记一个 CF 的 IP，
agentshare 的 access log 直接废掉。`update-cloudflare-ips.sh` 把 CF 公布的网段写成
`set_real_ip_from`，并让 realip 读 `CF-Connecting-IP`。

- 它会**先逐条严格校验 CIDR，再写文件**。不这么做的话，一个畸形网段要等 `nginx -t` 才暴露 ——
  而那时候坏文件已经装进去了，nginx 从此 reload 不了。
- ⚠️ CF 的 `ips-v4` **末尾没有换行**。两份列表拼成一个流时，最后一条 v4 和第一条 v6 会粘成
  `131.0.72.0/222400:cb00::/32` 这种东西，nginx 报 `host not found in set_real_ip_from` ——
  看着像 DNS 问题，实际是少了个换行。脚本已经把两份列表分开取。
  （同一个坑还会让 `wc -l` 少算一行。）

### 验证 real_ip 真的生效

**不能**从源站本机发一个带 `CF-Connecting-IP` 的请求来试 —— 源站不在 CF 网段里，
realip 会（正确地）忽略它，于是看起来"没生效"。要走真实路径：从外部打一次域名，
然后看两侧日志里的客户端地址是不是你自己的 IP。

```bash
tail -3 /var/log/nginx/access.log                      # 第一列应是真实客户端
journalctl -u agentshare -n 50 | grep -o '"ip":"[^"]*"' | sort | uniq -c
```

## 三、Cloudflare 橙云下的硬限制

**经 CF 的单文件上限 = 100 MiB**（Free 计划对请求体的限制）。实测：

| 上传大小 | 结果 |
|---|---|
| 99 MiB（103,809,024 B） | **201**，`x-request-id` 在，1.57s —— agentshare 处理的 |
| 101 MiB（105,906,176 B） | **413**，`content-type: text/html`、**没有** `x-request-id`、0.05s —— **CF 挡的** |

→ 所以超限时调用方拿到的是 **CF 的 HTML 错误页，不是 agentshare 的 JSON**。
`AGENTSHARE_MAX_UPLOAD` 设成 `100M` 就是为了让 `GET /` 里自述的上限跟真实能走通的一致。
**要传更大的文件，只能把域名改成灰云（DNS only）并配真证书** —— 代价是源站 IP 公开。

## 四、日常运维

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

# 跑一遍端到端自检（30 项断言，会自己清理干净）
AGENTSHARE_ADMIN_TOKEN=$(grep -oP '(?<=^AGENTSHARE_ADMIN_TOKEN=).*' /etc/agentshare.env) \
  bash scripts/smoke.sh https://<域名>
```

### 几个容易踩的

- **`systemctl is-active` 不等于"起来了"**。systemd 在进程 exec 之后就报 active，
  那时它还没 bind 端口。`install.sh` 现在是**轮询 `/healthz`** 才判定成功 ——
  先前的版本查单元状态再发请求，会跟自己的启动赛跑，输的那次看起来跟"启动失败"一模一样。
- **数据目录别在安装脚本里预建**。单元用了 `DynamicUser=` + `StateDirectory=`，
  systemd 会自己建并持有它；脚本先建一个"公开的"，systemd 就会把它迁移到
  `/var/lib/private/` 并在日志里报一条看着像出错的提示。
- **`RestrictAddressFamilies` 必须带 `AF_NETLINK`**（单元里已经带了）。
  Go 在 Linux 上靠 netlink 枚举网卡，少了它 `net.Interfaces()` 直接报错，
  而症状是"本机没有 IPv6"这种**看起来像环境问题**的错 —— 静默降级。
- **别在管道后面接 heredoc**。`tar -czf - . | ssh host 'tar -xzf - && bash -s' <<EOF …`
  里的 heredoc 会**抢走 stdin**，tar 收到的是脚本文本。（打包时用 `COPYFILE_DISABLE=1 tar …`
  可以少传 macOS 的 `._*` 同伴文件。）
