package rule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound 规则不存在。
var ErrNotFound = errors.New("规则不存在")

// Rule 是一条可配置规则。
type Rule struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Domain  string `json:"domain"`
	Enabled bool   `json:"enabled"`
	Expr    Expr   `json:"expr"`
	Action  string `json:"action"`
	Version int    `json:"version"`
	Updated string `json:"updated_at"`

	// RawExpr 保留原始 JSON，回给前端编辑器用；
	// Expr 是解析后的结构，用于求值。
	RawExpr json.RawMessage `json:"-"`
}

// VersionRow 是一条规则的历史版本快照。
// 规则改了要能回滚——运营最怕的是「昨天还好好的，今天全乱了」却查不到改了什么。
type VersionRow struct {
	Version int             `json:"version"`
	Expr    json.RawMessage `json:"expr"`
	Action  string          `json:"action"`
	At      string          `json:"created_at"`
}

// Store 规则持久化。
type Store struct{ DB *sql.DB }

// NewStore 构造。
func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

// List 列表。domain 为空表示不过滤。
func (s *Store) List(ctx context.Context, domain string, limit int) ([]Rule, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := "SELECT id, name, domain, enabled, expr, action, version, updated_at FROM mc_rule"
	args := []any{}
	if domain != "" {
		q += " WHERE domain = ?"
		args = append(args, domain)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Rule, 0, 16)
	for rows.Next() {
		var (
			r     Rule
			raw   []byte
			en    int
			upd   time.Time
		)
		if err := rows.Scan(&r.ID, &r.Name, &r.Domain, &en, &raw, &r.Action, &r.Version, &upd); err != nil {
			return nil, err
		}
		r.Enabled = en == 1
		r.Updated = upd.Format("2006-01-02 15:04:05")
		r.RawExpr = raw
		if err := json.Unmarshal(raw, &r.Expr); err != nil {
			// 单条规则存坏了不能让整个列表挂掉
			r.Expr = Expr{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get 单条。
func (s *Store) Get(ctx context.Context, id int64) (Rule, error) {
	var (
		r   Rule
		raw []byte
		en  int
		upd time.Time
	)
	err := s.DB.QueryRowContext(ctx,
		"SELECT id, name, domain, enabled, expr, action, version, updated_at FROM mc_rule WHERE id = ?", id).
		Scan(&r.ID, &r.Name, &r.Domain, &en, &raw, &r.Action, &r.Version, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	if err != nil {
		return Rule{}, err
	}
	r.Enabled = en == 1
	r.Updated = upd.Format("2006-01-02 15:04:05")
	r.RawExpr = raw
	_ = json.Unmarshal(raw, &r.Expr)
	return r, nil
}

// Create 新建。
func (s *Store) Create(ctx context.Context, r Rule) (int64, error) {
	raw, err := json.Marshal(r.Expr)
	if err != nil {
		return 0, fmt.Errorf("规则表达式无法序列化: %w", err)
	}
	en := 0
	if r.Enabled {
		en = 1
	}
	res, err := s.DB.ExecContext(ctx,
		"INSERT INTO mc_rule (name, domain, enabled, expr, action, version) VALUES (?,?,?,?,?,1)",
		r.Name, r.Domain, en, raw, r.Action)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	_, _ = s.DB.ExecContext(ctx,
		"INSERT INTO mc_rule_version (rule_id, version, expr, action) VALUES (?,1,?,?)", id, raw, r.Action)
	return id, nil
}

// Update 更新并写版本快照。
func (s *Store) Update(ctx context.Context, r Rule) error {
	raw, err := json.Marshal(r.Expr)
	if err != nil {
		return fmt.Errorf("规则表达式无法序列化: %w", err)
	}
	en := 0
	if r.Enabled {
		en = 1
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var ver int
	if err := tx.QueryRowContext(ctx, "SELECT version FROM mc_rule WHERE id = ?", r.ID).Scan(&ver); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	ver++
	if _, err := tx.ExecContext(ctx,
		"UPDATE mc_rule SET name=?, domain=?, enabled=?, expr=?, action=?, version=?, updated_at=NOW() WHERE id=?",
		r.Name, r.Domain, en, raw, r.Action, ver, r.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO mc_rule_version (rule_id, version, expr, action) VALUES (?,?,?,?)",
		r.ID, ver, raw, r.Action); err != nil {
		return err
	}
	return tx.Commit()
}

// Delete 删除（同时删历史，避免孤儿）。
func (s *Store) Delete(ctx context.Context, id int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM mc_rule_version WHERE rule_id = ?", id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM mc_rule WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// Versions 历史版本。
func (s *Store) Versions(ctx context.Context, id int64) ([]VersionRow, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT version, expr, action, created_at FROM mc_rule_version WHERE rule_id = ? ORDER BY version DESC LIMIT 50", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]VersionRow, 0, 8)
	for rows.Next() {
		var (
			v   VersionRow
			at  time.Time
			raw []byte
		)
		if err := rows.Scan(&v.Version, &raw, &v.Action, &at); err != nil {
			return nil, err
		}
		v.Expr = raw
		v.At = at.Format("2006-01-02 15:04:05")
		out = append(out, v)
	}
	return out, rows.Err()
}
