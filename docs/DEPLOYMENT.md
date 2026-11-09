# 部署手册（公网）

本文覆盖**前端、后端、数据库、中间件**四层的完整上线流程。
目标读者：拿到这套代码、要在自己的服务器上把它跑起来并对外提供访问的人。

---

## 一、拓扑：三层网段是这份部署的核心

```
              Internet
                 │
        ┌────────▼────────┐
        │  nginx (:80/443)│   edge 网段
        │  TLS / 限流 / CSP│
        └───┬─────────┬───┘
            │         │
     ┌──────▼──┐  ┌───▼──────┐
     │  web    │  │ gateway  │   edge 网段（应用侧）
     │ nginx   │  │  :8080   │
     │  :80    │  │  Go      │
     └─────────┘  └───┬──────┘
                      │
          ┌───────────┴───────────┐
          │      data 网段        │  internal: true
     ┌────▼────┐          ┌───────▼──┐
     │  mysql  │          │  redis   │   ← 均不 publish 端口
     │  :3306  │          │  :6379   │
     └─────────┘          └──────────┘
     ┌─────────┐  ┌──────────┐  ┌───────┐
     │  kafka  │  │ rabbitmq │  │ minio │
     │  :9092  │  │  :5672   │  │ :9000 │   ← 只有 minio 的 S3 口对公网
     └─────────┘  └──────────┘  └───────┘

          observ 网段：prometheus / grafana / node-exporter
```

三个设计决定，都是有代价的取舍：

**1. 数据库完全不暴露端口。**
`data` 网段标了 `internal: true`，容器连外网出口都没有。
代价：本地想用 Navicat 连库得走 `docker compose exec` 或临时加端口映射；
收益：应用被打穿时，攻击者也摸不到 3306 —— 不是"密码难猜"，是**网络层不可达**。

**2. `/metrics` 只挂在 observ 网段。**
gateway 同时接入 `edge`（给 nginx 转发）和 `observ`（给 Prometheus 抓取）。
边缘 nginx 里再加一条 `allow 内网 / deny all` 作为第二道保险。
代价：多一个网段；收益：监控数据不随公网泄露内部路由结构。

**3. Grafana 默认绑 `127.0.0.1:3000`。**
要看得走 SSH 隧道。这是刻意的 —— 监控面板常常是整套系统里鉴权最弱的一环。
代价：看面板麻烦；收益：不会因为一个弱口令把全量业务指标送出去。

**4. Kafka / RabbitMQ / MinIO 同样在 data 网段，但网关不依赖它们启动。**
`depends_on` 只列了 mysql 和 redis —— 消息队列或对象存储没起来时，
网关照常启动，对应能力降级（写接口返回 503、事件先落发件箱等补发）。
反过来加进 `depends_on` 会让「Kafka 挂了就进不了管理台」，
而这恰恰是运维最需要登录看一眼的时候。

MinIO 的 S3 口是唯一对公网的例外：导出文件的预签名链接直接指向它。
桶本身不公开，链接有有效期；控制台 9001 留在 data 网段里走隧道。

---

## 二、前置条件

| 项 | 要求 | 说明 |
|---|---|---|
| Docker | 24+ | 需要 compose v2（`docker compose` 子命令） |
| 磁盘 | ≥ 40G | MySQL 数据 + Prometheus 15 天 + 镜像 |
| 内存 | ≥ 4G | MySQL buffer pool 配了 512M，Prometheus 约 1G |
| 域名 | 已解析到本机 A 记录 | 证书签发需要 |
| 防火墙 | 只开 22 / 80 / 443 | 其余一律关闭 |

---

## 三、首次部署

### 1. 准备环境变量

```bash
cd deploy
cp .env.prod.example .env.prod
chmod 600 .env.prod
```

逐个替换 `CHANGE_ME_*`。三个必填项缺失时 compose 会直接拒绝启动（用了 `${VAR:?}` 语法），
这是故意的 —— 宁可启动失败，也不要带着空密钥上线。

生成随机值：

```bash
openssl rand -base64 48   # 用于 MC_TOKEN_SECRET
openssl rand -base64 24   # 用于各密码
```

### 2. 准备证书

首次签发（先让 80 端口可用，ACME 校验走 HTTP）：

```bash
mkdir -p deploy/certs deploy/certbot-www

# 临时用自签证书让 nginx 能起来（没有证书它起不来）
openssl req -x509 -nodes -days 1 -newkey rsa:2048 \
  -keyout deploy/certs/privkey.pem \
  -out    deploy/certs/fullchain.pem \
  -subj "/CN=placeholder.local"
```

起服务后换正式证书：

```bash
docker run --rm \
  -v "$PWD/deploy/certbot-www:/var/www/certbot" \
  -v "$PWD/deploy/certs:/etc/letsencrypt/live/console" \
  certbot/certbot certonly --webroot -w /var/www/certbot \
  -d console.example.com --email ops@example.com --agree-tos -n

# certbot 写的是 live/<domain>/ 目录，按 nginx 期望的路径软链过来
ln -sf /etc/letsencrypt/live/console/fullchain.pem deploy/certs/fullchain.pem
ln -sf /etc/letsencrypt/live/console/privkey.pem   deploy/certs/privkey.pem
```

续期加一条 cron（每月 1 号）：

```cron
0 3 1 * * cd /opt/loyalty-console && docker compose -f deploy/docker-compose.prod.yml exec nginx nginx -s reload
```

### 3. 构建与启动

```bash
cd deploy
docker compose -f docker-compose.prod.yml --env-file .env.prod up -d --build
```

**首次启动会慢**（3~5 分钟）：MySQL 初始化要跑 `02-schema.sql`（建 15 张表）
和 `03-seed.sql`（约 37 万字符的种子数据）。`mysql-data` 卷已存在时**不会重跑**。

观察启动进度：

```bash
docker compose -f docker-compose.prod.yml ps
docker compose -f docker-compose.prod.yml logs -f mysql
```

### 4. 验证

```bash
# 网关存活（走内网，不经 nginx）
docker compose -f docker-compose.prod.yml exec gateway \
  /usr/local/bin/gateway -healthcheck && echo GATEWAY_OK

# 端到端：登录拿 ticket
curl -s -X POST https://console.example.com/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<你设的密码>"}' | head -c 200

# 前端可访问
curl -sI https://console.example.com/ | head -5
```

浏览器打开域名 → 用 `MC_ADMIN_USER` / `MC_ADMIN_PASS` 登录。

> **注意**：这里的登录是本系统自己的用户名口令，不与任何外部登录服务耦合。
> 生产环境如果要多用户，需要把 `auth.Issuer` 换成一个真正查用户表的实现 ——
> 当前实现是单管理员账号，够展示够练习，不够做真实权限体系。

---

## 四、数据层：初始化、备份、恢复

### 初始化顺序

`/docker-entrypoint-initdb.d/` 里的文件按文件名顺序执行：

1. `01-schema.sql` —— 预留位（库由 `MYSQL_DATABASE` 创建）
2. `02-schema.sql` —— 15 张表
3. `03-seed.sql` —— 种子数据，且**每张表先 `DELETE` 再 `INSERT`**

第 3 条里的 `DELETE ... ; INSERT IGNORE ...` 顺序不是随手写的：
种子数据每次重新生成，`INSERT IGNORE` 撞上同主键会**静默保留旧值**，
表现为"规则改了但库里还是老数据"。全量重建才能保证结果可复现。

### 备份

```bash
docker compose -f docker-compose.prod.yml exec -T mysql \
  mysqldump -uroot -p"$MYSQL_ROOT_PASSWORD" \
  --single-transaction --routines --triggers mc_core \
  | gzip > backup-$(date +%F).sql.gz
```

`--single-transaction` 让 InnoDB 在不锁表的情况下拿到一致快照。
每天跑一次，保留 7 天。

### 恢复

```bash
gunzip -c backup-2026-11-07.sql.gz | \
  docker compose -f docker-compose.prod.yml exec -T mysql \
  mysql -uroot -p"$MYSQL_ROOT_PASSWORD" mc_core
```

---

## 五、发布与回滚

### 发布

```bash
# 1. 打 tag，前端 Sentry 的 release 取的就是 APP_VERSION
export APP_VERSION=1.2.0

# 2. 只重建变化的服务
docker compose -f docker-compose.prod.yml --env-file .env.prod \
  up -d --build gateway web

# 3. 确认健康
docker compose -f docker-compose.prod.yml ps
```

**只重建 gateway 和 web**，不碰 mysql/redis —— 后者重建会带断连。

### 前端镜像必须带上的两个变量

`web` 镜像是在**构建期**把配置打进静态资源的，所以改完必须重建镜像，`docker compose restart` 不生效。

| 变量 | 不传的后果 | 现在怎么传 |
|---|---|---|
| `VITE_SENTRY_DSN` | 线上**零监控**。`monitor.ts` 里 `if (DSN)` 在 DSN 为空时被压缩器判死，整个 Sentry SDK 被 tree-shaking 移除（实测 index chunk 218KB → 69KB）——包更小了，但什么错误都不会报 | 宿主环境变量 `VITE_SENTRY_DSN=...`，compose 已把它转到构建参数 `SENTRY_DSN` |
| `SENTRY_AUTH_TOKEN` | **不生成也不上传 sourcemap**，线上堆栈只有 `index-xxx.js:1:48213` 这种压缩坐标，看不出是哪一行 | 目前 Docker 构建**还没接**，见下 |

**APP_VERSION 在容器里要显式传**：容器内没有 `.git`（node:alpine 也没装 git），
`vite.config.ts` 自动生成 `<version>+<sha>` 的这条路走不通，会退化成纯版本号。
所以发布时务必 `export APP_VERSION=1.2.0`（compose 默认值是 `1.0.0`，不改的话每次发布的
Sentry release 都叫同一个名字，分不清是哪次上线引入的问题）。

**sourcemap 的接法（Dockerfile 现状：未接 token）**

现在 `web/Dockerfile` 只接了 `APP_VERSION` / `SENTRY_DSN` / `SENTRY_ENV`，
`SENTRY_AUTH_TOKEN` 没有任何入口，所以**用 compose 构建出来的镜像，堆栈一定不会还原**。
要还原，二选一：

**A. 宿主机/CI 先构建，再打进镜像**（最简单，推荐）

```bash
cd web
SENTRY_AUTH_TOKEN_FILE=/path/outside/repo/sentryauthtoken.txt \
VITE_SENTRY_DSN=... \
APP_VERSION=1.2.0 \
  pnpm build          # 构建时上传 map 并删除，dist 里不留 .map
```

**B. 用 BuildKit secret 在镜像内传**（需要改 Dockerfile + compose）

```dockerfile
RUN --mount=type=secret,id=sentry_auth_token \
    SENTRY_AUTH_TOKEN_FILE=/run/secrets/sentry_auth_token pnpm build
```

```yaml
build:
  secrets: [sentry_auth_token]
secrets:
  sentry_auth_token:
    file: /path/outside/repo/sentryauthtoken.txt
```

**不要用 `ARG SENTRY_AUTH_TOKEN`**：build arg 会留在镜像层和构建缓存里，等于把带写权限的
私钥写进镜像。secret 只在构建容器内挂载，不进层、不进 `docker history`。

### 回滚

镜像按 tag 回退，不用改代码：

```bash
APP_VERSION=1.1.0 docker compose -f docker-compose.prod.yml up -d --build web gateway
```

数据库变更没有自动回滚。改 schema 前先备份，回滚时按备份恢复。
**这是有意的边界**：自动化的 schema 回滚工具（如 golang-migrate down）在真出事时
往往比手工恢复更容易造成二次损坏，这套项目规模下不值得引入。

---

## 六、安全清单（上线前逐条勾）

- [ ] `deploy/.env.prod` 已 `chmod 600`，且在 `.gitignore` 里
- [ ] `MC_TOKEN_SECRET` 不是示例值、长度 ≥ 32 字节
- [ ] 防火墙只开 22/80/443；docker 没有 `0.0.0.0` 的数据库端口映射
- [ ] 证书已换正式证书，`curl -I https://域名` 返回 HSTS 头
- [ ] `MC_ALLOW_DEV_SECRET` **未设置**（设了会允许空密钥启动）
- [ ] Grafana 改过默认口令，或保持只绑 127.0.0.1
- [ ] `MC_CORS_ORIGINS` 留空（同源）或只填确定的前端域名，不要 `*`
- [ ] MySQL root 口令与业务账号口令不同
- [ ] 备份任务已跑通一次，并且**实际恢复验证过**

最后一条最重要：没验证过的备份等于没有备份。

---

## 七、常见故障

| 现象 | 先查什么 |
|---|---|
| 容器起不来，日志说 `required variable ... is missing` | `.env.prod` 缺变量，或没传 `--env-file` |
| nginx 起来就退出 | `deploy/certs/` 里没有 `fullchain.pem` / `privkey.pem` |
| 页面 502 | gateway 没健康：`docker compose logs gateway`；常见是 MySQL 还没起来 |
| 页面能开但接口全 401 | ticket 过期（`MC_TOKEN_TTL_MIN`）或 `MC_TOKEN_SECRET` 改过 |
| Grafana 面板全空 | Prometheus 抓不到：`curl gateway:8080/metrics`（在网关容器内）；确认 gateway 挂在 observ 网段 |
| 登录一直转圈 | `MC_REDIS_ADDR` / `MC_REDIS_PASSWORD` 不对，缓存层超时 |
| 前端报 CSP 拦截 | 新增了外部域名，要同步改 `nginx/conf.d/site.conf` 的 `connect-src` / `script-src` |

---

## 八、这台机器上不该做的事

- **不要把 `docker.sock` 挂给任何容器**。等于给了容器宿主机 root。
- **不要在 gateway 容器里跑 shell 命令**。distroless 镜像里压根没有 shell ——
  这是有意的，减少被拿下后的可用工具。
- **不要把 Grafana 的 13000 端口直接开在公网**。要开也必须先过 nginx 的 Basic Auth，
  或者干脆用 SSH 隧道。
- **不要在 compose 里给 mysql 加 `ports: "3306:3306"` 图省事**。要用就用 `exec`。

---

## 九、端口分配

本机已有其他项目在用一组端口，本项目**一律避开** 3000 / 3001 / 3002 / 8000 / 8001 / 8088 / 8089。

| 服务 | 宿主机端口 | 容器内端口 | 说明 |
|------|-----------|-----------|------|
| 前端 dev server | 5173 | — | Vite 默认 |
| 网关 | 8080 | 8080 | `MC_ADDR` |
| MySQL | 13306 | 3306 | |
| Redis | 16379 | 6379 | |
| Kafka | 19092 | 29092 | 双 listener，见 compose 注释 |
| RabbitMQ 管理台 | 15672 | 15672 | |
| RabbitMQ AMQP | 15674 | 5672 | |
| MinIO S3 | 19000 | 9000 | |
| MinIO 控制台 | 19001 | 9001 | |
| Prometheus | 19090 | 9090 | |
| Grafana | 13000 | 3000 | |

注意最后一列：**Grafana 的 3000 是容器内端口**，宿主机映射到 13000。
容器内端口不占用宿主机，不受禁用列表约束；宿主机侧一律用上表的端口。

上表是**本地开发**的映射。生产编排里只有 MinIO 的 S3 口（默认 19000）
对宿主机开放，Kafka / RabbitMQ / MinIO 控制台全部只在 data 网段内可达，
需要时用 SSH 隧道。这样公网上能扫到的端口只有 80/443 和对象存储那一个。
