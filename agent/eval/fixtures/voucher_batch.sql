-- 券批次健康度（voucher_health）评测数据
--
-- 用途：本地/评测环境跑 eval 时，让这条规则能稳定产出「命中」和「不该命中」两类样本。
-- 用法：mysql -h <host> -u <user> -p <db> < eval/fixtures/voucher_batch.sql
--
-- 判定依据见 server/internal/diag/checks.go：
--   lifecycle_state = 3 且 quota_issued > 0  → 已删除但券在用户手里
--   batch_state = 1 且 quota_total = 0       → 启用了但发不出去
--   quota_issued > quota_total               → 超发
--   草稿 + 未发放                            → 正常，规则不该报

DELETE FROM ops_voucher_batch WHERE batch_uid BETWEEN 900001 AND 900006;

INSERT INTO ops_voucher_batch
    (batch_uid, batch_title, campaign_uid, face_amount,
     quota_total, quota_issued, batch_state, lifecycle_state, voided)
VALUES
    -- 超发：应有 high 级 finding
    (900001, '评测-超发批次', 1, 10.00, 100, 150, 1, 1, 0),
    -- 启用但配额为 0：应有 mid 级 finding
    (900002, '评测-零配额批次', 1, 20.00, 0, 0, 1, 1, 0),
    -- 已删除但发过券：应有 high 级 finding
    -- （voided 是查询层黑名单，lifecycle_state 是业务删除态，两者不是一回事）
    (900003, '评测-已删除仍发放', 1, 5.00, 100, 12, 2, 3, 0),
    -- 正常：不该命中
    (900004, '评测-正常批次', 1, 8.00, 100, 30, 1, 1, 0),
    -- 草稿且未发放：不该命中
    (900005, '评测-草稿批次', 1, 8.00, 100, 0, 0, 1, 0),
    -- 软删除：查询层就应该过滤掉，不该出现在结果里
    (900006, '评测-软删除批次', 1, 8.00, 0, 0, 1, 1, 1);
