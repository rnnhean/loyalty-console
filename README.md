# 会员运营台（Loyalty Console）

一个可完整跑起来、可公网部署的**会员运营后台**：Vite + React + Ant Design 前端，Go 网关 + MySQL/Redis/Kafka/RabbitMQ/MinIO 后端，带 Prometheus/Grafana/Sentry 三条监控信号。

22 个业务页面全部接真实数据，中间件每一个都在链路上承担实际职责，而不是为了凑技术栈摆在那里。

## 目录

| | | |
|---|---|---|
| [一、这个项目解决什么问题](#一这个项目解决什么问题) | [七、网关是什么](#七网关是什么) | [十三、连数据库与 Redis](#十三连数据库与-redis) |
| [二、业务介绍](#二业务介绍) | [八、页面地址与 GUI 入口](#八页面地址与-gui-入口) | [十四、配置与环境](#十四配置与环境) |
| [三、业务流程](#三业务流程四条完整链路) | [九、事务性发件箱](#九事务性发件箱outbox是什么) | [十五、目录结构](#十五目录结构) |
| [四、技术栈](#四技术栈) | [十、数据库和中间件怎么看变化](#十数据库和中间件怎么看变化) | [十六、测试](#十六测试) |
| [五、本地启动](#五本地启动) | [十一、日志怎么看](#十一日志怎么看) | [十七、更多文档](#十七更多文档) |
| [六、Docker 怎么用](#六docker-怎么用) | [十二、Nginx](#十二nginx) | [十八、安全须知](#十八安全须知) |

**专项文档**：

- [docs/MONITORING.md](docs/MONITORING.md) —— 监控怎么配、怎么看、怎么排查（Prometheus / Grafana / Sentry / beacon）
- [docs/DATA-MODEL.md](docs/DATA-MODEL.md) —— 25 张表的设计
- [docs/CONFIGURATION.md](docs/CONFIGURATION.md) —— 三套环境的配置体系
- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) —— 公网部署

---

## 一、这个项目解决什么问题

会员运营后台的典型形态是「一堆列表页 + 一堆表单」：商品目录、权益规则、交易审核、渠道归因、运营活动、系统配置。看起来简单，真正做起来会反复撞上同一批问题：

| 问题 | 具体表现 | 这里的做法 |
|---|---|---|
| 页面骨架重复 | 「筛选 + 表格 + 分页」复制二十多遍，改一处要改二十遍 | 页面 = 一份列定义，骨架只实现一次，见 [`console.ts`](web/src/config/console.ts) |
| 首屏请求瀑布 | 一个页面串行发 3~10 个请求，首屏要等四五轮 RTT | 服务端 **BFF 聚合**，前端只发 1 个请求，网关并发扇出后合并返回 |
| 中间件只是摆设 | Redis 只当缓存、MQ 只当异步、对象存储只当网盘 | 每个中间件都对应一个**具体的业务正确性问题**（见下） |
| 出事了不知道 | 靠用户投诉才发现故障 | 指标、日志、链路追踪三条信号，各答一个不同的问题 |

### 中间件各自解决什么

选型不是为了让架构图好看，每个都对应一个真实故障模式：

| 中间件 | 在链路上的职责 | 不引入会怎样 |
|---|---|---|
| **Redis · 缓存** | 产品目录、定价策略等低频变更数据的二级缓存 | 每次列表查询都打 DB，连接池被读请求占满 |
| **Redis · 分布式锁 + 幂等** | 同一条规则的并发处置只执行一次，重复请求返回同一份结果 | 并发处置同一条规则会重复发放/重复扣减 |
| **Kafka + 发件箱** | 交易事件流 → 异步写审计留痕，配 Outbox 保证「业务落库成功但事件丢失」不会发生 | 审计与业务双写不一致；或用同步双写拖慢主流程 |
| **RabbitMQ · 延迟队列** | 权益到期回收：TTL + DLX，到期自动触发回收，不靠轮询扫表 | 定时扫描全表，数据量大了扫不动且有时延 |
| **MinIO（S3 协议）** | 导出文件存储，预签名 URL 直连下载 | 大文件经由网关中转，占满应用内存与带宽 |
| **Prometheus + Grafana** | 后端 RED/USE 指标、缓存命中、消息丢失、锁租约 | 出事了只能靠用户投诉才知道 |
| **Sentry + Web Vitals** | 前端 JS 异常、性能指标，双通道上报 | 白屏只能等用户反馈 |

---

## 二、业务介绍

一个**会员运营中台**，围绕「卖会员」这件事组织，分六个业务域、22 个页面：

```
商品域 cat_   卖什么       商品档案 → 单品 → 售卖卡 → 定价策略 → 策略卡绑定
权益域 bft_   买完得到什么  会员层级 → 权益包 → 权益规则 → 任务门槛
交易域 trd_   怎么付款      结算页配置 → 代扣模板 → 立减规则 → 操作留痕
渠道域 chn_   从哪来        商户档案 → 分销节点 → 归因记录
运营域 ops_   怎么推        投放计划 → 券批次 → 推广位
配置域 cfg_   全局开关      产品目录 → 字典项 → 集成模块 → 特性开关
```

### 一句话讲清业务链

```
渠道把用户带来  →  用户在结算页选一个售卖卡  →  付款走代扣模板
     ↑                                              ↓
  归因记录（算谁的业绩）                    权益包按规则发放，挂到会员层级
                                                    ↓
                                        到期后经 RabbitMQ 延迟队列自动回收
                                                    ↓
                                      全程写审计事件，经 Kafka 异步落库
```

### 一个真实约束：产品是分区键

**绝大多数页面不选产品就没有数据**——这不是 bug，是刻意的。产品是数据的分区键，未传 `product` 参数时接口返回空集而不是全量，避免一次误调用把整库扫出来。各页面的产品下拉框也不是共用一份，而是取**该表真实出现过的产品值**，否则必然出现「选了产品却查不到数据」。

---

## 三、业务流程（四条完整链路）

### 1. 页面首屏：BFF 聚合

```
浏览器 ──1 个请求──> 网关 /api/bff/{域}/overview
                        ├─ errgroup 并发扇出（SetLimit=6）
                        ├─ 各领域服务独立查库
                        └─ 合并成一个响应返回
```

**部分失败不整体失败**：某个区块查不到时，其余照常返回，前端按 block 降级显示。HTTP 状态是 200，但会打 `mc_aggregate_degraded_total` —— 只看错误率发现不了「页面成功了但少了一块」，必须单独盯这个计数器。

并发度取 6 而非不限：MySQL 连接池按 `MaxOpenConns=32` 设计，若每请求扇出 20，16 个并发用户就能占满池子。

### 2. 权益到期回收：延迟队列而非轮询

```
写库同时发一条延迟消息（TTL = 到期时间）
        ↓
到期后进入死信交换机 → 消费 → 回收权益 → 写审计
```

不用定时扫表的原因：扫表有最大时延（取决于扫描周期），且数据量增长后单次扫描成本不可控。延迟队列是**准时**的。进死信则意味着「到期了但没处理成功」，权益该失效却没失效，是资损风险，所以单独配了 `RabbitDeadLetter` 告警。

### 3. 交易审计：发件箱保证不丢

```
业务事务内 ──写业务表
           └─写 outbox（同一个事务）      ← 保证原子性
                    ↓
            后台协程扫描 outbox → 投递 Kafka → 标记已发
```

不能直接双写「业务库 + Kafka」：Kafka 投递失败或进程崩溃时，业务已经提交、事件却永远丢了。Outbox 把「投递」变成可重试的状态机，重试耗尽进 `dropped` 状态并告警。

### 4. 并发处置：锁 + 幂等

同一条规则被两个请求同时处置时，靠 `SET NX` 租约锁串行化，处理完把结果按 `trace_id` 缓存，重复请求直接返回同一份结果（`outcome=replay`）。租约超时会打 `mc_lock_lost_total`——那说明持锁期间业务跑过了租约时间。

---

## 四、技术栈

**前端**
| 项 | 选型 | 版本 |
|---|---|---|
| 构建 | Vite | 5.4 |
| 框架 | React | 18.3 |
| 组件库 | Ant Design | 5.20 |
| 路由 | react-router-dom | 6.26 |
| 样式 | Less | 4.2 |
| 语言 | TypeScript | 5.5 |
| 测试 | Playwright | 1.65 |
| 错误监控 | @sentry/react + Web Vitals | 10.78 / 4.2 |

**后端**
| 项 | 选型 |
|---|---|
| 语言 | Go 1.26 |
| HTTP | Gin 1.10 |
| MySQL 驱动 | go-sql-driver/mysql 1.8 |
| Redis | redis/go-redis v9 |
| Kafka 客户端 | segmentio/kafka-go |
| RabbitMQ | rabbitmq/amqp091-go |
| 对象存储 | minio-go/v7（S3 兼容） |
| 指标 | prometheus/client_golang |
| 并发编排 | golang.org/x/sync（errgroup） |
| 限流 | golang.org/x/time（令牌桶） |

**基础设施**
MySQL 8.0 · Redis 7 · Kafka 3.9（KRaft，无 ZooKeeper）· RabbitMQ 3.13 · MinIO · Prometheus 2.54 · Grafana 11.2

---

## 五、本地启动

### 前置条件

- Docker Desktop（中间件全部跑在容器里）
- Go **1.26+**（`go.mod` 声明了 `go 1.26.0`）
- Node **20+** + pnpm（本机验证过 v20.19.5）

> **关于 Go 版本**：`GOTOOLCHAIN` 默认为 `auto`，所以即使本机装的是 1.22，只要在 `server/` 目录下执行 `go build` / `go run`，Go 会自动下载并使用 1.26 工具链，不会报错。但在仓库根目录执行 `go version` 看到的仍是本机版本——那不是编译用的版本。要确认实际版本，进 `server/` 再执行一次。

### 1. 起中间件

```bash
# 只想起业务所需的最小集合（MySQL/Redis/Kafka/RabbitMQ/MinIO）
docker compose -f deploy/docker-compose.yml up -d

# 连监控栈一起起（多加 Prometheus + Grafana）
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.observ.yml up -d
```

首次启动会自动执行 `deploy/mysql/init/` 下的 SQL（建表 + 全量种子数据），**25 张表全部有数据**，不需要再手工灌。

> 端口都刻意避开了默认值（3306/6379/9092/5672/9000），防止和本机已装的实例抢端口。

等健康检查通过：

```bash
docker compose -f deploy/docker-compose.yml ps
```

### 2. 起后端网关

```bash
cd server
go run ./cmd/gateway
```

```bash
cd server
go run ./cmd/gateway
```

**不需要任何环境变量前缀**。网关启动时会自动加载 `deploy/env/local.env`，日志第一行会说明用的是哪份配置：

```json
{"level":"INFO","msg":"config_loaded","env":"local","config_file":"..\\deploy\\env\\local.env"}
```

这份文件里已经包含本地开发需要的全部配置：登录口令 `demo1234`、MySQL/Redis/Kafka/RabbitMQ/MinIO 的连接地址、限流参数、日志级别。

想临时改某一项，命令行前缀即可（**真实环境变量优先于文件**）：

```bash
MC_LOG_LEVEL=debug go run ./cmd/gateway
```

> **关于 `MC_ADDR=127.0.0.1:8080`**：配置文件里已经设成回环地址。默认的 `:8080` 会监听所有网卡，Windows 防火墙每次遇到新的可执行文件路径都会弹授权框；绑回环后不再弹，而 Prometheus 容器通过 `host.docker.internal` 仍能抓到指标。代价是同一局域网内的其他设备无法访问——只有需要手机真机调试时才改回 `:8080`。

> **关于 `MC_ALLOW_DEV_SECRET=1`**：配置文件里开着，作用是没设 `MC_TOKEN_SECRET` 时用进程内随机密钥。**公网部署绝不允许**，否则重启即全体掉线。staging / prod 的配置里这一项都是 `0`。

### 3. 起前端

```bash
cd web
pnpm install
pnpm dev
```

访问 **http://localhost:5173**，用 `admin` / `demo1234` 登录。

Vite 会自动加载 `web/.env.development`，`/api` 已代理到 `127.0.0.1:8080`，不需要单独配后端地址。

> **必须用 `localhost` 而不是 `127.0.0.1`**：Vite 只监听 IPv6 回环 `[::1]:5173`，`127.0.0.1:5173` 打不通。

### 4. 验证跑通了

```bash
# 一行搞定：登录 + 五个中间件 + 9 个业务接口
node scripts/verify-env-config.mjs
```

输出长这样：

```
登录: code=0 token长度=90
基础设施连通性: blob/kafka/outbox/rabbitmq/redis_lock 全部 ok:true
业务接口: 9/9 个接口正常
```

手工查也可以：

```bash
curl http://127.0.0.1:8080/healthz          # {"status":"ok"}
curl http://127.0.0.1:8080/api/infra/health  # 需要带 token
```

---

## 六、Docker 怎么用

### 为什么中间件全跑容器

MySQL / Redis / Kafka / RabbitMQ / MinIO 五个组件，本机装一遍要处理版本冲突、配置文件、卸载残留；用容器则一行命令起、一行命令清，换机器不用重来。**业务代码（Go / 前端）不走容器**，因为本地开发要热更新和断点调试，容器反而是负担。

| 用什么跑 | 为什么 |
|---|---|
| 中间件 → 容器 | 要的是「环境一致、随时重建」 |
| 后端 Go → 本机 `go run` | 要的是编译快、能调试 |
| 前端 → 本机 `pnpm dev` | 要的是热更新 |

### 编排文件的分工

| 文件 | 环境 | 起什么 | 端口 |
|---|---|---|---|
| `deploy/docker-compose.yml` | 本地 | 5 个中间件 | 全部 publish（13306 / 16379 / 19092 / 15674 / 19000） |
| `deploy/docker-compose.observ.yml` | 本地 | + Prometheus、Grafana | 19090 / 13000 |
| `deploy/docker-compose.staging.yml` | 测试 | 全部 + nginx + 网关 | 80（443 待证书） |
| `deploy/docker-compose.prod.yml` | 生产 | 全部 + nginx + 网关 | 只有 80/443 |

后两份带 `-f` 叠加的用法只在「本地要连监控」时用得上：

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.observ.yml up -d
```

监控栈单独一份是因为它是**可选**的——只想跑业务时不必背 Prometheus + Grafana 两个容器的内存。

### 常用命令

```bash
# 起（后台）
docker compose -f deploy/docker-compose.yml up -d

# 看状态与健康检查
docker compose -f deploy/docker-compose.yml ps

# 跟随某个服务的日志
docker logs -f mc-mysql

# 进容器（比如连 MySQL）
docker exec -it mc-mysql mysql -uroot -proot123 mc_core

# 停掉但保留数据卷
docker compose -f deploy/docker-compose.yml down

# 停掉并删除数据卷（★ 数据全丢，下次启动重新从 SQL 初始化）
docker compose -f deploy/docker-compose.yml down -v
```

`down -v` 是「重置到初始状态」的唯一正确姿势。想重跑 `deploy/mysql/init/` 里的种子 SQL，必须先把 `mc-mysql-data` 卷删掉——**那些 SQL 只在数据卷为空时执行一次**，不删卷改再多 SQL 也没用。

### 容器日志怎么轮转

三份编排都定义了 `x-logging`，所有服务通过 `logging: *default-logging` 继承：

| 环境 | 上限 |
|---|---|
| 本地 | 10MB × 3 / 服务 |
| staging / prod | 20MB × 3 或 × 5 / 服务 |

Docker 默认**不轮转**，不配这个跑久了能把磁盘写满——这是 Docker 最经典的坑之一。

> ⚠️ 轮转只管 **stdout/stderr**。如果用具名卷把日志写成文件（比如给 nginx 挂 `/var/log/nginx`），那个卷**不受轮转管辖**，会无限增长。所以我们的 nginx 日志显式写 `/dev/stdout`。

### 网络分段（生产）

`docker-compose.prod.yml` 划了三个网段：

| 网段 | 谁在里面 | 能否出公网 |
|---|---|---|
| `edge` | nginx、web、gateway | 能 |
| `data` | MySQL、Redis、Kafka、RabbitMQ、MinIO | **不能**（`internal: true`） |
| `observ` | Prometheus、Grafana、node-exporter | 能 |

关键在 `data` 段的 `internal: true`：**数据库和缓存的端口根本没有暴露到任何主机端口**，即使应用被拿下，攻击者也摸不到 MySQL 的 3306——因为它不存在于可路由网络上。gateway 同时挂三个网段，是唯一能跨过去的那一个。

### 卷与数据

| 卷 | 存什么 | 删了会怎样 |
|---|---|---|
| `mc-mysql-data` | 业务数据 | 下次启动重新执行 init SQL，回到初始种子状态 |
| `mc-redis-data` | 缓存（AOF 持久化） | 无所谓，缓存本来就是可丢的 |
| `mc-kafka-data` | 消息 | 未消费的事件丢失 |
| `mc-rabbitmq-data` | 队列定义与消息 | 延迟任务丢失 |
| `mc-minio-data` | 导出文件 | 之前的导出下载链接失效 |
| `mc-prometheus-data` | 指标历史 | 图表回到空的 |
| `mc-grafana-data` | 面板与用户 | 回到 provisioning 的初始状态 |

### 镜像来源

| 镜像 | 说明 |
|---|---|
| `mysql:8.0` | |
| `redis:7-alpine` | 体积小 |
| `apache/kafka:3.9.0` | KRaft 模式，**不需要 ZooKeeper** |
| `rabbitmq:3.13-management` | 带管理界面 |
| `bitnamilegacy/minio:latest` | 注意是 `bitnamilegacy` 而非 `bitnami`——Bitnami 调整了镜像仓库策略，旧的 tag 迁到了 legacy 命名空间 |
| `prom/prometheus:v2.54.1` | |
| `grafana/grafana:11.2.0` | |
| `prom/node-exporter:v1.8.2` | 仅生产 |
| `nginx:1.27-alpine` | 仅 staging / prod |

### 排错

**容器起了但连不上**
先看健康状态：`docker compose ps`。`(unhealthy)` 说明健康检查没过，看 `docker logs <容器名>`。

**端口被占**
`netstat -ano | grep :13306` 找到占用进程。所有端口都刻意避开了默认值，还冲突的话改编排里的映射。

**改了 init SQL 但数据没变**
数据卷已经初始化过了。`down -v` 删卷重来。

**Kafka 连不上但容器是健康的**
本地用了双 listener：容器内走 `kafka:9092`，宿主机走 `localhost:19092`。宿主机程序必须连 **19092**，连 9092 会失败。

---

## 七、网关是什么

### 先把名字说清楚

这个项目里有两个东西都叫「网关」，容易混：

| 叫法 | 是什么 | 在哪 |
|---|---|---|
| **nginx** | 反向代理，管 HTTPS、静态文件 | 只在线上，本地没有 |
| **Go 网关**（gateway） | 一个 Go 程序，就是后端本身 | 本地 `go run ./cmd/gateway`，线上是容器 |

**这一节讲的是后者**。它不是什么神秘的基础设施，就是一个普通的 Go HTTP 服务，`server/cmd/gateway/main.go` 里 `func main()` 启动它，监听 8080。

### 它做什么

一句话：**所有 `/api/*` 请求的唯一入口**。前端只跟它说话，它跟后面的数据库和中间件说话。

```
前端 (5173)  ──HTTP/JSON──▶  Go 网关 (8080)  ← 你启动的那个进程
                                  │
                                  ├──▶ MySQL       查业务数据
                                  ├──▶ Redis       缓存、分布式锁、幂等
                                  ├──▶ Kafka       交易事件 → 审计
                                  ├──▶ RabbitMQ    延迟队列 → 权益到期回收
                                  └──▶ MinIO       导出文件
```

浏览器不可能直接连 MySQL，中间必须有一个 HTTP 服务替它去连。**所有请求都经过网关这一点，就是「单一入口」的含义**——正因为全站流量都从这一个口子过，才能在这里统一做鉴权、限流、打点。

### 为什么必须有它

**没有它也能做，只是每件事都要在前端和各个业务模块里重复一遍。**

| 不管的话 | 网关统一做 | 代码位置 |
|---|---|---|
| 每个接口都要自己验 token | 一次校验，把操作人放进 context 往下传 | `gateway.Auth()` |
| 每个接口都要自己写限流 | 令牌桶，按路由分组；关键写接口再按业务 ID 加一层 | `gateway.RateLimit()` / `gateway.RateLimitByKey()` |
| 每个请求都要自己记日志 | 统一结构化日志，自动带 `trace_id` | `gateway.Metrics()` |
| 每个接口都要自己统计耗时 | 统一打点，Prometheus 直接抓 | 同上 |
| 某个接口 panic 一次就整个进程崩 | 捕获后返回 500，记完整堆栈 | `gateway.Recover()` |
| 页面要调 3 个接口，前端串行等 | BFF 聚合，并发扇出合成 1 个响应 | `aggregate.go` |

最后一条是**首屏快的关键**：工作台一个页面原本要串行发好几个请求，现在只发 1 个（比如 `/api/bff/catalog/overview`），网关内部用 `errgroup` 并发查 4 个列表再合并。总耗时 ≈ 最慢的那一个，而不是所有加起来。

扇出的并发上限是 6（`fanoutLimit`）。**单个子查询失败不会拖垮整个响应**：那一个块返回空 + 错误标记，其余照常返回，同时 `mc_aggregate_degraded_total` 指标 +1。

### 它不做什么

**网关不写业务逻辑。** 它不查表、不算积分、不判断规则。那些在 `internal/domain/` 里。

网关只做「转发 + 顺手加点动作」。这条边界很重要——一旦开始往网关里塞业务判断，它就会变成一个什么都管的上帝对象。

### 启动时它装配了什么

看 `server/cmd/gateway/main.go` 的 `run()`，顺序是这样的：

```
1. 加载配置            deploy/env/local.env
2. 建日志              slog，JSON 格式写 stdout
3. 连 MySQL + Redis    store.Open()      ← 连不上直接退出
4. 建六个领域服务       catalog / benefit / trade / channel / operation / setting
5. 连 MinIO            失败也能起，导出功能降级
6. 连 Kafka / Rabbit   失败也能起，异步功能降级
7. 注册中间件和路由     r.Use(...) + 各个 api.GET(...)
8. 监听 8080
```

第 5、6 步「失败也能起」是刻意设计的：**Kafka 挂了不应该导致整个管理台登不进去**。对应能力会返回 503，但登录、查列表这些核心功能照常。可以在 `GET /api/infra/health` 看到每个组件的实时状态。

### 中间件链的顺序

`main.go:156` 这一行决定了处理顺序：

```go
r.Use(
    gateway.Trace(),    // ① 先生成 trace_id，后面所有日志都要用
    gateway.Recover(),  // ② 兜住 panic
    gateway.Metrics(),  // ③ 计时 + 打点 + 记访问日志
    gateway.CORS(...),  // ④ 跨域
)
```

**顺序不能随便调**：`Trace()` 必须最外层，否则后面的日志没有 `trace_id`；`Recover()` 必须在 `Metrics()` 外面，否则 panic 之后 `Metrics()` 里面那些 `defer` 算出来的耗时和状态码就不对了。

三组路由分开限流（这是**按来源**那一层）：

| 路由 | 鉴权 | 限流 | 为什么 |
|---|---|---|---|
| `POST /api/auth/login` | 无 | **最严**（`MC_LOGIN_RPS`，本地 2 QPS） | 防暴力破解 |
| `POST /api/telemetry/beacon` | 无 | 单独一档（`MC_TELEMETRY_RPS`，本地 50 QPS） | 匿名可达，埋点量大，不能挤占业务的令牌桶 |
| `/api/*` 其余 | 有 | 正常（`MC_RATE_RPS`，本地 30 QPS） | |

> 这三个默认值（代码里是 2 / 5 / 30）都能被 `deploy/env/local.env` 覆盖。当前配置文件把埋点调到了 50，其余保持默认。

### 两层限流：按「谁在刷」和按「刷的是哪条」

上面那三组是**第一层**，按 `路由 + 调用方` 分桶（调用方取 `X-Caller-Id`，没有就是 IP）。它防的是**单个来源**把配额吃光。

```
RateLimit()  →  key = route + "|" + caller
```

但它有个漏洞：换个 IP 就绕过。10 台机器各刷 1 次同一个 `rule_uid`，按来源看谁都没超，可数据库那一行已经被钉死了。

所以关键写接口还挂**第二层**，按 `路由 + 业务 ID` 分桶：

```
RateLimitByKey()  →  key = route + "|biz|" + rule_uid
```

| 层 | key | 挡住什么 | 挡住了怎么办 |
|---|---|---|---|
| `RateLimit` | 路由 + 调用方 | 一个 IP/调用方狂刷 | 封来源、加验证码 |
| `RateLimitByKey` | 路由 + 业务 ID | 大量来源一起刷同一条数据 | 加缓存、加锁、把热点拆开 |

**两层是正交的，缺一层都有洞**：

- 只有第一层 → 肉鸡/多 IP 打同一条数据，每个来源都达标，数据被打爆
- 只有第二层 → 一个来源轮着刷一万个不同 ID，每个 ID 的桶都是满的，照样打到库里

目前挂在三条路由上：

| 路由 | 业务 key | 为什么这条要挂 |
|---|---|---|
| `POST /api/benefit/expire-rule` | `rule_uid`（JSON body） | 要对规则行做 `UPDATE ... WHERE` + 拿分布式锁，是最热的一行 |
| `POST /api/benefit/expiry-task` | `rule_uid`（JSON body） | 同上，而且投递延迟任务是写放大 |
| `GET /api/export/jobs/:id` | `id`（路径参数） | 前端导出后轮询，容易被写成死循环每秒刷 |

配额是 `MC_BIZ_RPS` / `MC_BIZ_BURST`（本地 10 / 20）。**比第一层松是正常的**——它统计的是所有来源对同一个 ID 的合计，天然比单来源高一个数量级。但它必须远低于「让那一行扛不住」的量，这才是它的意义。

两个实现细节值得记一下：

1. **取不到 key 就放行**。参数校验是 handler 的事，不是限流层的事。而且如果所有非法请求都拿到空 key，它们会共用同一个桶——等于给攻击者一个免费的全局限流开关，把正常用户也一并打回。
2. **中间件读 body 之后必须塞回去**。`c.Request.Body` 是一次性流，读完不还原，下游 `ShouldBindJSON` 拿到空 body 直接报 400。`KeyFromJSON` 里做了 `io.NopCloser(bytes.NewReader(raw))` 还原。

### 你自己的代码在哪

| 文件 | 干什么 |
|---|---|
| `server/cmd/gateway/main.go` | **入口**。装配所有组件、注册路由。要看「启动流程」就看这里 |
| `server/cmd/gateway/handlers.go` | 各业务接口的 handler，把 domain 服务的返回包成 HTTP 响应 |
| `server/cmd/gateway/infra.go` | 中间件连接、健康检查 |
| `server/internal/gateway/gateway.go` | **中间件实现**：认证、限流、超时、指标、CORS、panic 恢复 |
| `server/internal/gateway/aggregate.go` | BFF 聚合，六个 `*Overview` |
| `server/internal/gateway/telemetry.go` | 前端埋点上报的接收与白名单校验 |

### 怎么验证它在跑

```bash
# 存活（不需要登录）
curl http://127.0.0.1:8080/healthz
# {"status":"ok"}

# 中间件连通性（需要先登录拿 token）
node scripts/verify-env-config.mjs
```

看它现在在处理什么请求：

```bash
# 实时日志（本地是终端输出；容器里用 docker logs）
docker logs -f <容器名>

# 指标（QPS、延迟、错误率）
curl http://127.0.0.1:8080/metrics | grep mc_gateway_request_total
```

### 常见误解

**「网关是不是一种中间件？」**
不是。中间件是它连出去的东西（MySQL、Kafka…）。网关是那个"连出去"的程序本身。

**「是不是必须用网关？」**
单体应用直连数据库也能跑。网关的价值在「统一」——鉴权、限流、日志、指标只写一遍。项目小了是负担，页面一多、模块一多，没有它就会陷入「每个接口都重新实现一遍」。

**「nginx 和它重复了吗？」**
不重复。nginx 在前面管 HTTPS 和静态文件，Go 网关在后面管业务 API。线上是 `浏览器 → nginx → Go 网关 → 数据库`。

---

## 八、页面地址与 GUI 入口

### 前端页面

| 地址 | 页面 |
|---|---|
| http://localhost:5173/ | 工作台（指标卡 + 概览） |
| http://localhost:5173/login | 登录 |
| `/catalog/spu` `/catalog/sku` `/catalog/cards` `/catalog/pricing` `/catalog/binds` | 商品域 5 页 |
| `/benefit/tiers` `/benefit/groups` `/benefit/rules` `/benefit/tasks` | 权益域 4 页 |
| `/trade/checkouts` `/trade/debits` `/trade/promos` | 交易域 3 页 |
| `/channel/merchants` `/channel/nodes` `/channel/attribution` | 渠道域 3 页 |
| `/operation/campaigns` `/operation/vouchers` `/operation/slots` | 运营域 3 页 |
| `/setting/products` `/setting/dict` `/setting/modules` `/setting/toggles` | 配置域 4 页 |

### 中间件 GUI

| 服务 | 地址 | 凭据 |
|---|---|---|
| **MinIO 控制台** | http://localhost:19001 | `mcadmin` / `mcadmin123` |
| **RabbitMQ 管理台** | http://localhost:15672 | `mc` / `mc123`，vhost `/console` |
| **Grafana** | http://localhost:13000 | `admin` / `admin`（本地已开匿名 Admin） |
| **Prometheus** | http://localhost:19090 | 无鉴权 |
| Prometheus 抓取目标 | http://localhost:19090/targets | — |
| Prometheus 告警列表 | http://localhost:19090/alerts | — |
| Prometheus 已加载规则 | http://localhost:19090/rules | — |
| Grafana 总览面板直达 | http://localhost:13000/d/mc-console-overview | — |

### 监控入口

监控分三块，各有各的界面，**没有统一的入口**：

| 看什么 | 去哪 |
|---|---|
| 后端指标（QPS / 延迟 / 错误率 / 缓存 / 限流 / 锁 / BFF 降级） | Grafana http://localhost:13000 |
| 后端指标原始数据、告警规则 | Prometheus http://localhost:19090 |
| 前端异常堆栈（哪个文件哪一行） | Sentry 官网控制台 → Issues |
| 前端运行日志（结构化，自动带 `trace_id`） | Sentry 官网控制台 → Logs |
| 前端自定义指标（应用启动、接口失败计数、Web Vitals 分位数） | Sentry 官网控制台 → Metrics |
| 前端性能剖析（哪段 JS 在空转 / 哪个函数慢） | Sentry 官网控制台 → Profiles |
| 前端错误率 / PV / Web Vitals | Grafana →「前端体验」组 |
| 某一次请求的完整经过 | 网关日志，按 `trace_id` grep（见第十一章） |
| 数据最后有没有真的写进库 | 快照 diff（见第十章） |

> **为什么没有「前端监控」这个菜单页**：要看的数据都不在业务库里——异常、日志、指标、性能剖析在 Sentry、
> PV 与 Web Vitals 在 Prometheus。自己再做一个页面只是把外部数据转发一遍，多一层要维护的转发层。
> `web/src/lib/monitor.ts` 是**上报的库**，只负责往外发，没有界面。

**具体怎么看、每个面板回答什么问题、怎么从一条用户抱怨查到根因**，全部写在
[docs/MONITORING.md](docs/MONITORING.md)。那份文档九章、约 800 行，是这块内容的唯一出处，
这里不再重复。

---

## 九、事务性发件箱（outbox）是什么

### 一句话

**把「要发消息」这件事，先写成数据库里的一行记录，跟业务数据在同一个事务里提交。** 发消息的动作交给后台协程去做。

### 它解决的那个问题

业务数据在 MySQL，事件要去 Kafka，这是两个系统，**没有共同的事务**。于是「更新业务表 + 发 Kafka 消息」只有两种写法，两种都会错：

| 顺序 | 出事时 | 后果 |
|---|---|---|
| 先写库，再发消息 | 库提交成功、发消息时进程崩了 / Kafka 不可达 | **库改了，下游永远不知道**。存量数据对不上，且没有任何报错 |
| 先发消息，再写库 | 消息发出去了、事务回滚了 | **下游拿着一条假事件做对账**，等于凭空多出一笔 |

这两种错误都**不会在测试环境暴露**——它们只在「恰好那一刻出错」时发生，而且发生时安安静静。这是最难查的一类 bug：没有异常、没有日志、只有几天后对账时发现数字不对。

### 怎么破

把「发消息」这个动作本身，落成一行数据库记录：

```
BEGIN;
  UPDATE bft_entitlement_rule SET lifecycle_state = 0
   WHERE rule_uid = ? AND lifecycle_state = 1;
  INSERT INTO msg_outbox (event_id, topic, payload, state, next_retry_at)
   VALUES (...);                        -- 同一个事务
COMMIT;
```

业务回滚 → 消息记录一起回滚；业务提交 → 消息记录一定在。**原子性由数据库事务保证，不依赖任何外部系统可达。**

真正投递由 `outbox.Store.Run()` 这个后台协程做，默认每 2 秒扫一轮，`main.go` 里就是这一行：

```go
go in.outbox.Run(ctx, in.kafka, 2*time.Second)
```

### 状态机

`msg_outbox.state` 只有三个值，数字直接落库（改它要配迁移）：

| 值 | 名字 | 含义 | 怎么出去 |
|---|---|---|---|
| `0` | `pending` | 待发 | 下一轮扫描会捡起来 |
| `1` | `sent` | 已发 | 终态 |
| `2` | `dropped` | 重试耗尽，转人工 | 终态，**必须有人看** |

### 重试与退避

投递失败不立刻放弃，指数退避封顶 1 分钟：`1s → 2s → 4s → … → 60s`，写回 `next_retry_at`，下一轮扫描跳过还没到点的记录。

重试上限默认 **50 次**（`maxRetry`）。这个数不是拍脑袋的：按 1 分钟封顶算，50 次大约给下游留**近一小时**的自愈窗口。反过来，如果配成 8 次、封顶 64 秒，只能扛三分钟——**一次 Kafka 滚动重启就足以把正常事件判成 dropped**。这个组合是错的，代码注释里专门写了。

超过上限 → `state = 2` + 打 `Error` 级日志 `outbox_dropped` + 指标 `mc_outbox_flushed_total{result="dropped"}` 加一。

> **dropped 是「已经数据不一致」的信号，不是「等会儿再说」。** 其他告警都可以拖，这条不行。

### 代价：至少一次，不是恰好一次

必须讲清楚：**outbox 保证的是「不丢」，不保证「不重」。**

- 消息发出去了、但把 `state` 改成 `sent` 时数据库失败 → 下一轮会**再发一遍**。
- 进程在「发完」和「标已发」之间被 kill → 同上。

所以消费侧**必须去重**。本项目用的是 `msg_consumed_event` 表：消费时先把 `event_id` 插进去，主键冲突就说明处理过了，直接跳过。

> **发件箱和消费去重是一对，只做一个等于没做。** 只做 outbox：消息不丢但会重复，下游把同一笔扣减做两遍。只做去重：不重复但会丢，因为源头根本没保证送达。

### 怎么观察它

| 看什么 | 在哪看 | 正常值 | 异常信号 |
|---|---|---|---|
| 积压条数 | Grafana `mc_outbox_backlog` | 平时 0 或个位数 | 持续大于 50 且不降 → `OutboxBacklogGrowing` 告警 |
| 补发结果 | `mc_outbox_flushed_total{result}` | `ok` 在涨 | `dropped` 出现即数据不一致 |
| 表里有没有卡住 | `SELECT state, COUNT(*) FROM msg_outbox GROUP BY state` | 绝大多数是 1 | 大量 0 且 `next_retry_at` 停在过去 → 补发协程停了 |
| 具体的错 | `SELECT * FROM msg_outbox WHERE state=2` | 空 | 看 `last_error` |

```bash
# 一条命令看积压分布
docker exec mc-mysql mysql -uroot -proot123 mc_core -e \
  "SELECT state, COUNT(*) cnt, MAX(retry_count) max_retry FROM msg_outbox GROUP BY state"
```

### 代码在哪

| 文件 | 干什么 |
|---|---|
| `server/internal/outbox/outbox.go` | 全部实现：`Enqueue` 登记、`Run` 循环、`flush` 投递、`markRetry` 退避、`markDropped` 兜底 |
| `server/cmd/gateway/main.go` | 启动补发协程 |
| `server/cmd/gateway/infra_handlers.go` | 示例：`lockDemoHandler` 里在事务内调 `outbox.Enqueue` |
| `server/internal/domain/export.go` | `ExpireBenefitRule` 的 `inTx` 回调——事件登记点 |

### 三个常见误解

**「有了 outbox 就不需要重试了」** —— 反了。outbox 本身就是重试机制，它替代的是「调用方自己写重试 + 补偿脚本」。

**「outbox 可以替代消息队列」** —— 不能。outbox 是**投递保证**，不是**传输通道**。消息最终还是要进 Kafka，否则下游没法多消费者消费。

**「消费侧不用去重，outbox 会处理」** —— 见上面的代价那节，这是最常见也最致命的误解。

---

## 十、数据库和中间件怎么看变化

### 先说清楚为什么截图不够

页面截图能证明「前端显示成什么样」，**证明不了数据真的写进去了**。一个页面完全可能：

- 前端本地状态更新了，请求其实失败了
- 请求成功了，但写的是另一张表
- 写进去了，但没进消息队列，下游永远收不到
- 缓存命中所以看起来对，实际数据库是错的

数据库、Redis、Kafka 这三个**没有图形界面**（MySQL 有 DBeaver 但要手动刷，Redis 和 Kafka 基本只能敲命令）。所以「改了一个业务操作之后到底发生了什么」要用另一套办法取证：**改动前存一份状态快照，改动后再存一份，diff 两份。**

### 一条命令跑通

```bash
# 1) 改动前
node scripts/snapshot.mjs before

# 2) 做你的业务操作（前端点一下、或者 curl 打一个接口）

# 3) 改动后
node scripts/snapshot.mjs after

# 4) 只看变了什么
node scripts/snapshot-diff.mjs before after
```

快照落在 `docs/shots/snapshots/<tag>.json`，是纯结构信息（表名 / 行数 / key 前缀 / topic offset / 队列长度），**不含任何凭据或业务数据**。连接信息从 `deploy/env/local.env` 现读现用，只在内存里。

### 它采了什么

| 数据源 | 采什么 | 为什么这样采 |
|---|---|---|
| **MySQL** | 25 张表各多少行 | 用一条 `UNION ALL` 查完，不开 25 次连接 |
| **Redis** | 总 key 数 + 按前缀分组 | 看 `idem:done` / 缓存 / 锁 各自的量，比看单个 key 有用 |
| **Kafka** | 每个 topic 各分区最新 offset 之和 | offset 是累计值，涨了就是有新消息 |
| **RabbitMQ** | 每个队列的 待消费 / 未确认 / 消费者数 | 队列积压是**要报警**的信号 |
| **MinIO** | 每个桶里的对象文件数 | 导出功能是否真的落了文件 |

### 一个真实例子

对一条**生效中**的权益规则触发失效，跑完 diff 得到：

```
── MySQL（表行数） ──
  ↑ msg_consumed_event: 10 → 11  (+1)
  ↑ msg_outbox: 10 → 11  (+1)
  ↑ trd_audit_trace: 11 → 12  (+1)

── Redis（key 数量） ──
  + idem:done = 1

── Kafka（topic 最新 offset） ──
  ↑ console.events: 12 → 13  (+1)
```

这五行不是五个独立现象，**是一条因果链**：

```
POST /api/benefit/expire-rule
  │
  ├→ MySQL  trd_audit_trace    +1  审计留痕（谁在什么时候动了什么）
  ├→ MySQL  msg_outbox         +1  事件在同一个事务里登记
  ├→ Kafka  console.events     +1  2 秒内被补发协程推上去
  ├→ MySQL  msg_consumed_event +1  消费者收到并记账（去重表）
  └→ Redis  idem:done          +1  幂等键，30s 内重放直接返回缓存
```

**这才是能拿来说明「数据真的从 HTTP 走到了库里、又流到了队列」的证据。** 截图永远给不出这一层。

### 怎么判断异常

| 现象 | 说明什么 | 该看哪 |
|---|---|---|
| diff 完全无变化 | 改动**没落库**——或者落到了别的库 / 别的 vhost | 先确认接口 `code=0`，再确认库名 |
| MySQL 变了、Kafka offset 没变 | outbox 登记了但**没推上去**，补发协程可能停了 | `mc_outbox_backlog`、`state=0` 的行 |
| Kafka offset 涨了、MySQL 没变 | 消费侧没在处理，或写的是别的表 | 消费者日志、`msg_consumed_event` |
| RabbitMQ 待消费数持续上涨 | **消费者没跑，或消费不过来** | 消费者是否存活、`mc_rabbit_*` 指标 |
| Redis 出现长期存在的 `lock:*` | **锁泄漏**，请求结束没释放 | 看锁的 TTL 设置和 defer 是否真的执行 |
| MinIO 桶数涨了但页面下载 404 | 文件落盘了，但元数据 / 索引没写 | 导出任务的 status 流转 |

### 不用脚本的手工查法

```bash
# MySQL：看某张表最近有没有新行
docker exec mc-mysql mysql -uroot -proot123 mc_core -e \
  "SELECT COUNT(*) FROM msg_outbox; SELECT * FROM msg_outbox ORDER BY out_uid DESC LIMIT 3"

# 库名注意：是 mc_core（不是 member_console，那个是历史遗留的空库）

# Redis：看有哪些 key 前缀
docker exec mc-redis redis-cli --scan --count 1000 | head -30

# Kafka：看 topic 消息总量
docker exec mc-kafka /opt/kafka/bin/kafka-get-offsets.sh \
  --bootstrap-server localhost:9092 --topic console.events --time -1

# RabbitMQ：看队列积压
docker exec mc-rabbitmq rabbitmqctl list_queues -p /console name messages consumers

# MinIO：看桶里有多少文件
docker exec mc-minio sh -c 'ls /data; du -sh /data/*'
```

> **`docker compose down -v` 之后所有数据都没了**，`deploy/mysql/init/*.sql` 会重新执行一遍，行数回到初始值。快照数字突然整体归零时先想想是不是干了这件事。

### 有界面的三个：直接看

| 服务 | 地址 | 看什么 |
|---|---|---|
| **RabbitMQ 管理台** | http://localhost:15672（`mc` / `mc123`，vhost `/console`） | Queues 页看每个队列的 Ready / Unacked / Consumers；Connections 看谁连着 |
| **MinIO 控制台** | http://localhost:19001（`mcadmin` / `mcadmin123`） | 桶列表、对象数、文件大小 |
| **Grafana** | http://localhost:13000 | 后端指标全貌，见 [docs/MONITORING.md](docs/MONITORING.md) |

---

## 十一、日志怎么看

日志是**结构化 JSON**，每条一个对象，字段固定：

```json
{
  "ts": "2026-11-09T16:30:09.2088345+08:00",
  "level": "INFO",
  "msg": "http_request",
  "trace_id": "afe69d6d3e27d8e9702990d96f46cfa5",
  "route": "/api/catalog/spu",
  "method": "GET",
  "status": 200,
  "duration_ms": 9.0431,
  "client_ip": "127.0.0.1"
}
```

`msg` 是事件类型，其余字段是事件属性。类型不同，字段也不同。

### 日志在哪

| 运行方式 | 位置 | 怎么看 |
|---|---|---|
| 本地 `go run` | 终端输出 | 直接看，或重定向到文件 |
| 本地/生产容器 | Docker json-file | `docker logs -f <容器名>` |
| 生产 nginx | Docker json-file | `docker logs -f loyalty-console-nginx-1` |

编排里都配了轮转（本地 10MB×3，生产 20MB×5），不会把盘写满。

### 三种查法

**1. 按 `trace_id` 串起一次请求 —— 最有价值**

这是这套日志的核心设计。前端生成 `trace_id` → 请求头带上 → nginx 透传 → 网关读取 → 每条日志都带 → 响应头回传前端。用户报错时截图里就有这个 id，拿它一路查下去：

```bash
# 本地（日志重定向到文件的情况）
grep "afe69d6d3e27d8e9702990d96f46cfa5" "$TEMP"/gw*.log

# 容器
docker logs loyalty-console-gateway-1 2>&1 | grep "afe69d6d3e27d8e9702990d96f46cfa5"
```

**2. 只看出错**

```bash
grep -E '"level":"(ERROR|WARN)"' gw.log
```

**3. 只看 5xx**

```bash
grep '"status":5' gw.log
```

### 日志事件类型

| `msg` | 来源 | 什么时候看 |
|---|---|---|
| `http_request` | 网关中间件 | 所有 HTTP 访问（`/metrics`、`/healthz` 已过滤） |
| `config_loaded` | 启动 | **确认这次用的是哪份配置**，排查配置错乱第一眼看它 |
| `gateway_started` | 启动 | 确认监听地址 |
| `using_ephemeral_token_secret` | 启动 | WARN，说明没设 `MC_TOKEN_SECRET`，重启会掉线 |
| `frontend_event` | beacon | **前端报错，带 `trace_id`** |
| `bff_block_failed` | BFF 聚合 | 单个子查询失败、整体降级 |
| `outbox_scan_failed` / `outbox_dropped` | Outbox | 事件写 Kafka 失败 |
| `kafka_bad_message` / `kafka_message_dead` | Kafka 消费 | 消息解析失败、进死信 |
| `rabbit_task_failed` / `rabbit_requeue_failed` | Rabbit 消费 | 异步任务失败、重入队失败 |
| `export_done` / `export_failed` | 导出任务 | 异步导出结果 |
| `panic_recovered` | 恢复中间件 | 进程没崩但请求挂了，带完整堆栈 |

### 抓前端报错的完整流程

1. 用户在页面看到报错，截图里有 `trace_id`（或让他在控制台复制 `X-Trace-Id` 响应头）
2. 去 Sentry 找这条异常，看堆栈和设备
3. 拿 `trace_id` grep 网关日志，看这次请求进到哪一层、SQL 有没有超时
4. 如果 nginx 日志也在收集，同样能 grep 到——三个服务的 `trace_id` 是同一个

### 本地 vs 生产

| | 本地 | 生产 |
|---|---|---|
| 日志位置 | 终端 / `$TEMP/gw*.log` | `docker logs`（json-file 轮转） |
| 保留期 | 手动管理 | 20MB × 5 / 服务 |
| 集中检索 | 无，靠 grep | **无**，靠 grep |
| 重启后 | 文件还在 | **容器删了就没了** |

> ⚠️ 生产重启前先留一份：`docker compose logs > backup-$(date +%s).log`。`docker compose down` 会删容器，日志跟着消失，而这往往正是你最需要它的时候。
>
> 要「查三天前那次报错」得上日志聚合系统（Loki + Promtail，约 40 行 yaml）。当前纯 JSON 的格式正好是 Loki 能直接解析的，不需要写正则。但项目现在没有，属于已知缺口。

---

## 十二、Nginx

### 它是什么，为什么需要

前端 `web/` 构建出来是一堆静态文件（`.html` / `.js` / `.css`）。浏览器不认 Go，也不会直连 8080。线上必须有个进程负责三件事：

1. 用户访问 `https://域名/` → 读磁盘上的 `index.html` 返回
2. 用户访问 `https://域名/api/xxx` → 转发给网关
3. 处理 HTTPS 证书、gzip、安全响应头

这就是 nginx 的全部工作。

### 本地开发：不用 nginx

本地用 Vite 自带的 dev server，它只做了一件和 nginx 相同的事——`web/vite.config.ts` 里：

```ts
proxy: {
    '/api': { target: env.VITE_API_ORIGIN || 'http://127.0.0.1:8080', changeOrigin: true },
},
```

浏览器请求 `/api/spu` → Vite 收到后转发给 8080。**为什么需要它**：直接请求 `http://127.0.0.1:8080/api/spu` 会被浏览器判定为跨域；走代理后浏览器以为自己请求的是同域 `localhost:5173`，不触发跨域检查。这东西**只在开发期存在**。

| | 本地开发 | 线上 |
|---|---|---|
| 静态文件 | Vite 内存里现编译 | nginx 读磁盘 |
| `/api` 转发 | Vite proxy | nginx `proxy_pass` |
| HTTPS | 无 | nginx 终止 TLS |
| 端口 | 5173 | 80 / 443 |

### 线上：两层 nginx

`deploy/docker-compose.prod.yml` 里有两个 nginx，别混：

| 服务 | 角色 | 暴露 | 配置 |
|---|---|---|---|
| `nginx` | 边缘网关，对公网 | `80` / `443` | `deploy/nginx/nginx.conf` + `conf.d/` |
| `web` | 只装静态文件 | 仅容器网络内 | `web/nginx.conf` |

**外层**负责证书和安全头，把请求分给下面两个：

```nginx
location /api/   { proxy_pass http://console_api; }    # → gateway:8080
location /       { proxy_pass http://console_web; }    # → web:80
```

**内层 web** 只有一句关键的：

```nginx
try_files $uri $uri/ /index.html;
```

单页应用必备。用户刷新 `/trade/checkouts` 时磁盘上没有这个文件，回退到 `index.html` 让前端路由接管，否则 404。

**为什么要两层**：外层管证书和公网入口，内层管静态文件。证书轮换只重启外层，不用动静态服务。

### 外层配置里的几个关键点

**`trace_id` 透传**（`conf.d/proxy_common.inc`）：

```nginx
proxy_set_header X-Trace-Id $http_x_trace_id;
```

前端带了就带着，没带就让 Go 网关自己生成。这是「前后端日志能串起来」的前提。

**日志写 stdout**：

```nginx
access_log /dev/stdout main buffer=32k flush=5s;
error_log  /dev/stderr warn;
```

⚠️ **不要给 `/var/log/nginx` 挂具名卷**。官方镜像里 `access.log` 是指向 `/dev/stdout` 的符号链接，挂卷会覆盖它，后果有两个：`docker logs nginx` 看不到访问日志，而且那个卷不受 `logging:` 轮转管辖，会无限增长把盘写满。

`buffer=32k flush=5s` 是攒够 32KB 或每 5 秒落一次，避免每条请求一次 syscall。

**日志格式是 JSON，且带 `trace_id`**（`deploy/nginx/nginx.conf`）：

```nginx
log_format main escape=json '{
    "ts":"$time_iso8601",
    "status":$status,
    "request_time":$request_time,
    "upstream_time":"$upstream_response_time",
    "trace_id":"$sent_http_x_trace_id",
    ...
}';
```

`$sent_http_x_trace_id` 和 Go 网关日志里的 `trace_id` 是**同一个值**。用户在 nginx 日志里定位到一条，就能跳到网关日志看同一次请求的内部细节。

**安全头**（`conf.d/site.conf`）：`X-Content-Type-Options`、`X-Frame-Options`、`Referrer-Policy`、`Permissions-Policy` 都带了，TLS 用 1.2+ 并关掉弱套件。

### 改完怎么验证

```bash
# 语法检查（会去解析上游主机名和证书，所以需要模拟）
docker run --rm \
  --add-host gateway:127.0.0.1 --add-host web:127.0.0.1 \
  -v "$PWD/deploy/nginx/nginx.conf:/etc/nginx/nginx.conf:ro" \
  -v "$PWD/deploy/nginx/conf.d:/etc/nginx/conf.d:ro" \
  -v "$PWD/deploy/certs:/etc/nginx/certs:ro" \
  nginx:1.27-alpine nginx -t
```

看到 `syntax is ok` / `test is successful` 就对了。

> staging 编排里暂时只开 80 端口、没挂证书目录——域名和证书就绪后再放开 `443` 和 `./certs` 挂载。

### 本地要不要用 nginx

**不需要**。Vite dev server 已经覆盖了开发期需要的一切（热更新、代理），再套一层 nginx 只会增加改配置的负担。nginx 只在验证「生产构建产物能否正确跑起来」时才用得上——那时候用 `deploy/docker-compose.prod.yml` 里的两层结构。

---

## 十三、连数据库与 Redis

### 用 DBeaver 连 MySQL

| 项 | 值 |
|---|---|
| 驱动 | MySQL 8 |
| Host | `127.0.0.1` |
| Port | `13306` |
| Database | `mc_core` |
| 用户名 / 密码 | `root` / `root123`（应用账号是 `app` / `app123`） |

> 连接新建后密码只能通过 GUI 设置——DBeaver 用自己加密的 `credentials-config.json`，没法手工生成。

### 用客户端连 Redis

Redis **无密码**，DB `0`：

```
地址：127.0.0.1:16379
```

主流 GUI（RedisInsight / Tiny RDM / Another Redis Desktop）填上面这一项即可。

> 注意：Redis 里的 key 大多 **300 秒过期**，隔一会儿去看可能是空的——这是缓存 TTL 的正常表现，不代表没数据。要验证缓存生效，看 `/metrics` 里的 `mc_cache_hits_total{namespace="redis"}` 或直接重复打同一个接口。

### 数据库表速查

```bash
docker exec -it mc-mysql mysql -uroot -proot123 mc_core -e "SHOW TABLES"
```

---

## 十四、配置与环境

配置分**三套环境**，同一份代码。后端启动时按 `MC_ENV` 自动加载，前端按 Vite 的 `mode` 加载。

| 环境 | 后端配置 | 前端配置 | 编排文件 | 状态 |
|---|---|---|---|---|
| local | `deploy/env/local.env` | `web/.env.development` | `docker-compose.yml` | **已跑通** |
| staging | `deploy/env/staging.env` | `web/.env.staging` | `docker-compose.staging.yml` | 未部署，域名待定 |
| prod | `deploy/env/prod.env` | `web/.env.production` | `docker-compose.prod.yml` | 未部署，域名待定 |

> staging / prod 的配置是**骨架**：结构、变量清单、优先级都已就位，域名相关项留空，从未部署验证过。补齐清单见 [docs/CONFIGURATION.md](docs/CONFIGURATION.md)。

### 优先级

**后端**（`server/internal/config`）：

```
真实环境变量  >  deploy/env/<MC_ENV>.env  >  代码里的默认值
```

- `MC_ENV` 决定读哪个文件，**默认 `local`**（忘记设时最坏情况是连不上生产库，而不是反过来）
- 已存在的环境变量**不会被文件覆盖** —— compose 的 `environment` 段、CI secrets 靠这一点生效
- **文件不存在不算错误**，生产容器里通常没有这个文件

> ⚠️ 「文件不覆盖已存在的变量」指的是**变量存在与否**。如果变量存在但值为空，`env()` 会当成没设、退回代码默认值——所以 `KEY=` 的语义是「交给默认值」。

**前端**（Vite）：

```
.env.<mode>.local  >  .env.<mode>  >  .env.local  >  .env
```

### 两条容易踩的坑

**① 前端的空值会盖住别的文件**

Vite 里 `.env.development` 的优先级**高于** `.env.local`。所以哪怕 `.env.development` 里只写一行空的 `VITE_SENTRY_DSN=`，也会盖住你放进 `.env.local` 的真实 DSN，表现是「明明配了却不生效」。

因此：**不想启用某个变量就别定义它**，别写空行占位。想在本机覆盖就写到 `.env.development.local`（优先级最高，且不进仓库）。

**② 前端 `VITE_` 变量会被打进 bundle**

浏览器里能翻到。这层只能放公开信息（后端地址、Sentry DSN、版本号）。任何口令、密钥都不能放这里。

### 最常改的几个

| 变量 | 本地取值 | 说明 |
|---|---|---|
| `MC_ADDR` | `127.0.0.1:8080` | 监听地址。`:` 开头等于监听所有网卡（`0.0.0.0`），Windows 会弹防火墙授权 |
| `MC_ADMIN_USER` / `MC_ADMIN_PASS` | `admin` / `demo1234` | 登录凭据 |
| `MC_TOKEN_SECRET` | 空 | 签发 token 的密钥；空则需 `MC_ALLOW_DEV_SECRET=1` |
| `MC_ALLOW_DEV_SECRET` | `1` | 开发期放行开关，**staging/prod 必须是 `0`** |
| `MC_TOKEN_TTL_MIN` | `720` | token 有效期（分钟） |
| `MC_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

完整清单——前端 `VITE_*`（含采样率）、构建期 `SENTRY_*`、Docker 构建参数、后端全部 36 个
`MC_*`（带默认值）、编排层变量——在 [docs/CONFIGURATION.md · 环境变量总表](docs/CONFIGURATION.md#五环境变量总表)。

### 数据源

| 变量 | 本地取值 |
|---|---|
| `MC_MYSQL_DSN` | `root:root123@tcp(127.0.0.1:13306)/mc_core?...` |
| `MC_REDIS_ADDR` | `127.0.0.1:16379` |
| `MC_MYSQL_MAX_OPEN` / `MC_MYSQL_MAX_IDLE` | `32` / `8` |

### 消息与对象存储

| 变量 | 本地取值 |
|---|---|
| `MC_KAFKA_BROKERS` | `127.0.0.1:19092` |
| `MC_KAFKA_TOPIC` / `MC_KAFKA_GROUP` | `console.events` / `console-audit` |
| `MC_RABBIT_URL` | `amqp://mc:mc123@127.0.0.1:15674/%2Fconsole` |
| `MC_BLOB_ENDPOINT` | `127.0.0.1:19000` |
| `MC_BLOB_ACCESS_KEY` / `MC_BLOB_SECRET_KEY` | `mcadmin` / `mcadmin123` |

> `MC_RABBIT_URL` 的端口是 **15674**（AMQP 协议口）；15672 是管理界面（浏览器开），别弄混。
>
> `MC_BLOB_PUBLIC_ENDPOINT` 是**浏览器**访问对象存储用的地址。预签名 URL 把 Host 算进了签名，这里写错浏览器会拿到 `SignatureDoesNotMatch`。

### 三套环境的关键差异

| 项 | local | staging | prod |
|---|---|---|---|
| `MC_ALLOW_DEV_SECRET` | `1` | `0` | `0` |
| `MC_LOG_LEVEL` | `info` | `debug` | `info` |
| `MC_RATE_RPS` | 30 | 60（压测放宽） | 30 |
| `MC_BIZ_RPS` | 10 | 20 | 10 |
| Kafka 保留 | 1 小时 | 24 小时 | 72 小时 |
| 端口 publish | 全部 | 部分 | 只有 80/443 |

完整说明见 [docs/CONFIGURATION.md](docs/CONFIGURATION.md)。

---

## 十五、目录结构

```
loyalty-console/
├── deploy/                     部署与基础设施
│   ├── env/                        ★ 三套环境配置（local / staging / prod）
│   │   ├── local.env                   已跑通，直接可用
│   │   ├── staging.env                 骨架，域名待填
│   │   └── prod.env                    骨架，域名待填
│   ├── docker-compose.yml          本地中间件
│   ├── docker-compose.observ.yml   本地监控栈（可选）
│   ├── docker-compose.staging.yml  测试环境编排（未部署）
│   ├── docker-compose.prod.yml     生产编排
│   ├── mysql/init/                 建表 + 种子数据（首次启动自动执行）
│   ├── nginx/                      反向代理与 TLS
│   ├── prometheus/                 抓取配置 + 告警规则
│   └── grafana/                    数据源与面板 provisioning
├── docs/                       专项文档（见下）
├── scripts/                    数据抓取与验证脚本
│   ├── verify-env-config.mjs       配置改完跑一遍：登录 + 中间件 + 业务接口
│   ├── check-zero-env.mjs          验证零环境变量启动时配置能否自动加载
│   ├── snapshot.mjs                ★ 数据层快照（MySQL 每表行数 / Redis key / Kafka offset / Rabbit 队列 / MinIO 对象）
│   ├── snapshot-diff.mjs           ★ 对比两份快照，只打变化的那些项
│   ├── check-md-swallow.mjs        文档体检：揪出被 shell 吞掉的片段（中文间双空格、空括号等）
│   ├── check-readme-anchors.mjs    文档体检：校验目录锚点能否对上真实标题
│   └── shoot-all.js                全站截图（22 个业务页 + 交互 + 监控页 → docs/shots/）
├── server/                     Go 后端
│   ├── cmd/gateway/                入口：路由注册、启动装配
│   ├── internal/
│   │   ├── config/                 ★ 按 MC_ENV 加载 deploy/env/*.env
│   │   ├── domain/                 六个领域的业务查询
│   │   ├── gateway/                鉴权、限流、超时、指标、BFF 聚合
│   │   ├── store/                  数据源与分层缓存
│   │   ├── mq/                     Kafka / RabbitMQ 封装
│   │   ├── outbox/                 发件箱
│   │   ├── lock/                   分布式锁
│   │   ├── idempotency/            幂等
│   │   ├── blob/                   对象存储
│   │   ├── exporter/               异步导出
│   │   ├── auth/                   token 签发校验
│   │   └── observ/                 指标与日志
│   └── pkg/trace/                  trace_id 透传
└── web/                        前端
    ├── .env.development            本地开发配置（入库）
    ├── .env.development.local      本机覆盖，如 Sentry DSN（不入库）
    ├── .env.staging / .env.production
    ├── src/
    │   ├── config/console.ts       ★ 菜单/路由/表格列的唯一事实来源
    │   ├── pages/DataPage.tsx      通用列表骨架（22 个页面共用）
    │   ├── pages/Dashboard.tsx     工作台
    │   ├── pages/Login.tsx         登录
    │   ├── layout/                 控制台外壳
    │   └── lib/                    http 封装、监控上报
    └── e2e/                        Playwright 端到端
```

### 加一个业务页面要改什么

只改 `web/src/config/console.ts`，加一个 `PageDef`。路由、菜单、表格、分页、搜索框、产品下拉全部自动生成——这是那份配置存在的全部理由。

```ts
{
    path: '/catalog/binds',
    title: '策略卡绑定',
    desc: '定价策略与售卖卡的挂载关系',
    api: '/api/catalog/binds',
    columns: [
        { key: 'uid', title: 'ID', width: 100 },
        { key: 'policy_uid', title: '定价策略 ID', width: 140 },
        { key: 'card_uid', title: '售卖卡 ID', width: 140 },
        { key: 'ordinal', title: '展示顺序', width: 110 },
    ],
}
```

---

## 十六、测试

```bash
cd server
go test ./...                          # 后端单元测试（不需要起任何中间件）
go vet ./...

cd web
pnpm typecheck                         # TypeScript 类型检查
pnpm exec playwright test              # 全量 E2E（需要完整环境）
pnpm exec playwright test e2e/infra.spec.ts   # 只测中间件
```

分两层，**职责不同**：

| 层 | 跑起来要什么 | 测什么 |
|---|---|---|
| **Go 单元测试** | 什么都不用，`go test ./...` 直接跑 | 纯逻辑：脱敏、退避、幂等键、限流器行为 |
| **Playwright E2E** | 5 个中间件 + 网关 + 前端全起 | 真实链路：数据真的进库、事件真的被消费、文件真的能下载 |

### Go 单元测试（17 个测试函数 / 33 条用例）

| 包 | 文件 | 覆盖 | 为什么它必须存在 |
|---|---|---|---|
| `internal/gateway` | `telemetry_test.go` | `safePath` 的 13 种输入 + 3 类截断边界（ASCII / 中文 / emoji） | 它出错的表现是**悄悄**把 `?token=xxx` 写进日志和指标标签，不报错、不 500 |
| `internal/gateway` | `ratelimit_test.go` | 并发不超 burst、桶按 key 隔离、令牌恢复、janitor 清理 | 隔离失效 = 一个 ID 被打爆就全站打死；janitor 失效 = 内存随 ID 数线性涨直到 OOM |
| `internal/outbox` | `outbox_test.go` | 退避序列、单调性、溢出、rune 截断 | 算错不报错，只让「下游恢复后多久追上」悄悄变味 |
| `internal/idempotency` | `idempotency_test.go` | 分段敏感性、拼接碰撞、空字段、格式 | 碰撞 = 两个不同操作被当成同一个，账对不上且无任何异常 |

这几条测试**做过反向验证**：临时删掉 `Fingerprint` 里的分隔符 → 碰撞测试立刻红并打印出碰撞的 key；临时把 `RateLimiter.Allow` 的 key 写死 → 隔离测试立刻红；临时把 `safePath` 的截断退回 `raw[:n]` → 中文和 emoji 两个子用例立刻红（打印出 `\xf0\x9f\x98` 这种半个字符）。删掉对应代码后测试不变红，那测试就是摆设。

**已知缺口：`-race` 跑不了。** 本机 `CGO_ENABLED=0` 且无 gcc，`go test -race` 直接报错。所以上面「限流器并发正确」只证明了**行为**（放行数不超限、桶不串），**没有**证明「没有数据竞争」。要补上需要装 mingw-w64 后跑 `CGO_ENABLED=1 go test -race ./internal/gateway/`。

### E2E（33 条）

| 文件 | 覆盖 |
|---|---|
| `infra.spec.ts` | 中间件闭环：MySQL 分页一致性、Redis 缓存与锁幂等、Rabbit→Kafka→MySQL 到期链路、MinIO 导出下载、Kafka 死信、发件箱积压、**限流两层各一条** |
| `monitoring.spec.ts` | 监控栈：Prometheus 抓到网关、告警规则加载、**规则引用的指标是否真有定义**、Grafana 面板渲染 |
| `smoke.spec.ts` | 业务冒烟：鉴权、22 个页面出数、搜索、退出 |
| `screenshots.spec.ts` | 页面截图产出到 `docs/shots/` |

共 33 条，2026-11-09 全量跑通（1.4 分钟）。

**限流那三条是刻意拆开写的**，因为它们防的是不同的攻击，任一层失效另一层都不会报错：

| 用例 | 断言 | 失效意味着 |
|---|---|---|
| 30 个不同来源并发刷同一 ID | 必须出现 429 | 按业务 ID 限流没生效，多 IP 打同一条数据无人拦 |
| 打满一个 ID 后换新 ID | 必须 200 | 桶没按 ID 分开，等于刷一次就把全站打死 |
| 同一来源连打 70 个不同 ID | 必须出现 429 | 加了按 ID 限流却把按来源限流删了 |

> 这三条做过反向验证：把 `MC_BIZ_BURST` 临时调到 100000（等于关掉第二层），第一条立刻变红，另两条仍绿——说明它们测的确实是各自那一层，不是同一件事的三种写法。

> `monitoring.spec.ts` 里那条「告警规则引用的指标必须存在」是为一个真实事故加的：曾有三条告警规则引用的指标**从未被埋点**，Prometheus 不报错、规则状态照样 `ok`（`no data` 也算 ok），于是永久不会触发也永远没人发现。现在这条测试会直接失败。

**仍然没覆盖的**：

- **并发正确性**（券的 `FOR UPDATE`、锁租约过期）靠代码评审，没有任何测试证明它对。要测必须起真 Redis/MySQL，那已经不是单元测试了，而 E2E 只断言业务结果，看不到中间状态。
- **staging 环境从未部署**：编排文件和配置都在，域名待定，没真起过。

---

## 十七、更多文档

| 文档 | 内容 |
|---|---|
| [docs/MONITORING.md](docs/MONITORING.md) | **监控的唯一出处**（9 章 / 约 800 行）：三条信号的分工、打点清单与选型理由、Prometheus 与 Grafana 是什么关系、面板怎么看、Sentry 与 beacon 双通道、告警分级、PV/UV/Web Vitals 在哪查、完整的排查手册 |
| [docs/DATA-MODEL.md](docs/DATA-MODEL.md) | 25 张表的设计、命名约定、域前缀、跨域引用策略 |
| [docs/CONFIGURATION.md](docs/CONFIGURATION.md) | **三套环境的配置体系**：加载优先级、待办清单、常见错误 |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | 公网部署：三层网段拓扑、TLS、构建与启动、验证 |

> README 只回答「怎么把它跑起来」，监控怎么用全部在 `MONITORING.md`。这两份内容**不互相复制**——以前是 README 里 400 行、`MONITORING.md` 里 250 行讲同一件事，改一处忘一处，现在只留一个出处。

---

## 十八、安全须知

- **凭据全部走环境变量**，仓库里只有 `.example` 和三套配置的占位符，全是假值。真正生效的口令由部署环境注入。
- **三套 env 文件是故意入库的**：`deploy/env/*.env` 和 `web/.env.*` 里没有真实密钥，只有端口、限流、开关这类公开参数。带真实值的 `.env.local` / `.env.development.local` 已在 `.gitignore` 里。
- **`MC_ALLOW_DEV_SECRET=1` 只用于本地**。staging / prod 配置里都是 `0`——未设 `MC_TOKEN_SECRET` 时启动直接失败，比「静悄悄用随机密钥、重启后全体掉线」安全。
- **前端 `VITE_` 变量会被打进 bundle**，浏览器可见。这层只能放公开信息，任何密钥都不能放。
- **预签名 URL 的 Host 参与签名**，`MC_BLOB_PUBLIC_ENDPOINT` 必须与浏览器实际访问的地址一致。
- **`scripts/data/` 整个目录不进仓库**——里面是抓取过程中的原始数据，可能含个人信息。
- `/metrics` 在生产只在内网网段可达，不对公网开放。
- **nginx 日志必须写 stdout**，不要给 `/var/log/nginx` 挂具名卷（会破坏轮转、`docker logs` 也看不到）。

---

## 十九、运营配置：规则 / 作业 / 脱敏

这三块是「配置系统本身」的能力，不属于任何一个业务域，入口在侧栏「运维配置」。

### 1. 可配置规则（`/operation/rules`）

和「运营体检」的区别：体检规则**写在代码里**，改判定要发版；这里的规则是运营**在页面上配**的。

条件用闭集 DSL，不用 JS / CEL：

```json
{"all":[{"field":"member.login_days","op":"gte","value":30},
        {"field":"tier.state","op":"eq","value":1}]}
```

算子只有 `eq / ne / gt / gte / lt / lte / in / nin / exists`，嵌套最多 8 层。
**这不是保守，是安全边界**：规则通过 HTTP 提交，一旦允许「表达式里执行代码」，
它就成了远程代码执行入口。

三个必须有的能力：

| 能力 | 为什么 |
|---|---|
| **试运行** | 改一条规则影响成千上万会员，先看样例数据下的命中情况 |
| **解释** | 返回每一步的「实际值 vs 期望值」，而不是只回一个 false |
| **版本快照** | 改坏了要能回滚，还要能查到「昨天和今天差在哪」 |

### 2. 定时作业（`/setting/jobs`）

DB 轮询 + Redis 分布式锁（复用 `internal/lock`），不引外部调度框架。

- `cron` 是五段式，自己解析（`internal/scheduler/cron.go`），支持 `*`、`*/n`、`a-b`、逗号组合
- 并发策略：`skip`（默认，上次没跑完就跳过）/ `queue` / `allow`
- 执行失败**也推进 next_run_at**——否则失败的作业会在每个心跳里被反复触发，变成死循环
- **立即执行**按钮：运维等不到 cron 点，必须能手动跑一次

内置命令：`cleanup_expired_rules`（只删停用满 30 天的）、`snapshot_metrics`、`purge_mask_audit`。

### 3. 字段脱敏（`/setting/masking`）

三层优先级：**显式策略 > 远端识别服务 > 本地识别**。

- 识别服务是独立的 Python 服务（`mask-service/`，FastAPI），改规则重启即可，不用重新编译网关
- **远端挂了自动降级到本地正则**，展示路径上的外部依赖必须尽力而为
- 导出也走同一条策略（导出是最容易漏的泄漏点）
- 明文访问必须带理由并写审计表 `mc_mask_audit`

### 本地跑起来

```bash
# 依赖（含 mask-service）
cd deploy && docker compose up -d

# 新表结构（已有数据卷时 initdb 不会自动跑）
docker exec -i mc-mysql mysql -uroot -proot123 --default-character-set=utf8mb4 mc_core \
  < deploy/mysql/init/05-ops-schema.sql

# Python 单测
docker exec mc-mask-service sh -c "cd /app && python -m pytest test_redact.py -q"

# 后端 / 前端
cd server && ./bin/mc-gateway.exe
cd web && npx vite --port 5181

# 端到端（需要 MC_ADMIN_PASS）
node scripts/test-ops-e2e.mjs      # 44 项：规则/作业 CRUD/脱敏 + 远端识别 + 降级
node scripts/test-agent-rules.mjs  # 真实大模型：explain_rule / simulate_rules
node scripts/test-ops-ui.mjs       # 有头浏览器：三个页面的只读交互（13 项）
node scripts/test-ops-ui-crud.mjs  # 有头浏览器：三个页面的新建/编辑/删除（12 项）
```

### 指标

| 指标 | 说明 |
|---|---|
| `mc_rule_eval_total{result}` | 规则求值次数（hit / miss） |
| `mc_rule_eval_duration_seconds{scope}` | 求值耗时（single / batch） |
| `mc_job_run_total{job_name,result}` | 作业执行结果 |
| `mc_job_duration_seconds{job_name}` | 作业耗时 |
| `mc_job_skipped_total{job_name,reason}` | 被跳过的作业 |
| `mc_mask_applied_total{source,kind}` | 脱敏生效（policy / remote / local） |
| `mc_mask_plaintext_total{field}` | 明文访问次数（审计指标，正常应接近 0） |

> 标签名是 `job_name` 而不是 `job`：`job` 是 Prometheus 抓取配置的保留标签，
> 自定义指标里再用同名标签会被覆盖（实测面板上全变成 `gateway`）。

看板：<http://127.0.0.1:13000/d/mc-ops-config>

### 已知问题

| 问题 | 影响 | 现状 |
|---|---|---|
| 模型会重复调用同一个工具 | 实测同一问题里 `explain_rule` 被调 3 次，单次问答成本从 0.045 元涨到 0.09 元 | 结论正确但浪费。现有判重只拦「参数完全相同」，模型微调 `data` 就能绕过。计划：同工具调用次数硬上限 + 提示词明确「结果已足够时直接给结论」 |
| 网关缺 DB 重连 | Docker/WSL 重启后 `driver: bad connection`，网关启动直接失败 | 重启 Docker Desktop 可恢复。根治需要连接池健康检查（`SetConnMaxLifetime` 已有，但启动时 ping 失败仍需重试） |
