package scheduler

import (
	"testing"
	"time"
)

func TestParseCronValid(t *testing.T) {
	good := []string{
		"* * * * *",
		"0 * * * *",
		"*/5 * * * *",
		"0 9 * * 1-5",
		"30 2,14 * * *",
		"0 0 1 */3 *",
		"15 3-8/2 * * *",
	}
	for _, s := range good {
		if _, err := ParseCron(s); err != nil {
			t.Fatalf("%q 应合法，实际 %v", s, err)
		}
	}
}

func TestParseCronInvalid(t *testing.T) {
	bad := []string{
		"",
		"* * * *",        // 段数不够
		"* * * * * *",    // 段数过多
		"60 * * * *",     // 分钟越界
		"* 24 * * *",     // 小时越界
		"* * 0 * *",      // 日从 1 开始
		"* * * 13 *",     // 月越界
		"*/0 * * * *",    // 步长为 0
		"5-1 * * * *",    // 区间倒置
		"a * * * *",      // 非数字
		"* * * * 7",      // 周只到 6
	}
	for _, s := range bad {
		if _, err := ParseCron(s); err == nil {
			t.Fatalf("%q 应被拒绝", s)
		}
	}
}

func TestCronNextBasic(t *testing.T) {
	sch, err := ParseCron("0 9 * * *") // 每天 9:00
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 27, 10, 30, 0, 0, time.Local)
	got := sch.Next(base)
	want := time.Date(2026, 11, 28, 9, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("期望 %v 实际 %v", want, got)
	}
}

func TestCronNextEveryFiveMinutes(t *testing.T) {
	sch, _ := ParseCron("*/5 * * * *")
	base := time.Date(2026, 11, 27, 10, 31, 0, 0, time.Local)
	got := sch.Next(base)
	want := time.Date(2026, 11, 27, 10, 35, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("期望 %v 实际 %v", want, got)
	}
}

func TestCronNextWeekdayOnly(t *testing.T) {
	sch, _ := ParseCron("0 9 * * 1-5")
	// 2026-11-28 是周六，下一次应是周一 11-30 09:00
	base := time.Date(2026, 11, 28, 10, 0, 0, 0, time.Local)
	got := sch.Next(base)
	if got.Weekday() != time.Monday {
		t.Fatalf("应跳到周一，实际 %v (%v)", got, got.Weekday())
	}
	if got.Hour() != 9 || got.Minute() != 0 {
		t.Fatalf("时间应为 09:00，实际 %v", got)
	}
}

// 永远不会触发的表达式必须返回零值，而不是死循环找下去。
func TestCronNextImpossibleDateReturnsZero(t *testing.T) {
	sch, err := ParseCron("0 0 30 2 *") // 2 月 30 日
	if err != nil {
		t.Fatal(err)
	}
	got := sch.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local))
	if !got.IsZero() {
		t.Fatalf("应返回零值，实际 %v", got)
	}
}
