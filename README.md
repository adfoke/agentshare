# agentshare

**给 agent 用的文件中转站** —— 上传、分享、下载，全部走一个纯 HTTP API。
没有网页，没有登录页，没有前端构建产物。一个 Go 二进制 + 一个数据目录，就是全部。

```
上传   curl -T ./report.pdf -H "Authorization: Bearer $T"            $BASE/v1/files
分享   curl -X POST -H "Authorization: Bearer $T" -d '{"file_id":"f_..."}' $BASE/v1/shares
下载   curl -O -J $BASE/s/<share-token>            # 拿到链接的人不需要任何凭据
```

---

## 为什么是这些设计

面向 agent 的服务，瓶颈不在人机界面上，而在「一个只会发 HTTP 请求的程序能不能
自己搞明白该干什么」。所以：

- **请求体就是文件本身。** 不需要 multipart 包装，不需要 base64，不需要
  `{"file": "..."}` 这种信封。`curl -T`、`requests.put(data=open(f,'rb'))`、
  管道直传都天然可用。multipart 也收（取第一个 file part），因为很多工具默认就这么发。
- **内容寻址。** 每个文件按 SHA-256 存一份，元数据单独一张表。
  同样的字节上传两次不会占两份磁盘；**重传同一个文件是幂等的**（返回原来那条记录，
  不是复制一份），所以网络抖动时客户端可以放心重试。
- **自描述。** `GET /` 返回端点表 + 可直接粘贴的 curl 命令；`GET /openapi.json` 是
  完整的 OpenAPI 3.1；`GET /llms.txt` 是写给 agent 读的纯文本说明书，里面的 URL 会
  按请求方的地址替换好。一个只知道 URL 的 agent 能自己把整个面摸清楚。
- **错误是给程序看的。** 统一 `{"error":{"code","message","hint"}}`，`code` 稳定可分支；
  `hint` 通常直接写出下一步该调哪个接口。
- **每个响应都带绝对 URL。** 拿到 `file.urls.content` 就能直接用，不用自己拼路径。
- **能力链接而不是账号。** 给别人一个文件，不是给他建账号，是给他一条 `/s/<token>`。
  链接可以设过期时间、下载次数上限，随时吊销。

## 快速开始

```bash
make build                       # 需要 Go 1.21+；产物在 bin/agentshare
./bin/agentshare --data-dir ./data --listen 127.0.0.1:8080
```

第一次启动会自动生成 root token，写进 `data/admin.token`（权限 0600）并打印一次：

```
WARN generated a root token; it is saved in the data directory
     path=/abs/path/data/admin.token token=ash_xxxxxxxx...
```

```bash
export T=ash_xxxxxxxx...        # 或者 AGENTSHARE_ADMIN_TOKEN=... 启动
export B=http://127.0.0.1:8080

curl -sS -H "Authorization: Bearer $T" $B/v1/whoami          # 我这个 token 能干什么
echo hi | curl -sS -T - -H "Authorization: Bearer $T" -H "X-Filename: hi.txt" $B/v1/files
curl -sS -H "Authorization: Bearer $T" "$B/v1/files?limit=10"
```

跑一遍端到端自检（30 项，会自己清理干净）：

```bash
AGENTSHARE_ADMIN_TOKEN=$T ./scripts/smoke.sh $B
```

## 权限模型

| 凭据 | 能做什么 |
|---|---|
| **root token**（`AGENTSHARE_ADMIN_TOKEN`） | 全部：上传、下载、列表、删除、建分享、管理 token |
| **受管 token**（`POST /v1/tokens`） | 只做 scopes 里写的事。`upload` / `download` / `list` / `delete` / `share` / `admin`，可以带过期时间 |
| **分享链接**（`/s/<token>`） | 只能下载那一个文件。**不需要任何凭据** —— 持有链接就是凭据 |

也就是说：**owner 全权，别人只能通过分享链接看和拿**。给一个 CI 任务发个
`["upload"]` 的 token、30 天过期，它连列表都看不见；给同事一条 7 天 / 3 次的分享链接，
他不需要知道你这个服务的存在。

token 秘密只在创建那一次返回，之后只能吊销不能取回（库里存的是 SHA-256）。

## API 一览

公开（无需凭据）：`GET /` `GET /healthz` `GET /openapi.json` `GET /llms.txt`
`GET /s/{token}` `GET /s/{token}/info`。其余都要 bearer token。

| 方法 | 路径 | scope | 说明 |
|---|---|---|---|
| POST/PUT | `/v1/files` | upload | 传一个文件，body 是内容 |
| POST | `/v1/text` | upload | 传一小段 UTF-8 文本（JSON 里带 `name`/`content`） |
| PUT | `/v1/content/{sha256}` | upload | 按摘要上传，内容对不上就 400；可安全重试 |
| GET | `/v1/files` | list | 列表，支持 `q` `tag` `since` `until` `order` `cursor` |
| GET | `/v1/files/{id}` | download | 元数据 |
| GET | `/v1/files/{id}/content` | download | 下载字节，支持 Range / ETag / If-None-Match |
| DELETE | `/v1/files/{id}` | delete | 删除，连带吊销它的所有分享链接 |
| GET | `/v1/content/{sha256}` | download | 按摘要下载（不需要知道文件 id） |
| GET | `/v1/content/{sha256}/meta` | download | 问「这些字节你有吗」，不传输内容 |
| POST | `/v1/shares` | share | 建分享链接：`ttl`、`max_downloads`、`label` |
| GET | `/v1/shares` | share | 列分享链接（可按 `file_id` 过滤） |
| DELETE | `/v1/shares/{id}` | share | 吊销 |
| POST | `/v1/tokens` | admin | 签发受管 token |
| GET | `/v1/tokens` | admin | 列 token（不含秘密） |
| DELETE | `/v1/tokens/{id}` | admin | 吊销 |
| GET | `/v1/whoami` | — | 当前凭据的身份和 scopes |
| GET | `/v1/stats` | list | 计数与占用 |
| GET/HEAD | `/s/{token}` | — | 凭分享链接下载（HEAD 不消耗下载次数） |
| GET | `/s/{token}/info` | — | 看看链接后面是什么，不消耗次数 |

上传时的元数据走 header，也可以全部改走 query string（有些 agent 框架更愿意拼 URL）：

```
X-Filename / ?name=      存储用的文件名
X-Tag      / ?tag=       标签，可重复
X-Ttl      / ?ttl=       30m / 24h / 7d / 2w / 秒数；0 或 never = 不过期
X-Meta     / ?meta=      你自己的 JSON 对象，原样存下来
X-Sha256                 断言内容摘要，不符就 400
X-Content-Type           覆盖自动识别
```

## 存储与一致性

```
<data-dir>/
  agentshare.db          元数据（SQLite，WAL）—— files / shares / tokens 三张表
  blobs/ab/cd/<sha256>   文件内容，按摘要分片存放，写下去就不再改动
  tmp/                   上传中的暂存文件，提交时 rename 进 blobs/
  admin.token            自动生成的 root token（0600）
```

- **提交是原子的**：先写 `tmp/`，`fsync`，再 `rename` 进 `blobs/`，读的人永远看不到半个文件。
- **引用计数**：多条记录可以指向同一份 blob（同内容不同文件名）。删掉一条不会动字节，
  删掉最后一条才回收 —— 所以 `DELETE` 的返回里有 `blob_freed`。
- **janitor**：默认每分钟扫一次。过期的 files / shares / tokens 直接删，
  然后做一次全量 blob 巡检，把没人引用的字节（包括崩溃留下的）收掉，顺带清 `tmp/` 里
  超过一小时的残留文件。全量而不是计数器，因为它同时能收拾掉「进程被杀在两次操作之间」的残局。
- 时间戳一律存 **unix 秒整数**。存成带时区的文本，比较时会在第 11 个字符上和
  `datetime('now')` 悄无声息地对不上。

## 部署到 VPS

```bash
# 一次性：装成 systemd 服务，自动生成并保存 root token
make build-linux
scp bin/agentshare-linux-amd64 deploy/install.sh deploy/agentshare.service root@host:/tmp/ash/
ssh root@host 'bash /tmp/ash/install.sh'

# 之后每次发版
HOST=root@host ./deploy/deploy.sh
```

`install.sh` 是幂等的：重跑只换二进制，**不会动**已有的 token 和数据目录。

- `deploy/agentshare.service` —— 单元文件，带一套加固（`DynamicUser`、`ProtectSystem=strict`、
  `CapabilityBoundingSet=` 清空等）。⚠️ 里面 `RestrictAddressFamilies` **必须包含
  `AF_NETLINK`**：Go 在 Linux 上是通过 netlink 枚举网卡的，少了它 `net.Interfaces()` 会报错，
  而症状是「本机没有 IPv6」这种**看起来像环境问题**的错，不是权限错 —— 这是个会静默降级的坑。
- `deploy/nginx.conf.example` —— 反代。两个关键点：`client_max_body_size 0`
  （默认 1 MiB 会把大文件拦在 nginx 就 413），`proxy_request_buffering off`
  （否则 nginx 先把整个上传写进自己的临时文件，再转发）。
  用反代时记得给 agentshare 设 `AGENTSHARE_BASE_URL` 和 `AGENTSHARE_BEHIND_PROXY=true`，
  否则响应里的 URL 会指向 `127.0.0.1:8080`。
- 默认监听 `127.0.0.1:8080`，**不是** `0.0.0.0`：它假定前面有反代。要直接对外就说清楚
  `--listen 0.0.0.0:8080` 并自己想好 TLS。

## 配置

优先级：命令行 flag > 环境变量（`AGENTSHARE_` 前缀）> 默认值。

| flag | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `--listen` | `AGENTSHARE_LISTEN` | `127.0.0.1:8080` | 监听地址 |
| `--data-dir` | `AGENTSHARE_DATA_DIR` | `./data` | 数据目录 |
| `--base-url` | `AGENTSHARE_BASE_URL` | 空 | 响应里 URL 的前缀；空 = 按请求推导 |
| `--admin-token` | `AGENTSHARE_ADMIN_TOKEN` | 空 | root token；空 = 读 `data/admin.token`，没有就生成 |
| `--max-upload` | `AGENTSHARE_MAX_UPLOAD` | `5G` | 单次上传上限，`0` = 不限 |
| `--max-ttl` | `AGENTSHARE_MAX_TTL` | `0` | 寿命上限，超了**报错**而不是悄悄改短 |
| `--default-ttl` | `AGENTSHARE_DEFAULT_TTL` | `0` | 没指定寿命时的默认值，`0` = 永久 |
| `--janitor-interval` | `AGENTSHARE_JANITOR_INTERVAL` | `1m` | 清理频率，`0` = 关掉 |
| `--trusted-proxies` | `AGENTSHARE_TRUSTED_PROXIES` | `127.0.0.1/32,::1/128` | 信谁的 `X-Forwarded-For` |
| `--behind-proxy` | `AGENTSHARE_BEHIND_PROXY` | `false` | 才读 `X-Forwarded-Proto/Host` 来拼 URL |
| `--cors-origin` | `AGENTSHARE_CORS_ORIGIN` | `*` | 空 = 不发 CORS 头 |
| `--log-level` / `--log-format` | `AGENTSHARE_LOG_LEVEL` / `_FORMAT` | `info` / 看是否 TTY | `debug,info,warn,error` / `json,text` |

`--behind-proxy` 默认关是有意的：关着的时候，客户端没法用 `X-Forwarded-Host`
影响服务端吐回来的 URL。

## 客户端

`scripts/ash` 是一个可选的 curl 封装（不是第二套实现，每条命令就是一次 curl）。
不想用直接删掉即可：

```bash
export ASH_BASE=https://files.example.com ASH_TOKEN=ash_...
ash put ./report.pdf --ttl 7d --tag build
ash ls --tag build
ash share f_abc123 --ttl 1d --max 3
ash get https://files.example.com/s/<token>
```

`ash put` / `ash text` 输出一行制表符分隔的 `<id> <sha256> <url>`，`cut -f1` 就能取 id。

## 测试

```bash
make test          # 48 个用例（api 26 / store 10 / config 7 / blob 5），不联网、不依赖 cgo
make vet
make smoke         # 打一个已经在跑的实例，30 项端到端断言
bash deploy/install.sh   # （在服务器上）装完会顺带打一次 /healthz
```

这个 API 其实有三份手写的「接口清单」：`GET /` 的端点表、`openapi.json`、以及 gin
真正注册的路由。任何两份漂移，读的那一方就在被骗。所以有两个对账测试：

- `TestOpenAPICoversEveryDeclaredEndpoint` —— 端点表 ↔ openapi.json
- `TestRouterMatchesDeclaredEndpoints` —— 端点表 ↔ **真实注册的路由**（读 `router.Routes()`）

第二个是关键的：前两份都是文档，只有第三个是服务端真的会响应什么。
（它第一次跑就抓到 `/favicon.ico` 没被记录 —— 那个路由是刻意存在的，用来挡掉浏览器
和扫描器的噪声请求，所以进了测试里一份带注释的例外名单，而不是删掉了事。）

## 明确的边界

没做的、以及为什么：

- **没有 UI**，一行 HTML 都没有。`GET /` 返回的是 JSON。
- **没有断点续传**：Range 支持的是**下载**；上传中断要从头再来。
  按摘要重传是幂等的，代价只是重传流量。
- **没有多副本/对象存储后端**。blob 就落本地磁盘，接口收在一个包内，
  要换 S3/R2 就是换掉那个包的实现。
- **没有限速**。真放公网要限速的话，放在反代那一层比放在这里合适。
- **没有多用户**。root + 受管 token 是刻意的：这是「一个人/一个团队的文件中转站」，
  不是网盘。所有人的文件在同一个命名空间里。

## 许可证

[MIT](LICENSE) © 2026 adfoke

## 目录

```
cmd/agentshare/      入口：配置、日志、janitor、优雅退出
internal/api/        HTTP 层：路由、鉴权、handler、OpenAPI、llms.txt
internal/store/      元数据（SQLite）：files / shares / tokens
internal/blob/       内容寻址存储：暂存、提交、GC
internal/config/     配置：flag + 环境变量
internal/id/         短 ID 生成（Crockford base32）
deploy/              systemd 单元、安装脚本、发布脚本、nginx 示例
scripts/             ash（curl 封装）、smoke.sh（端到端自检）
```
