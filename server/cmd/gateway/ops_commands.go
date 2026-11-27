package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/example/loyalty-console/server/internal/lock"
	"github.com/example/loyalty-console/server/internal/scheduler"
)

// lockerAdapter 把 internal/lock 适配成 scheduler.Locker。
// 只做类型转换，不做重试——拿不到锁就是拿不到，让调度器跳过本次才对。
type lockerAdapter struct{ l *lock.Locker }

func (a lockerAdapter) Acquire(ctx context.Context, key string) (scheduler.ReleaseFunc, bool) {
	if a.l == nil {
		// Redis 不可用时降级为「不互斥」：单副本环境本来也不需要锁，
		// 而多副本环境下宁可重复执行一次清理，也不要整个调度停摆。
		return noopRelease{}, true
	}
	m, err := a.l.Acquire(ctx, key)
	if err != nil || m == nil {
		return nil, false
	}
	return m, true
}

type noopRelease struct{}

func (noopRelease) Release(context.Context) error { return nil }

// registerOpsCommands 注册作业命令。
//
// 每个命令都返回一句人话摘要，写进执行记录——运维看历史时，
// 「删除 3 条」比「ok」有用得多。
func registerOpsCommands(r *scheduler.Runner, db *sql.DB) {
	r.Register("cleanup_expired_rules", func(ctx context.Context) (string, error) {
		// 只清「已停用且 30 天没动过」的规则。启用中的永远不碰——
		// 清理任务误删启用规则，是运营最不能接受的事故。
		res, err := db.ExecContext(ctx,
			"DELETE FROM mc_rule WHERE enabled = 0 AND updated_at < DATE_SUB(NOW(), INTERVAL 30 DAY)")
		if err != nil {
			return "", err
		}
		n, _ := res.RowsAffected()
		return fmt.Sprintf("清理停用规则 %d 条", n), nil
	})

	r.Register("snapshot_metrics", func(ctx context.Context) (string, error) {
		var rules, enabledRules, jobs, audits int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mc_rule").Scan(&rules); err != nil {
			return "", err
		}
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM mc_rule WHERE enabled = 1").Scan(&enabledRules); err != nil {
			return "", err
		}
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mc_job").Scan(&jobs); err != nil {
			return "", err
		}
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM mc_mask_audit WHERE created_at > DATE_SUB(NOW(), INTERVAL 1 DAY)").
			Scan(&audits); err != nil {
			return "", err
		}
		_, err := db.ExecContext(ctx,
			`INSERT INTO mc_metric_snapshot (rule_count, rule_enabled_count, job_count, mask_audit_24h, created_at)
			 VALUES (?,?,?,?,NOW())`, rules, enabledRules, jobs, audits)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("快照：规则 %d（启用 %d）/ 作业 %d / 24h 明文访问 %d",
			rules, enabledRules, jobs, audits), nil
	})

	r.Register("purge_mask_audit", func(ctx context.Context) (string, error) {
		// 审计保留 180 天：合规检查通常看半年内的记录，
		// 更久的数据占空间且几乎不会被查。
		res, err := db.ExecContext(ctx,
			"DELETE FROM mc_mask_audit WHERE created_at < DATE_SUB(NOW(), INTERVAL 180 DAY)")
		if err != nil {
			return "", err
		}
		n, _ := res.RowsAffected()
		return fmt.Sprintf("清理过期审计 %d 条", n), nil
	})
}

// opsLogger 给调度器用的 slog，带固定字段。
func opsLogger(base *slog.Logger) *slog.Logger {
	return base.With("component", "scheduler")
}
