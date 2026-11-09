package outbox

import (
	"testing"
	"time"
)

// 退避序列的测试。
//
// 为什么退避算法值得单测：它算错不会报错，只会让「下游恢复后多久能追上」
// 这个时间悄悄变长或变短。算得太久 → 下游修好了还在干等；
// 算得太短 → 下游已经挂了还被重试风暴再踩一脚。
// 这类问题在 E2E 里要等几十分钟才观察得到，所以必须在纯函数层面钉死。

func TestBackoffForSequence(t *testing.T) {
	cases := []struct {
		retry int // 已失败次数（retry_count 当前值）
		want  time.Duration
	}{
		// retry_count 初值 0，第一次失败时 next=1 → 2^1 = 2s
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{3, 16 * time.Second},
		{4, 32 * time.Second},
		// 2^6 = 64s > 60s，从这里开始封顶
		{5, time.Minute},
		{6, time.Minute},
		{10, time.Minute},
		{49, time.Minute},
	}

	for _, c := range cases {
		if got := backoffFor(c.retry); got != c.want {
			t.Errorf("backoffFor(%d) = %v, 期望 %v", c.retry, got, c.want)
		}
	}
}

// 退避必须单调不减：重试次数越多，等待时间不能变短。
//
// 这条比逐点比对更强 —— 将来有人改了公式（比如换成带抖动的），
// 只要单调性还在就不会打破「越失败等越久」这个契约。
func TestBackoffMonotonic(t *testing.T) {
	prev := time.Duration(-1)
	for retry := 0; retry < 100; retry++ {
		got := backoffFor(retry)
		if got < prev {
			t.Fatalf("退避不单调：retry=%d 得 %v，比 retry=%d 的 %v 还短",
				retry, got, retry-1, prev)
		}
		prev = got
	}
}

// 永远不能返回 <= 0 的退避值。
//
// ★ 这条是真的会踩的坑：Go 里 `1 << n` 当 n 超过类型位宽时，
//   常量表达式会编译失败，但**变量**会得到 0。一旦 maxRetry 被调到
//   63 以上，高位那几次重试算出的 backoff = 0，next_retry_at 就等于
//   当前时间 —— 退避彻底失效，变成紧凑重试风暴，比不重试还糟。
//
// backoffFor 里对 next >= 63 做了显式封顶。这条测试就是那个封顶的哨兵。
func TestBackoffNeverZero(t *testing.T) {
	for retry := 0; retry < 200; retry++ {
		got := backoffFor(retry)
		if got <= 0 {
			t.Fatalf("backoffFor(%d) = %v，退避失效会变成重试风暴", retry, got)
		}
		if got > backoffCap {
			t.Fatalf("backoffFor(%d) = %v，超过了封顶 %v", retry, got, backoffCap)
		}
	}
}

// truncate 按 rune 截断，不能把中文切出半个字符。
//
// last_error 会写进数据库、也会被读出来打日志。切坏的表现是
// 日志和 Grafana 上是乱码 —— 不影响功能，但排查问题时会误导人。
func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"abcdef", 6, "abcdef"},
		// 中文：3 个字符 = 9 字节，按 rune 截 2 个
		{"中文测试", 2, "中文"},
		{"中文测试", 10, "中文测试"},
		{"", 5, ""},
		{"abc", 0, ""},
	}

	for _, c := range cases {
		if got := truncate(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, 期望 %q", c.in, c.n, got, c.want)
		}
	}
}

// 截断后的结果必须是合法 UTF-8 —— 这是 truncate 用 rune 而不是字节的**全部理由**。
func TestTruncateValidUTF8(t *testing.T) {
	// 造一个很长的中文串，逐个长度截断，每一次结果都必须是合法 UTF-8
	long := ""
	for i := 0; i < 100; i++ {
		long += "错"
	}
	for n := 0; n <= 10; n++ {
		got := truncate(long, n)
		for _, r := range got {
			if r == '�' {
				t.Fatalf("truncate(中文, %d) 产生了非法字符，长度 %d", n, len(got))
			}
		}
		if len([]rune(got)) != n {
			t.Fatalf("truncate(中文, %d) 截出 %d 个字符，期望 %d", n, len([]rune(got)), n)
		}
	}
}
