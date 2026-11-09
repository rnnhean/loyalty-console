package gateway

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// safePath 是「凭据不出门」这条底线的最后一道闸。
//
// 为什么这个函数的测试优先级高于业务逻辑：它出错的表现不是报错、
// 不是 500，而是**悄悄**把 ?token=xxx 写进日志、写进 Prometheus 标签，
// 然后日志落盘、标签被 Grafana 拉走。等发现时已经跟着备份、跟着
// 分享出去的截图散了一圈，删不干净。
//
// 所以这里的用例不是「覆盖分支」，而是**逐条复现真实出现过的泄漏形态**。
func TestSafePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// ── 必须丢掉的：query 里混凭据 ──
		{
			name: "query 带 token",
			in:   "/trade?order=1&_t=SECRET123",
			want: "/trade",
		},
		{
			name: "完整 URL 的 query 带 token",
			in:   "http://localhost:5173/catalog/spu?token=abc",
			want: "/catalog/spu",
		},
		{
			name: "query 带 password",
			in:   "/user?password=hunter2",
			want: "/user",
		},
		{
			name: "query 带中文参数也要丢",
			in:   "/search?q=张三&sid=deadbeef",
			want: "/search",
		},

		// ── 必须丢掉的：hash 里藏 token ──
		// OAuth 隐式流会把 access_token 放在 # 后面，只剥 ? 是不够的。
		{
			name: "hash 带 access_token",
			in:   "http://x/settings#access_token=zzz",
			want: "/settings",
		},
		{
			name: "query 和 hash 同时存在",
			in:   "/p?a=1#b=2",
			want: "/p",
		},

		// ── 正常路径必须原样保留 ──
		{
			name: "纯路径",
			in:   "/a/b/c",
			want: "/a/b/c",
		},
		{
			name: "无 query 的完整 URL",
			in:   "https://example.com/catalog/spu",
			want: "/catalog/spu",
		},

		// ── 边界 ──
		{
			name: "空串",
			in:   "",
			want: "",
		},
		{
			// 只有 host 没有 path：应该退化为 "/"，不能返回空
			// （空 path 会变成一个奇怪的标签值，影响 PV 按页面聚合）
			name: "只有 host",
			in:   "https://example.com",
			want: "/",
		},
		{
			name: "只有 host 带 query",
			in:   "https://example.com?token=x",
			want: "/",
		},
		{
			name: "只有 query 没有 path",
			in:   "?token=x",
			want: "/",
		},
		{
			// 前面已经修过的回归：https://example.com?token=x 曾切成空串
			name: "已知回归：host+query 不能切成空串",
			in:   "https://example.com?token=x",
			want: "/",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := safePath(c.in)
			if got != c.want {
				t.Fatalf("safePath(%q) = %q, 期望 %q", c.in, got, c.want)
			}
			// ★ 额外的不变式：任何情况下结果里都不能出现 ? # 或 =。
			// 这条比逐条比对更强 —— 将来有人改了实现、加了个新 case 漏了，
			// 只要它把凭据带出来了这里就会红。
			if strings.ContainsAny(got, "?#=") {
				t.Fatalf("safePath(%q) = %q，结果里仍带 query/hash 特征字符", c.in, got)
			}
			// ★ 第二条不变式：输入非空时，结果不能是空串。
			// 空 path 会变成一个独立的标签值，污染 PV 按页面聚合。
			if c.in != "" && got == "" {
				t.Fatalf("safePath(%q) = %q，非空输入切出了空 path", c.in, got)
			}
		})
	}
}

// 长度截断：超长 URL 必须被截断，而且必须在**字符边界**上。
func TestSafePathTruncate(t *testing.T) {
	// 纯 ASCII 超长路径
	long := "/" + strings.Repeat("a", maxURLStored+100)
	got := safePath(long)
	if len(got) != maxURLStored {
		t.Fatalf("超长 ASCII 路径应截断到 %d，实际 %d", maxURLStored, len(got))
	}

	// 中文路径：按字节截断会切出半个字符，写进日志就是乱码。
	// 这里用**恰好**踩在边界上的长度，而不是随便一个超长串 ——
	// 只有让第 maxURLStored 个字节落在一个 3 字节字符的中间，
	// 才真的能观察到切坏的效果。
	t.Run("中文超长", func(t *testing.T) {
		// "/" 占 1 字节，剩 maxURLStored-1 字节放下标。
		// maxURLStored-1 若不整除 3，最后一个"中"必然被切开。
		n := (maxURLStored - 1) / 3
		zh := "/" + strings.Repeat("中", n+1)
		got := safePath(zh)
		if len(got) > maxURLStored {
			t.Fatalf("未被截断：%d > %d", len(got), maxURLStored)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("截断点落在字符中间，切出了非法 UTF-8（长度 %d）：%q", len(got), got)
		}
		// 边界必须真的被用到：退到字符边界后最多只损失一个字符的字节数（≤3），
		// 如果退得更多，说明实现变成了「整串扫描截断」而不是「退到边界」。
		if loss := maxURLStored - len(got); loss > 3 {
			t.Fatalf("截断退得过多：%d 字节（上限 %d，实际 %d）", loss, maxURLStored, len(got))
		}
	})

	// 反向用例：刚好不需要回退时，必须一个字节都不浪费。
	t.Run("中文恰好对齐", func(t *testing.T) {
		// 让总长度正好等于 maxURLStored 且落在字符边界上。
		// maxURLStored = 256，前缀 "/" 1 字节 → 剩 255 字节 = 85 个汉字。
		zh := "/" + strings.Repeat("中", (maxURLStored-1)/3)
		zh += strings.Repeat("a", maxURLStored-len(zh))
		got := safePath(zh)
		if len(got) != maxURLStored {
			t.Fatalf("边界对齐时应保留 %d 字节，实际 %d", maxURLStored, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("对齐场景切出了非法 UTF-8：%q", got)
		}
	})

	// 四字节字符（emoji）也要在边界上，2/3 字节的判据不能写死。
	t.Run("四字节字符", func(t *testing.T) {
		emoji := "😀" // U+1F600，4 字节
		zh := "/" + strings.Repeat(emoji, maxURLStored)
		got := safePath(zh)
		if !utf8.ValidString(got) {
			t.Fatalf("四字节字符被切断：%q", got)
		}
		if len(got) > maxURLStored {
			t.Fatalf("未截断：%d > %d", len(got), maxURLStored)
		}
	})
}
