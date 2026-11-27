package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Concurrency 是同一作业重复触发时的处理方式。
type Concurrency string

const (
	// ConcurrencySkip 上一次还没跑完就跳过本次。
	// 默认值——大部分清理类作业重复执行没有意义。
	ConcurrencySkip Concurrency = "skip"
	// ConcurrencyQueue 排队，等上一次结束再跑。
	ConcurrencyQueue Concurrency = "queue"
	// ConcurrencyAllow 允许并发。
	// 只对幂等的快照类作业开放，乱用会写出双份数据。
	ConcurrencyAllow Concurrency = "allow"
)

// Job 一个定时作业。
type Job struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Cron        string `json:"cron"`
	Command     string `json:"command"`
	Concurrency string `json:"concurrency"`
	TimeoutSec  int    `json:"timeout_sec"`
	Enabled     bool   `json:"enabled"`

	NextRunAt  string `json:"next_run_at"`
	LastRunAt  string `json:"last_run_at"`
	LastStatus string `json:"last_status"`
	LastError  string `json:"last_error"`
}

// ErrJobNotFound 作业不存在。
var ErrJobNotFound = errors.New("作业不存在")

// Store 作业持久化。
type Store struct{ DB *sql.DB }

// NewStore 构造。
func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

const jobCols = "id, name, cron_expr, command, concurrency, timeout_sec, enabled, " +
	"IFNULL(DATE_FORMAT(next_run_at,'%Y-%m-%d %H:%i:%s'),''), " +
	"IFNULL(DATE_FORMAT(last_run_at,'%Y-%m-%d %H:%i:%s'),''), IFNULL(last_status,''), IFNULL(last_error,'')"

func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var en int
	if err := sc.Scan(&j.ID, &j.Name, &j.Cron, &j.Command, &j.Concurrency, &j.TimeoutSec,
		&en, &j.NextRunAt, &j.LastRunAt, &j.LastStatus, &j.LastError); err != nil {
		return Job{}, err
	}
	j.Enabled = en == 1
	return j, nil
}

// List 列表。
func (s *Store) List(ctx context.Context) ([]Job, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+jobCols+" FROM mc_job ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Job, 0, 8)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Get 单条。
func (s *Store) Get(ctx context.Context, id int64) (Job, error) {
	row := s.DB.QueryRowContext(ctx, "SELECT "+jobCols+" FROM mc_job WHERE id = ?", id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	return j, err
}

// Create 新建，并立刻算出第一次触发时间。
func (s *Store) Create(ctx context.Context, j Job) (int64, error) {
	sch, err := ParseCron(j.Cron)
	if err != nil {
		return 0, err
	}
	next := sch.Next(time.Now())
	en := 0
	if j.Enabled {
		en = 1
	}
	res, err := s.DB.ExecContext(ctx,
		"INSERT INTO mc_job (name, cron_expr, command, concurrency, timeout_sec, enabled, next_run_at) VALUES (?,?,?,?,?,?,?)",
		j.Name, j.Cron, j.Command, j.Concurrency, j.TimeoutSec, en, next)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Update 更新定义。改了 cron 要重算下次触发时间。
func (s *Store) Update(ctx context.Context, j Job) error {
	sch, err := ParseCron(j.Cron)
	if err != nil {
		return err
	}
	next := sch.Next(time.Now())
	en := 0
	if j.Enabled {
		en = 1
	}
	res, err := s.DB.ExecContext(ctx,
		"UPDATE mc_job SET name=?, cron_expr=?, command=?, concurrency=?, timeout_sec=?, enabled=?, next_run_at=? WHERE id=?",
		j.Name, j.Cron, j.Command, j.Concurrency, j.TimeoutSec, en, next, j.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrJobNotFound
	}
	return nil
}

// Delete 删除。
func (s *Store) Delete(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, "DELETE FROM mc_job WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrJobNotFound
	}
	return nil
}

// Due 取出到点该跑的作业。
func (s *Store) Due(ctx context.Context, now time.Time) ([]Job, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT "+jobCols+" FROM mc_job WHERE enabled = 1 AND next_run_at <= ? ORDER BY next_run_at LIMIT 20", now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Job, 0, 4)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// MarkRun 跑完更新状态与下次触发时间。
func (s *Store) MarkRun(ctx context.Context, id int64, status, errMsg string, next time.Time) error {
	_, err := s.DB.ExecContext(ctx,
		"UPDATE mc_job SET last_run_at=NOW(), last_status=?, last_error=?, next_run_at=? WHERE id=?",
		status, errMsg, next, id)
	return err
}

// RunRow 一次执行记录。
type RunRow struct {
	ID       int64  `json:"id"`
	JobID    int64  `json:"job_id"`
	Status   string `json:"status"`
	Duration int64  `json:"duration_ms"`
	Error    string `json:"error"`
	At       string `json:"started_at"`
}

// RecordRun 写执行记录。
func (s *Store) RecordRun(ctx context.Context, jobID int64, status string, durMS int64, errMsg string) error {
	_, err := s.DB.ExecContext(ctx,
		"INSERT INTO mc_job_run (job_id, status, duration_ms, error) VALUES (?,?,?,?)",
		jobID, status, durMS, errMsg)
	return err
}

// Runs 执行历史。
func (s *Store) Runs(ctx context.Context, jobID int64, limit int) ([]RunRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	q := "SELECT id, job_id, status, duration_ms, IFNULL(error,''), DATE_FORMAT(started_at,'%Y-%m-%d %H:%i:%s') FROM mc_job_run"
	args := []any{}
	if jobID > 0 {
		q += " WHERE job_id = ?"
		args = append(args, jobID)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RunRow, 0, limit)
	for rows.Next() {
		var r RunRow
		if err := rows.Scan(&r.ID, &r.JobID, &r.Status, &r.Duration, &r.Error, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
