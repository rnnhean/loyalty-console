package masking

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/example/loyalty-console/server/internal/observ"
)

// Service 脱敏服务：策略读取 + 应用 + 审计。
type Service struct {
	DB     *sql.DB
	Remote *RemoteClient
}

// NewService 构造。
func NewService(db *sql.DB, remote *RemoteClient) *Service {
	return &Service{DB: db, Remote: remote}
}

// ListPolicies 读策略。
func (s *Service) ListPolicies(ctx context.Context) ([]Policy, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT field, strategy, keep_prefix, keep_suffix, mask_char, roles, note FROM mc_mask_policy ORDER BY field")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Policy, 0, 16)
	for rows.Next() {
		var (
			p     Policy
			roles string
		)
		if err := rows.Scan(&p.Field, &p.Strategy, &p.KeepPrefix, &p.KeepSuffix, &p.MaskChar, &roles, &p.Note); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(roles), &p.Roles)
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertPolicy 新建或更新策略。
func (s *Service) UpsertPolicy(ctx context.Context, p Policy) error {
	if p.Field == "" {
		return errors.New("字段名不能为空")
	}
	switch p.Strategy {
	case StrategyMask, StrategyHide, StrategyPlain:
	default:
		return errors.New("策略必须是 mask / hide / plain")
	}
	if p.MaskChar == "" {
		p.MaskChar = DefaultMaskChar
	}
	roles, _ := json.Marshal(p.Roles)
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO mc_mask_policy (field, strategy, keep_prefix, keep_suffix, mask_char, roles, note)
		 VALUES (?,?,?,?,?,?,?)
		 ON DUPLICATE KEY UPDATE strategy=VALUES(strategy), keep_prefix=VALUES(keep_prefix),
		   keep_suffix=VALUES(keep_suffix), mask_char=VALUES(mask_char), roles=VALUES(roles),
		   note=VALUES(note), updated_at=NOW()`,
		p.Field, p.Strategy, p.KeepPrefix, p.KeepSuffix, p.MaskChar, string(roles), p.Note)
	return err
}

// DeletePolicy 删除策略。
func (s *Service) DeletePolicy(ctx context.Context, field string) error {
	res, err := s.DB.ExecContext(ctx, "DELETE FROM mc_mask_policy WHERE field = ?", field)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("策略不存在")
	}
	return nil
}

// ApplyResult 一次脱敏结果，带来源标记，便于排查「为什么没脱敏」。
type ApplyResult struct {
	Masked string `json:"masked"`
	Kind   string `json:"kind"`
	Source string `json:"source"` // policy / remote / local / none
}

// Apply 对一个字段值应用脱敏。
//
// 优先级：显式策略 > 远端识别 > 本地识别。
// 远端失败**不报错**，静默降级——展示路径上的外部依赖必须是尽力而为的。
func (s *Service) Apply(ctx context.Context, p *Policy, field, value, role string) ApplyResult {
	if value == "" {
		return ApplyResult{Masked: value, Kind: string(KindNone), Source: "none"}
	}
	if p != nil {
		out := ApplyResult{
			Masked: Apply(*p, value, role),
			Kind:   string(detectKind(field)),
			Source: "policy",
		}
		observ.MaskAppliedTotal.WithLabelValues(out.Source, out.Kind).Inc()
		if p.Strategy == StrategyPlain && role != "" && contains(p.Roles, role) {
			observ.MaskPlaintextTotal.WithLabelValues(field).Inc()
		}
		return out
	}

	if s.Remote != nil {
		if out, err := s.Remote.Redact(ctx, field, value); err == nil {
			observ.MaskAppliedTotal.WithLabelValues("remote", out.Kind).Inc()
			return ApplyResult{Masked: out.Masked, Kind: out.Kind, Source: "remote"}
		}
	}

	kind := Detect(value)
	if kind == KindNone {
		return ApplyResult{Masked: value, Kind: string(kind), Source: "none"}
	}
	prefix, suffix := defaultKeep(kind)
	observ.MaskAppliedTotal.WithLabelValues("local", string(kind)).Inc()
	return ApplyResult{
		Masked: maskValue(value, prefix, suffix, DefaultMaskChar),
		Kind:   string(kind),
		Source: "local",
	}
}

// Audit 记录一次明文访问。
//
// 为什么要单独一张表而不是打进应用日志：
// 审计要能按「谁、什么时候、看了哪些字段、什么理由」检索，
// 而应用日志是给人肉眼看的、还会被轮转掉。
func (s *Service) Audit(ctx context.Context, operator, role, field, reason string, count int) error {
	_, err := s.DB.ExecContext(ctx,
		"INSERT INTO mc_mask_audit (operator, role, field, reason, row_count, created_at) VALUES (?,?,?,?,?,NOW())",
		operator, role, field, reason, count)
	return err
}

// RecentAudit 最近审计记录。
func (s *Service) RecentAudit(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx,
		"SELECT operator, role, field, reason, row_count, created_at FROM mc_mask_audit ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0, limit)
	for rows.Next() {
		var (
			op, role, field, reason string
			cnt                     int
			at                      time.Time
		)
		if err := rows.Scan(&op, &role, &field, &reason, &cnt, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"operator": op, "role": role, "field": field,
			"reason": reason, "row_count": cnt,
			"created_at": at.Format("2006-01-02 15:04:05"),
		})
	}
	return out, rows.Err()
}
