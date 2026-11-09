-- 补齐此前为空表的种子数据。
--
-- 原则：每一条都挂到已存在的业务对象上（campaign_uid / card_uid / node_uid /
-- merchant_uid / policy_uid 都取自真实种子），不造孤立数据 —— 否则页面上
-- 看起来有数，点进去全是断链，比空着更糟。
--
-- 全部使用显式主键 + INSERT IGNORE：可重复执行，重建库时不会因为
-- auto_increment 漂移把关联打乱。

SET NAMES utf8mb4;
USE `mc_core`;
START TRANSACTION;

-- ── 配置域：通用开关 ────────────────────────────────────────
-- 覆盖「已开 / 已关 / 灰度百分比」三种形态，让页面上的状态区分得出来。
DELETE FROM `cfg_feature_toggle`;
INSERT IGNORE INTO cfg_feature_toggle
(toggle_uid,toggle_key,product_title,toggle_value,remarks)
VALUES
(1, 'checkout.v2.enabled',        '文档云 Pro',        'true',    '新版收银台灰度'),
(2, 'checkout.v2.rollout',        '文档云 Pro',        '30',      '灰度比例，单位百分比'),
(3, 'benefit.expiry.auto_reclaim','文档云 Pro',        'true',    '权益到期自动回收'),
(4, 'export.async.enabled',       '',                  'true',    '异步导出总开关'),
(5, 'audit.kafka.persist',        '',                  'true',    '审计事件走 Kafka 落库'),
(6, 'promo.stackable',            '扫描王-pdf转换大师', 'false',   '优惠是否可叠加'),
(7, 'pricing.dynamic.cap',        '超级会员',          'true',    '动态定价封顶'),
(8, 'notification.expiry.ahead',  '',                  '86400',   '到期前多少秒提醒');

-- ── 渠道域：归因记录 ────────────────────────────────────────
-- node_uid / merchant_uid 取自真实渠道种子，attr_payload 用可解析的 JSON，
-- 页面展开时不至于报 "invalid JSON"。
DELETE FROM `chn_attribution_record`;
INSERT IGNORE INTO chn_attribution_record
(record_uid,node_uid,merchant_uid,source_kind,landing_ref,attr_payload,created_at)
VALUES
(1, 620, 1, 'qr_code',    'https://example.test/landing/a1', '{"channel":"线下物料","scene":"门店海报","cost":0}',        '2026-10-01 09:12:00'),
(2, 620, 1, 'link',       'https://example.test/landing/a2', '{"channel":"公众号","scene":"菜单栏","cost":120.5}',         '2026-10-03 14:20:00'),
(3, 622, 2, 'qr_code',    'https://example.test/landing/b1', '{"channel":"线下物料","scene":"易拉宝","cost":0}',           '2026-10-05 11:05:00'),
(4, 622, 2, 'mini_app',   'pages/index?from=ad',             '{"channel":"小程序","scene":"开屏广告","cost":860.0}',       '2026-10-06 08:40:00'),
(5, 623, 3, 'link',       'https://example.test/landing/c1', '{"channel":"信息流","scene":"feedslot-7","cost":2400.0}',    '2026-10-08 16:30:00'),
(6, 623, 3, 'staff_share','staff:10086',                     '{"channel":"员工转发","scene":"个人二维码","cost":0}',       '2026-10-09 10:15:00'),
(7, 620, 2, 'qr_code',    'https://example.test/landing/d1', '{"channel":"异业合作","scene":"合作方柜台","cost":500.0}',   '2026-10-11 13:50:00'),
(8, 622, 1, 'link',       'https://example.test/landing/e1', '{"channel":"短信","scene":"召回批次-202610","cost":38.0}',   '2026-10-12 20:05:00');

-- ── 商品域：定价策略与售卖卡绑定 ────────────────────────────
-- 先补几条策略（原表只有 1 条兜底策略），再把策略绑到真实 card_uid 上。
-- 只有 1 条策略时，绑定表填了也没有对照关系，看不出"一个策略挂多张卡"的结构。
INSERT IGNORE INTO cat_pricing_policy
(policy_uid,policy_title,product_title,priority_rank,traffic_weight,cap_switch,client_scope,
 policy_kind,publish_state,lifecycle_state,effective_from,effective_to,created_at)
VALUES
(203113, '新客首月优惠',   '文档云 Pro',        10, 40, 2, 0, 1, 4, 1, '2026-10-01 00:00:00', '2026-12-31 23:59:59', '2026-10-01 10:00:00'),
(203114, '续费保价',       '文档云 Pro',        20, 30, 1, 0, 2, 4, 1, '2026-10-01 00:00:00', '2027-03-31 23:59:59', '2026-10-02 11:30:00'),
(203115, 'PC 端专项',      '超级会员',          15, 20, 2, 1, 3, 4, 1, NULL, NULL,                                 '2026-10-05 09:20:00'),
(203116, '移动端专项',     '超级会员',          15, 25, 2, 2, 3, 4, 1, NULL, NULL,                                 '2026-10-05 09:25:00'),
(203117, '沉睡召回折扣',   '扫描王-pdf转换大师', 50, 10, 2, 0, 1, 2, 1, '2026-11-01 00:00:00', '2026-11-30 23:59:59', '2026-10-28 15:00:00');

-- bind_uid 用固定段，避免和其他种子撞号
DELETE FROM `cat_policy_card_bind`;
INSERT IGNORE INTO cat_policy_card_bind
(bind_uid,policy_uid,card_uid,ordinal)
VALUES
(90001, 203112, 208, 0),
(90002, 203112, 334, 1),
(90003, 203113, 208, 0),
(90004, 203113, 328, 1),
(90005, 203114, 334, 0),
(90006, 203115, 334, 0),
(90007, 203116, 208, 0),
(90008, 203117, 328, 0);

-- ── 运营域：券批次与推广位 ──────────────────────────────────
-- campaign_uid 全部取自真实投放计划，页面上的"所属活动"能点得开。
DELETE FROM `ops_voucher_batch`;
INSERT IGNORE INTO ops_voucher_batch
(batch_uid,batch_title,campaign_uid,face_amount,quota_total,quota_issued,batch_state,lifecycle_state,created_at)
VALUES
(5001, '双十一 20 元券',   123, 20.00, 5000, 1832, 2, 1, '2026-10-20 10:00:00'),
(5002, '新客 5 折券',      255,  0.00, 10000, 4211, 2, 1, '2026-10-21 09:30:00'),
(5003, '沉默用户召回券',   368, 10.00,  800,   96, 1, 1, '2026-10-25 14:00:00'),
(5004, '员工内购福利券',   123, 50.00,  300,  300, 3, 1, '2026-10-18 11:00:00'),
(5005, '测试批次-勿投',   255,  1.00,    10,    0, 0, 0, '2026-10-29 17:40:00');

DELETE FROM `ops_promotion_slot`;
INSERT IGNORE INTO ops_promotion_slot
(slot_uid,slot_title,slot_kind,product_title,template_ref,lifecycle_state,slot_payload,created_at)
VALUES
(7001, '首页首屏 Banner',   1, '文档云 Pro',         'tpl-banner-01', 1, '{"width":1200,"height":400,"interval":5}',      '2026-09-15 10:00:00'),
(7002, '收银台弹窗',        2, '文档云 Pro',         'tpl-modal-03',  1, '{"trigger":"before_pay","dismissible":true}',   '2026-09-16 10:00:00'),
(7003, '个人中心金刚位',    3, '超级会员',           'tpl-grid-02',   1, '{"columns":4,"maxItems":8}',                    '2026-09-18 15:20:00'),
(7004, '启动页插屏',        4, '扫描王-pdf转换大师', 'tpl-splash-01', 1, '{"skipAfter":3,"freqCapPerDay":2}',             '2026-09-20 09:00:00'),
(7005, '底部悬浮条',        1, '',                   'tpl-float-01',  2, '{"dismissible":true,"autoHideMs":8000}',        '2026-09-25 16:45:00');

-- ── 交易域：代扣模板与支付优惠规则 ──────────────────────────
-- merchant_code 取自真实商户档案；platform_kind 覆盖三种支付渠道。
DELETE FROM `trd_debit_template`;
INSERT IGNORE INTO trd_debit_template
(template_uid,template_title,platform_kind,template_payload,lifecycle_state,created_at)
VALUES
(8001, '微信代扣-首次签约', 'weixin', '{"contract":"wx_contract_v2","retry":3,"cycle":31}', '1', '2026-09-10 10:00:00'),
(8002, '支付宝代扣-首次签约','zfb',   '{"contract":"ali_agreement_v3","retry":2,"cycle":31}', '1', '2026-09-10 10:05:00'),
(8003, '抖音支付代扣',      'douyin', '{"contract":"dy_contract_v1","retry":3,"cycle":31}', '1', '2026-09-12 14:30:00'),
(8004, '微信代扣-续费',     'weixin', '{"contract":"wx_contract_v2","scene":"renew","retry":5}', '1', '2026-09-15 09:00:00'),
(8005, '支付宝代扣-停用模板','zfb',   '{"contract":"ali_agreement_v1"}', '0', '2026-09-01 11:00:00');

DELETE FROM `trd_promo_rule`;
INSERT IGNORE INTO trd_promo_rule
(promo_uid,product_title,merchant_code,pay_methods,cond_priority,lifecycle_state,created_at)
VALUES
(8001, '文档云 Pro',         '00955', 'weixin,zfb',        10, 1, '2026-09-10 10:00:00'),
(8002, '文档云 Pro',         '00955', 'douyin',            20, 1, '2026-09-10 10:10:00'),
(8003, '扫描王-pdf转换大师', '00954', 'weixin',            10, 1, '2026-09-11 09:20:00'),
(8004, '扫描王-pdf转换大师', '00954', 'zfb,weixin',        15, 1, '2026-09-11 09:25:00'),
(8005, '测试产品验收',       '00953', 'weixin,zfb,douyin',  5, 0, '2026-09-12 16:00:00');

COMMIT;
