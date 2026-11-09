package mq

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/example/loyalty-console/server/internal/observ"
)

// ── RabbitMQ：延迟任务 ─────────────────────────────────────
//
// 场景：权益到期前 3 天提醒、券批次到期自动回收、报表异步导出。
// 这类任务的共同点是「现在创建，将来某刻执行」。
//
// 为什么不用 Kafka 做：Kafka 没有原生延迟能力，只能靠轮询表或
// 另建一个定时 topic 扫描。RabbitMQ 用 **TTL + 死信交换机（DLX）**
// 是原生能力，代价只是几条队列声明。
//
// 机制：
//
//	定时任务 --publish--> delay 队列（无消费者，队列上设 x-message-ttl）
//	                          ↓ TTL 到期，消息变成「死信」
//	                    死信交换机 DLX ──路由键──> 目标队列
//	                          ↓
//	                    消费者手动 ack
//
// ── 两个必须知道的坑 ──
//
// 坑 1：TTL 只检查队头。
//   RabbitMQ 只判断队首消息是否过期，队首没过期，后面的消息即使 TTL 更短
//   也不会被提前死信。所以**不同延迟级别的消息必须分队列**，
//   否则 30 天的消息挡在 3 天的消息前面，后者要等 30 天。
//
// 坑 2：死信路由键只能配在队列上，不能按消息设置。
//   `x-dead-letter-routing-key` 是 QueueDeclare 的参数，不是 Publishing 的
//   Header。按消息设置会被静默忽略，消息过期后无处可去、被直接丢弃 ——
//   这是排查起来最痛苦的一类问题。
//   解决：把「延迟级别 × 目标队列」两个维度展开成独立的队列。
//   本系统 3 档延迟 × 2 个目标 = 6 条延迟队列，声明一次即可。

const (
	// Exchange 主交换机（direct）。
	Exchange   = "console.tasks"
	DLExchange = "console.dlx"

	// QueueExpiry 权益到期处理队列。
	QueueExpiry = "task.benefit.expiry"
	// QueueExport 报表导出队列。
	QueueExport = "task.report.export"
)

// delayTiers 延迟档位。key 会拼进队列名，value 是毫秒。
//
// 档位是「固定区间」而不是任意值，原因是坑 1：
// 每条队列只能有一个 TTL，想支持任意延迟就得为每个延迟值建队列（无限增长）。
// 业务上按档位分流就够用；真需要任意精度，应该换延迟插件或时间轮。
//
// 5s 这一档是给测试和小延迟场景的：没有它，5 秒的任务会被塞进 3 天的队列，
// 表现出来就是「任务投递成功但永远不执行」——很难一眼看出的坑。
var delayTiers = map[string]int64{
	"5s":  5 * 1000,
	"1m":  60 * 1000,
	"3d":  3 * 24 * 3600 * 1000,
	"7d":  7 * 24 * 3600 * 1000,
	"30d": 30 * 24 * 3600 * 1000,
}

// delayOrder 按从小到大排列，选取时找第一个 >= 目标延迟的档位。
var delayOrder = []string{"5s", "1m", "3d", "7d", "30d"}

// targets 目标队列清单。
var targets = []string{QueueExpiry, QueueExport}

// Task 延迟任务。
type Task struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"` // 决定落到哪个目标队列
	TraceID string          `json:"trace_id"`
	RunAt   time.Time       `json:"run_at"` // 期望执行时间，消费方二次校验
	Payload json.RawMessage `json:"payload"`
}

// TaskHandler 任务处理器。
type TaskHandler func(ctx context.Context, t Task) error

// Broker RabbitMQ 连接封装。
type Broker struct {
	conn     *amqp.Connection
	ch       *amqp.Channel
	prefetch int
	logger   *slog.Logger

	mu      sync.Mutex // 发布用的 channel 不是并发安全的，发布要串行
	closed  atomic.Bool
	stopCh  chan struct{}
	stopOne sync.Once
}

// BrokerConfig 连接配置。
type BrokerConfig struct {
	// URL 形如 amqp://user:pass@host:port/vhost
	URL string
	// Prefetch 每个消费者未 ack 的消息上限。
	// 太大：一个消费者攥着一堆消息不处理，其他消费者饿着；
	// 太小：网络往返成为瓶颈。10 是通用起点。
	Prefetch int
}

// Dial 建立连接并声明拓扑。
func Dial(cfg BrokerConfig, logger *slog.Logger) (*Broker, error) {
	conn, err := amqp.Dial(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("rabbit dial: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("rabbit channel: %w", err)
	}
	if cfg.Prefetch <= 0 {
		cfg.Prefetch = 10
	}
	if err := ch.Qos(cfg.Prefetch, 0, false); err != nil {
		ch.Close()
		conn.Close()
		return nil, fmt.Errorf("rabbit qos: %w", err)
	}

	b := &Broker{conn: conn, ch: ch, prefetch: cfg.Prefetch, logger: logger, stopCh: make(chan struct{})}
	if err := b.declareTopology(); err != nil {
		b.Close()
		return nil, err
	}
	go b.watchConnection()
	return b, nil
}

// declareTopology 声明交换机、队列与绑定。所有声明都是幂等的，
// 但参数必须与已存在的定义完全一致，否则 broker 返回 406 并关闭 channel。
func (b *Broker) declareTopology() error {
	if err := b.ch.ExchangeDeclare(Exchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange: %w", err)
	}
	if err := b.ch.ExchangeDeclare(DLExchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare dlx: %w", err)
	}

	// 1. 目标队列：真正被消费的地方，从主交换机按队列名直接投递
	for _, q := range targets {
		if _, err := b.ch.QueueDeclare(q, true, false, false, false, nil); err != nil {
			return fmt.Errorf("declare queue %s: %w", q, err)
		}
		if err := b.ch.QueueBind(q, q, Exchange, false, nil); err != nil {
			return fmt.Errorf("bind queue %s: %w", q, err)
		}
		// 死信交换机也要能到这里：延迟队列过期后走这条路
		if err := b.ch.QueueBind(q, q, DLExchange, false, nil); err != nil {
			return fmt.Errorf("bind dlq %s: %w", q, err)
		}
	}

	// 2. 延迟队列：档位数 × 2 目标。
	// 每条队列把「过期后去哪」直接焊死在声明参数里，规避坑 2。
	for _, tier := range delayOrder {
		for _, target := range targets {
			name := delayQueueName(tier, target)
			args := amqp.Table{
				"x-message-ttl": delayTiers[tier],
				// ← 只能在这里设，不能按消息设
				"x-dead-letter-exchange":    DLExchange,
				"x-dead-letter-routing-key": target,
			}
			if _, err := b.ch.QueueDeclare(name, true, false, false, false, args); err != nil {
				return fmt.Errorf("declare delay queue %s: %w", name, err)
			}
			if err := b.ch.QueueBind(name, name, Exchange, false, nil); err != nil {
				return fmt.Errorf("bind delay %s: %w", name, err)
			}
		}
	}
	return nil
}

func delayQueueName(tier, target string) string {
	return "delay." + tier + "." + target
}

func (b *Broker) watchConnection() {
	notify := b.conn.NotifyClose(make(chan *amqp.Error, 1))
	select {
	case <-b.stopCh:
		return
	case err := <-notify:
		if err != nil {
			b.logger.Error("rabbit_connection_lost", "err", err.Error())
		}
	}
}

// PublishDelayed 发布延迟任务。
//
// delay 会就近向上取整到 3d / 7d / 30d 三档，超过 30 天按 30 天处理。
func (b *Broker) PublishDelayed(ctx context.Context, t Task, delay time.Duration) error {
	if b.closed.Load() {
		return fmt.Errorf("rabbit broker closed")
	}
	if t.ID == "" {
		return fmt.Errorf("rabbit task requires id")
	}
	target := QueueExpiry
	if t.Type == "report.export" {
		target = QueueExport
	}
	route := delayQueueName(pickDelayTier(delay), target)

	body, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	err = b.ch.PublishWithContext(ctx, Exchange, route, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent, // 持久化：broker 重启后任务不丢
		MessageId:    t.ID,
		Timestamp:    time.Now(),
		Body:         body,
		Headers:      amqp.Table{"trace_id": t.TraceID},
	})
	if err != nil {
		observ.RabbitPublished.WithLabelValues(target, "error").Inc()
		return err
	}
	observ.RabbitPublished.WithLabelValues(target, "ok").Inc()
	return nil
}

// pickDelayTier 选档位：取第一个「TTL >= 目标延迟」的档，向上取整。
//
// 为什么向上而不是向下：向下取整会让任务**提前**执行，
// 对「到期回收」这类业务是错的 —— 权益还没到期就被收走了。
// 宁可晚几毫秒，也不能早。
func pickDelayTier(d time.Duration) string {
	ms := d.Milliseconds()
	for _, t := range delayOrder {
		if ms <= delayTiers[t] {
			return t
		}
	}
	return "30d" // 超过最长档，按 30 天处理
}

// Consume 消费目标队列。
//
// 手动 ack：自动 ack 在「已投递、业务未完成」时崩溃会丢任务。
// 代价是至少一次语义，处理方要按 Task.ID 幂等。
//
// **每个消费者单独开一个 channel**，不复用 Broker.ch：
//   - amqp.Channel 不是并发安全的，发布与消费共用会踩到框架的竞态；
//   - 更重要的是 ack 语义：channel 一旦因协议错误被 broker 关闭，
//     该 channel 上所有未 ack 的消息会被整体重新入队。让每个消费者
//     独立成 channel，一个消费者的故障就不会波及其他消费者。
func (b *Broker) Consume(ctx context.Context, queue string, h TaskHandler) error {
	ch, err := b.conn.Channel()
	if err != nil {
		return fmt.Errorf("consume channel %s: %w", queue, err)
	}
	defer ch.Close()

	if err := ch.Qos(b.prefetch, 0, false); err != nil {
		return fmt.Errorf("consume qos %s: %w", queue, err)
	}
	msgs, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume %s: %w", queue, err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-b.stopCh:
			return nil
		case d, ok := <-msgs:
			if !ok {
				return nil
			}
			var t Task
			if err := json.Unmarshal(d.Body, &t); err != nil {
				observ.RabbitConsumed.WithLabelValues(queue, "bad").Inc()
				b.logger.Error("rabbit_bad_task", "queue", queue, "err", err.Error())
				_ = d.Nack(false, false) // 格式错误，重投无意义
				// 格式错误的消息要计成死信，否则「消费侧出问题」在指标上是静默的
				observ.RabbitDeadLettered.WithLabelValues(queue).Inc()
				continue
			}
			// 二次校验：TTL 只保证「至少等这么久」，队列积压时可能更晚。
			if !t.RunAt.IsZero() && time.Now().Before(t.RunAt) {
				remain := time.Until(t.RunAt)
				if err := b.PublishDelayed(ctx, t, remain); err != nil {
					b.logger.Warn("rabbit_requeue_failed", "task", t.ID, "err", err.Error())
					_ = d.Nack(false, true) // 回队列，别丢
					continue
				}
				_ = d.Ack(false)
				continue
			}
			if err := h(ctx, t); err != nil {
				observ.RabbitConsumed.WithLabelValues(queue, "failed").Inc()
				observ.RabbitDeadLettered.WithLabelValues(queue).Inc()
				b.logger.Warn("rabbit_task_failed", "task", t.ID, "type", t.Type, "err", err.Error())
				// requeue=false：不无限重投，避免毒消息顶死队列。
				_ = d.Nack(false, false)
				continue
			}
			observ.RabbitConsumed.WithLabelValues(queue, "ok").Inc()
			_ = d.Ack(false)
		}
	}
}

// Close 关闭连接。可重复调用。
func (b *Broker) Close() error {
	b.stopOne.Do(func() { close(b.stopCh) })
	b.closed.Store(true)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ch != nil {
		_ = b.ch.Close()
	}
	if b.conn != nil {
		return b.conn.Close()
	}
	return nil
}
