# 监控方案

三支柱：**Sentry（前端错误 + 性能）+ Prometheus / Grafana（后端指标）+ 结构化日志（slog JSON）**。
三者不是并列的"都做一遍"，而是各有明确分工，谁替不了谁。

---

## 一、先定框架：三条信号各自回答什么问题

| 框架 | 回答的问题 | 在本项目里落在哪 |
|---|---|---|
| **RED** | 服务好不好？(Rate / Errors / Duration) | 后端 HTTP 指标 |
| **USE** | 资源累不累？(Utilization / Saturation / Errors) | node-exporter + 在途请求数 |
| **四黄金信号** | 用户有没有受影响？(延迟/流量/错误/饱和度) | 上面两者的并集，用作总纲 |

这三套指标体系只是**看问题的角度**，真正决定监控有没有用的是两件事：

1. **标签基数控制** —— 用 gin 的 `c.FullPath()`（`/api/catalog/spu`）而不是 `c.Request.URL.Path`。
   后者带 ID 就是无限基数，Prometheus 内存会爆。这条是硬约束，不是优化建议。
2. **数据面信号** —— 只看错误率会漏掉"接口 200 但少了一块数据"。
   所以 BFF 聚合的降级单独打点（`mc_aggregate_degraded_total`）。

---

## 二、前端：Sentry

### 错误 vs 性能的采样率不同

```ts
Sentry.init({
  dsn: import.meta.env.VITE_SENTRY_DSN,
  sampleRate: 1.0,          // 错误全采
  tracesSampler: () => Number(import.meta.env.VITE_TRACES_SAMPLE_RATE ?? 1), // 性能默认全采
});
```

**为什么分开？** 错误事件量小（正常时一天几十条），全采不花钱且必须全采 ——
漏掉的那条可能正好是唯一一次真实崩溃。

性能事件量大（每次导航都是一条 trace），所以采样率做成环境变量。**本项目的默认值是 1.0（全采），
不是 0.1**：这是低流量的内部运营后台，导航量很小，10% 采样会让 Traces / Profiles 时有时无，
演示时看起来就像坏了。这个量级下全采的配额开销可以忽略，换来的是三页稳定有数据；
真上到高流量时用 `VITE_TRACES_SAMPLE_RATE=0.1` 把成本压下去即可。

这是"抽样"的正确用法：**抽样是为了控成本，不是为了省事**。
凡是"少了它就没法定位问题"的信号，一律不许采样；能不能采、采多少，由流量规模决定。

### 三条必须做的处理

**1. URL 收敛**
查询串里经常带 token、uid、手机号。`beforeSend` 里把 URL 砍成 `origin + pathname`：

```ts
url: scrubUrl(event.request?.url)   // 只保留协议+域名+路径
```

**2. 敏感 header 剔除**
`authorization` / `cookie` / `set-cookie` / `x-trace-id` 一律不进 Sentry。

**3. 用 `trace_id` 把前后端串起来**
前端每次请求生成 `X-Trace-Id`，后端原样回写并写进日志。
Sentry 事件上打 `trace_id` tag，出问题时可以在 Grafana / 日志里按同一个 id 捞。

> 这一条是整套方案里最有价值的连接：**没有 trace_id，前后端监控就是两座孤岛**，
> 前端报"接口 500"、后端日志一堆 record，靠时间戳对不齐。

### Web Vitals 走双通道

`web-vitals` 采集 LCP / CLS / INP / FCP / TTFB，同时发两处：

- Sentry breadcrumb —— 报错时能看"崩之前用户等了多久"
- 自建 `/api/telemetry/beacon` —— 进 Prometheus，能做**聚合视角**（"今天 LCP 良好率跌到 60%"）

为什么要自建而不是全靠 Sentry？因为 Sentry 的 performance 是按 trace 抽样的，
**抽样后算良好率会有偏差**；而 beacon 是全量计数，适合算比率。
两者量的东西不同，不是重复建设。

### 埋点接口本身必须限流 + 白名单

`POST /api/telemetry/beacon` 是公网可写接口，防护做在服务端：

- 独立令牌桶（`MC_TELEMETRY_RPS`），不和业务接口抢配额
- `kind` / `name` / `rating` 全部白名单枚举，不合法直接丢弃
- 单次 body 上限 64KB、事件数上限 50、URL 只存 256 字符且剥掉查询串
- 永远返回 204 —— 埋点失败不该影响用户

---

## 三、后端：Prometheus + Grafana

### 打点清单与选择理由

| 指标 | 类型 | 为什么是这个类型 |
|---|---|---|
| `mc_http_requests_total` | Counter | 只增不减，用 rate 求速率 |
| `mc_http_request_duration_seconds` | **Histogram** | ★ 必须 Histogram。Summary 的分位数**跨实例合不了**，多副本时 p99 会算错 |
| `mc_http_in_flight_requests` | Gauge | 可增可减，看饱和 |
| `mc_cache_hits_total` / `misses` | Counter | 算命中率 |
| `mc_cache_singleflight_collapsed_total` | Counter | 防击穿实际挡下了多少次 |
| `mc_aggregate_fanout_duration_seconds` | Histogram | 扇出耗时分布 |
| `mc_aggregate_degraded_total` | Counter | 按 `aggregate` / `block` 分，定位哪一块降级 |
| `mc_gateway_rate_limited_total` | Counter | 按 `route` / `scope`；`scope=caller` 是单来源刷，`scope=biz_key` 是多来源刷同一 ID |
| `mc_gateway_auth_fail_total` | Counter | 扫接口的早期信号 |
| `mc_frontend_vital_value` | Histogram | 前端体验，按 `metric` 分 |
| `mc_frontend_events_total` | Counter | 按 `kind` / `name` / `rating` |
| `mc_mq_kafka_consumed_total` | Counter | 按 `topic` / `result`（ok/bad/dlq），`dlq` 非零 = 有事件没被处理 |
| `mc_mq_rabbit_dead_lettered_total` | Counter | 延迟任务进死信，到期回收漏处理最危险 |
| `mc_lock_acquire_total` / `lost_total` | Counter | 抢锁结果 / 租约丢失；**label 用冒号前缀不用完整 key** |
| `mc_idempotency_outcome_total` | Counter | `proceed` / `replay` / `busy` / `degraded` |
| `mc_outbox_backlog` | **Gauge** | 此刻待补发条数。积压是瞬时量，用 Counter 会得到一个没意义的累计值 |
| `mc_outbox_flushed_total` | Counter | 补发结果 `ok` / `retry` / `dropped`，`dropped` 是数据不一致的开始 |
| `mc_export_jobs_total` | Counter | 按 `status`，卡在 `running` 说明导出协程死了 |
| `mc_blob_operations_total` | Counter | MinIO 读写，按 `op` / `result` |

锁的 label 这里单独说一句：如果用 `lock="benefit.expire.rule:10001"` 这种完整 key，
每个业务对象都会生成一条时间序列，几千个规则就能把 Prometheus 撑爆。
代码里用 `scopeOf(key)` 取冒号前缀（`benefit.expire.rule`），只保留有限个取值。

Histogram 的桶是**按业务量级选的**，不是抄默认值：

```
HTTP 延迟桶：5ms 10ms 25ms 50ms 100ms 250ms 500ms 1s 2s 5s
扇出块数桶：1 2 4 6 8 12 20   ← 和 fanoutLimit=6 对齐
Vitals 桶：  0.1 0.25 0.5 1 1.5 2.5 3 4 6 10
```

桶选错了，`histogram_quantile` 出来的数就是错的。**这是最容易被忽略的一处**。

### 面板怎么看

打开 Grafana（本地 `http://localhost:13000`），预置面板 `会员运营台 · 总览`：

1. **顶部四格**是判断"现在正不正常"的第一眼：
   请求速率 / 5xx 错误率 / P99 / 在途请求。
   **四格都正常就不用往下看了** —— 这是减少无关操作的设计。

2. **BFF 降级**这一栏是本项目独有的信号。
   页面 200 但少了数据块，错误率完全看不出来，只有这里能看见。

3. **缓存命中率 < 50%** 要警觉：要么 TTL 太短，要么 key 每次都不同（后者是 bug）。

4. **前端 Vitals 良好率**：LCP 良好率低于 75%（Google 基线）就该看首屏包大小了。


### Grafana 和 Prometheus 到底是什么关系

#### 一句话区分

| | Prometheus | Grafana |
|---|---|---|
| 是 | **时序数据库 + 抓取器** | **画图工具** |
| 干的事 | 每 15 秒去你的服务 `/metrics` 抓一次数字，存进自己的库 | 连上 Prometheus 查数，画成折线图 / 仪表盘 |
| 有没有界面 | 有，但是工具型的（只能手打查询） | 有，是给人看的 |
| 类比 | **仓库** | **橱窗** |

**顺序是：你的代码打点 → `/metrics` 暴露 → Prometheus 定时抓走存起来 → Grafana 画出来。**

两者可以分开用：不装 Grafana，Prometheus 自己的 `/graph` 页面也能查（丑但能用）；但 Prometheus 不能替你画面板，Grafana 也不能自己采数据。

#### Prometheus 到底存了什么

存的是**带时间戳的数字 + 一组标签**，一行长这样：

```
mc_http_requests_total{route="/api/catalog/spu", status="200"}  1523  @17:50:01
mc_http_requests_total{route="/api/catalog/spu", status="200"}  1541  @17:50:16
mc_http_requests_total{route="/api/catalog/spu", status="200"}  1559  @17:50:31
```

四段：**指标名 + 标签（花括号里）+ 值 + 时间戳**。每 15 秒追加一行，所以叫「时序数据库」。

**关键点：它存的是原始累计值，不是结论。** `1523` 这个数本身没有意义；有意义的是「从 1541 涨到 1559，这 15 秒里有 18 次请求」——这就是 `rate()` 在算的东西。

**为什么不能直接用 MySQL 存指标**

| | MySQL 存业务 | Prometheus 存指标 |
|---|---|---|
| 存什么 | 当前状态（这条订单现在是什么） | 随时间变化的数（每秒多少次） |
| 支持的操作 | 增删改查都有 | **只追加，不支持 UPDATE** |
| 查询方式 | `SELECT` 精确匹配 | `rate()` / `histogram_quantile()` 算速率和分位 |
| 数据量 | 每笔业务一行 | 每个指标每 15 秒一行 |
| 保留期 | 永久 | 通常 7~15 天就删（老数据没价值，占空间） |

两者不能互换：拿 MySQL 存指标，几千万个时间点写进去查不动；拿 Prometheus 存订单，它连 `UPDATE` 都没有。

#### 本地 Prometheus 的四个页面

| 做什么 | 地址 |
|---|---|
| **手打 PromQL 查指标** | http://localhost:19090/graph |
| 抓取目标活着没 | http://localhost:19090/targets |
| 告警当前状态 | http://localhost:19090/alerts |
| 有哪些告警规则 | http://localhost:19090/rules |

最常用的三条查询：

```promql
# 每秒请求数
sum(rate(mc_http_requests_total[5m]))

# P99 延迟
histogram_quantile(0.99, sum by (le) (rate(mc_http_request_duration_seconds_bucket[5m])))

# 5xx 错误率
sum(rate(mc_http_requests_total{status=~"5.."}[5m]))
  / sum(rate(mc_http_requests_total[5m]))
```

> **一直要记住的一句话**：Prometheus 存的是**仓库**，Grafana 是**橱窗**。没有 Grafana，Prometheus 自己也能查；没有 Prometheus，Grafana 是空壳。

#### 你的代码在哪打点

`server/internal/observ/observ.go` 里定义所有指标，比如：

```go
// 请求计数，按 路由/方法/状态码 切分
RequestTotal = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "mc", Subsystem: "http", Name: "requests_total",
    },
    []string{"route", "method", "status"},
)
```

`Namespace + Subsystem + Name` 拼起来就是指标全名 → **`mc_http_requests_total`**。

四种指标类型，用之前先想清楚要问什么：

| 类型 | 能做什么 | 例子 | 典型误用 |
|---|---|---|---|
| **Counter** | 只增不减，算「累计发生了多少次」 | `mc_http_requests_total` | 拿它直接看「此刻多少」，应该是 `rate()` |
| **Gauge** | 可增可减，算「此刻是多少」 | `mc_http_in_flight_requests` | 拿它算「总共多少次」，反了 |
| **Histogram** | 分布，算分位数 | `mc_http_request_duration_seconds` | 直接取平均值，长尾被稀释 |
| **Summary** | 分布，但分位数**不能跨实例合并** | — | 多副本时算出来的 p99 是错的 |

本项目**全用 Histogram 不用 Summary**，就是因为线上是多副本，Summary 的分位数合不了。

#### Grafana 面板里那些查询怎么读

```promql
sum(rate(mc_http_requests_total{route=~"$route"}[5m]))
```

从里往外读：

| 片段 | 含义 |
|---|---|
| `mc_http_requests_total` | 原始 Counter（累计值） |
| `{route=~"$route"}` | 按标签筛选，`$route` 是面板顶部的下拉框变量 |
| `[5m]` | 取最近 5 分钟的数据点 |
| `rate(...)` | 换算成「每秒多少次」，Counter 必须过 `rate` 才有意义 |
| `sum(...)` | 把各路由加起来，得到总量 |

```promql
histogram_quantile(0.99, sum by (le) (rate(mc_http_request_duration_seconds_bucket{route=~"$route"}[5m])))
```

| 片段 | 含义 |
|---|---|
| `..._bucket` | Histogram 的桶，每个 `le` 是一个「小于等于」边界 |
| `sum by (le)` | **必须先按 `le` 聚合**，否则分位数算不出来 |
| `histogram_quantile(0.99, ...)` | 从桶里估算 p99 |

> **`sum by (le)` 那一步漏了是最常见的错误**，Grafana 会直接不画线。

#### 本地这套有什么、没什么

| 组件 | 地址 | 有 |
|---|---|---|
| **Prometheus** | http://localhost:19090 | 抓网关 `/metrics`，14 条告警规则 |
| **Grafana** | http://localhost:13000 | 一个总览面板（20 个 panel，5 个分组） |
| **node-exporter** | — | **没有**。所以「主机资源（USE）」那两个面板永远是 No data |
| **alertmanager** | — | **没有**。告警只在 Prometheus 的 `/alerts` 页面能看到状态，不会发通知 |
| **Loki** | — | **没有**。日志不进 Prometheus，走的是 stdout + `docker logs` |

> 那两个 No data 的面板不是配错了，是本地没装 node-exporter。要补的话在 compose 里加一个 `prom/node-exporter` 即可，不影响其他面板。

#### 为什么用这一套而不是别的

| 替代方案 | 为什么没选 |
|---|---|
| 直接用日志统计 | 日志是逐条事件，算 QPS 要扫全量；指标是预聚合的，一条时间序列就是一个数 |
| 自建 MySQL 存指标 | 时序数据的写入模式和关系库完全不同，量一大就写不动 |
| 商业 APM（Datadog 等） | 要花钱、要联网。Prometheus + Grafana 本地零成本，且是云原生事实标准 |
| Summary 代替 Histogram | 多副本时分位数合并会算错 |

---


### 本地怎么看

本地没有 nginx，监控栈是独立一份编排，和中间件分开起：

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.observ.yml up -d
```

| 地址 | 用途 |
|------|------|
| http://127.0.0.1:13000 | Grafana，预置面板已自动加载 |
| http://127.0.0.1:19090 | Prometheus，`/targets` 看采集状态 |
| http://127.0.0.1:8080/metrics | gateway 原始指标 |

本地版和生产版的**唯一实质差异**是采集目标：本地 gateway 跑在宿主机上
（`go run ./cmd/gateway`），Prometheus 在容器里，所以要经
`host.docker.internal` 反查宿主机；生产版两个都是容器，直接写 `gateway:8080`。

本地不加载 `alerts.yml`：里面大量规则依赖 `node_*` 主机指标，
而 Windows 上的 node-exporter 量到的是 Docker Desktop 的 Linux VM，
内存/磁盘数字和宿主机对不上，加载了只会满屏误报。

**本地 Grafana 开了匿名 Admin 免登录**，只监听 `127.0.0.1:13000`。
这是开发便利，生产编排里 `GF_AUTH_ANONYMOUS_ENABLED: "false"` 且绑回环。

### 抓取配置的取舍

`scrape_interval: 15s`，保留 15 天。
密度再高对 p99 更准，但磁盘按天线性涨；15 天足够回溯"昨天下午那次抖动"。
真需要更长的历史，应该用远端存储（Thanos / VictoriaMetrics），而不是把本机磁盘堆满。

---

## 四、日志：结构化是底线，不是加分项

后端用 `slog` 输出 JSON：

```json
{"time":"2026-11-07T14:30:01Z","level":"INFO","msg":"request",
 "trace_id":"a1b2...","route":"/api/catalog/spu","status":200,
 "dur_ms":12.4,"operator":"admin"}
```

**为什么一定要 JSON？** 因为日志的价值在"能按字段聚合"。
一坨非结构化文本，出问题时只能 `grep`，而 `grep` 回答不了"今天哪条路由最慢"。

三条纪律：

1. **不打印请求体 / 响应体**。里面有 token、有用户数据，
   打出来等于发出去（终端输出经常被整段贴进 issue）。
2. **错误日志带 `trace_id`**。这是从"用户报错"到"哪一行"的唯一路径。
3. **日志级别可调，但生产不关 `INFO`**。
   关掉 `INFO` 省的那点磁盘，会在出事时让你完全看不到上下文。

nginx 的 JSON 日志**刻意不记查询串**（`log_format` 里没有 `$request`，
只有 `$request_method` + `$uri`）—— 查询串是 token 泄漏最常见的入口。

---

## 五、告警：只报"用户已经受影响"

`deploy/prometheus/alerts.yml` 里每一条都能对应一个具体动作。
刻意**没有**"CPU > 70%"这类阈值 —— CPU 高但延迟正常，说明还没到瓶颈，
响了只会训练大家忽略告警。

| 告警 | 触发条件 | 意味着什么 |
|---|---|---|
| `GatewayDown` | `up == 0` 持续 1m | 服务没了 |
| `HighErrorRate` | 5xx 占比 > 5% 持续 3m，且请求量有统计意义 | 用户真在失败 |
| `AuthFailureSpike` | 鉴权失败 > 5/s | 有人在扫 token，或凭据过期 |
| `HighP99Latency` | P99 > 1s 持续 5m | 慢到用户会感知 |
| `InFlightSaturated` | 在途 > 100 持续 5m | 吞吐见顶 |
| `CacheHitRateLow` | 命中率 < 50% 持续 15m | 缓存没起作用 |
| `AggregationDegraded` | 降级 > 1/s | **200 但少数据，错误率看不出来** |
| `FrontendPoorLCP` | LCP 良好率 < 75% | 首屏慢 |
| `FrontendErrorSurge` | JS 错误 > 2/s | 某次发版引入 |
| `OutboxBacklogGrowing` | 发件箱积压 > 50 持续 10m | 下游堵了，或补发协程停了 |
| `OutboxDropped` | 有事件重试耗尽被丢弃 | **已经数据不一致**，需要人工补发 |
| `KafkaDeadLetter` | 消费进死信 | 事件永久没被处理 |
| `RabbitDeadLetter` | 延迟任务进死信 | 该失效的权益没失效 |
| `LockLost` | 锁租约丢失 | 持锁逻辑超过了租约，查慢查询/无超时的外部调用 |

消息类告警的阈值刻意只看「有没有丢」，不看「快不快」：
延迟是可恢复的，丢事件不是。`OutboxDropped` 和 `KafkaDeadLetter`
都给了 critical，因为它们的后果要人工对账，不能靠自然恢复。

`HighErrorRate` 里那个 `clamp_min(..., 0.5)` 是有意的：
凌晨只有 2 个请求、其中 1 个失败，就是 50% 错误率 —— 不设分母下限的话，
每天都在误报。**告警的难点不是设阈值，是防噪音。**

---

## 六、成本与留存

| 项 | 策略 | 理由 |
|---|---|---|
| Sentry 错误 | 全采 | 漏一条可能就漏了唯一一次崩溃 |
| Sentry 性能 | 默认全采（env 可调） | 低流量内部后台，全采让 Traces/Profiles 稳定可见；上量后用 `VITE_TRACES_SAMPLE_RATE` 下调 |
| Prometheus | 15 天本地 | 覆盖"最近一次抖动"；更长用远端存储 |
| 容器日志 | 20MB × 5 轮转 | 不轮转迟早吃满磁盘 |
| 埋点事件 | 服务端白名单 + 限流 | 公网可写接口，不限流等于开了个免费压测入口 |
## 七、前端监控：Sentry 与自建 beacon

### 先说一个容易误解的点

**本项目的「前端监控」没有自己的页面。**

打开 http://localhost:5173 侧边栏，你只会看到 22 个业务页，没有「监控」这一项。原因是前端监控的产出**不在这个应用里**：

| 你以为的 | 实际是 |
|---|---|
| 后台里有个「前端监控」菜单页 | **没有**。`web/src/pages/` 下只有 `Dashboard` / `DataPage` / `Login` 三个文件 |
| 监控数据在某个 API 里能查到 | 不进业务库。beacon 打进 Prometheus 指标，Sentry 事件进 Sentry 云端 |
| 有实时错误列表的界面 | 那就是 **Sentry 的控制台**，不在本项目里 |

`web/src/lib/monitor.ts` 是**上报的库**，不是页面。它做三件事：初始化 Sentry、注册全局错误捕获、把事件 POST 到 `/api/telemetry/beacon`。**它只负责往外发，没有任何界面。**

> **为什么不自己做一个监控页？** 因为要看的两样东西都不在业务库里：异常堆栈在 Sentry、指标在 Prometheus。自己再做一个页面，本质是把两边数据转发一遍，多一层要维护的转发层，而且永远落后于 Sentry 官方的面包屑/版本对比能力。**已经成熟的工具直接用它自己的界面。**

### 两条上报路径，同时走

| | Sentry | 自建 beacon |
|---|---|---|
| 上什么 | 异常堆栈、设备、面包屑、Web Vitals | 只上「发生了什么」的计数 |
| 依赖 | 外部 SaaS，要 DSN | 无，打到自己的网关 |
| 谁维护 | 第三方 | 你自己 |
| 看什么 | **单个用户到底怎么错的**（有堆栈） | **多少人在错**（有聚合） |
| 界面在哪 | **Sentry 官网控制台**（不在本项目） | Grafana「前端体验」组 + 网关日志 |

**为什么两套都要**：Sentry 能告诉你「第 37 行 undefined」，但它看不到全站错误率。beacon 能告诉你「5 分钟内 30 个 JS 错误」，但不给堆栈。前者查个案，后者看趋势。

### Sentry 怎么用

1. 打开 Sentry 项目面板 → **Issues**。
2. 每条 Issue 点进去看：**堆栈**（哪一行）、**Breadcrumbs**（出错前用户点了什么、发了什么请求）、**Tags**（浏览器/系统/版本）、**设备分布**。
3. 修完点 **Resolve**，再出会自动变成 **Regressed**——这比你自己记「修了没」可靠。

> **Sentry 的界面长什么样**：左侧是 Issues / Performance / Releases 这些模块，点 Issues 出来的是一个按「最近出现时间 / 出现次数 / 影响用户数」排序的错误列表，每条点进去是堆栈 + 面包屑的详情页。**它本身就是可视化的监控 UI**，只是不在你的域名下。本地 `.env.development.local` 里配的 DSN 决定了数据进哪个项目。

**验证 Sentry 通了**：浏览器打开 http://localhost:5173，F12 控制台执行

```js
throw new Error('sentry 验证');
```

几秒后 Sentry 的 Issues 里应出现这条。**这一步必须在浏览器里做**，命令行发请求验证不到 DSN 是否生效。

采样率是 `tracesSampler` 默认 **1.0（全采）**、`sampleRate: 1.0`（错误全采）；高流量时可用 `VITE_TRACES_SAMPLE_RATE` 下调。上报前会剔除 `authorization`、`cookie` 等敏感头，URL 里的 token 也会抹掉。

### beacon 怎么用

beacon 的产出在**两个地方**，看的问题不一样：

```promql
# 1) 聚合趋势 —— Prometheus 里有指标
#    哪些类型的事件、各多少次（标签是 kind / name / rating）
mc_frontend_events_total{kind="js_error"}

#    LCP 的分布（标签只有 metric，rating 在 events_total 上）
mc_frontend_vital_value{metric="LCP"}

#    等级统计只能从 events_total 拿
mc_frontend_events_total{kind="web_vital", name="LCP", rating="poor"}
```

```bash
# 2) 单条明细 —— 网关日志里有，带 trace_id
docker logs -f mc-gateway 2>&1 | grep telemetry
```

> `kind` 是**白名单枚举**：`page_view` / `web_vital` / `js_error` / `api_error` / `resource_error` / `unhandled`。不接受任意值，否则谁都能构造请求把指标基数打爆。

### 一次前后端串联排查的完整流程

1. 用户说「点保存没反应」。
2. 先看 Sentry 有没有 JS 异常 → 有堆栈就直接定位。
3. 没有异常就查 beacon 指标 → `mc_frontend_events_total{kind="js_error"}` 有没有涨。
4. 还没有，说明前端没崩，是**请求本身失败** → 拿用户说的操作时间点，去网关日志里按时间翻，找 `status` 非 2xx 的 `http_request`。
5. 找到那条日志的 `trace_id`，`grep` 同一个 `trace_id` → 能看到这次请求在网关内部经过的每一层。
6. 最后用快照方法，确认那次请求到底有没有真的写库。

**`trace_id` 是唯一能把三次跳转串起来的东西**：浏览器 → 网关 → 下游。日志里没有它，上面第 4 步就断了。

### 本地开发看前端监控，就看 Grafana

**一句话：本地看 Grafana，线上查问题看 Sentry。**

| | Grafana | Sentry |
|---|---|---|
| 在哪 | `http://localhost:13000` | `https://<组织>.sentry.io/issues/?project=<项目ID>` |
| 本地能用吗 | **能，实时** | 能，但有上报延迟 + 要联网 |
| 有什么 | 错误**趋势**、PV、Web Vitals、错误率 | 错误**堆栈**、面包屑、设备 |
| 缺什么 | 没有堆栈，不知道错在第几行 | 没有全站聚合，看不到「多少人受影响」 |

**打开位置**：`http://localhost:13000/d/mc-console-overview`，往下滚到 **「前端体验」** 这一组，四个面板：

| 面板 | 回答什么 | 正常值 |
|---|---|---|
| **Web Vitals 良好率** | LCP/FCP/TTFB 有多少比例达到 Google 判定线 | 三条线都在 0.75 以上 |
| **Web Vitals p75** | 那 25% 最慢的用户等了多久 | LCP < 2500ms、FCP < 1800ms、TTFB < 800ms |
| **PV（按页面）** | 哪个页面被看得最多 | 按你的操作应该能对上 |
| **前端事件（错误/上报）** | JS 错误、接口错误、PV 各自的速率 | `js_error` 应该接近 0 |

再往下还有**「前端错误率（JS 错误 / PV）」**，这才是最该盯的一个数：**错误数除以 PV，得到「每看一次页面会错几次」**。绝对值涨不一定是坏事（可能只是流量涨了），比率涨才是真出问题。

**本地 Grafana 看不到什么（要清楚这些边界）**

| 想看 | Grafana 能给吗 | 该去哪 |
|---|---|---|
| 错误堆栈、第几行 | 不能 | Sentry Issues |
| 报错前用户点了什么 | 不能 | Sentry Breadcrumbs |
| 具体某次请求的完整经过 | 不能 | 网关日志，按 `trace_id` grep |
| 数据最后有没有写进库 | 不能 | 快照 diff |
| 主机 CPU/内存 | 不能（本地没装 node-exporter） | 看面板上的说明 |

### 验证数据链路有没有通

三步，任何一步断了下游都是空的：

```bash
# 1) 前端有没有在发？打开页面后用 F12 → Network 搜 beacon，应该看到 POST /api/telemetry/beacon
# 2) 网关有没有收到并记成指标？
curl -s http://127.0.0.1:8080/metrics | grep mc_frontend_events_total
# 3) Prometheus 有没有抓到？
curl -s 'http://127.0.0.1:19090/api/v1/query?query=mc_frontend_events_total' | head -c 500
```

第 2 步有、第 3 步没有 → 抓取配置或目标 DOWN；第 1 步有、第 2 步没有 → 检查 beacon 是否被限流（`MC_TELEMETRY_RPS`）或白名单拒绝。

### 两个必须知道的时序特性

1. **LCP 是「页面卸载或切到后台」时才上报最终值**。刚打开页面时 LCP 没有数据，FCP/TTFB 会先到。**「FCP 有、LCP 没有」是正常的。**
2. **PV 只在 pathname 变化时记一条**。翻页、筛选（只改 query）不算新页面，否则一次翻页就虚增一个 PV。

### Sentry 的每一页分别靠什么喂数据

打开 Sentry 控制台，左侧有一排导航：Issues / Traces / Logs / Metrics / Profiles / Replays …

**每一页是空的还是满的，取决于 SDK 里有没有对应的那一小段代码。** 版本对了不等于有数据 ——
这是最容易误判的地方：升级完 SDK、日志页面仍然显示「Set up the SDK」，看的人会以为没配好。

| 页面 | 靠什么产生数据 | 本项目状态 | 配置在哪 |
|---|---|---|---|
| **Issues** | `Sentry.init` + `ErrorBoundary` 自动捕获 | ✅ | `web/src/lib/monitor.ts` |
| **Traces** | `browserTracingIntegration()` + `tracesSampler` | ✅ 默认全采（可用 env 下调） | 同上 |
| **Logs** | 显式调用 `Sentry.logger.*` | ✅ 走 `logEvent()` | 同上 |
| **Metrics** | 显式调用 `Sentry.metrics.count/gauge/distribution` | ✅ 走 `countMetric()` / `distributionMetric()` | 同上 |
| **Profiles** | `browserProfilingIntegration()` + `Document-Policy` 响应头 | ✅ 凡被采样的 trace 都附一份 profile（默认全采，即每次加载都有） | monitor.ts + vite.config.ts + nginx |

**各自发什么：**

| 时机 | Logs | Metrics | 说明 |
|---|---|---|---|
| 应用启动 | `app_started`（info） | `app_start`（counter） | 让 Logs/Metrics 页立刻脱离「Set up」状态，也能看出这次会话是哪个版本 |
| 路由切换 | `page_view`（debug） | — | 用 debug 级，避免把默认视图刷屏；要 PV 数去看 Prometheus 的 `mc_frontend_events_total` |
| 接口失败 | `api_error`（warn / error） | `api_error`（counter） | 5xx 记 error，4xx 记 warn |
| Web Vitals | 面包屑 | `web_vital`（distribution） | Prometheus 那边只有分档计数，这里能看真实分位数的 p75/p95 |

**为什么不用 `consoleLoggingIntegration`。** v10 提供了一个集成，把 `console.*` 全量转发到 Sentry。
看着省事，但后台系统的 console 里混着订单号、用户标识、接口返回体，转发等于把这些原样送进
第三方 SaaS —— 和 `scrubUrl` / `scrubHeaders` 守住的那条底线直接冲突。

所以改成**显式打点 + 按构造脱敏**：只有 `logEvent()` / `countMetric()` 会发，属性类型被收窄成
`string | number | boolean`，调用方在类型层面就没法塞进去一个对象或一整个接口返回体。
想加敏感字段必须自己显式写上来 —— 而不是「不小心带出去了才发现」。

**Profiles 有一个额外的坑：`Document-Policy: js-profiling`。**
Chrome 的 JS Self-Profiling API 受响应头门控。没有这个头，浏览器直接拒绝采集样本，
而 Sentry SDK **不报错**——表现就是 Profiles 页永远空白，看起来像集成没生效。
开发环境在 `vite.config.ts` 的 `server.headers`，生产在 `deploy/nginx/conf.d/site.conf`，两处必须都有。

**Profiles 的第二个坑，是采样率会被「乘」起来。**
`profileLifecycle: 'trace'` 的语义是「把 profile 挂在 trace 上」，所以一个会话要被剖析，必须**同时**满足两件事：

1. 这个 profile 会话被采样，概率 = `profileSessionSampleRate`；
2. 它所属的那次根 trace 恰好被追踪采样，概率 = `tracesSampler` 的返回值。

真实剖析率是两者**相乘**。曾经踩过的坑：两个都填 0.1，实际只有 **0.1 × 0.1 = 1%** ——
表现就是 Profiles 页几乎永远是空的、几十次刷新偶尔冒一条，而且排查时很难联想到是乘法。

本项目的取法：`profileSessionSampleRate` 默认 **1.0**（`VITE_PROFILE_SAMPLE_RATE`），
`tracesSampler` 默认 **1.0**（`VITE_TRACES_SAMPLE_RATE`）。含义是「每次被追踪的加载都附一份 profile」，
低流量的内部后台下就等于「每次加载都有」。**如果为了省配额把 `VITE_TRACES_SAMPLE_RATE` 调低，
要记得 profile 率会跟着一起降**（例：降成 0.1，实际剖析率就是 0.1 × 1.0 = 10%，而不是 10% 里的 100%）。

**对真实 Sentry 项目的验证**（用仓库里的真实 DSN、Playwright 观察发往 `*.sentry.io` 的请求，
只统计 envelope 的 item 类型与 HTTP 状态，不读取上报内容）：

```
一次页面加载 → envelope item = session / transaction / profile_chunk / log / trace_metric
所有上报请求 HTTP 200
```

`log` / `trace_metric` 到 Sentry 后，Logs / Metrics 页立刻脱离「Set up」状态；
`transaction` / `profile_chunk` 喂 Traces / Profiles。这证明真实项目「确实收下了这三类数据」，
而不只是「本地 SDK 能拼出 envelope」。

### 从 v7 升到 v10 实际改了什么

原来的版本是 `"@sentry/react": "^7.118.0"`（解析到 7.120.4）。升级分两部分：**换依赖**和**补接入**。

**第一部分：换依赖。** 代码里原有那套（`browserTracingIntegration()`、`ErrorBoundary` 的 `fallback`
收函数）本来就是 v8 之后的写法，不是 v7 的老 API，所以这部分零改动：

```json
- "@sentry/react": "^7.118.0",
+ "@sentry/react": "^10.78.0",
```

逐条核对 v8 / v9 / v10 的破坏性变更，**与原有代码相关的**只有这些：

| 版本 | 变更 | 对本项目的影响 |
|---|---|---|
| v8 | 集成改用 `browserTracingIntegration()`；`ErrorBoundary` 的 `fallback` 收函数 | 代码本来就是这个写法，无改动 |
| v9 | 移除 `enableTracing`、`getCurrentHub()` / `Hub`、`captureUserFeedback`、旧 metrics API | 都没用到 |
| v9 | 默认不再采集 IP（需显式 `sendDefaultPii: true`） | 与「不存 IP」原则一致，保持默认 |
| v10 | 底层升级到 OpenTelemetry v2 | 浏览器端行为不变 |
| v10 | 移除 FID 指标 | 本项目用 LCP / CLS / INP / FCP / TTFB，不涉及 |

**第二部分：补接入。** 光换版本，Logs / Metrics / Profiles 三页仍然是空的（第一张表已经说明了原因）。
所以新增了：

- `logEvent()` —— 显式打点，属性只收标量；
- `countMetric()` / `distributionMetric()` —— 计数器与分位数；
- `browserProfilingIntegration()` + `profileSessionSampleRate` + `profileLifecycle: 'trace'`；
- `enableLogs: true`（10.71 起默认就是 true，显式写出来是为了让排查时不用去猜版本行为）；
- `Document-Policy: js-profiling`（vite dev + nginx prod 两处）；
- 两个采样率做成环境变量：`VITE_TRACES_SAMPLE_RATE`（默认 **1.0**，低流量内部后台全采；
  高流量时下调）与 `VITE_PROFILE_SAMPLE_RATE`（默认 1.0）。**注意两者在 `profileLifecycle: 'trace'`
  下是相乘关系**（见上一节），profile 率保持 1.0 才能让「被采样的 trace 都有 profile」，
  否则两个都调成 0.1 一乘就只剩 1%；

**验证不是「类型检查过了」就算数，也不是「页面没报错」就算数。**
实际做的是：起一个带假 DSN 的 dev server（`http://abc123@127.0.0.1:18999/1`），用 Playwright
拦截上报，然后**把 envelope 拆开看里面有哪些 item 类型** —— 每种类型对应一个页面：

```
Document-Policy = "js-profiling"
item 类型统计 = {"session":3,"transaction":1,"profile_chunk":1,"log":1,"trace_metric":1}
  log body = page_view    | level = debug
  log body = app_started  | level = info
  metric = app_start   | type = counter      | value = 1
  metric = web_vital   | type = distribution | value = 163
  metric = web_vital   | type = distribution | value = 712

Logs     : ✓ 有 log item          → Logs 页
Metrics  : ✓ 有 metric item       → Metrics 页
Profiles : ✓ 有 profile item      → Profiles 页
Document-Policy: ✓ js-profiling
```

`log` / `trace_metric` / `profile_chunk` 三种 item 分别对应 Logs / Metrics / Profiles 三页，
所以这条输出同时证明了「三页都会真的有数据」，而不只是「SDK 能初始化」。
DSN 指向 `127.0.0.1`，验证数据没出本机。

> **当初给项目定版本时**，直接写了 `^7.x` 而没有先核对 Sentry 当前主流是哪个大版本，这是个疏漏。
> 后来核对完才发现升级面远小于「跨三个大版本」这个说法给人的印象——上面那张表就是全部改动面。

### 源码映射（sourcemap）：异常堆栈能不能落到源码行

生产包会被打包压缩：多文件合成一两个 chunk、变量名缩短、换行去掉。这时报错只能给到
「`index-BkcrGFRS.js:1:48213`」这种压缩后坐标，**没有 sourcemap 就等于定位不到自己写的哪一行**。
所以 README 里「Sentry 能告诉你哪个文件哪一行」这句话，前提就是这份映射存在。

**怎么接的**：用官方的 `@sentry/vite-plugin`，在 `vite build` 阶段上传 sourcemap 到 Sentry、
并建对应 release 的 artifact。配置在 `web/vite.config.ts`。

**三条安全约束（缺一条源码就会泄露）**：

1. **`build.sourcemap: 'hidden'`**：照常生成 `.map` 给上传用，但 bundle 里**不写 `sourceMappingURL`**。
   普通的 `sourcemap: true` 会让浏览器 devtools 顺着引用把完整源码下下来 —— 等于公开源码。
2. **上传后删除**：插件配了 `filesToDeleteAfterUpload: ['dist/**/*.map']`，传完即删，产物里不留 map。
3. **没有 token 就根本不生成 map**：`sourcemap: authToken ? 'hidden' : false`。
   没配 token 的人本地 `pnpm build` 产出的 `dist` 里是 0 个 `.map`，源码不会随部署泄露。

**token 怎么给（红线）**：`SENTRY_AUTH_TOKEN` 是带写权限的私钥，**只从环境变量读，绝不进仓库**。
本项目支持两种来源：直接给 `SENTRY_AUTH_TOKEN`，或给 `SENTRY_AUTH_TOKEN_FILE` 指向一个仓库外的密钥文件
（`vite.config.ts` 在本进程内读取，不打印）。例如：

```bash
SENTRY_AUTH_TOKEN_FILE="D:/path/to/sentryauthtoken.txt" pnpm build
```

没配这两个变量时，构建会**静默跳过上传**，本地照常出包。

**release 必须对得上**：插件上传时用的 release 名和运行时 `monitor.ts` 里的 `RELEASE` 由
`vite.config.ts` 统一算一次，再写回 `process.env` 供两者共用，所以两边天然一致。

取值优先级：

1. 命令行/CI 显式注入的 `VITE_APP_VERSION`（本条最高优先）
2. 自动生成 `<package.json 的 version>+<git 短提交号>`，例如 `1.0.0+1502e67`
3. 拿不到 git（如 Docker 构建没带 `.git`）时退化成纯版本号

注意：`.env.*` 文件里的 `VITE_APP_VERSION` **不再生效**——那是写死的常量，发布多少次都叫
同一个名字，Sentry 里就没法回答「这个错误是哪次发布引入的」。正式发包建议不传，让自动值
生效即可。

**验证**（不是「配置写完了」就算数）：带 token 跑一次 `pnpm build`，看两件事：

```
[sentry-vite-plugin] Info: Successfully uploaded source maps to Sentry
dist 里 *.map 数量 = 0        # 上传后已删除
```

**为空 DSN 的构建会连 SDK 一起摇掉**：`import.meta.env.VITE_SENTRY_DSN` 是构建期常量，`monitor.ts` 里 `if (DSN)` 在 DSN 为空时会被压缩器直接判死，整块 Sentry SDK 随之被 tree-shaking 移除。实测同一个构建，index chunk 从 218KB 降到 69KB。好处是没配监控的包不带多余体积；代价是这份包**完全不上报**，所以正式环境必须在构建时注入 DSN。

### SDK、SaaS、控制台是三样东西

| 名字 | 是什么 | 在哪 | 本项目里对应 |
|---|---|---|---|
| **SDK** | 一段**前端代码**，塞进你的 bundle，负责收集数据并往外发 | `web/src/lib/monitor.ts` | `@sentry/react` + `web-vitals` |
| **SaaS** | **第三方公司的服务器**，接收、存储、去重、聚合这些数据 | Sentry 官方机房 | 你配的 `VITE_SENTRY_DSN` 指向它 |
| **控制台** | SaaS 提供的**网页界面**，用来看数据 | 浏览器里一个网址 | Sentry 官网的 dashboard |

**三者缺一不可**：只有 SDK 没 SaaS，数据发出去没地方存；只有 SaaS 没 SDK，什么数据都收不到；有前两个没打开控制台，你只是没去看。

**所以「Sentry 是 SDK 吗」——既是也不是。** `@sentry/react` 是 SDK；Sentry 这个产品是 SDK + SaaS + 控制台的整体。

**「前端是不是得引入埋点 SDK」——本项目已经引入了**，见 `web/package.json`：

```json
"@sentry/react": "^10.78.0",
"web-vitals": "^4.2.3"
```

`@sentry/react` 抓异常和性能，`web-vitals` 抓核心体验指标（LCP/CLS/INP/FCP/TTFB）。**不需要再装别的。**

> **想自建、不用 SaaS 行不行？** 可以，代价是自己在服务器上部署 Sentry 的整套服务（Postgres + Redis + Kafka + ClickHouse + 几个 worker，内存 8G 起）。对一个后台管理系统不值得。

### 前端可视化监控 UI 的 url

**这个项目里没有。** 必须是 Sentry 官网的控制台，网址形如：

```
https://<你的组织>.sentry.io/issues/?project=<项目ID>
```

`<你的组织>` 和 `<项目ID>` 在 Sentry 后台里能看到。**DSN 里也带着这两个信息**，但那属于凭据文件，不该被完整读取。

**怎么自己找到它**：

1. 浏览器打开 https://sentry.io 并登录。
2. 左侧 **Projects** → 找到本项目的 project。
3. 点进去就是 Issues 列表，地址栏里那串 URL 就是控制台入口。

### PV / UV / LCP / FCP / CLS / INP / TTFB 分别在哪看

先把这些缩写拆开：

| 缩写 | 全称 | 是什么 | 好坏标准 |
|---|---|---|---|
| **PV** | Page View | **页面浏览次数**。同一个用户刷新 5 次算 5 次 | 越多越好，看趋势 |
| **UV** | Unique Visitor | **独立访客数**。同一个人 1 天内多次访问算 1 个 | 看真实用户规模 |
| **LCP** | Largest Contentful Paint | 最大内容块画完的时间。**「主要内容多久能看到」** | < 2.5s 良好 |
| **FCP** | First Contentful Paint | 第一块内容画出来的时间。**「多久不是白屏」** | < 1.8s 良好 |
| **CLS** | Cumulative Layout Shift | 累积布局偏移。**「页面元素有没有乱跳」** | < 0.1 良好 |
| **INP** | Interaction to Next Paint | 交互到下一帧的时间。**「点了多久有反应」** | < 200ms 良好 |
| **TTFB** | Time To First Byte | 首字节时间。**「服务器多久开始回数据」** | < 800ms 良好 |

**LCP vs FCP 的区别**：FCP 是「开始有东西了」，LCP 是「主要内容齐了」。一个页面可能 0.3s 就出了骨架（FCP 好），但大图 4s 才加载完（LCP 差）。**用户感知的是 LCP。**

**去哪看，一张表：**

| 指标 | 在哪看 | 具体怎么查 |
|---|---|---|
| **Web Vitals（LCP/FCP/CLS/INP/TTFB）** | **Grafana** | 「前端体验」组 → **Web Vitals 良好率** |
| **Web Vitals（带页面对比）** | **Sentry** | Performance → Web Vitals → 按页面排序 |
| **PV（按页面）** | **Grafana** | 见下方 PromQL |
| **PV（总数）** | **Grafana** | 同上，去掉 `by (path)` |
| **UV** | ⚠️ **本项目算不了准确值** | 见下面「关于 UV」 |
| **JS 错误** | **Sentry**（有堆栈）+ Grafana（有趋势） | Issues 页 / 「前端事件」面板 |

**PV 的 PromQL**：

```promql
# 总 PV（每分钟）
sum(rate(mc_frontend_events_total{kind="page_view"}[1m])) * 60

# 各页面的 PV，按页面拆
sum by (path) (rate(mc_frontend_events_total{kind="page_view"}[5m])) * 60

# 某个页面今天一共被看了多少次
increase(mc_frontend_events_total{kind="page_view", path="/catalog/spu"}[1d])
```

**Web Vitals 的 PromQL**：

> ★ 桶边界在 Prometheus 里是**浮点字符串**：`2500` 会序列化成 `le="2500.0"`，
> 写 `le="2500"` 匹配不到，分位数会静默变成 0。

```promql
# LCP 的良好率（低于 2.5s 的比例）
clamp_max(
  sum(increase(mc_frontend_vital_value_bucket{metric="LCP", le="2500.0"}[$__rate_interval]))
  /
  clamp_min(sum(increase(mc_frontend_vital_value_bucket{metric="LCP", le="+Inf"}[$__rate_interval])), 1)
, 1)

# LCP 的 p75（Google 用的是 p75 不是平均值）
histogram_quantile(0.75, sum by (le) (rate(mc_frontend_vital_value_bucket{metric="LCP"}[15m])))

# 把五个指标一起看
sum by (metric) (rate(mc_frontend_vital_value_bucket{le="+Inf"}[5m]))
```

**关于 UV —— 必须说实话**

**这个项目算不出准确的 UV**，原因是**刻意不采集**：

- UV 要按「人」去重，最少需要 IP、cookie 或设备指纹。
- 本项目**不存 IP**（日志里连 query 都丢掉了，见 `safePath()`），也不生成设备指纹。
- 理由是：这是一套后台管理系统，用户本来就是少数几个运营人员，UV 的统计价值远低于它带来的隐私负担和合规风险。

**能拿到的近似值**（都只是近似，别当成 UV 用）：

| 近似方式 | 怎么做 | 偏差 |
|---|---|---|
| 按 `page_view` 的独立 5 分钟窗口估算 | `count(sum by (path) (increase(...[5m])) > 0)` | 同日多次访问会重复计 |
| 看 Sentry 的 **Users** 维度 | Sentry 自己有会话 ID 去重 | 只统计报错的用户，样本有偏 |
| 看 nginx access log 的 IP（生产） | `awk '{print $1}' \| sort -u \| wc -l` | 要额外做隐私评估 |

> **真要有可靠 UV，得在后端加一层「会话表」**：登录后发一个不透明 session id，按 session 去重。这需要改表和改登录流程，本项目没做——**这是明确的边界，不是遗漏**。

### 一个容易踩的坑：LCP 可能在 Grafana 里一直没数据

`mc_frontend_vital_value` 是 Histogram，**只有用户真实访问过页面才会产生数据点**。本地开发时你自己开着页面，数据会来；但如果没打开过前端，Grafana 那块就是空的。

另外 `web-vitals` 的 `onLCP` 在**页面卸载或切到后台时**才报告最终值，所以：
- 刚打开页面后马上看 Grafana，LCP 可能还没上报。
- 需要停留几秒再切走，或者刷新一次触发。

**FCP/TTFB 会早得多**——它们在页面加载早期就触发了，所以「FCP 有数据、LCP 没有」是正常的时序现象，不是 bug。

---

## 八、四个地方各管一段：完整排查手册

这是最常被问的问题。答案是**四个地方，各管一段**，不能互相替代：

```
用户点了一下
   │
   ├─[1] 浏览器里发生了什么     → Sentry（异常 + 堆栈）
   │                            → beacon 指标（错误率 + Web Vitals）
   │
   ├─[2] 请求发出去了吗         → 浏览器 F12 Network
   │
   ├─[3] 后端处理得快不快       → Prometheus / Grafana（QPS、延迟、错误率）
   │
   └─[4] 后端内部哪一步慢了     → 网关结构化日志（按 trace_id 串）
```

### 一表速查

| 你想知道 | 去哪看 | 具体位置 |
|---|---|---|
| 前端**有没有报错**（哪一行） | Sentry | Issues → 点进某条 → 堆栈 |
| 前端**报错多不多**（趋势） | Grafana | 「前端体验」组 → 前端事件 |
| 前端**首屏快不快** | Grafana | 「前端体验」组 → Web Vitals 良好率 |
| 前端**哪个页面慢** | Sentry 或 beacon | Sentry Performance / `mc_frontend_vital_value{metric="LCP"}` |
| 后端**每秒多少请求** | Grafana | 「RED」组 → 请求速率 |
| 后端**错误率** | Grafana | 「RED」组 → 错误率（5xx） |
| 后端**慢不慢** | Grafana | 「RED」组 → P99 延迟 |
| 后端**哪个接口慢** | Grafana | 「请求速率（按路由）」+ 顶部 `route` 下拉框筛 |
| 后端**现在是不是被压满了** | Grafana | 「RED」组 → 在途请求 |
| 后端**某一次请求到底怎么了** | 网关日志 | 按 `trace_id` grep |
| 后端**有没有在限流** | Grafana | 「网关」组 → 被限流 & 鉴权失败 |
| **数据到底写没写进库** | 快照 diff | `scripts/snapshot.mjs` |

### 后端：Grafana 看全貌，日志看个案

**Grafana 总览面板**分五组，从上往下就是排查顺序：

| 分组 | 面板 | 先看哪个 |
|---|---|---|
| **RED** | 请求速率 / 错误率(5xx) / P99 延迟 / 在途请求 | **先看这四个**。错误率不 0 或 P99 飙高，问题就在这组 |
| **网关** | 被限流 & 鉴权失败 / BFF 聚合耗时与降级 | 用户说「有时候数据缺一块」→ 看 BFF 降级 |
| **缓存与数据源** | 缓存命中率 / singleflight 合并 / 主机资源 | 数据库压力大 → 先看命中率是不是掉了 |
| **前端体验** | Web Vitals 良好率 / 前端事件 | 用户说「页面卡」→ 先排除前端 |
| **消息与一致性** | 发件箱积压 / 消息消费与锁健康度 | 数据不同步 → 看这组 |

**为什么要看 P99 而不是平均值**：

100 个请求，99 个 10ms、1 个 5 秒。平均 = 60ms，看起来很正常；但**那 1 个用户等了 5 秒**。P99 是 5000ms，一眼就能看出问题。

**为什么 In-Flight 重要**：它是「此刻正在处理」的数量。如果它持续上涨不回落，说明请求进来得比处理得快——下一步就是超时和雪崩。**这是崩溃前的最后一个信号。**

**日志怎么补上「哪一步慢」**：

```bash
# 找到那条慢请求的 trace_id
docker logs mc-gateway 2>&1 | grep '"route":"/api/bff/catalog/overview"' | tail -20

# 用 trace_id 把这次请求过程中所有日志串起来
docker logs mc-gateway 2>&1 | grep '<刚才那个 trace_id>'
```

日志里每条都带 `trace_id`，所以能还原「这一次请求在网关内部经过了哪几步、各花多久」。

### 一次完整排查的例子

**用户说：「工作台有时候是空的」。**

1. **Grafana → 网关组 → BFF 聚合：耗时与降级**
   → 看到 `mc_aggregate_degraded_total{aggregate="catalog", block="pricing"}` 在涨。
   说明：聚合里 `pricing` 这个块在失败，bundle 返回了残缺数据。

2. **Grafana → RED 组 → 错误率**
   → 5xx 是 0。说明不是整个接口挂了，只是某个子查询。

3. **Grafana → 按路由的请求速率**
   → 顶部 `route` 下拉选 `/api/bff/catalog/overview`，看这条路由的延迟是否同时变高。

4. **日志定位**
   ```bash
   docker logs mc-gateway 2>&1 | grep 'aggregate_degraded'
   ```
   找到具体的 `block`、错误信息和 `trace_id`。

5. **确认数据层**
   如果怀疑是数据库超时，用快照 diff 对比一下当时的表行数有没有异常变化。

**这个流程的意义**：每一步都排除一种可能，而不是靠猜。**指标先告诉你「有没有问题」，日志再告诉你「问题在哪一步」，快照最后告诉你「数据有没有真的受影响」。**

### 一句话记住分工

> **Sentry 管「谁、在哪一行、怎么错的」；beacon / Grafana 管「有多少、多快、从什么时候开始的」；日志管「这一次请求里具体发生了什么」；快照管「数据最后有没有变」。**

四个都缺一块。只看 Sentry 不知道影响面，只看 Grafana 不知道堆栈，只看日志不能聚合，只看快照不知道用户感受。


---

---

## 九、这套方案没做的事（明确的边界）

- **没有分布式追踪（OpenTelemetry）**。当前规模（单实例、6 个领域服务在一个进程里）
  用 `trace_id` + 日志足够；引入 OTel 的收益要等多服务拆分之后才显现。
- **没有日志聚合（Loki / ELK）**。现在看日志靠 `docker compose logs`。
  上多副本时需要 Loki，但那是规模问题，不是设计缺陷。
- **没有 SLO / 错误预算**。要真正做，得先把"哪些路由算关键路径"定下来 ——
  这个决定需要业务方参与，不是监控能自己定的。
- **前端 beacon 没有做采样**。全量计数是为了算准确的比率；
  如果量级涨到每天百万级，要改成按 session 采样，否则会成为负担。