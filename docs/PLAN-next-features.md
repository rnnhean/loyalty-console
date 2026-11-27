# loyalty-console 功能补充计划（基于原版脱敏）

> 来源：`d/projects/vip_ai_member`（公司真实仓库，**只提取架构模式，不复制任何代码、业务字段、数据**）。
> 目标：给 loyalty-console 补上原版里有、当前缺的能力，同时把所有命名、数据、模块做脱敏。

---

## 一、原版能力盘点 → 脱敏映射

| 原版模块（不对外出现） | 实际职责 | 脱敏后命名 | 是否补 |
|---|---|---|---|
| `membercloudrule` | 可配置规则 + gRPC 执行 | `rule`（规则引擎） | ✅ |
| `ordercrontab` | MySQL 作业表 + 定时执行 | `scheduler`（定时任务） | ✅ |
| `order_decrypt` | 敏感字段解密/展示控制 | `masking`（字段脱敏） | ✅ |
| `member_data/radar_assembler` | 多源聚合到看板 | `insight`（指标快照） | ⭕ 可选 |
| `member_data/err_log` | 错误日志采集 | 已有 Sentry + Prometheus | ❌ 不重复造 |
| `member_data/prometheus` | 指标暴露 | 已有 | ❌ |
| `exquisite/engine` | 活动引擎 | 合并进 `rule` | — |
| `exquisite/retelimit` | 限流 | 已有 gateway 限流 | ❌ |
| `exquisite/aesjsoncrypt` | 报文加密 | 合并进 `masking` 的加解密 | — |
| `member_cashier` | 收银台 | 交易域已有 | ❌ |
| `eagleeye` | 全链路追踪 | 已有 Langfuse/Prometheus | ❌ |
| `benchmark` | 压测 | 已有 `scripts/load-agent.mjs` | ❌ |

**脱敏规则（强制）**：
- 不出现任何真实业务名词（会员/订单/支付领域的具体叫法一律换成通用词）
- 变量名、方法名、模块名、目录名、表名、字段名全部重命名
- 示例数据用虚构品牌（沿用现有的「星澜科技」体系），不出现任何真实 ID / 手机号 / 订单号
- 不复制原版注释、注释里的业务规则描述也要重写

---

## 二、要补的三块

### 1. 规则引擎 `server/internal/rule/`

**解决什么**：现在诊断规则（`/api/diag/checks`）全是硬编码的 8 条。
运营改一条判断条件要发版——这是真实运营系统的痛点。

**设计**
```
rule/
  model.go      规则/规则集定义（条件树 + 动作）
  dsl.go        条件 DSL 解析与校验（闭集，防注入）
  engine.go     求值器（AND/OR/NOT + 比较运算）
  store.go      规则持久化（MySQL）+ 版本快照
  service.go    增删改查 + 试运行（dry-run）
```
- 条件 DSL：`{"all":[{"field":"tier.state","op":"eq","value":1}]}`
- 算子闭集：`eq/ne/gt/gte/lt/lte/in/nin/exists`
- **dry-run**：给定样例数据先跑一遍，看命中哪些规则，不落库
- 规则变更留版本快照，可回滚

**与 Agent 联动（关键，补 agent 关键词）**
- 新增工具 `explain_rule`：Agent 可以解释「为什么某条规则没命中」
- 新增工具 `simulate_rule`：给定输入试跑规则，返回命中链路

**前端**：`/operation/rules` 列表 + 编辑器 + 试运行面板

---

### 2. 定时任务 `server/internal/scheduler/`

**解决什么**：现在所有计算都是请求驱动的。真实系统必有定时对账 / 清理 / 快照。

**设计**
```
scheduler/
  job.go        作业定义（cron 表达式、超时、并发策略）
  store.go      作业表 + 执行记录表
  runner.go     调度循环 + 分布式锁（复用现有 lock 包）
  handlers.go   手动触发 / 暂停 / 恢复
```
- 基于 **DB 轮询 + 分布式锁**（不引外部调度框架，符合项目风格）
- 三种并发策略：`allow`（并发允许）/ `skip`（跳过）/ `queue`（排队）
- 执行记录：开始/结束/耗时/结果/错误，保留最近 N 次
- **失败重试**：指数退避，最多 3 次

**前端**：`/setting/jobs` 列表 + 执行历史 + 手动触发按钮

---

### 3. 字段脱敏 `server/internal/masking/`

**解决什么**：运营台会展示用户手机号、证件号。现在直接返回明文，导出更危险。

**设计**
```
masking/
  policy.go     字段策略（手机号/证件/邮箱/自定义正则）
  engine.go     按用户权限决定「看明文 / 看掩码 / 看不到」
  audit.go      谁在什么时候看了明文（审计日志）
  crypt.go      对称加密（存储时加密、展示时按权限解密）
```
- 策略可配置：`138****5678` / `13800005678` / 完全不返回
- **导出也脱敏**（容易被忽略的泄漏点）
- 明文访问必须带原因，写审计表

**前端**：`/setting/masking` 策略配置页；其他页面按策略自动生效

---

## 三、执行顺序（一次只做一个，边做边测）

| 阶段 | 内容 | 验证方式 |
|---|---|---|
| 1 | 规则引擎后端 + 单测 | Go 单测 + dry-run 接口 |
| 2 | 规则引擎前端页 | 有头浏览器 CRUD + 试运行 |
| 3 | Agent 两个新工具 | 真实 LLM 问「这条规则为什么没命中」 |
| 4 | 定时任务后端 | 单测 + 手动触发 + 锁并发验证 |
| 5 | 定时任务前端 | 有头浏览器触发 + 看历史 |
| 6 | 脱敏后端 + 单测 | 单测 + curl 验证脱敏生效 |
| 7 | 脱敏前端 | 有头浏览器验证列表/导出的脱敏 |
| 8 | 三层监控复验 | Prometheus 新指标 / Grafana 新面板 / Langfuse 新工具 span |
| 9 | 文档 | README + INTERVIEW 补对应问答 |
| 10 | 提交推送 | 按文件名 add，凭据扫描后 push |

---

## 四、新指标（Prometheus）

| 指标 | 说明 |
|---|---|
| `mc_rule_eval_total{result}` | 规则求值次数与结果 |
| `mc_rule_eval_duration_seconds` | 求值耗时 |
| `mc_job_run_total{job,result}` | 定时作业执行结果 |
| `mc_job_duration_seconds{job}` | 作业耗时 |
| `mc_mask_applied_total{field}` | 脱敏命中次数 |
| `mc_mask_plaintext_access_total{field}` | 明文访问次数（审计用） |

---

## 五、风险与边界

- **不放真实业务规则**：示例规则全部是虚构的（如「连续 30 天未登录 → 打标」）
- **不引入外部队列/调度框架**：复用现有 MySQL + Redis + 现有 lock 包
- **每个模块必须有自己的单测**，且 dry-run 必须有反例测试（非法 DSL 要拒绝）
- **改动不破坏现有 22 个页面**：新页面挂在现有导航下，路由独立

---

## 六、待确认

- 三块是否全做？还是先做「规则引擎 + Agent 联动」（最能体现 agent 能力）？
- 是否要第四块「指标快照 `insight`」？
