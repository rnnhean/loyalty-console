// Package exporter —— 列表数据导出。
//
// 为什么不在 handler 里直接 c.CSV() 返回：
//
//  1. **大结果集会撑爆内存**。10 万行 × 20 列的数据全量装进内存再序列化，
//     峰值可能几百 MB；而导出是低频操作，为它长期占着内存是浪费。
//     本实现流式写：查一批 → 编码一批 → 写一批，内存恒定在「一批」的大小。
//
//  2. **请求会超时**。网关层有 6s 超时，10 万行的导出远超这个时间。
//     所以走「提交任务 → 立即返回任务号 → 轮询状态 → 拿预签名 URL 下载」，
//     下载带宽由对象存储承担，不占应用连接。
//
//  3. **多实例下文件必须共享**。导出结果写本地磁盘，用户在另一个实例上
//     轮询就查不到。对象存储天然共享。
//
// 这就是为什么导出和 MinIO 是一组：单独有 MinIO 没有导出场景，或者
// 单独做导出还用本地文件，两者都不成立。
package exporter

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example/loyalty-console/server/internal/blob"
	"github.com/example/loyalty-console/server/internal/observ"
)

// Column 一列的定义。Value 从一行数据里取值。
//
// 用结构体而不是反射 + tag：反射在这个场景里既慢又难调试，
// 而且列顺序、表头文案这类运营需求改得比结构体字段频繁。
type Column[T any] struct {
	Header string
	Value  func(T) string
}

// Source 数据源。每次 Next 返回一批，返回空切片表示结束。
//
// 游标由调用方自己维护（通常是自增主键），而不是 OFFSET：
// 导出期间如果有新数据插入，OFFSET 分页会漏行或重复行；
// keyset 分页（WHERE id > lastID）不会。
type Source[T any] interface {
	Next(ctx context.Context) ([]T, error)
}

// FuncSource 用函数实现的 Source。
type FuncSource[T any] func(ctx context.Context) ([]T, error)

func (f FuncSource[T]) Next(ctx context.Context) ([]T, error) { return f(ctx) }

// Job 导出任务的状态快照。
type Job struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"` // queued / running / done / failed
	Rows      int64     `json:"rows"`
	URL       string    `json:"url,omitempty"` // done 时才有
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	DoneAt    time.Time `json:"done_at,omitempty"`
}

// Service 导出服务。
//
// 任务状态只存在进程内 map 里 —— 这是**有意识的简化**，
// 单实例部署够用；多实例要把状态放 Redis，否则轮询打到别的实例会查不到。
// 这里保留了 mutex，就是为了说明「为什么需要它」以及后来怎么替换。
type Service struct {
	store  *blob.Store
	logger *slog.Logger

	mu   sync.RWMutex
	jobs map[string]*Job

	// 并发闸门：导出吃 CPU 和 DB 连接，不限并发会把正常请求挤死。
	sem chan struct{}

	seq atomic.Int64
}

// NewService 构造。concurrency 是同时允许跑的导出任务数。
func NewService(store *blob.Store, logger *slog.Logger, concurrency int) *Service {
	if concurrency <= 0 {
		concurrency = 2
	}
	return &Service{
		store:  store,
		logger: logger,
		jobs:   make(map[string]*Job),
		sem:    make(chan struct{}, concurrency),
	}
}

// Submit 提交一个导出任务，立即返回任务快照。
//
// 任务在后台 goroutine 里跑。注意这里**不接收请求的 ctx**：
// 请求返回后 ctx 就被取消，而导出需要继续执行。
// 这是「异步任务」与「同步请求」在上下文管理上的本质区别，
// 也是最容易写错的地方。
func Submit[T any](s *Service, kind string, cols []Column[T], src Source[T]) Job {
	id := fmt.Sprintf("%s-%d", kind, s.seq.Add(1))
	now := time.Now()
	job := &Job{ID: id, Kind: kind, State: "queued", CreatedAt: now}

	s.mu.Lock()
	s.jobs[id] = job
	s.mu.Unlock()
	observ.ExportJobs.WithLabelValues("queued").Inc()

	go func() { // 每次状态变更都在锁内改，避免和 Get 里的读产生数据竞争。
		// 用独立的 ctx 并设上限：导出卡死时不能永远占着并发闸门。
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		s.set(job, func(j *Job) { j.State = "running" })
		observ.ExportJobs.WithLabelValues("running").Inc()

		s.sem <- struct{}{}
		rows, err := runExport(ctx, s, job, cols, src)
		<-s.sem

		if err != nil {
			s.set(job, func(j *Job) {
				j.State = "failed"
				j.Error = err.Error()
				j.DoneAt = time.Now()
			})
			observ.ExportJobs.WithLabelValues("failed").Inc()
			s.logger.Error("export_failed", "job", job.ID, "kind", kind, "err", err.Error())
			return
		}
		observ.ExportRows.Observe(float64(rows))
		s.set(job, func(j *Job) {
			j.State = "done"
			j.Rows = rows
			j.DoneAt = time.Now()
		})
		observ.ExportJobs.WithLabelValues("done").Inc()
		s.logger.Info("export_done", "job", job.ID, "kind", kind, "rows", rows)
	}()

	return job.snapshot()
}

// snapshot 在锁内拷贝一份，避免把内部指针交给调用方后被并发改写。
func (j *Job) snapshot() Job { return *j }

// runExport 流式导出。
//
// 关键点：用 io.Pipe 把「CSV 编码」和「上传」接起来。
// 不用 bytes.Buffer —— 那等于把整个文件攒在内存里，前面的设计就白做了。
// Pipe 的写端阻塞时，上游自然减速，天然形成背压。
//
// 写成独立的泛型函数而不是 Service 的方法：Go 不允许方法带类型参数。
func runExport[T any](ctx context.Context, s *Service, job *Job, cols []Column[T], src Source[T]) (int64, error) {
	pr, pw := io.Pipe()

	// 编码 goroutine 要把「行数」回传给调用方，所以结果结构里带 rows。
	type encodeResult struct {
		rows int64
		err  error
	}
	done := make(chan encodeResult, 1)

	go func() {
		var total int64
		// 统一出口：任何一步失败都 CloseWithError，让上传端感知并中止
		bail := func(err error) {
			done <- encodeResult{rows: total, err: err}
			_ = pw.CloseWithError(err)
		}

		w := csv.NewWriter(pw)
		header := make([]string, len(cols))
		for i, c := range cols {
			header[i] = c.Header
		}
		if err := w.Write(header); err != nil {
			bail(err)
			return
		}

		row := make([]string, len(cols))
		for {
			if err := ctx.Err(); err != nil {
				bail(err)
				return
			}
			items, err := src.Next(ctx)
			if err != nil {
				bail(err)
				return
			}
			if len(items) == 0 {
				break
			}
			for _, it := range items {
				for i, c := range cols {
					row[i] = c.Value(it)
				}
				if err := w.Write(row); err != nil {
					bail(err)
					return
				}
				total++
			}
			// 攒批 flush，避免每行都触发一次管道写
			w.Flush()
			if err := w.Error(); err != nil {
				bail(err)
				return
			}
		}
		w.Flush()
		done <- encodeResult{rows: total, err: w.Error()}
		_ = pw.Close()
	}()

	// -1 让 SDK 走分片上传：我们事先不知道总大小
	key := blob.ExportKey(job.ID)
	putErr := s.store.Put(ctx, s.store.ExportBucket(), key, pr, -1, "text/csv; charset=utf-8")

	// 上传失败时 pipe 的读端要关掉，否则编码 goroutine 会永远阻塞在写端
	if putErr != nil {
		_ = pr.CloseWithError(putErr)
	}
	res := <-done
	if putErr != nil {
		return res.rows, putErr
	}
	if res.err != nil {
		return res.rows, res.err
	}

	url, err := s.store.PresignGet(ctx, s.store.ExportBucket(), key)
	if err != nil {
		return res.rows, err
	}
	s.set(job, func(j *Job) { j.URL = url })
	return res.rows, nil
}

func (s *Service) set(job *Job, fn func(*Job)) {
	s.mu.Lock()
	fn(job)
	s.mu.Unlock()
}

// Get 查询任务状态。
func (s *Service) Get(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	return j.snapshot(), true
}

// Cleanup 清掉超过 maxAge 的已完成任务记录，返回清理条数。
//
// 长跑进程必须做这件事：任务记录只增不删，内存会一直涨。
// 这属于「自己写的东西自己收拾」，不能指望 GC。
func (s *Service) Cleanup(maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, j := range s.jobs {
		if j.State == "done" || j.State == "failed" {
			if j.DoneAt.Before(cutoff) {
				delete(s.jobs, id)
				n++
			}
		}
	}
	return n
}

// str 常用取值转换，避免每个 Column 都写一遍。
func Str[T any](f func(T) string) func(T) string { return f }

// Int 把整数列转成字符串。
func Int[T any](f func(T) int) func(T) string {
	return func(v T) string { return strconv.Itoa(f(v)) }
}

// Int64 把 int64 列转成字符串。
func Int64[T any](f func(T) int64) func(T) string {
	return func(v T) string { return strconv.FormatInt(f(v), 10) }
}

// Money 金额列，保留两位小数。
func Money[T any](f func(T) float64) func(T) string {
	return func(v T) string { return strconv.FormatFloat(f(v), 'f', 2, 64) }
}
