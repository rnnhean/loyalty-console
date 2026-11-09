-- ============================================================
-- mc_core —— 会员运营中台核心库
--
-- 表名与字段名遵循上表的域前缀约定，跨域引用一律走业务编号而非物理外键。
-- 命名约定：
--   表名 = <域前缀>_<实体>          域前缀：cat/bft/trd/chn/ops/cfg
--   字段 = 全小写蛇形，动词过去式（created_at/voided）
--   主键 = <实体>_uid（BIGINT），业务编号 = <实体>_no（VARCHAR）
-- ============================================================

CREATE DATABASE IF NOT EXISTS mc_core
  DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
USE mc_core;

-- ── 商品域 cat_ ─────────────────────────────────────────────

-- 商品标准单元
CREATE TABLE IF NOT EXISTS cat_spu_archive (
  spu_uid        BIGINT       NOT NULL AUTO_INCREMENT,
  spu_title      VARCHAR(128) NOT NULL COMMENT '商品标题',
  spu_kind       TINYINT      NOT NULL DEFAULT 0 COMMENT '商品类型',
  settle_cycle   INT          NOT NULL DEFAULT 0 COMMENT '结算周期(天)',
  retail_amount  DECIMAL(12,2) NOT NULL DEFAULT 0 COMMENT '标准零售价',
  lifecycle_state TINYINT     NOT NULL DEFAULT 1 COMMENT '1草稿 2在售 3停售',
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (spu_uid),
  KEY idx_spu_state (lifecycle_state, voided)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='商品标准单元档案';

-- 单品
CREATE TABLE IF NOT EXISTS cat_sku_inventory (
  sku_uid        BIGINT       NOT NULL AUTO_INCREMENT,
  spu_uid        BIGINT       NOT NULL DEFAULT 0,
  sku_label      VARCHAR(128) NOT NULL COMMENT '单品名称',
  spu_title      VARCHAR(128) NOT NULL DEFAULT '' COMMENT '冗余：所属商品',
  unit_amount    DECIMAL(12,2) NOT NULL DEFAULT 0 COMMENT '单价',
  billable_span  INT          NOT NULL DEFAULT 0 COMMENT '计费时长(天)',
  client_scope   TINYINT      NOT NULL DEFAULT 0 COMMENT '终端类型',
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (sku_uid),
  KEY idx_sku_spu (spu_uid),
  KEY idx_sku_scope (client_scope, lifecycle_state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='单品库存档案';

-- 售卖卡
CREATE TABLE IF NOT EXISTS cat_offer_card (
  card_uid       BIGINT       NOT NULL AUTO_INCREMENT,
  card_title     VARCHAR(128) NOT NULL COMMENT '卡片标题',
  card_variant   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '卡片变体',
  cover_ref      VARCHAR(255) NOT NULL DEFAULT '' COMMENT '封面引用',
  client_scope   TINYINT      NOT NULL DEFAULT 0,
  layout_payload JSON         NULL COMMENT '版式配置',
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (card_uid),
  KEY idx_card_scope (client_scope, lifecycle_state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='售卖卡';

-- 定价策略
CREATE TABLE IF NOT EXISTS cat_pricing_policy (
  policy_uid     BIGINT       NOT NULL AUTO_INCREMENT,
  policy_title   VARCHAR(128) NOT NULL COMMENT '策略名',
  product_title  VARCHAR(128) NOT NULL DEFAULT '' COMMENT '所属产品',
  priority_rank  INT          NOT NULL DEFAULT 0 COMMENT '优先级',
  traffic_weight INT          NOT NULL DEFAULT 0 COMMENT '流量权重',
  cap_switch     TINYINT      NOT NULL DEFAULT 2 COMMENT '封顶开关',
  client_scope   TINYINT      NOT NULL DEFAULT 0,
  policy_kind    TINYINT      NOT NULL DEFAULT 0 COMMENT '策略类型',
  publish_state  TINYINT      NOT NULL DEFAULT 0 COMMENT '0未发布 4已发布',
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  effective_from DATETIME     NULL,
  effective_to   DATETIME     NULL,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (policy_uid),
  KEY idx_policy_product (product_title),
  KEY idx_policy_scope (client_scope, publish_state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='定价策略';

-- 策略-卡片绑定
CREATE TABLE IF NOT EXISTS cat_policy_card_bind (
  bind_uid       BIGINT NOT NULL AUTO_INCREMENT,
  policy_uid     BIGINT NOT NULL,
  card_uid       BIGINT NOT NULL,
  ordinal        INT    NOT NULL DEFAULT 0,
  PRIMARY KEY (bind_uid),
  KEY idx_bind_policy (policy_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='策略与售卖卡绑定关系';

-- ── 权益域 bft_ ─────────────────────────────────────────────

-- 会员层级
CREATE TABLE IF NOT EXISTS bft_tier_profile (
  tier_uid       BIGINT       NOT NULL AUTO_INCREMENT,
  tier_label     VARCHAR(64)  NOT NULL COMMENT '层级名，如 SVIP',
  product_title  VARCHAR(128) NOT NULL DEFAULT '' COMMENT '所属产品',
  tier_rank      INT          NOT NULL DEFAULT 0 COMMENT '层级序号',
  service_code   VARCHAR(64)  NOT NULL DEFAULT '',
  origin_tag     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '来源标记',
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  online_state   TINYINT      NOT NULL DEFAULT 0,
  rule_expr      VARCHAR(512) NOT NULL DEFAULT '' COMMENT '准入表达式',
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (tier_uid),
  KEY idx_tier_product (product_title)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='会员层级';

-- 权益包
CREATE TABLE IF NOT EXISTS bft_entitlement_group (
  group_uid      BIGINT       NOT NULL AUTO_INCREMENT,
  group_title    VARCHAR(128) NOT NULL,
  product_title  VARCHAR(128) NOT NULL DEFAULT '',
  tier_uid       BIGINT       NOT NULL DEFAULT 0,
  group_kind     TINYINT      NOT NULL DEFAULT 0,
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (group_uid),
  KEY idx_group_tier (tier_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='权益包';

-- 权益规则
CREATE TABLE IF NOT EXISTS bft_entitlement_rule (
  rule_uid       BIGINT       NOT NULL AUTO_INCREMENT,
  group_uid      BIGINT       NOT NULL DEFAULT 0,
  rule_title     VARCHAR(128) NOT NULL,
  benefit_kind   TINYINT      NOT NULL DEFAULT 0 COMMENT '权益类型',
  quota_value    DECIMAL(12,2) NOT NULL DEFAULT 0 COMMENT '额度',
  quota_unit     VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '额度单位',
  claim_cycle    INT          NOT NULL DEFAULT 0 COMMENT '领取周期(天)',
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (rule_uid),
  KEY idx_rule_group (group_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='权益规则';

-- 任务门槛
CREATE TABLE IF NOT EXISTS bft_task_threshold (
  task_uid       BIGINT       NOT NULL AUTO_INCREMENT,
  task_title     VARCHAR(128) NOT NULL,
  product_title  VARCHAR(128) NOT NULL DEFAULT '',
  trigger_kind   TINYINT      NOT NULL DEFAULT 0,
  threshold_value INT         NOT NULL DEFAULT 0,
  reward_group_uid BIGINT     NOT NULL DEFAULT 0,
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (task_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='任务门槛';

-- ── 交易域 trd_ ─────────────────────────────────────────────

-- 结算配置
CREATE TABLE IF NOT EXISTS trd_checkout_profile (
  profile_uid    BIGINT       NOT NULL AUTO_INCREMENT,
  profile_title  VARCHAR(128) NOT NULL,
  product_title  VARCHAR(128) NOT NULL DEFAULT '',
  client_scope   TINYINT      NOT NULL DEFAULT 0,
  center_ref     VARCHAR(255) NOT NULL DEFAULT '' COMMENT '会员中心引用',
  layout_payload JSON         NULL,
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (profile_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='结算配置';

-- 支付通道
CREATE TABLE IF NOT EXISTS trd_promo_rule (
  promo_uid      BIGINT       NOT NULL AUTO_INCREMENT,
  product_title  VARCHAR(128) NOT NULL DEFAULT '',
  merchant_code  VARCHAR(64)  NOT NULL DEFAULT '',
  pay_methods    VARCHAR(255) NOT NULL DEFAULT '' COMMENT '支付方式清单,逗号分隔',
  cond_priority  INT          NOT NULL DEFAULT 0,
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (promo_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='支付优惠规则';

-- 代扣模板
CREATE TABLE IF NOT EXISTS trd_debit_template (
  template_uid   BIGINT       NOT NULL AUTO_INCREMENT,
  template_title VARCHAR(128) NOT NULL,
  platform_kind  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT 'weixin/zfb/douyin',
  template_payload JSON       NULL,
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (template_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='代扣模板';

-- 操作留痕
CREATE TABLE IF NOT EXISTS trd_audit_trace (
  trace_uid      BIGINT       NOT NULL AUTO_INCREMENT,
  target_kind    VARCHAR(64)  NOT NULL DEFAULT '',
  target_ref     VARCHAR(64)  NOT NULL DEFAULT '',
  action_kind    VARCHAR(64)  NOT NULL DEFAULT '',
  operator_ref   VARCHAR(64)  NOT NULL DEFAULT '',
  detail_payload JSON         NULL,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (trace_uid),
  KEY idx_trace_target (target_kind, target_ref)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='操作留痕';

-- ── 渠道域 chn_ ─────────────────────────────────────────────

-- 商户档案
CREATE TABLE IF NOT EXISTS chn_merchant_profile (
  merchant_uid   BIGINT       NOT NULL AUTO_INCREMENT,
  merchant_code  VARCHAR(64)  NOT NULL COMMENT '商户编号',
  merchant_alias VARCHAR(128) NOT NULL DEFAULT '' COMMENT '商户别名',
  merchant_kind  TINYINT      NOT NULL DEFAULT 0,
  node_uid       BIGINT       NOT NULL DEFAULT 0 COMMENT '所属分销节点',
  is_primary     TINYINT      NOT NULL DEFAULT 0,
  settle_kind    TINYINT      NOT NULL DEFAULT 0,
  business_kind  TINYINT      NOT NULL DEFAULT 0,
  consume_cap    DECIMAL(12,2) NOT NULL DEFAULT 0,
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (merchant_uid),
  UNIQUE KEY uk_merchant_code (merchant_code),
  KEY idx_merchant_node (node_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='商户档案';

-- 分销节点
CREATE TABLE IF NOT EXISTS chn_distribution_node (
  node_uid       BIGINT       NOT NULL AUTO_INCREMENT,
  node_title     VARCHAR(128) NOT NULL,
  node_code      VARCHAR(64)  NOT NULL DEFAULT '',
  parent_uid     BIGINT       NOT NULL DEFAULT 0,
  node_kind      TINYINT      NOT NULL DEFAULT 0,
  attr_payload   JSON         NULL,
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (node_uid),
  KEY idx_node_parent (parent_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='分销节点';

-- 归因记录
CREATE TABLE IF NOT EXISTS chn_attribution_record (
  record_uid     BIGINT       NOT NULL AUTO_INCREMENT,
  node_uid       BIGINT       NOT NULL DEFAULT 0,
  merchant_uid   BIGINT       NOT NULL DEFAULT 0,
  source_kind    VARCHAR(64)  NOT NULL DEFAULT '',
  landing_ref    VARCHAR(255) NOT NULL DEFAULT '',
  attr_payload   JSON         NULL,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (record_uid),
  KEY idx_attr_node (node_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='归因记录';

-- ── 运营域 ops_ ─────────────────────────────────────────────

-- 投放计划
CREATE TABLE IF NOT EXISTS ops_campaign_plan (
  campaign_uid   BIGINT       NOT NULL AUTO_INCREMENT,
  campaign_title VARCHAR(128) NOT NULL,
  product_title  VARCHAR(128) NOT NULL DEFAULT '',
  campaign_kind  TINYINT      NOT NULL DEFAULT 0,
  starts_at      DATETIME     NULL,
  ends_at        DATETIME     NULL,
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  audience_payload JSON       NULL,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (campaign_uid),
  KEY idx_campaign_window (starts_at, ends_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='投放计划';

-- 券批次
CREATE TABLE IF NOT EXISTS ops_voucher_batch (
  batch_uid      BIGINT       NOT NULL AUTO_INCREMENT,
  batch_title    VARCHAR(128) NOT NULL,
  campaign_uid   BIGINT       NOT NULL DEFAULT 0,
  face_amount    DECIMAL(12,2) NOT NULL DEFAULT 0,
  quota_total    INT          NOT NULL DEFAULT 0,
  quota_issued   INT          NOT NULL DEFAULT 0,
  batch_state    TINYINT      NOT NULL DEFAULT 0,
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (batch_uid),
  KEY idx_batch_campaign (campaign_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='券批次';

-- 推广位
CREATE TABLE IF NOT EXISTS ops_promotion_slot (
  slot_uid       BIGINT       NOT NULL AUTO_INCREMENT,
  slot_title     VARCHAR(128) NOT NULL,
  slot_kind      TINYINT      NOT NULL DEFAULT 0,
  product_title  VARCHAR(128) NOT NULL DEFAULT '',
  template_ref   VARCHAR(64)  NOT NULL DEFAULT '',
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  slot_payload   JSON         NULL,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (slot_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='推广位';

-- ── 配置域 cfg_ ─────────────────────────────────────────────

-- 字典项
CREATE TABLE IF NOT EXISTS cfg_dictionary_entry (
  entry_uid      BIGINT       NOT NULL AUTO_INCREMENT,
  dict_kind      VARCHAR(64)  NOT NULL COMMENT '字典类型',
  entry_code     VARCHAR(64)  NOT NULL DEFAULT '',
  entry_label    VARCHAR(255) NOT NULL DEFAULT '',
  ordinal        INT          NOT NULL DEFAULT 0,
  extra_payload  JSON         NULL,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (entry_uid),
  KEY idx_dict_kind (dict_kind, ordinal)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='字典项';

-- 集成模块
CREATE TABLE IF NOT EXISTS cfg_integration_module (
  module_uid     BIGINT       NOT NULL AUTO_INCREMENT,
  module_title   VARCHAR(128) NOT NULL,
  product_title  VARCHAR(128) NOT NULL DEFAULT '',
  module_kind    VARCHAR(64)  NOT NULL DEFAULT '',
  endpoint_ref   VARCHAR(255) NOT NULL DEFAULT '',
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  config_payload JSON         NULL,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (module_uid),
  KEY idx_module_product (product_title)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='集成模块';

-- 通用开关
CREATE TABLE IF NOT EXISTS cfg_feature_toggle (
  toggle_uid     BIGINT       NOT NULL AUTO_INCREMENT,
  toggle_key     VARCHAR(128) NOT NULL,
  product_title  VARCHAR(128) NOT NULL DEFAULT '',
  toggle_value   VARCHAR(255) NOT NULL DEFAULT '',
  remarks        VARCHAR(255) NOT NULL DEFAULT '',
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (toggle_uid),
  UNIQUE KEY uk_toggle_key (toggle_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='通用开关';

-- 产品目录
CREATE TABLE IF NOT EXISTS cfg_product_catalog (
  product_uid    BIGINT       NOT NULL AUTO_INCREMENT,
  product_code   VARCHAR(64)  NOT NULL DEFAULT '',
  product_title  VARCHAR(128) NOT NULL,
  display_title  VARCHAR(128) NOT NULL DEFAULT '' COMMENT '对外展示名',
  service_phone  VARCHAR(64)  NOT NULL DEFAULT '',
  ordinal        INT          NOT NULL DEFAULT 0,
  lifecycle_state TINYINT     NOT NULL DEFAULT 1,
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  voided         TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (product_uid),
  UNIQUE KEY uk_product_title (product_title)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='产品目录';

-- ── 消息可靠性 msg_ ─────────────────────────────────────────
--
-- 这两张表解决的是同一类问题：**消息系统和数据库是两个系统，
-- 一个成功另一个失败时必须能自洽**。没有这两张表，Kafka 只提供
-- at-least-once，落到业务侧就变成「至少写两遍」或「一笔都不写」。

-- 消费去重：一个 (事件, 消费组) 只允许被成功处理一次。
--
-- 关键是这张表和业务写入在**同一个事务**里：
--   插入成功 → 业务写入成功 → 一起提交；
--   业务写入失败 → 一起回滚，占位也不会留。
-- 这样才不存在「占了位但业务没做」的永久丢失窗口。
CREATE TABLE IF NOT EXISTS msg_consumed_event (
  event_id       VARCHAR(128) NOT NULL,
  consumer_group VARCHAR(64)  NOT NULL,
  consumed_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (event_id, consumer_group),
  KEY idx_consumed_at (consumed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='消费去重台账';

-- 发件箱：业务写库时把「要发的事件」一起写进这张表，
-- 由独立协程扫表补发。解决的是「库改了但消息没发出去」。
--
-- state: 0 待发 / 1 已发 / 2 放弃（超过重试上限，进人工排查）
-- next_retry_at 让退避策略完全落在数据上，进程重启不丢进度。
CREATE TABLE IF NOT EXISTS msg_outbox (
  out_uid        BIGINT       NOT NULL AUTO_INCREMENT,
  event_id       VARCHAR(128) NOT NULL,
  topic          VARCHAR(64)  NOT NULL,
  partition_key  VARCHAR(128) NOT NULL DEFAULT '',
  payload        MEDIUMBLOB   NOT NULL,
  state          TINYINT      NOT NULL DEFAULT 0,
  retry_count    INT          NOT NULL DEFAULT 0,
  next_retry_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_error     VARCHAR(255) NOT NULL DEFAULT '',
  created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (out_uid),
  UNIQUE KEY uk_outbox_event (event_id),
  KEY idx_outbox_due (state, next_retry_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='发件箱';
