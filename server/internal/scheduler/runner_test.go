package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeRepo 是内存版 JobRepo。
// 并发策略的时序逻辑（上一次没跑完时第二次怎么办）必须能脱离 MySQL 测，
// 否则「queue 会不会真的排队」这种问题只能靠人肉盯着日志猜。
type fakeRepo struct {
	mu       sync.Mutex
	jobs     map[int64]Job
	due      []Job
	runs     []string // "jobID:status"
	marked   []time.Time
	getErr   error
	dueErr   error
}

func newFakeRepo(jobs ...Job) *fakeRepo {
	m := map[int64]Job{}
	for _, j := range jobs {
		m[j.ID] = j
	}
	return &fakeRepo{jobs: m, due: jobs}
}

func (f *fakeRepo) Due(_ context.Context, _ time.Time) ([]Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dueErr != nil {
		return nil, f.dueErr
	}
	out := f.due
	f.due = nil // 只返回一次，模拟「到点被取走」
	return out, nil
}

func (f *fakeRepo) Get(_ context.Context, id int64) (Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return Job{}, f.getErr
	}
	j, ok := f.jobs[id]
	if !ok {
		return Job{}, ErrJobNotFound
	}
	return j, nil
}

func (f *fakeRepo) MarkRun(_ context.Context, id int64, status, _ string, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked = append(f.marked, next)
	j := f.jobs[id]
	j.LastStatus = status
	f.jobs[id] = j
	return nil
}

func (f *fakeRepo) RecordRun(_ context.Context, jobID int64, status string, _ int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, formatRun(jobID, status))
	return nil
}

func formatRun(id int64, status string) string {
	return itoa(id) + ":" + status
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func (f *fakeRepo) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

// fakeLocker 记录加锁次数，用来区分「本地 inflight 拦住」和「分布式锁拦住」。
type fakeLocker struct {
	mu     sync.Mutex
	locked map[string]int
	deny   bool
}

func newFakeLocker() *fakeLocker { return &fakeLocker{locked: map[string]int{}} }

func (l *fakeLocker) Acquire(_ context.Context, key string) (ReleaseFunc, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.deny {
		return nil, false
	}
	l.locked[key]++
	return nopRelease{}, true
}

type nopRelease struct{}

func (nopRelease) Release(context.Context) error { return nil }

func job(id int64, conc Concurrency) Job {
	return Job{
		ID:          id,
		Name:        "测试作业",
		Cron:        "*/5 * * * *",
		Command:     "noop",
		Concurrency: string(conc),
		TimeoutSec:  5,
		Enabled:     true,
	}
}

// 「上次没跑完就跳过」是默认策略，必须真的跳过而不是排队。
func TestLaunchSkipWhenInflight(t *testing.T) {
	repo := newFakeRepo(job(1, ConcurrencySkip))
	r := NewRunner(repo, newFakeLocker(), nil)

	started := make(chan struct{})
	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	r.Register("noop", func(context.Context) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		close(started)
		<-release
		return "ok", nil
	})

	j, _ := repo.Get(context.Background(), 1)
	r.launch(context.Background(), j)
	<-started

	// 第二次触发：此时 inflight 里还有 1
	r.launch(context.Background(), j)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("skip 策略下第二次不应执行，实际执行了 %d 次", got)
	}
	close(release)
}

// queue：上一次结束后，排队的这次必须真的被执行。
func TestLaunchQueueEventuallyRuns(t *testing.T) {
	repo := newFakeRepo(job(2, ConcurrencyQueue))
	r := NewRunner(repo, newFakeLocker(), nil)

	var calls int
	var mu sync.Mutex
	first := make(chan struct{})
	unblock := make(chan struct{})
	r.Register("noop", func(context.Context) (string, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			close(first)
			<-unblock
		}
		return "ok", nil
	})

	j, _ := repo.Get(context.Background(), 2)
	r.launch(context.Background(), j)
	<-first
	r.launch(context.Background(), j) // 排队

	mu.Lock()
	before := calls
	mu.Unlock()
	if before != 1 {
		t.Fatalf("排队时不应立即执行第二次，实际 %d", before)
	}

	close(unblock) // 放行第一次

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 {
			return // 排队的这次执行了
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("queue 策略下排队的作业始终没有执行")
}

// allow：允许并发，两次都要跑。
func TestLaunchAllowConcurrent(t *testing.T) {
	repo := newFakeRepo(job(3, ConcurrencyAllow))
	r := NewRunner(repo, newFakeLocker(), nil)

	var calls int
	var mu sync.Mutex
	gate := make(chan struct{})
	r.Register("noop", func(context.Context) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-gate
		return "ok", nil
	})

	j, _ := repo.Get(context.Background(), 3)
	r.launch(context.Background(), j)
	time.Sleep(30 * time.Millisecond)
	r.launch(context.Background(), j) // 应放行

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 {
			close(gate)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(gate)
	t.Fatal("allow 策略下第二次没有被放行")
}

// 分布式锁被别人持有时，本实例必须让路，不能重复执行。
func TestLaunchRespectsLocker(t *testing.T) {
	repo := newFakeRepo(job(4, ConcurrencyAllow))
	locker := newFakeLocker()
	locker.deny = true
	r := NewRunner(repo, locker, nil)

	var calls int
	var mu sync.Mutex
	r.Register("noop", func(context.Context) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return "ok", nil
	})

	j, _ := repo.Get(context.Background(), 4)
	r.launch(context.Background(), j)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Fatalf("拿不到锁时不应执行，实际执行了 %d 次", got)
	}
}

// 执行失败也要推进 next_run_at。
// 不推进的话，失败作业会在每个心跳被重新触发，变成事实上的死循环。
func TestExecuteAdvancesOnFailure(t *testing.T) {
	repo := newFakeRepo(job(5, ConcurrencySkip))
	r := NewRunner(repo, newFakeLocker(), nil)
	r.Register("noop", func(context.Context) (string, error) {
		return "", errors.New("故意的失败")
	})

	j, _ := repo.Get(context.Background(), 5)
	if _, err := r.execute(context.Background(), j, false); err == nil {
		t.Fatal("应返回错误")
	}

	repo.mu.Lock()
	marked := len(repo.marked)
	runs := append([]string{}, repo.runs...)
	repo.mu.Unlock()

	if marked == 0 {
		t.Fatal("失败后也必须推进 next_run_at，否则会变成死循环")
	}
	if len(runs) != 1 || runs[0] != "5:error" {
		t.Fatalf("应记录一条 error 执行历史，实际 %v", runs)
	}
}

// 命令没注册时要记 error 而不是 panic 或静默成功。
func TestExecuteUnknownCommand(t *testing.T) {
	repo := newFakeRepo(job(6, ConcurrencySkip))
	r := NewRunner(repo, newFakeLocker(), nil)

	j, _ := repo.Get(context.Background(), 6)
	if _, err := r.execute(context.Background(), j, false); err == nil {
		t.Fatal("未注册的命令应返回错误")
	}
	repo.mu.Lock()
	runs := append([]string{}, repo.runs...)
	repo.mu.Unlock()
	if len(runs) != 1 || runs[0] != "6:error" {
		t.Fatalf("应记录一条 error，实际 %v", runs)
	}
}

// tickOnce 遇到读库失败不能崩，要安静返回等下一次心跳。
func TestTickOnceSurvivesRepoError(t *testing.T) {
	repo := newFakeRepo()
	repo.dueErr = errors.New("数据库挂了")
	r := NewRunner(repo, newFakeLocker(), nil)
	r.tickOnce(context.Background()) // 不应 panic
}

// 永不触发的 cron 要被标成 never 而不是反复重试。
func TestAdvanceNeverFires(t *testing.T) {
	bad := job(7, ConcurrencySkip)
	bad.Cron = "0 0 30 2 *" // 2 月 30 日
	repo := newFakeRepo(bad)
	r := NewRunner(repo, newFakeLocker(), nil)
	r.Register("noop", func(context.Context) (string, error) { return "ok", nil })

	j, _ := repo.Get(context.Background(), 7)
	r.advance(context.Background(), j)

	repo.mu.Lock()
	got := repo.jobs[7]
	repo.mu.Unlock()
	if got.LastStatus != "never" {
		t.Fatalf("永不触发的 cron 应标记 never，实际 %q", got.LastStatus)
	}
}
