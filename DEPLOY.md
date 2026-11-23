# 部署说明（aws-quickwork-2api / quickwork-openai）

本仓库是**单个 Go 可执行文件**：`login` 拿 Amazon Quick 的 OAuth token 存 SQLite，`server` 把
`/v1/chat/completions` 翻译成上游 WebSocket 帧。没有 Python、没有官方桌面端、没有 outpost 进程。

```
浏览器 ──OAuth(PKCE)──> quickproxy login ──> data/quickwork.db(SQLite)
OpenAI SDK ──HTTP──> quickproxy server ──WSS──> Amazon Quick ──> 文本/工具调用
```

## 0. 已验证结论（本机 Windows）

| 检查项 | 结果 |
|---|---|
| `go build ./cmd/quickproxy` | 通过，产出 `bin/quickproxy.exe`（约 17.4 MB，CGO 关闭） |
| `quickproxy server` | 正常监听 `127.0.0.1:8080`，自动建 `data/quickwork.db` |
| `GET /healthz` | `{"service":"quickwork-openai","status":"ok"}` |
| `GET /v1/models` | 返回 `smart/fast/balanced/auto` |
| `POST /v1/chat/completions`（未登录） | `no accounts configured; run quickproxy login` |
| `quickproxy login` | 正常打印 discovery URL，等待浏览器回调 |
| `GET /admin/`（设了密码） | 200，登录页；未登录访问 `/admin/api/*` 返回 401 |
| `POST /admin/api/auth/login` | 正确密码 200 + `HttpOnly` cookie；错密码 401，连续 10 次后 429 |
| `/admin` 写操作 | 缺 `X-CSRF-Token` 返回 403；改密码后旧会话立即失效 |
| 未设密码时 `/admin` | 全部路径 503 `admin_disabled`，`/v1` 不受影响 |
| `PUT /admin/api/config` | 落盘原子写入，返回 `restart_required`；`webui` 段即时生效 |

硬性前提：**Go 1.26+**（go.mod 写死 `go 1.26.3`），且仓库目前**没有 GitHub Release**
（`.github/workflows/release.yml` 只在打 `v*` tag 时才产出四个平台的二进制），所以只能自己编译。

## 1. 编译

```powershell
# Windows
cd E:\dsh\aws-quickwork-2api
go build -o bin\quickproxy.exe ./cmd/quickproxy
```

```bash
# Linux / macOS
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/quickproxy ./cmd/quickproxy
```

`modernc.org/sqlite` 是纯 Go 实现，交叉编译不需要 C 工具链：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/quickproxy-linux-amd64 ./cmd/quickproxy
```

## 2. 登录上游账号（唯一的人工步骤）

```powershell
.\bin\quickproxy.exe login --provider social
```

- 终端打印 `https://quick.aws.com/sn?qwSurface=desktop:<nonce>:18080:1.2.4660`
- 在**同一台机器**的浏览器打开，登录你的 Amazon Quick 账号
- 命令行在本机 `18080` 收回调，完成 PKCE、换 token、写 SQLite
- 成功输出：`account 1 ready: <email> (social, <ws_host>)`

多账号：重复执行 `login`。同 email + tenant 会更新而不是新增，`server` 按 ID 轮询。

企业 SSO：`.\bin\quickproxy.exe login -provider enterprise -region us-east-1`

### 服务器上登录（回调端口不可达）

```bash
./bin/quickproxy login -manual
```

1. 复制终端打印的 discovery URL，在任意浏览器打开登录；
2. 浏览器跳到 `http://localhost:18080/callback?...` —— 打不开是正常的，把**完整 URL** 粘回终端；
3. 终端再打印 authorize URL，同样在浏览器打开并登录；
4. 浏览器跳到 `http://127.0.0.1:18080/?code=...&state=...`，再粘回终端；
5. 工具自动判断阶段并入库，全程不需要服务器开放任何入站端口。

也支持管道：`echo "<callback-url>" | ./bin/quickproxy login -manual`

## 3. 启动反代

```powershell
.\bin\quickproxy.exe server        # 等价于 .\bin\quickproxy.exe（无子命令默认 server）
```

```bash
./bin/quickproxy server -config /etc/quickproxy/config.json
```

启动日志会打印 `config/db/accounts/auth` 与模型、超时、会话策略。

```bash
curl http://127.0.0.1:8080/v1/models -H "Authorization: Bearer local"

curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer local" -H "Content-Type: application/json" \
  -d '{"model":"smart","stream":true,"messages":[{"role":"user","content":"用三句话解释 QUIC"}]}'
```

## 4. 管理前端 `/admin`

管理前端和 API 共用端口和进程，静态资源已编译进二进制，不需要额外的目录、端口或 Node 构建。

```powershell
# 设置管理密码（写 webui.password_hash，清空 webui.password）
.\bin\quickproxy.exe passwd -password '换成你的长密码'

# 启动后日志会打印入口
.\bin\quickproxy.exe server
# management interface on: http://127.0.0.1:8080/admin/
```

不设密码也能启动，但 `/admin` 会统一返回 503 并提示怎么设置；`/v1` 完全不受影响。

`config.json` 对应字段：

```json
"webui": {
  "enabled": true,
  "password": "",
  "password_hash": "pbkdf2-sha256$...",
  "session_ttl": "12h",
  "max_attempts": 10,
  "trust_proxy": false
}
```

- `webui.password` 是明文兜底；`webui.password_hash` 优先，`passwd` 只写哈希。
- `max_attempts` 是同一 IP 每 5 分钟的失败次数上限。
- `trust_proxy` 只在反代后面且需要按真实 IP 限速时打开；打开前确认代理会覆盖 `X-Forwarded-For`。
- 改密码会立刻让所有管理会话失效，需要在 `/admin` 重新登录。

页面里能做的事：账号列表 / 刷新 / 删除、网页内完成 OAuth（手动回填 callback URL 或本机 18080 回调）、编辑 `config.json`（`webui` 即时生效，其余字段标注需重启）、最近 500 条请求日志、Playground（用管理会话鉴权，不需要 API key）。

反向代理只需要转发同一个上游，不用为 `/admin` 单独建 vhost：

```
quick.example.com {
    reverse_proxy 127.0.0.1:8080
    flush_interval -1
}
```

跨站保护已内置：会话 cookie 是 `SameSite=Strict` + `Path=/admin`，写操作校验 CSRF 与 `Origin`，管理端点不返回 CORS 头。

## 5. 对外开放（可选，务必先做鉴权）

默认 `addr` 是 `127.0.0.1:8080`，**项目明确不建议直接暴露公网**。要远程用，两步：

1. `config.json` 写 key：

```json
{ "api_keys": ["sk-换成你的随机串"] }
```

2. 只让反代监听内网/本机，由 Caddy 或 Nginx 做 TLS：

```json
{ "addr": "127.0.0.1:8080" }
```

Caddyfile：

```
quick.example.com {
    reverse_proxy 127.0.0.1:8080
    # SSE 必须关缓冲
    flush_interval -1
}
```

Nginx：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_buffering off;        # 流式响应必需
    proxy_read_timeout 600s;    # 上游 request_timeout 默认 5m
    proxy_set_header Authorization $http_authorization;
}
```

客户端配置：

```python
from openai import OpenAI
client = OpenAI(base_url="https://quick.example.com/v1", api_key="sk-换成你的随机串")
```

## 6. 常驻运行

### systemd（Linux）

```ini
# /etc/systemd/system/quickproxy.service
[Unit]
Description=quickwork-openai proxy
After=network-online.target

[Service]
Type=simple
User=quickproxy
WorkingDirectory=/opt/quickproxy
ExecStart=/opt/quickproxy/bin/quickproxy server -config /opt/quickproxy/config.json
Restart=always
RestartSec=3
# token 与 client secret 在 SQLite 里
UMask=0077

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now quickproxy
journalctl -u quickproxy -f
```

`-manual` 登录要在同一 `WorkingDirectory` 下执行，否则会写到另一个库。

### Docker Compose（推荐）

仓库自带完整的容器化文件：

| 文件 | 作用 |
|---|---|
| `Dockerfile` | 多阶段构建：`golang:1.26-alpine` 编译，`alpine` 运行，非 root（uid 10001） |
| `docker/entrypoint.sh` | 启动前把环境变量渲染进 `config.json`，再 exec 代理 |
| `docker-compose.yml` | 默认拉取 GHCR 预构建镜像，挂载 `./data`，带 healthcheck 与日志轮转 |
| `docker-compose.build.yml` | 本地改代码时叠加使用，改为就地构建 |
| `.env.example` | 端口、管理密码、API key、OAuth 默认值模板 |
| `.github/workflows/docker.yml` | push main / 打 tag 时构建 amd64+arm64 并推 GHCR，另带 amd64 冒烟测试 |

```bash
cp .env.example .env          # 至少改 WEBUI_PASSWORD、API_KEYS
docker compose pull
docker compose up -d
docker compose logs -f
```

然后打开 `http://127.0.0.1:<WEB_PORT>/admin/`，密码是 `.env` 里的 `WEBUI_PASSWORD`。

本地构建（不打 GHCR，直接用当前代码）：

```bash
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
```

数据只有一处：`./data` 目录里的 `config.json` + `quickwork.db`。Linux 上它由容器内 uid 10001 写入：

```bash
sudo chown -R 10001:10001 ./data
```

#### 环境变量如何生效

`docker/entrypoint.sh` 每次启动先跑 `quickproxy config -from-env`，把环境变量合并进 `/app/data/config.json`，再启动 `server`。改完 `.env` 执行 `docker compose up -d`（会重建容器）即生效，不用手改 JSON。

| 变量 | 对应配置 | 说明 |
|---|---|---|
| `WEB_PORT` | compose 端口映射 | 宿主机端口，容器内固定 8080 |
| `ADDR` | `addr` | 容器内监听地址，默认 `0.0.0.0:8080` |
| `API_KEYS` | `api_keys` | 逗号分隔；留空则 `/v1` 不鉴权 |
| `MODELS` / `DEFAULT_MODEL` | 同名 | 逗号分隔的模型别名 |
| `REQUEST_TIMEOUT` / `MAX_ATTEMPTS` | 同名 | 上游超时与换号次数 |
| `TIMEZONE` / `LOCALE` | 同名 | 透传给 `user_message` |
| `THINKING_EFFORT` | 同名 | 空 / `off` / `low` / `medium` / `high` |
| `SESSION_REUSE` / `SESSION_TTL` / `KEEP_SESSIONS` | 同名 | 上游会话策略 |
| `OAUTH_PROVIDER` / `OAUTH_STAGE` / `OAUTH_REGION` / `OAUTH_CALLBACK_PORT` / `OAUTH_TIMEOUT` | 同名 | 网页内登录的预填值 |
| `WEBUI_ENABLED` / `WEBUI_PASSWORD` / `WEBUI_PASSWORD_HASH` / `WEBUI_SESSION_TTL` / `WEBUI_MAX_ATTEMPTS` / `WEBUI_TRUST_PROXY` | `webui.*` | 管理界面；`_HASH` 优先于 `_PASSWORD` |

优先级是「环境变量 > 文件里已有的值」，空变量不覆盖。想完全不接管配置文件，设 `CONFIG_FROM_ENV=false`。

注意密码的归属：`.env` 里的 `WEBUI_PASSWORD` 每次重建都会写回 `config.json`，所以在 `/admin` 里改的密码会被它覆盖。要以页面修改为准，就把 `.env` 的 `WEBUI_PASSWORD` 留空，或用 `WEBUI_PASSWORD_HASH`，或直接 `CONFIG_FROM_ENV=false`。

#### 容器内登录上游

宿主机浏览器无法回调容器里的 `127.0.0.1:18080`，「本机回调」模式在 Docker 里不可用（页面会提示）。两种做法：

```bash
# A. 网页：/admin → 登录上游 → 选「手动回填 callback URL」
# B. 命令行：走 entrypoint，自动带 -config
docker compose run --rm -it quickproxy login -manual
```

登录用的 discovery/authorize URL 都是上游域名，在任意浏览器打开即可，最后把地址栏里的完整 callback URL 粘回终端或网页。

## 7. 网络与端口

出站 443/TCP 必须通：

| 域名 | 用途 |
|---|---|
| `quick.aws.com` | social 登录 discovery（prod） |
| `oidc.<idc-region>.amazonaws.com` | OIDC client 注册 / token / refresh |
| 登录时打印的 `ws_host`（`*.appintegrations.*.plato.ai.aws.dev` 等） | 聊天 WebSocket |

入站：`addr`（默认 8080）+ 登录时的 `18080`（仅本机、仅登录期）。企业 OIDC 需要访问企业 IdP。

## 8. 关键配置项（部署角度）

| 字段 | 部署含义 |
|---|---|
| `addr` | 只监听本机就保持 `127.0.0.1:8080`，前面挂 TLS |
| `api_keys` | 非空才开鉴权；公网场景必须设 |
| `database` | 相对路径基于 config.json 所在目录；内含 access/refresh token，`chmod 600` |
| `request_timeout` | 默认 `5m`，反向代理的 read timeout 要大于它 |
| `refresh_before` | 默认 `10m`，到期前自动 refresh |
| `max_attempts` | 默认 3，失败换号重试（流式已开始则不换） |
| `session_reuse` | 默认 true，复用上游 thread 只发新增消息，避开 64 KB 单帧上限 |
| `chat_agent_id` | 指向自定义 Quick agent，用来甩掉内置 60K token 提示词 |
| `thinking_effort` | `off/low/medium/high`，空=上游默认 |
| `timezone` / `locale` | 透传给 `user_message`，默认 `UTC` / `en-US`，国内可改 `Asia/Shanghai` / `zh-CN` |
| `webui.enabled` | 默认 true；设为 false 后 `/admin` 返回 503，`/v1` 不受影响 |
| `webui.password` / `webui.password_hash` | 管理密码；哈希优先，用 `quickproxy passwd` 写入 |
| `webui.session_ttl` | 管理会话有效期，默认 `12h`，重启进程同时清空所有会话 |
| `webui.max_attempts` | 同一 IP 每 5 分钟失败上限，默认 10 |
| `webui.trust_proxy` | 在可信反代后面才开，用于按真实 IP 限速 |

## 9. 运维命令

```bash
./bin/quickproxy accounts              # ID/邮箱/provider/到期/使用次数/状态/last_error
./bin/quickproxy refresh               # 刷新全部 token
./bin/quickproxy refresh -account 1
./bin/quickproxy logout -account 1
./bin/quickproxy logout -all
./bin/quickproxy passwd -password '新密码'   # 写 webui.password_hash，清空明文密码
```

## 10. 常见坑

- **`go 1.26.3` 编译不过**：升级工具链，或临时降 `go.mod` 的 go 行（上游代码没有用新语法，一般可编）。
- **没有 Release 二进制**：只能自编译或自己打 tag 触发 workflow。
- **登录一直等待**：浏览器与命令不在同一台机器，回调端口打不开 → 用 `-manual`。
- **`cannot listen on 127.0.0.1:18080`**：端口被占，`-port` 换一个。
- **`no accounts configured` / `upstream_session_unavailable`**：先 `login`。
- **`401 invalid_api_key`**：`api_keys` 与请求头不一致（`Authorization: Bearer` 或 `X-Api-Key`）。
- **流式像一次性吐出**：反代开了 buffering，Nginx 要 `proxy_buffering off`，Caddy 要 `flush_interval -1`。
- **单轮超过 64 KB**：优先转成上游附件；附件不可用时裁剪中间内容，日志有 `user_message trimmed`。
- **模型自称 AWS Quick / 不听话**：内置 agent 提示词在服务端，换 `chat_agent_id` 才能真正替换。
- **`/admin` 503 `admin_disabled`**：没设管理密码，`quickproxy passwd -password '...'` 或写 `webui.password`，然后重启。
- **`/admin` 登录 429**：5 分钟内失败超过 `webui.max_attempts`；等窗口过期或调大该值。挂反代时所有请求都算代理 IP，必要时开 `webui.trust_proxy`。
- **配置页保存报 `permission denied`**：运行用户对 `config.json` 没有写权限（Docker 常见，见上面的 `chown 10001`）。
- **改了配置但行为没变**：只有 `webui` 段即时生效，其它字段要重启进程。
- **备份**：只需备份 `config.json` + `data/quickwork.db`，且两者都含密钥，别进 git（`.gitignore` 已排除）。
