package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/example/loyalty-console/server/internal/observ"
)

// Command 是作业真正要干的事。
// 返回 error 表示失败，runner 会记进历史并保留下次重试的机会（等下一个 cron 点）。
type Command func(ctx context.Context) (string, error)

// JobRepo 是 runner 对存储的最小依赖。
//
// 抽成接口不是为了「面向接口编程」的教条，而是因为并发策略
// （skip / queue / allow）这类时序逻辑必须能脱离 MySQL 单测——
// 用真实库测「上一次没跑完时第二次怎么办」既慢又难构造。
type JobRepo interface {
	Due(ctx context.Context, now time.Time) ([]Job, error)
	Get(ctx context.Context, id int64) (Job, error)
	MarkRun(ctx context.Context, id int64, status, errMsg string, next time.Time) error
	RecordRun(ctx context.Context, jobID int64, status string, durMS int64, errMsg string) error
}

// Runner 调度循环。
type Runner struct {
	Store     JobRepo
	Locker    Locker
	Tick      time.Duration
	Log       *slog.Logger
	cmds      map[string]Command
	inflight  sync.Map // jobID -> struct{}
	stopCh    chan struct{}
	stopOnce  sync.Once
}

// Locker 是分布式锁的最小接口。
// 只依赖这一个方法，是为了让 runner 能被单测注入假实现，
// 而不是为了抽象而抽象——真实现直接复用 internal/lock。
type Locker interface {
	// Acquire 拿锁；拿不到返回 nil（表示别人正在跑）。
	Acquire(ctx context.Context, key string) (ReleaseFunc, bool)
}

// ReleaseFunc 释放锁。
type ReleaseFunc interface{ Release(ctx context.Context) error }

// NewRunner 构造。
func NewRunner(store JobRepo, locker Locker, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		Store:  store,
		Locker: locker,
		Tick:   10 * time.Second,
		Log:    log,
		cmds:   map[string]Command{},
		stopCh: make(chan struct{}),
	}
}

// Register 注册一个可被作业引用的命令。
func (r *Runner) Register(name string, cmd Command) { r.cmds[name] = cmd }

// Commands 列出已注册命令，给前端下拉框用。
func (r *Runner) Commands() []string {
	out := make([]string, 0, len(r.cmds))
	for k := range r.cmds {
		out = append(out, k)
	}
	return out
}

// Start 起调度循环。
// 用固定间隔轮询而不是精确定时器：作业定义存在库里、随时会被改，
// 每次心跳重新读库比维护一堆定时器简单可靠得多。
func (r *Runner) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(r.Tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.stopCh:
				return
			case <-ticker.C:
				r.tickOnce(ctx)
			}
		}
	}()
}

// Stop 停止循环。
func (r *Runner) Stop() { r.stopOnce.Do(func() { close(r.stopCh) }) }

func (r *Runner) tickOnce(ctx context.Context) {
	due, err := r.Store.Due(ctx, time.Now())
	if err != nil {
		r.Log.Warn("读取到期作业失败", "err", err)
		return
	}
	for _, j := range due {
		r.launch(ctx, j)
	}
}

// RunNow 手动触发一次，忽略 cron 与 enabled。
// 运维最常做的事就是「等不到点了，先跑一次看看」。
func (r *Runner) RunNow(ctx context.Context, id int64) (string, error) {
	j, err := r.Store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return r.execute(ctx, j, true)
}

// launch 判断并发策略后执行。
func (r *Runner) launch(ctx context.Context, j Job) {
	key := fmt.Sprintf("job:%d", j.ID)

	// 先看本实例有没有在跑——本地判断比抢锁便宜
	if _, running := r.inflight.Load(j.ID); running {
		switch Concurrency(j.Concurrency) {
		case ConcurrencySkip:
			r.Log.Info("作业仍在执行，跳过本次", "job", j.Name)
			observ.JobSkippedTotal.WithLabelValues(j.Name, "inflight").Inc()
			r.advance(ctx, j)
			return
		case ConcurrencyQueue:
			// 排队：等当前这次结束后由下面的 goroutine 接着跑
			go func() {
				for {
					time.Sleep(time.Second)
					if _, ok := r.inflight.Load(j.ID); !ok {
						break
					}
				}
				r.launch(ctx, j)
			}()
			return
		case ConcurrencyAllow:
			// 放行
		}
	}

	// 跨实例互斥。拿不到锁说明别的副本在处理，本实例直接让路。
	release, ok := r.Locker.Acquire(ctx, key)
	if !ok {
		r.Log.Info("作业被其他实例持有，跳过", "job", j.Name)
		observ.JobSkippedTotal.WithLabelValues(j.Name, "locked").Inc()
		return
	}

	r.inflight.Store(j.ID, struct{}{})
	go func() {
		defer func() {
			r.inflight.Delete(j.ID)
			_ = release.Release(context.Background())
		}()
		if _, err := r.execute(ctx, j, false); err != nil {
			r.Log.Warn("作业执行失败", "job", j.Name, "err", err)
		}
	}()
}

// execute 真正执行作业并记录结果。
func (r *Runner) execute(ctx context.Context, j Job, manual bool) (string, error) {
	cmd, ok := r.cmds[j.Command]
	if !ok {
		msg := fmt.Sprintf("未注册的命令 %q", j.Command)
		_ = r.Store.RecordRun(ctx, j.ID, "error", 0, msg)
		r.advance(ctx, j)
		return "", fmt.Errorf("%s", msg)
	}

	timeout := time.Duration(j.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	out, err := cmd(runCtx)
	dur := time.Since(started).Milliseconds()

	status := "ok"
	errMsg := ""
	if err != nil {
		status = "error"
		errMsg = err.Error()
	}
	observ.JobRunTotal.WithLabelValues(j.Name, status).Inc()
	observ.JobDuration.WithLabelValues(j.Name).Observe(time.Since(started).Seconds())
	_ = r.Store.RecordRun(ctx, j.ID, status, dur, errMsg)
	r.advance(ctx, j)

	r.Log.Info("作业执行完成", "job", j.Name, "status", status, "ms", dur, "out", out)
	return out, err
}

// advance 把 next_run_at 推到下一个 cron 点。
// 即使执行失败也推进——否则一条失败的作业会在每个心跳里被反复触发，
// 变成事实上的死循环。
func (r *Runner) advance(ctx context.Context, j Job) {
	sch, err := ParseCron(j.Cron)
	if err != nil {
		return
	}
	next := sch.Next(time.Now())
	if next.IsZero() {
		r.Log.Warn("cron 表达式永不触发，作业已停用", "job", j.Name, "cron", j.Cron)
		_ = r.Store.MarkRun(ctx, j.ID, "never", "cron 表达式永不触发", time.Now().AddDate(100, 0, 0))
		return
	}
	status := "ok"
	if last, err := r.Store.Get(ctx, j.ID); err == nil && last.LastStatus != "" {
		status = last.LastStatus
	}
	_ = r.Store.MarkRun(ctx, j.ID, status, "", next)
}
