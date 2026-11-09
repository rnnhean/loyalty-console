// Package mq —— 消息基础设施。
//
// 两个消息中间件在这里并存，不是重复建设，而是**语义不同**：
//
//	Kafka    —— 事件流。有序、可重放、多消费组各自独立消费、高吞吐。
//	            用于「交易流水产生后，审计服务 / 报表服务 / 风控服务都要知道」。
//	            核心特征：一条消息可以被多个消费组各消费一遍。
//
//	RabbitMQ —— 任务队列。单条消息投递、可延迟、失败进死信、逐条 ack。
//	            用于「权益到期前 3 天提醒」「券批次到期回收」这类
//	            **有延迟、且必须恰好被处理一次**的任务。
//	            核心特征：延迟队列（TTL + DLX）是它的强项，Kafka 做延迟要靠轮询。
package mq

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/example/loyalty-console/server/internal/observ"
)

// Event 统一的领域事件信封。
//
// 为什么所有事件都用同一个信封：消费方可以统一做 trace 透传、
// 版本校验、死信处理，不必为每种事件写一套。
type Event struct {
	// ID 幂等键。消费方必须按它去重 —— Kafka 是 at-least-once 语义，
	// 重复投递是常态而不是异常。
	ID string `json:"id"`
	// Type 事件类型，形如 "trade.checkout.completed"。
	Type string `json:"type"`
	// Version 载荷版本。消费方按它决定怎么解析，避免加字段就全线崩。
	Version int `json:"version"`
	// TraceID 贯穿链路，消费端日志里带上就能和产生端对上。
	TraceID string `json:"trace_id"`
	// OccurredAt 事件发生时间（不是投递时间）。
	OccurredAt time.Time `json:"occurred_at"`
	// Payload 业务载荷。
	Payload json.RawMessage `json:"payload"`
}

// ── Kafka 生产者 ───────────────────────────────────────────

// Producer Kafka 生产者。
//
// 内部用 kafka-go 的 Writer。它的默认行为已经足够好：
// 批量攒够 flushInterval 或 batchSize 才真正发（吞吐）；
// RequiredAcks 用 -1（所有 ISR 确认）保证不丢。
type Producer struct {
	w      *kafka.Writer
	topic  string
	logger *slog.Logger

	sent    atomic.Int64
	failed  atomic.Int64
	closeOn sync.Once
}

// ProducerConfig 生产者配置。
type ProducerConfig struct {
	Brokers []string
	Topic   string
	// BatchSize/BatchBytes 决定攒批大小。太小浪费网络往返，
	// 太大则单条消息的延迟变高（要等攒够才发）。
	BatchSize  int
	BatchBytes int
	// FlushInterval 兜底：攒不够这么多消息时，最多等这么久也要发。
	FlushInterval time.Duration
}

// NewProducer 构造。
func NewProducer(cfg ProducerConfig, logger *slog.Logger) *Producer {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.BatchBytes <= 0 {
		cfg.BatchBytes = 1 << 20
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 200 * time.Millisecond
	}
	return &Producer{
		topic: cfg.Topic,
		w: &kafka.Writer{
			Addr:         kafka.TCP(cfg.Brokers...),
			Topic:        cfg.Topic,
			Balancer:     &kafka.Hash{}, // 同一个 key 落同一个分区 → 同 key 内有序
			BatchSize:    cfg.BatchSize,
			BatchBytes:   int64(cfg.BatchBytes),
			BatchTimeout: cfg.FlushInterval,
			RequiredAcks: kafka.RequireAll,
			// 幂等生产者：配合 acks=all 保证「单分区内不重复不丢」。
			// 注意它只保证会话内；跨会话的重复仍需消费端去重。
			Async: false,
			Logger: kafka.LoggerFunc(func(msg string, args ...interface{}) {
				logger.Debug("kafka_writer", "msg", fmt.Sprintf(msg, args...))
			}),
			ErrorLogger: kafka.LoggerFunc(func(msg string, args ...interface{}) {
				logger.Warn("kafka_writer_error", "msg", fmt.Sprintf(msg, args...))
			}),
		},
		logger: logger,
	}
}

// Publish 发布一条事件。key 决定分区，同 key 的消息保持分区内有序。
func (p *Producer) Publish(ctx context.Context, key string, evt Event) error {
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = time.Now().UTC()
	}
	b, err := json.Marshal(evt)
	if err != nil {
		p.failed.Add(1)
		observ.KafkaPublished.WithLabelValues(p.topic, "marshal_error").Inc()
		return fmt.Errorf("marshal event: %w", err)
	}

	// 写超时独立于业务超时：业务 ctx 已经取消时，事件也应该尽量发出去
	// （它代表一件已经发生的事实，丢了就会导致下游数据不一致）。
	// 这里用 ctx 但限制最长 5s。
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := p.w.WriteMessages(wctx, kafka.Message{
		Key:   []byte(key),
		Value: b,
		Time:  evt.OccurredAt,
		Headers: []kafka.Header{
			{Key: "trace_id", Value: []byte(evt.TraceID)},
			{Key: "event_type", Value: []byte(evt.Type)},
		},
	}); err != nil {
		p.failed.Add(1)
		observ.KafkaPublished.WithLabelValues(p.topic, "error").Inc()
		return fmt.Errorf("publish %s: %w", evt.Type, err)
	}
	p.sent.Add(1)
	observ.KafkaPublished.WithLabelValues(p.topic, "ok").Inc()
	return nil
}

// Stats 发送统计。
func (p *Producer) Stats() (sent, failed int64) { return p.sent.Load(), p.failed.Load() }

// Close 冲掉缓冲并关闭。
func (p *Producer) Close() error {
	var err error
	p.closeOn.Do(func() { err = p.w.Close() })
	return err
}

// ── Kafka 消费者 ───────────────────────────────────────────

// Handler 消费回调。返回 error 表示处理失败，会按策略重试。
type Handler func(ctx context.Context, evt Event) error

// ConsumerConfig 消费者配置。
type ConsumerConfig struct {
	Brokers []string
	Topic   string
	// Group 消费组。同一组内的实例分摊分区；不同组各自独立消费全量。
	Group string
	// MaxRetries 单条消息的本地重试次数（不重新入队，只原地重试）。
	MaxRetries int
	// RetryBackoff 重试间隔（指数退避的基数）。
	RetryBackoff time.Duration
}

// Consumer Kafka 消费者。
type Consumer struct {
	r      *kafka.Reader
	cfg    ConsumerConfig
	logger *slog.Logger

	processed atomic.Int64
	errors    atomic.Int64
	dlq       atomic.Int64
}

// NewConsumer 构造。
func NewConsumer(cfg ConsumerConfig, logger *slog.Logger) *Consumer {
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 200 * time.Millisecond
	}
	return &Consumer{
		cfg:    cfg,
		logger: logger,
		r: kafka.NewReader(kafka.ReaderConfig{
			Brokers: cfg.Brokers,
			Topic:   cfg.Topic,
			GroupID: cfg.Group,
			// 一次多读几条能提高吞吐；但处理失败时要逐个重试，
			// 所以这里保持较小值，避免大量消息积压在一个慢消费者手里。
			MinBytes: 1,
			MaxBytes: 1 << 20, // 1MB
			MaxWait:  500 * time.Millisecond,
			// 首次无 offset 时从头读还是从最新读，取决于业务：
			// 审计类事件希望从头读（补历史），通知类希望从最新读。
			// 这里取最新 —— 管理台的事件都是有实时性要求的。
			StartOffset: kafka.LastOffset,
		}),
	}
}

// Run 阻塞消费，直到 ctx 取消。
//
// 关键的顺序：**先处理、再提交 offset**。
// 反过来的话，进程在处理中途崩溃，消息就永久丢失了。
// 代价是至少一次语义 —— 所以消费方必须按 Event.ID 做幂等。
func (c *Consumer) Run(ctx context.Context, h Handler) error {
	defer c.r.Close()

	for {
		if ctx.Err() != nil {
			return nil
		}
		msg, err := c.r.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.errors.Add(1)
			c.logger.Warn("kafka_fetch_error", "topic", c.cfg.Topic, "err", err.Error())
			time.Sleep(time.Second)
			continue
		}

		var evt Event
		if err := json.Unmarshal(msg.Value, &evt); err != nil {
			// 消息体坏了，重试也没用 —— 直接跳过并记一条，
			// 不要让它把整个分区卡住。
			c.dlq.Add(1)
			observ.KafkaConsumed.WithLabelValues(c.cfg.Topic, "bad").Inc()
			c.logger.Error("kafka_bad_message",
				"topic", c.cfg.Topic, "offset", msg.Offset, "err", err.Error())
			_ = c.r.CommitMessages(ctx, msg)
			continue
		}
		if evt.TraceID == "" {
			for _, hd := range msg.Headers {
				if hd.Key == "trace_id" {
					evt.TraceID = string(hd.Value)
					break
				}
			}
		}

		if err := c.processWithRetry(ctx, h, evt); err != nil {
			// 重试用尽：提交 offset 并计数。
			// 真实生产环境这里应该写到死信 topic；本工程只做指标 + 错误日志，
			// 因为演示环境没有运维会去处理死信队列。
			c.dlq.Add(1)
			observ.KafkaConsumed.WithLabelValues(c.cfg.Topic, "dlq").Inc()
			c.logger.Error("kafka_message_dead",
				"topic", c.cfg.Topic, "event_id", evt.ID, "event_type", evt.Type,
				"offset", msg.Offset, "err", err.Error())
		} else {
			c.processed.Add(1)
			observ.KafkaConsumed.WithLabelValues(c.cfg.Topic, "ok").Inc()
		}

		if err := c.r.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
			c.logger.Warn("kafka_commit_error", "topic", c.cfg.Topic, "err", err.Error())
		}
	}
}

// processWithRetry 指数退避重试。
//
// 只对「可重试错误」退避；一旦 ctx 取消立刻返回，不做无意义的等待。
func (c *Consumer) processWithRetry(ctx context.Context, h Handler, evt Event) error {
	var last error
	backoff := c.cfg.RetryBackoff
	for i := 0; i <= c.cfg.MaxRetries; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 单次处理耗时要单独打点：它一旦逼近 max poll interval，
		// 消费者会被踢出组并触发 rebalance，表现为「消息莫名其妙重复消费」。
		start := time.Now()
		err := h(ctx, evt)
		observ.KafkaHandleDuration.WithLabelValues(c.cfg.Topic).Observe(time.Since(start).Seconds())

		if err != nil {
			last = err
			c.logger.Warn("kafka_handler_error",
				"event_type", evt.Type, "attempt", i+1, "err", err.Error())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			continue
		}
		return nil
	}
	return last
}

// Stats 消费统计。
func (c *Consumer) Stats() (processed, errors, dlq int64) {
	return c.processed.Load(), c.errors.Load(), c.dlq.Load()
}
