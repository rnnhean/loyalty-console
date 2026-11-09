// Package outbox —— 事务性发件箱。
//
// 它解决的是一类跨系统的原子性问题：业务数据在 MySQL，事件要去 Kafka，
// 两个系统没有共同事务。直写的两种顺序都错：
//
//	先写库再发消息 → 「库改了、消息没发出去」，下游永远不知道这件事发生了；
//	先发消息再写库 → 「消息说订单成了、库其实回滚了」，下游拿着假事件做对账。
//
// 发件箱把「发消息」这个动作本身落成一行数据库记录，于是它和业务写
// 共享同一个事务：业务回滚，消息记录也回滚。真正投递交给独立协程，
// 失败了就按退避重试，进程重启也不会丢进度（进度在 next_retry_at 上）。
//
// 代价要讲清楚：投递至少一次，不是恰好一次。所以消费侧仍然必须去重 ——
// 发件箱和消费去重是配套的，只做一个等于没做。
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/example/loyalty-console/server/internal/mq"
	"github.com/example/loyalty-console/server/internal/observ"
)

// 状态机。数字直接落库，所以改动要配迁移。
const (
	StatePending = 0 // 待发
	StateSent    = 1 // 已发
	StateDropped = 2 // 超过重试上限，转人工
)

// Publisher 只依赖「能发」这一个方法，便于测试里换成内存实现。
type Publisher interface {
	Publish(ctx context.Context, key string, evt mq.Event) error
}

// Store 发件箱仓储 + 补发协程。
type Store struct {
	db       *sql.DB
	logger   *slog.Logger
	maxRetry int
	scanSize int
}

// NewStore maxRetry 是重试上限，超过就置为 dropped 并告警，
// 不再无限重试 —— 永远发不出去的消息留在队列里只会拖慢后续扫描。
//
// 上限要配得上「下游能停多久」。按 1 分钟退避封顶算，50 次约等于
// 给下游留了近一小时的自愈窗口；8 次配 64 秒退避只能扛三分钟，
// 一次 Kafka 滚动重启就能把正常事件判死 —— 这个组合是错的。
func NewStore(db *sql.DB, logger *slog.Logger, maxRetry int) *Store {
	if maxRetry <= 0 {
		maxRetry = 50
	}
	return &Store{db: db, logger: logger, maxRetry: maxRetry, scanSize: 100}
}

// Enqueue 在业务事务里登记一条待发事件。
//
// 必须传调用方的事务句柄，不能用 db —— 用 db 就等于换了个连接开新事务，
// 原子性当场消失，而这个错误在代码上完全看不出来。
func (s *Store) Enqueue(ctx context.Context, tx *sql.Tx, topic, key string, evt mq.Event) error {
	raw, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal outbox event: %w", err)
	}
	// uk_outbox_event 让「同一业务事务里重复登记同一事件」直接失败，
	// 而不是在投递阶段才发现重复。
	_, err = tx.ExecContext(ctx,
		`INSERT INTO msg_outbox (event_id, topic, partition_key, payload, state, next_retry_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		evt.ID, topic, key, raw, StatePending, time.Now())
	if err != nil {
		return fmt.Errorf("enqueue outbox: %w", err)
	}
	return nil
}

// Run 启动补发循环，ctx 取消时退出。
//
// 用「定时扫描 + 立即重扫」而不是长连接阻塞：发件箱的意义就是
// 不依赖下游可达性，所以它的驱动必须是最原始的轮询。
func (s *Store) Run(ctx context.Context, pub Publisher, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.flush(ctx, pub)
		}
	}
}

func (s *Store) flush(ctx context.Context, pub Publisher) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT out_uid, event_id, topic, partition_key, payload, retry_count
		   FROM msg_outbox
		  WHERE state = ? AND next_retry_at <= ?
		  ORDER BY out_uid
		  LIMIT ?`,
		StatePending, time.Now(), s.scanSize)
	if err != nil {
		// 扫表失败不致命，下一轮再来；但必须记日志，否则发件箱静默停摆。
		s.logger.Warn("outbox_scan_failed", "err", err.Error())
		return
	}

	type item struct {
		uid     int64
		eventID string
		topic   string
		key     string
		payload []byte
		retry   int
	}
	batch := make([]item, 0, s.scanSize)
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.uid, &it.eventID, &it.topic, &it.key, &it.payload, &it.retry); err != nil {
			s.logger.Warn("outbox_scan_decode_failed", "err", err.Error())
			continue
		}
		batch = append(batch, it)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.Warn("outbox_scan_iter_failed", "err", err.Error())
	}

	for _, it := range batch {
		if ctx.Err() != nil {
			return
		}
		var evt mq.Event
		if err := json.Unmarshal(it.payload, &evt); err != nil {
			// 载荷都解析不了，重试多少遍都一样，直接放弃。
			s.markDropped(ctx, it.uid, "payload decode: "+err.Error())
			observ.OutboxFlushed.WithLabelValues("dropped").Inc()
			continue
		}
		if err := pub.Publish(ctx, it.key, evt); err != nil {
			s.markRetry(ctx, it.uid, it.retry, err)
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE msg_outbox SET state = ? WHERE out_uid = ?`, StateSent, it.uid); err != nil {
			// 发都发出去了、标记失败：下一轮会再发一遍。
			// 接受重复而不是假装成功 —— 消费侧去重能兜住。
			s.logger.Warn("outbox_mark_sent_failed", "uid", it.uid, "err", err.Error())
		}
		observ.OutboxFlushed.WithLabelValues("ok").Inc()
	}

	// 顺便刷新积压量。单独查一次比在循环里累加更准，因为并发写入
	// 会让循环内的计数立刻过期。
	var pending int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM msg_outbox WHERE state = ?`, StatePending).Scan(&pending); err == nil {
		observ.OutboxBacklog.Set(float64(pending))
	}
}

// backoffCap 退避上限。
//
// 封顶不能太高 —— 封顶值决定「下游恢复后多久能追上」。
// 5 分钟封顶意味着下游修好后第一件事还是等 5 分钟。
const backoffCap = time.Minute

// backoffFor 返回第 retry+1 次重试前应该等多久。
//
// 序列（retry = 已失败次数）：
//
//	retry=0 → 2s    retry=1 → 4s    retry=2 → 8s
//	retry=3 → 16s   retry=4 → 32s   retry>=5 → 60s（封顶）
//
// 起点是 2s 而不是 1s：retry_count 初值为 0，第一次失败时 next=1，
// 所以 1<<next 最小就是 2。这里刻意保留这个行为，只把注释写对 ——
// 改起步值会改变线上重试节奏，属于行为变更，不该混在「补测试」里做。
//
// ★★ 溢出有两处，不在同一层，只挡一处是不够的：
//
//	第一处  `1 << n` 本身。n >= 位宽时变量版移位得 0，退避变成立即重试。
//	第二处  秒 → 纳秒的乘法。time.Duration 是 int64 纳秒，
//	        1<<33 秒 = 8.6e9 秒 ≈ 8.6e18 纳秒，已经逼近 int64 上限
//	        (9.2e18)，retry=33 时**变负数**。负数退避写进 next_retry_at
//	        等于「立刻重试」，比不设退避还糟 —— 而且它不会报错，
//	        只会安静地变成重试风暴。
//
// 所以这里的判据是**秒数上界**，不是移位宽度：只要秒数已经超过封顶
// 就直接返回封顶值，压根不做那次乘法。这个条件比 63 严得多，两层溢出一次挡掉。
func backoffFor(retry int) time.Duration {
	next := retry + 1
	// 封顶是 60 秒；只要 2^next 已经不可能小于封顶，就无需计算。
	// 1<<6 = 64 > 60，所以 next >= 6 一律封顶。
	if next >= 6 {
		return backoffCap
	}
	backoff := time.Duration(1<<uint(next)) * time.Second
	if backoff > backoffCap {
		backoff = backoffCap
	}
	return backoff
}

func (s *Store) markRetry(ctx context.Context, uid int64, retry int, cause error) {
	next := retry + 1
	if next >= s.maxRetry {
		s.markDropped(ctx, uid, cause.Error())
		observ.OutboxFlushed.WithLabelValues("dropped").Inc()
		s.logger.Error("outbox_dropped", "uid", uid, "retry", next, "err", cause.Error())
		return
	}
	backoff := backoffFor(retry)
	_, err := s.db.ExecContext(ctx,
		`UPDATE msg_outbox
		    SET retry_count = ?, next_retry_at = ?, last_error = ?
		  WHERE out_uid = ?`,
		next, time.Now().Add(backoff), truncate(cause.Error(), 250), uid)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Warn("outbox_mark_retry_failed", "uid", uid, "err", err.Error())
	}
	observ.OutboxFlushed.WithLabelValues("retry").Inc()
}

func (s *Store) markDropped(ctx context.Context, uid int64, reason string) {
	_, err := s.db.ExecContext(ctx,
		`UPDATE msg_outbox SET state = ?, last_error = ? WHERE out_uid = ?`,
		StateDropped, truncate(reason, 250), uid)
	if err != nil {
		s.logger.Warn("outbox_mark_dropped_failed", "uid", uid, "err", err.Error())
	}
}

// truncate 按 rune 截断，避免把中文 error 切出半个 UTF-8 字符写进库。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
