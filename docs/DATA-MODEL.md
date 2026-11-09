# 数据模型

## 一、命名约定

| 对象 | 规则 | 例子 |
|---|---|---|
| 表名 | `<域前缀>_<实体>`，全小写蛇形 | `cat_spu_archive`、`bft_tier_profile` |
| 主键 | `<实体>_uid` BIGINT AUTO_INCREMENT | `spu_uid`、`tier_uid` |
| 业务编号 | `<实体>_no` VARCHAR | `spu_no` |
| 时间字段 | 动词过去式 | `created_at`、`updated_at` |
| 逻辑删除 | `voided` TINYINT(1) | `WHERE voided = 0` |
| 状态 | `lifecycle_state` / `online_state` | 不用 `status` 这种一词多义的字段 |

域前缀共 7 个：

| 前缀 | 含义 | 页面 |
|---|---|---|
| `cat` | 商品（catalog） | SPU / SKU / 售卖卡 / 定价策略 |
| `bft` | 权益（benefit） | 会员等级 / 权益包 / 权益规则 / 任务阈值 |
| `trd` | 交易（trade） | 结算配置 / 交易流水 / 促销 / 代扣模板 |
| `chn` | 渠道（channel） | 商户档案 / 分销节点 / 归因记录 |
| `ops` | 运营（operation） | 投放计划 / 券批次 / 推广位 |
| `cfg` | 配置（setting） | 产品目录 / 字典 / 集成模块 / 特性开关 |
| `msg` | 消息（messaging） | 不对应页面，是可靠投递的内部表 |

## 二、表清单

| 表名 | 说明 |
|---|---|
| `cat_spu_archive` | 商品档案（SPU） |
| `cat_sku_inventory` | 单品库存（SKU） |
| `cat_pricing_policy` | 定价策略 |
| `cat_offer_card` | 售卖卡 |
| `cat_policy_card_bind` | 定价策略与售卖卡的绑定关系 |
| `bft_tier_profile` | 会员等级 |
| `bft_entitlement_group` | 权益包 |
| `bft_entitlement_rule` | 权益规则 |
| `bft_task_threshold` | 任务阈值配置 |
| `trd_checkout_profile` | 结算（收银台）配置 |
| `trd_order_ledger` | 交易流水 |
| `trd_pay_channel` | 支付通道 |
| `trd_debit_template` | 代扣模板 |
| `trd_promo_rule` | 促销规则 |
| `trd_audit_trace` | 交易审计轨迹 |
| `chn_merchant_profile` | 商户档案 |
| `chn_distribution_node` | 分销节点 |
| `chn_attribution_record` | 归因记录 |
| `ops_campaign_plan` | 投放计划 |
| `ops_voucher_batch` | 券批次 |
| `ops_promotion_slot` | 推广位 |
| `cfg_product_catalog` | 产品目录 |
| `cfg_dictionary_entry` | 业务字典 |
| `cfg_integration_module` | 集成模块 |
| `cfg_feature_toggle` | 特性开关 |
| `msg_consumed_event` | 消费去重台账（事件 ID + 消费组） |
| `msg_outbox` | 发件箱（事务内登记、异步补发） |

## 三、几条设计取舍

**1. 为什么用 BIGINT 自增主键而不是 UUID**
运营后台单库单表，自增主键的聚簇索引写入是顺序的；UUID 散列会让 InnoDB 页分裂，
数据量上来后写入性能差一倍以上。UUID 的优势（分布式生成、不可枚举）用不上。

**2. 为什么逻辑删除而不是物理删除**
运营数据要能追溯"这条规则什么时候被谁下掉的"。`voided` 一列换来的可追溯性，
比省下那点存储值钱。代价是所有查询都必须带 `voided = 0`，靠仓储层统一兜。

**3. 为什么产品名作为分区键而不是外键**
`product_title` 是字符串，理论上该建一张产品表、用 ID 关联。但这里刻意没有：
不同业务表的产品取值体系互不相同（商品域、权益域各有各的枚举），
强行统一成一个外键表会制造"选了没数据"的问题。
**这是业务现实，不是设计偷懒** —— 所以每个页面的产品下拉框各自去自己的表查 DISTINCT。

**4. 状态为什么不统一成一套枚举**
`lifecycle_state`（生命周期）与 `online_state`（上下架）是两个正交的维度：
一条规则可以"已生效但未上架"。合成一个字段后，
每次都要在代码里解释"这个值到底代表什么"，不如一开始就分开。

**5. 为什么消息可靠性要靠两张表，而不是中间件的配置**
Kafka 和 RabbitMQ 都只能保证"至少投递一次"，跨系统的原子性它们给不了：

- `msg_outbox`：业务写库时把"要发的事件"一起写进同一个事务。
  库改了、消息没发出去，记录还在表里，补发协程会一直重试 ——
  解决的是**生产者侧**的丢失。
- `msg_consumed_event`：消费时先插 `(event_id, consumer_group)` 主键，
  和业务写入同一个事务。冲突说明处理过，整体回滚 ——
  解决的是**消费者侧**的重复。

两张表缺一不可：只有 outbox，重复投递会把业务做两遍；
只有去重台账，"库改了但消息没发"依然无人知晓。
这也是为什么 `msg_consumed_event` 的主键是**复合键** ——
同一个事件应该被每个消费组各处理一次，不能用一个组的进度挡住另一个组。

## 四、种子数据

`deploy/mysql/init/03-seed.sql` 提供约 3000 行业务数据，覆盖全部主表的典型形态。
设计上满足两个要求：

- **可复现**：每张表先 `DELETE` 再 `INSERT`，重跑结果一致；
- **有形状**：不是随机字符串，而是符合各域真实业务语义的记录
  （会员等级随排名递增、权益包关联到等级、投放计划带时间窗）。

重新生成：

```bash
# 清空并重建（会丢掉手改的数据）
docker compose -f deploy/docker-compose.prod.yml down -v
docker compose -f deploy/docker-compose.prod.yml up -d
```
