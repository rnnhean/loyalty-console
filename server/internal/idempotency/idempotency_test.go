package idempotency

import (
	"strings"
	"sync"
	"testing"
)

// Fingerprint 的测试。
//
// 它算错的表现极度隐蔽：两个**不同**的操作算出同一个 key，
// 于是第二个请求被当成第一个的重放，直接返回缓存结果 ——
// 用户点"给 A 发券"，系统返回"给 B 发券"的成功响应。
// 没有报错、没有日志异常，只有账对不上。
//
// 所以这里的重点不是"能算出东西"，而是**不同的输入必须算出不同的 key**，
// 以及**分隔符不能让不同的分段拼出同一个串**。

// 基本契约：同样的输入永远得到同样的 key（幂等的前提）。
func TestFingerprintDeterministic(t *testing.T) {
	a := Fingerprint("admin", "expire-rule", "10103")
	b := Fingerprint("admin", "expire-rule", "10103")
	if a != b {
		t.Fatalf("相同输入算出不同 key：%s vs %s", a, b)
	}
	if len(a) != 32 {
		t.Fatalf("key 长度应为 32，实际 %d", len(a))
	}
}

// 任一字段变化都必须改变 key。
//
// 这是幂等键的**核心语义**：key 必须唯一标识"一个操作"。
// 漏了某个字段，那两件不同的事就会被合并。
func TestFingerprintSensitiveToEveryPart(t *testing.T) {
	base := Fingerprint("admin", "expire-rule", "10103")

	mutations := map[string]string{
		"换了操作人":  Fingerprint("other", "expire-rule", "10103"),
		"换了动作":   Fingerprint("admin", "grant-rule", "10103"),
		"换了目标":   Fingerprint("admin", "expire-rule", "10104"),
		"少一个字段":  Fingerprint("admin", "expire-rule"),
		"多一个字段":  Fingerprint("admin", "expire-rule", "10103", "extra"),
		"字段顺序颠倒": Fingerprint("expire-rule", "admin", "10103"),
	}
	for name, got := range mutations {
		if got == base {
			t.Errorf("%s 后 key 没变，两个不同操作会被当成同一个", name)
		}
	}
}

// ★ 分隔符的存在理由：不加分隔符，"ab"+"c" 和 "a"+"bc" 会拼成同一个串。
//
// 这是拼接哈希最经典的碰撞来源。实现里每个字段后都写了一个 0x00 字节，
// 就是为了让分段边界明确。这条测试把那行代码的作用钉死 ——
// 将来有人觉得"多写个 0 没用"删掉它，这里会红。
func TestFingerprintNoConcatCollision(t *testing.T) {
	pairs := [][2][]string{
		{{"ab", "c"}, {"a", "bc"}},
		{{"admin", "x"}, {"admi", "nx"}},
		{{"a", "b", "c"}, {"ab", "c"}},
		{{"a", "bc"}, {"a", "b", "c"}},
	}

	for _, p := range pairs {
		a := Fingerprint(p[0]...)
		b := Fingerprint(p[1]...)
		if a == b {
			t.Fatalf("%v 和 %v 算出了同一个 key %s，说明分段没被分隔",
				p[0], p[1], a)
		}
	}
}

// 空字段不能被忽略：("a", "", "b") 和 ("a", "b") 必须是两个 key。
//
// 现实里空值很常见（比如 operator 没取到），如果空字段被跳过，
// 就会和"少传一个字段"混为一谈。
func TestFingerprintEmptyPart(t *testing.T) {
	a := Fingerprint("a", "", "b")
	b := Fingerprint("a", "b")
	c := Fingerprint("a", "", "", "b")
	if a == b || a == c || b == c {
		t.Fatal("空字段被吞掉了，不同的字段结构算出了同一个 key")
	}
}

// 输出必须是 32 位小写十六进制 —— 它就是 Redis key 的一部分。
//
// 带来乱码或大写不一致会让「写进去的 key」和「查的时候的 key」对不上，
// 表现成幂等完全失效（每次都当成新请求）。
func TestFingerprintFormat(t *testing.T) {
	got := Fingerprint("admin", "expire-rule", "10103")
	const hex = "0123456789abcdef"
	for _, r := range got {
		if !strings.ContainsRune(hex, r) {
			t.Fatalf("key 含非小写十六进制字符 %q: %s", r, got)
		}
	}
}

// 并发调用必须稳定（Fingerprint 应当是无状态的纯函数）。
func TestFingerprintConcurrent(t *testing.T) {
	want := Fingerprint("admin", "expire-rule", "10103")
	var wg sync.WaitGroup
	errs := make(chan string, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := Fingerprint("admin", "expire-rule", "10103"); got != want {
				errs <- got
			}
		}()
	}
	wg.Wait()
	close(errs)
	for got := range errs {
		t.Errorf("并发调用算出不一致的 key：%s，期望 %s", got, want)
	}
}
