-- 运营中台新增的三块能力：规则引擎 / 定时作业 / 字段脱敏。
-- 命名统一 mc_ 前缀，和业务域表（bft_/cat_/chn_ 等）分开，
-- 便于按前缀做权限与备份策略。
--
-- 注意：docker initdb 只在数据卷为空时执行。
-- 已有环境请手动执行本文件，或 down -v 重建。

-- ── 规则引擎 ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS mc_rule (
  id          BIGINT       NOT NULL AUTO_INCREMENT,
  name        VARCHAR(64)  NOT NULL,
  domain      VARCHAR(32)  NOT NULL,
  enabled     TINYINT      NOT NULL DEFAULT 1,
  -- 条件表达式（JSON）。存 JSON 而不是拆成行，是因为它整体读写、
  -- 整体校验，拆表只会让「取一条规则」变成多表 join。
  expr        JSON         NOT NULL,
  action      VARCHAR(200) NOT NULL,
  version     INT          NOT NULL DEFAULT 1,
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_domain (domain),
  KEY idx_enabled (enabled)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='可配置运营规则';

CREATE TABLE IF NOT EXISTS mc_rule_version (
  id          BIGINT      NOT NULL AUTO_INCREMENT,
  rule_id     BIGINT      NOT NULL,
  version     INT         NOT NULL,
  expr        JSON        NOT NULL,
  action      VARCHAR(200) NOT NULL,
  created_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_rule_ver (rule_id, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='规则版本快照';

-- ── 定时作业 ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS mc_job (
  id           BIGINT      NOT NULL AUTO_INCREMENT,
  name         VARCHAR(64) NOT NULL,
  cron_expr    VARCHAR(64) NOT NULL,
  command      VARCHAR(64) NOT NULL,
  concurrency  VARCHAR(16) NOT NULL DEFAULT 'skip',
  timeout_sec  INT         NOT NULL DEFAULT 30,
  enabled      TINYINT     NOT NULL DEFAULT 1,
  next_run_at  DATETIME    NOT NULL,
  last_run_at  DATETIME    NULL,
  last_status  VARCHAR(16) NULL,
  last_error   VARCHAR(500) NULL,
  created_at   DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_due (enabled, next_run_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='定时作业定义';

CREATE TABLE IF NOT EXISTS mc_job_run (
  id          BIGINT       NOT NULL AUTO_INCREMENT,
  job_id      BIGINT       NOT NULL,
  status      VARCHAR(16)  NOT NULL,
  duration_ms BIGINT       NOT NULL DEFAULT 0,
  error       VARCHAR(500) NULL,
  started_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_job (job_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='作业执行历史';

-- ── 字段脱敏 ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS mc_mask_policy (
  field       VARCHAR(64)  NOT NULL,
  strategy    VARCHAR(16)  NOT NULL,
  keep_prefix INT          NOT NULL DEFAULT 0,
  keep_suffix INT          NOT NULL DEFAULT 0,
  mask_char   VARCHAR(4)   NOT NULL DEFAULT '*',
  roles       VARCHAR(200) NOT NULL DEFAULT '[]',
  note        VARCHAR(200) NULL,
  updated_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (field)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='字段脱敏策略';

CREATE TABLE IF NOT EXISTS mc_mask_audit (
  id         BIGINT      NOT NULL AUTO_INCREMENT,
  operator   VARCHAR(64) NOT NULL,
  role       VARCHAR(32) NOT NULL,
  field      VARCHAR(64) NOT NULL,
  reason     VARCHAR(200) NOT NULL,
  row_count  INT         NOT NULL DEFAULT 0,
  created_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_operator (operator, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='明文访问审计';

-- 定时作业产生的时间序列快照。
-- 为什么不直接让 Prometheus 抓：这类数字需要**按天留档**（合规口径），
-- 而 Prometheus 有保留期，过期的数据查不到。
CREATE TABLE IF NOT EXISTS mc_metric_snapshot (
  id                  BIGINT   NOT NULL AUTO_INCREMENT,
  rule_count          INT      NOT NULL DEFAULT 0,
  rule_enabled_count  INT      NOT NULL DEFAULT 0,
  job_count           INT      NOT NULL DEFAULT 0,
  mask_audit_24h      INT      NOT NULL DEFAULT 0,
  created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='运维指标快照';

-- ── 种子数据（全部虚构）─────────────────────────────────

INSERT INTO mc_rule (name, domain, enabled, expr, action, version) VALUES
('连续 30 天未登录打标', 'operation', 1,
 '{"all":[{"field":"member.login_days","op":"gte","value":30},{"field":"tier.state","op":"eq","value":1}]}',
 '给会员打「沉默」标签，进入唤醒名单', 1),
('高价值层级缺失在线状态', 'benefit', 1,
 '{"all":[{"field":"tier.rank","op":"gte","value":3},{"field":"tier.online","op":"eq","value":0}]}',
 '通知运营检查该层级是否应上线', 1),
('券配额异常', 'operation', 0,
 '{"any":[{"field":"rule.quota","op":"gt","value":1000},{"field":"rule.quota","op":"lt","value":1}]}',
 '标记为异常券规则，需人工复核', 1)
ON DUPLICATE KEY UPDATE name = VALUES(name);

INSERT INTO mc_job (name, cron_expr, command, concurrency, timeout_sec, enabled, next_run_at) VALUES
('过期规则清理', '0 3 * * *', 'cleanup_expired_rules', 'skip', 60, 1, DATE_ADD(NOW(), INTERVAL 1 DAY)),
('指标快照', '*/30 * * * *', 'snapshot_metrics', 'allow', 30, 1, DATE_ADD(NOW(), INTERVAL 30 MINUTE))
ON DUPLICATE KEY UPDATE name = VALUES(name);

INSERT INTO mc_mask_policy (field, strategy, keep_prefix, keep_suffix, mask_char, roles, note) VALUES
('member_phone', 'mask', 3, 4, '*', '["admin"]', '手机号：默认掩码，管理员可看明文'),
('member_id_card', 'hide', 0, 0, '*', '[]', '证件号：一律不返回明文'),
('member_email', 'mask', 2, 0, '*', '[]', '邮箱：保留前两位')
ON DUPLICATE KEY UPDATE strategy = VALUES(strategy);
