# 配置体系

三套环境，同一份代码。这份文档说明「哪份配置在哪、怎么加载、谁覆盖谁」。

| 环境 | 后端配置 | 前端配置 | 编排文件 | 状态 |
|---|---|---|---|---|
| local | `deploy/env/local.env` | `web/.env.development` | `deploy/docker-compose.yml` | **已跑通** |
| staging | `deploy/env/staging.env` | `web/.env.staging` | `deploy/docker-compose.staging.yml` | 未部署，域名待定 |
| prod | `deploy/env/prod.env` | `web/.env.production` | `deploy/docker-compose.prod.yml` | 未部署，域名待定 |

---

## 一、加载顺序与优先级

### 后端（Go）

```
真实环境变量  >  deploy/env/<MC_ENV>.env  >  代码里的默认值
```

- `MC_ENV` 决定读哪个文件，**默认 `local`**。默认给 local 而不是 prod 是安全考虑：忘记设时最坏情况是连不上生产库，而不是反过来。
- 已经存在的环境变量**不会被文件覆盖**。这是刻意设计——`docker compose` 的 `environment` 段、CI 的 secrets 都靠这一点生效。
- `MC_ENV_FILE` 可以指定任意路径，覆盖上面的推导规则。
- **文件不存在不算错误**。生产容器里通常没有这个文件，此时只用环境变量和默认值。

启动日志第一行会说清楚用的是哪份：

```json
{"level":"INFO","msg":"config_loaded","env":"local","config_file":"..\\deploy\\env\\local.env"}
```

排查「为什么连到了错误的库」时，先看这一行。

### 前端（Vite）

Vite 按 `mode` 加载，优先级从高到低：

```
.env.<mode>.local  >  .env.<mode>  >  .env.local  >  .env
```

- `pnpm dev` → mode = `development` → 读 `.env.development`
- `pnpm build` → mode = `production` → 读 `.env.production`
- `pnpm build --mode staging` → 读 `.env.staging`

`.env.local` 会覆盖任何 mode 的文件，用来放「只属于我这台机器」的东西（比如个人 Sentry DSN）。它已在 `.gitignore` 里。

> **注意**：`VITE_` 前缀的变量会被**打进前端 bundle**，浏览器里能翻到。这层只能放公开信息。

---

## 二、本地开发（唯一跑通的一套）

```bash
# 1. 中间件
docker compose -f deploy/docker-compose.yml up -d

# 2. 后端 —— 不需要任何环境变量前缀，配置自动加载
cd server && go run ./cmd/gateway

# 3. 前端
cd web && pnpm dev
```

访问 http://localhost:5173，用 `admin` / `demo1234` 登录。

### 验证配置正确

```bash
GW=http://127.0.0.1:8080 node scripts/verify-env-config.mjs
```

这个脚本会登录、查五个中间件的连通性（MySQL / Redis / Kafka / RabbitMQ / MinIO），再打 9 个业务接口确认能读到数据。改完配置跑一遍，比逐个手工验证快。

### 临时覆盖某一项

```bash
# 比如临时开 debug 日志，不修改文件
MC_LOG_LEVEL=debug go run ./cmd/gateway
```

### 本地默认端口

| 服务 | 端口 | 说明 |
|---|---|---|
| MySQL | 13306 | 避开本机 3306 |
| Redis | 16379 | 避开本机 6379 |
| Kafka | 19092 | broker 内部用 9092 |
| RabbitMQ | 15674 (AMQP) / 15672 (管理台) | 两个口别弄混 |
| MinIO | 19000 (S3) / 19001 (控制台) | |
| 网关 | 8080 | 只绑 `127.0.0.1` |
| 前端 | 5173 | Vite 只监听 IPv6 回环，必须用 `localhost` |

> 网关绑回环是为了避免 Windows 防火墙反复弹授权框。代价是局域网内其他设备访问不到——手机真机调试时临时改成 `:8080`。

---

## 三、staging / prod 待办清单

这两套目前是**骨架**：结构、变量清单、优先级都已就位，但域名相关项空着，且从未部署验证过。

### 域名确定后要改的地方

| 位置 | 项 | 填什么 |
|---|---|---|
| `deploy/env/staging.env` | `MC_BLOB_PUBLIC_ENDPOINT` | 浏览器能访问到的对象存储地址 |
| `deploy/env/staging.env` | `MC_CORS_ORIGINS` | 前后端分域时的前端域名 |
| `deploy/env/prod.env` | 同上两项 | |
| `web/.env.staging` | `VITE_SENTRY_DSN` | 建议单独建一个 Sentry 项目，别复用生产的；**不填则线上零监控** |
| `web/.env.production` | `VITE_SENTRY_DSN` | 生产 Sentry DSN；也可留空，改在构建时注入 |
| `deploy/docker-compose.staging.yml` | `nginx.ports` | 放开 `443`，补 `certs` 挂载 |

### 必须由部署环境注入、不能进仓库的值

```
MYSQL_ROOT_PASSWORD    MYSQL_PASSWORD      REDIS_PASSWORD
RABBIT_PASSWORD        MINIO_ROOT_USER     MINIO_ROOT_PASSWORD
MC_TOKEN_SECRET        MC_ADMIN_PASS       GRAFANA_PASSWORD
```

生成方式：`openssl rand -base64 48`。

另有一个**只用于构建期**的凭据，同样不能进仓库：

```
SENTRY_AUTH_TOKEN   # sourcemap 上传用，带写权限；不传则构建不生成也不上传 map
```

它只在 `pnpm build` 那几秒被读取（本项目优先用 `SENTRY_AUTH_TOKEN_FILE` 指向仓库外的文件），
运行时完全不需要。容器化构建的传法见 [DEPLOYMENT.md](DEPLOYMENT.md#前端镜像必须带上的两个变量)。

生产编排里这些变量都写了 `:?` 后缀，**没设就启动失败**，不会静默用默认值。

### 三套之间的关键差异

| 项 | local | staging | prod |
|---|---|---|---|
| `MC_ALLOW_DEV_SECRET` | `1`（临时密钥） | `0` | `0` |
| `MC_LOG_LEVEL` | `info` | `debug` | `info` |
| `MC_RATE_RPS` | 30 | 60（放宽，压测用） | 30 |
| `MC_LOGIN_RPS` | 2 | 5 | 2 |
| `MC_BIZ_RPS` | 10 | 20 | 10 |
| Kafka 保留 | 1 小时 | 24 小时 | 72 小时 |
| Prometheus 保留 | 7 天 | 7 天 | 15 天 |
| 端口是否 publish | 全部 | 部分 | 只有 80/443 |

`MC_ALLOW_DEV_SECRET=0` 意味着**未设 `MC_TOKEN_SECRET` 时启动直接失败**。这比「静悄悄用随机密钥、重启后所有人掉线」安全得多。

---

## 四、日志配置

### 三个环境都是同一个策略：写 stdout，由运行时轮转

| 运行方式 | 轮转方式 | 上限 |
|---|---|---|
| 本地 `go run` | 重定向到文件（自己管） | 无 |
| 本地容器 | `docker-compose.yml` 的 `x-logging` | 10MB × 3 |
| staging / prod | 各自的 `x-logging` | 20MB × 3 或 × 5 |

### 读日志

```bash
# 生产：跟随
docker logs -f loyalty-console-gateway-1

# 按 trace_id 串起一次请求（最有价值）
docker logs loyalty-console-gateway-1 2>&1 | grep "<trace_id>"
```

日志是纯 JSON，`level` / `route` / `trace_id` / `status` 都是独立字段，可以直接被 Loki、jq 之类解析，不需要写正则。

### nginx 日志必须是 stdout

官方镜像里 `access.log` 是指向 `/dev/stdout` 的符号链接。**挂一个具名卷到 `/var/log/nginx` 会覆盖它**，后果有两个：

1. `docker logs nginx` 看不到访问日志
2. 那个卷不受 `logging:` 轮转管辖，会无限增长把盘写满

所以编排里不挂这个卷，配置里也显式写 `/dev/stdout`。

---

## 五、环境变量总表

四类变量，生效位置完全不同。**「谁覆盖谁」见第一节**，这里只列全量和默认值。

判断某个变量属于哪一类，看一个问题：**它是在哪里被读的？**

| 类别 | 读取时机 | 改完要做什么 |
|---|---|---|
| 前端 `VITE_*` | 构建/启动时，值会被打进 bundle | 重建；dev server 重启 |
| 前端构建期 `SENTRY_*` | 只在 `pnpm build` 那几秒 | 重新构建 |
| 后端 `MC_*` | 进程启动时 | 重启进程/容器 |
| 编排层 | `docker compose` 解析 yml 时 | 重新 `up -d`（改 build args 要 `--build`） |

### 5.1 前端：会被打进 bundle（浏览器里能翻到）

写在 `web/.env.{development,staging,production}` 或 `web/.env.local`。

| 变量 | 默认 | 作用 | 注意 |
|---|---|---|---|
| `VITE_SENTRY_DSN` | 空 | Sentry 上报地址 | **空 = 完全不上报**，且整个 SDK 被 tree-shaking 移除 |
| `VITE_SENTRY_ENV` | 未设时取 `MODE` | Sentry 的 environment 标签 | 建议显式写 `development` / `staging` / `production` |
| `VITE_TRACES_SAMPLE_RATE` | `1` | 性能追踪采样率 | 1 = 全采；上高流量后下调 |
| `VITE_PROFILE_SAMPLE_RATE` | `1` | Profile 采样率 | 与 trace 采样**相乘**：0.1 × 0.1 = 1% |
| `VITE_API_ORIGIN` | `http://127.0.0.1:8080` | dev server 的 `/api` 代理目标 | 只在 `pnpm dev` 生效；生产同源部署不需要 |
| `VITE_APP_VERSION` | 自动 | Sentry 的 release 名 | **`.env` 里写的不再生效**，由 `vite.config.ts` 计算，见 [MONITORING.md](MONITORING.md) |

### 5.2 前端构建期：不进 bundle

只存在构建进程的环境里，`vite.config.ts` 读取。**不传就静默跳过**，构建照常成功。

| 变量 | 默认 | 作用 |
|---|---|---|
| `SENTRY_AUTH_TOKEN` | 空 | 上传 sourcemap 用的 token（带写权限） |
| `SENTRY_AUTH_TOKEN_FILE` | 空 | 指向仓库外 token 文件的路径（与上一条二选一，推荐这个） |
| `SENTRY_ORG` | `ebo-y9` | 上传目标组织 |
| `SENTRY_PROJECT` | `javascript-react` | 上传目标项目 |

没 token 时 `vite.config.ts` 连 `.map` 都不生成——避免源码随产物泄露。

### 5.3 Docker 构建参数（`web/Dockerfile`）

| 构建参数 | 转成的运行时变量 | 默认 |
|---|---|---|
| `APP_VERSION` | `VITE_APP_VERSION` | `dev` |
| `SENTRY_DSN` | `VITE_SENTRY_DSN` | 空 |
| `SENTRY_ENV` | `VITE_SENTRY_ENV` | `production` |

容器内没有 `.git`，自动生成的 `<version>+<sha>` 走不通，所以**发布时必须显式传 `APP_VERSION`**。

### 5.4 后端网关（Go，`MC_` 前缀）

代码：`server/cmd/gateway/main.go`。默认值取不到时用下表的值。

| 变量 | 默认 | 作用 |
|---|---|---|
| `MC_ENV` | `local` | 决定读 `deploy/env/<env>.env` |
| `MC_ENV_FILE` | 空 | 直接指定 env 文件路径，覆盖 `MC_ENV` 的推导 |
| `MC_ADDR` | `:8080` | 网关监听地址 |
| `MC_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `MC_MYSQL_DSN` | `root:root123@tcp(127.0.0.1:13306)/mc_core?...` | MySQL 连接串 |
| `MC_MYSQL_MAX_OPEN` | `32` | 最大连接数 |
| `MC_MYSQL_MAX_IDLE` | `8` | 最大空闲连接 |
| `MC_MYSQL_CONN_LIFE_MIN` | `30` | 连接最长存活（分钟） |
| `MC_REDIS_ADDR` | `127.0.0.1:16379` | Redis 地址 |
| `MC_REDIS_PASSWORD` | 空 | Redis 密码 |
| `MC_REDIS_DB` | `0` | Redis 库号 |
| `MC_BLOB_ENDPOINT` | `127.0.0.1:19000` | 对象存储 S3 接口（服务端用） |
| `MC_BLOB_PUBLIC_ENDPOINT` | `127.0.0.1:19000` | 给浏览器的地址，域名确定后要改 |
| `MC_BLOB_USE_SSL` | `0` | `1` 走 https |
| `MC_BLOB_ACCESS_KEY` | `mcadmin` | |
| `MC_BLOB_SECRET_KEY` | `mcadmin123` | |
| `MC_BLOB_EXPORT_BUCKET` | `console-export` | 导出文件桶 |
| `MC_BLOB_TMP_BUCKET` | `console-tmp` | 临时文件桶 |
| `MC_KAFKA_BROKERS` | `127.0.0.1:19092` | 逗号分隔 |
| `MC_KAFKA_TOPIC` | `console.events` | 审计事件 topic |
| `MC_KAFKA_GROUP` | `console-audit` | 消费组 |
| `MC_RABBIT_URL` | `amqp://mc:mc123@127.0.0.1:15674/%2Fconsole` | |
| `MC_TOKEN_SECRET` | 空 | **必须设**；未设且 `MC_ALLOW_DEV_SECRET≠1` 时启动失败 |
| `MC_ALLOW_DEV_SECRET` | `0` | `1` 允许用临时随机密钥（仅本地） |
| `MC_TOKEN_TTL_MIN` | `720` | 登录 token 有效期（分钟） |
| `MC_ADMIN_USER` | `admin` | 初始管理员 |
| `MC_ADMIN_PASS` | 空 | 初始管理员密码 |
| `MC_RATE_RPS` | `30` | 全局限流（每秒） |
| `MC_RATE_BURST` | `60` | 全局突发额度 |
| `MC_LOGIN_RPS` | `2` | 登录接口限流（突发固定 5） |
| `MC_TELEMETRY_RPS` | `5` | 前端监控上报限流（突发固定 20） |
| `MC_BIZ_RPS` | `10` | 业务写接口限流 |
| `MC_BIZ_BURST` | `20` | 业务突发额度 |
| `MC_REQUEST_TIMEOUT_SEC` | `6` | 单请求超时 |
| `MC_CORS_ORIGINS` | `http://localhost:5173,http://127.0.0.1:5173` | 逗号分隔 |

`MC_MYSQL_CONN_LIFE_MIN` 和 `MC_REQUEST_TIMEOUT_SEC` 三份 env 文件里都没写，走代码默认值。

### 5.5 编排层（`docker compose` 插值）

这些由 compose 从 `--env-file`（prod/staging）或宿主环境读取，再传进容器。

| 变量 | 默认 | 用途 |
|---|---|---|
| `MYSQL_ROOT_PASSWORD` | 无（生产 `:?` 必填） | MySQL root |
| `MYSQL_PASSWORD` | 无（生产 `:?` 必填） | 业务库账号密码 |
| `REDIS_PASSWORD` | 无（生产 `:?` 必填） | Redis |
| `RABBIT_PASSWORD` | 无（生产 `:?` 必填） | RabbitMQ |
| `MINIO_ROOT_USER` | 无（生产 `:?` 必填） | MinIO |
| `MINIO_ROOT_PASSWORD` | 无（生产 `:?` 必填） | MinIO |
| `MINIO_BUCKET` | `console-export` | MinIO 桶名 |
| `MINIO_API_PORT` | `19000` | 宿主端口 |
| `MINIO_PUBLIC_HOST` | 无 | 浏览器访问对象存储的域名 |
| `GRAFANA_PASSWORD` | 无（生产 `:?` 必填） | Grafana |
| `GRAFANA_ROOT_URL` | `http://localhost:13000` | Grafana 外部地址 |
| `MC_ADMIN_USER` | `admin` | 见 5.4 |
| `MC_ADMIN_PASS` | 空 | 见 5.4 |
| `MC_TOKEN_SECRET` | 无（生产 `:?` 必填） | 见 5.4 |
| `MC_CORS_ORIGINS` | 空 | 见 5.4 |
| `MC_LOG_LEVEL` | 按环境 | 见 5.4 |
| `APP_VERSION` | `1.0.0` / `staging` | 前端 release，见 5.3 |
| `VITE_SENTRY_DSN` | 空 | 前端监控，见 5.3 |

密码类用 `openssl rand -base64 48` 生成；生产编排里写了 `:?` 的，**没设就启动失败**。

---

## 六、常见错误

**「配置改了没生效」**
先看启动日志的 `config_loaded` 那一行，确认读的是哪个文件。最常见的原因是存在一个优先级更高的真实环境变量——文件不会覆盖它。

**「改了 .env 但程序读到的还是旧值」**
检查是不是该变量的值在 `.env` 里为空。`env()` 的语义是「空串等于没设」，所以 `KEY=` 会退回默认值。想显式设空请用 `KEY=""`（加载器会识别成空字符串，同样会退回默认……真要设空得改代码）。这条规则对 `MC_CORS_ORIGINS=` 这种「留空表示不启用」的项是有意的。

**「前端改了 .env 不生效」**
Vite 的 `.env` 只在**启动时**读一次，改完要重启 dev server。另外确认变量有 `VITE_` 前缀，否则不会被注入。

**「staging 起不来，报必须设置 XXX」**
编排里的 `:?` 后缀是保护。按第三节的清单补齐所有标「必须由部署环境注入」的变量。
