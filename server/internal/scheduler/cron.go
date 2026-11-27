// Package scheduler 承载定时作业。
//
// 不引外部调度框架（如 robfig/cron）的理由：
//   - 作业定义要落库、要能在页面上改、要能看到历史；
//     用库只能解决「算下一次时间」这一小步，剩下的还是得自己写。
//   - 五个字段的 cron 解析不到 150 行，测试成本低，且行为完全可控。
//
// 分布式约束：多个副本同时跑时，靠 Redis 锁保证同一作业同一时刻只有一个实例在执行。
package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule 是解析后的五段式 cron 表达式。
// 字段顺序与 Linux crontab 一致：分 时 日 月 周。
type Schedule struct {
	minute, hour, dom, month, dow uint64
	raw                           string
}

var (
	minuteRange = [2]int{0, 59}
	hourRange   = [2]int{0, 23}
	domRange    = [2]int{1, 31}
	monthRange  = [2]int{1, 12}
	dowRange    = [2]int{0, 6}
)

// ParseCron 解析五段式表达式。
//
// 支持的语法：`*`、`*/n`、`a`、`a-b`、`a-b/n`、逗号组合。
// 不支持 `@daily` 这类宏和秒级字段——支持面越小，行为越可预测。
func ParseCron(expr string) (*Schedule, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 必须是 5 段（分 时 日 月 周），实际 %d 段", len(fields))
	}
	ranges := [][2]int{minuteRange, hourRange, domRange, monthRange, dowRange}
	var bits [5]uint64
	for i, f := range fields {
		b, err := parseField(f, ranges[i][0], ranges[i][1])
		if err != nil {
			return nil, fmt.Errorf("第 %d 段 %q: %w", i+1, f, err)
		}
		bits[i] = b
	}
	return &Schedule{minute: bits[0], hour: bits[1], dom: bits[2], month: bits[3], dow: bits[4], raw: expr}, nil
}

// String 原始表达式。
func (s *Schedule) String() string { return s.raw }

func parseField(expr string, min, max int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(expr, ",") {
		if part == "" {
			return 0, fmt.Errorf("空段")
		}
		step := 1
		if idx := strings.Index(part, "/"); idx >= 0 {
			n, err := strconv.Atoi(part[idx+1:])
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("步长非法")
			}
			step = n
			part = part[:idx]
		}
		lo, hi := min, max
		switch {
		case part == "*":
			// 全范围
		case strings.Contains(part, "-"):
			segs := strings.SplitN(part, "-", 2)
			a, err1 := strconv.Atoi(segs[0])
			b, err2 := strconv.Atoi(segs[1])
			if err1 != nil || err2 != nil || a > b || a < min || b > max {
				return 0, fmt.Errorf("区间非法")
			}
			lo, hi = a, b
		default:
			v, err := strconv.Atoi(part)
			if err != nil || v < min || v > max {
				return 0, fmt.Errorf("数值 %q 超出范围 %d-%d", part, min, max)
			}
			lo, hi = v, v
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

// Next 计算 after 之后的下一次触发时间。
//
// 逐分钟向后找，最多找 366 天——超过就认定「这个表达式永远不会触发」
// （比如 2 月 30 日），返回零值让调用方跳过而不是死循环。
func (s *Schedule) Next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	deadline := t.AddDate(0, 0, 366)
	for t.Before(deadline) {
		if s.matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

func (s *Schedule) matches(t time.Time) bool {
	// Go 的 Sunday=0，和 crontab 一致，不需要换算
	return s.minute&(1<<uint(t.Minute())) != 0 &&
		s.hour&(1<<uint(t.Hour())) != 0 &&
		s.dom&(1<<uint(t.Day())) != 0 &&
		s.month&(1<<uint(int(t.Month()))) != 0 &&
		s.dow&(1<<uint(int(t.Weekday()))) != 0
}
