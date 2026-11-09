package gateway

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 限流器的测试。
//
// ★ 先说清楚这里的**能力边界**：本机 CGO_ENABLED=0 且没有 gcc，
//   `go test -race` 跑不起来。所以下面的用例证明的是「并发下的**行为**
//   是对的」（放行总数不超限、桶互不干扰），**不能**证明「没有数据竞争」。
//   要真正抓数据竞争，装 mingw-w64 后执行：
//
//       CGO_ENABLED=1 go test -race ./internal/gateway/
//
//   在那之前，「RateLimiter 的 map 读写是安全的」这条结论只来自
//   `go vet` 和代码评审，没有工具背书。这是已知缺口，不是已证明的事。

// TestRateLimiterBurstCap 并发打同一个 key：放行数不能超过 burst。
//
// 这是令牌桶最基本的契约。写错了（比如在锁外判断）的表现就是
// 并发下放行数超过 burst，配额形同虚设。
func TestRateLimiterBurstCap(t *testing.T) {
	const (
		burst  = 10
		gorout = 200
	)
	// rps 设成 0：令牌只靠初始 burst，不补充。
	// 否则测试跑得慢一点，桶里又攒出令牌，结果就不确定了。
	rl := NewRateLimiter(0, burst)
	defer rl.Close()

	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{}) // 让所有 goroutine 尽可能同时冲进去

	for i := 0; i < gorout; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if rl.Allow("same-key") {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != burst {
		t.Fatalf("同一 key 并发 %d 次，放行 %d 次；burst=%d，期望恰好 %d 次",
			gorout, got, burst, burst)
	}
}

// TestRateLimiterIsolation 不同 key 的桶必须互不影响。
//
// 这条防的是一个具体的实现错误：把 key 忘了传进 map，
// 或者所有请求共用一个桶。那样表现出来就是「A 把配额吃光，
// B 一个都进不去」—— 按业务 ID 限流会退化成全局限流，
// 正是一个人刷爆一个 ID 就把全站打死。
func TestRateLimiterIsolation(t *testing.T) {
	const burst = 5
	rl := NewRateLimiter(0, burst)
	defer rl.Close()

	// 先把 keyA 打空
	for i := 0; i < burst; i++ {
		if !rl.Allow("keyA") {
			t.Fatalf("keyA 第 %d 次就被拒了，burst=%d", i+1, burst)
		}
	}
	if rl.Allow("keyA") {
		t.Fatal("keyA 已打满却仍放行")
	}

	// keyB 必须是满额度的
	for i := 0; i < burst; i++ {
		if !rl.Allow("keyB") {
			t.Fatalf("keyA 打满后 keyB 第 %d 次被拒，说明两个 key 共用了一个桶", i+1)
		}
	}
}

// TestRateLimiterRefill 令牌会随时间恢复。
func TestRateLimiterRefill(t *testing.T) {
	// 20 rps、burst 1：打完 1 个令牌后，50ms 应该恢复 1 个。
	rl := NewRateLimiter(20, 1)
	defer rl.Close()

	if !rl.Allow("k") {
		t.Fatal("初始令牌没放行")
	}
	if rl.Allow("k") {
		t.Fatal("burst=1 却放行了第 2 次")
	}

	// 等足够久（100ms ≈ 2 个令牌的补充时间），必须恢复
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rl.Allow("k") {
			return // 恢复了
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("2 秒内令牌没有恢复，rps 可能没生效")
}

// TestRateLimiterJanitor 后台清理必须真的删掉闲置的桶。
//
// 为什么这条重要：按业务 ID 限流的 key 空间和业务表一样大。
// janitor 是内存不泄漏的**唯一**保障，它一旦失效，进程会随访问过的
// 不同 ID 数量线性涨内存，而且没有任何报错 —— 直到 OOM 才暴露。
//
// 这里直接操作内部 map 而不是等 5 分钟：等不起，也不该让测试依赖真实时间。
func TestRateLimiterJanitor(t *testing.T) {
	rl := NewRateLimiter(10, 1)
	defer rl.Close()

	// 造 100 个桶
	for i := 0; i < 100; i++ {
		rl.Allow(fmt.Sprintf("k%d", i))
	}

	rl.mu.RLock()
	n := len(rl.buckets)
	rl.mu.RUnlock()
	if n != 100 {
		t.Fatalf("应有 100 个桶，实际 %d", n)
	}

	// 手动把所有桶的 seen 时间拨到很久以前，模拟闲置
	rl.mu.Lock()
	for _, b := range rl.buckets {
		b.seen = time.Now().Add(-time.Hour)
	}
	rl.mu.Unlock()

	// janitor 的清理条件是 now.Sub(b.seen) > 3*every。
	// 用一个极小的 every 直接调一次，验证判定逻辑本身。
	rl.janitorOnce(10 * time.Millisecond)

	rl.mu.RLock()
	n = len(rl.buckets)
	rl.mu.RUnlock()
	if n != 0 {
		t.Fatalf("闲置超过 3*every 的桶没被清理，还剩 %d 个（内存会泄漏）", n)
	}
}
