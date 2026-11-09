package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/example/loyalty-console/server/internal/domain"
	"github.com/example/loyalty-console/server/internal/idempotency"
	"github.com/example/loyalty-console/server/internal/lock"
	"github.com/example/loyalty-console/server/internal/mq"
	"github.com/example/loyalty-console/server/internal/outbox"
	"github.com/example/loyalty-console/server/internal/store"
)

// infra 把所有中间件句柄收在一处，方便 main 装配与统一关闭。
//
// 用结构体而不是一堆零散变量：关闭顺序有依赖（先停消费者，
// 再关生产者，最后关连接池），集中管理才能保证顺序正确。
type infra struct {
	locker *lock.Locker
	guard  *idempotency.Guard
	kafka  *mq.Producer
	rabbit *mq.Broker
	outbox *outbox.Store
	state  *infraState
}

// startInfra 建立全部中间件连接。任何一个失败都不阻塞启动 ——
// 这是**降级优先**的取舍：
//
//	消息队列挂了 → 读接口照样能用，写接口记日志并返回明确错误；
//	对象存储挂了 → 只有导出不可用；
//	Redis 挂了   → 缓存穿透到 DB，锁降级为「不互斥」，幂等降级为「放行」。
//
// 反过来的做法（任一组件不可用就拒绝启动）在这种「一个后台管理台」的
// 场景里是错的：运维想打开页面看一眼，却因为 Kafka 没起来而进不去。
//
// 但要注意：这条策略只在**多组件各自可降级**时成立。如果某个组件
// 是核心链路（比如鉴权用的 Redis），那它必须 fail-fast。
func startInfra(ctx context.Context, logger *slog.Logger, st *store.Store) *infra {
	in := &infra{state: newInfraState()}

	// ── 分布式锁 + 幂等（依赖 Redis）──
	if st.RDB != nil {
		in.locker = lock.NewLocker(st.RDB, 15*time.Second)
		in.guard = idempotency.NewGuard(st.RDB)
		in.state.set("redis_lock", true, "")
	} else {
		in.state.set("redis_lock", false, "redis 未连接，锁与幂等降级")
	}

	// ── Kafka 生产者 ──
	brokers := envList("MC_KAFKA_BROKERS", "127.0.0.1:19092")
	topic := env("MC_KAFKA_TOPIC", "console.events")
	in.kafka = mq.NewProducer(mq.ProducerConfig{
		Brokers: brokers,
		Topic:   topic,
	}, logger)
	in.state.set("kafka", true, "topic="+topic)

	// ── 发件箱（依赖 DB + Kafka）──
	//
	// 补发协程只在两者都在时启动。Kafka 不可用时，事件照样先落进
	// msg_outbox，等补发循环发现下游恢复再发出去 —— 这正是发件箱的用途，
	// 所以这里不需要「Kafka 挂了就别登记」这种逻辑。
	if st.DB != nil {
		in.outbox = outbox.NewStore(st.DB, logger, 50)
		if in.kafka != nil {
			go in.outbox.Run(ctx, in.kafka, 2*time.Second)
		}
		in.state.set("outbox", true, "")
	} else {
		in.state.set("outbox", false, "db 未连接")
	}

	// ── RabbitMQ ──
	//
	// vhost 是 /console，在 AMQP URL 里必须写成 %2Fconsole。
	// 直接写 /console 会被解析成 vhost 名为 "console"（少了前导斜杠），
	// 报 "vhost console not found" —— 这条错法很常见，所以这里显式注释。
	if url := env("MC_RABBIT_URL", "amqp://mc:mc123@127.0.0.1:15674/%2Fconsole"); url != "" {
		broker, err := mq.Dial(mq.BrokerConfig{URL: url, Prefetch: 10}, logger)
		if err != nil {
			logger.Warn("rabbit_unavailable", "err", err.Error())
			in.state.set("rabbitmq", false, err.Error())
		} else {
			in.rabbit = broker
			in.state.set("rabbitmq", true, "")
		}
	}

	return in
}

// closeInfra 逆序关闭。
func (in *infra) closeInfra() {
	if in.rabbit != nil {
		_ = in.rabbit.Close()
	}
	if in.kafka != nil {
		_ = in.kafka.Close()
	}
}

// ── 事件消费：交易事件 → 异步审计 ─────────────────────────
//
// 为什么审计要异步：审计记录是「写入的附带动作」，
// 同步写会让保存接口的延迟多一次 INSERT 和一次索引维护。
// 但——**审计的可靠性不能因此降低**，所以主流程仍然同步落 DB，
// 这里消费事件是为了派生统计（按人、按对象聚合的操作频次）。
//
// 这个设计看起来「重复写」了，实际职责不同：
//   trd_audit_trace  ← 事实来源，同步写，不可丢
//   trd_audit_rollup ← 派生统计，异步写，可重算

func startAuditConsumer(ctx context.Context, in *infra, trade *domain.TradeService, logger *slog.Logger) {
	if in.kafka == nil {
		return
	}
	consumer := mq.NewConsumer(mq.ConsumerConfig{
		Brokers: envList("MC_KAFKA_BROKERS", "127.0.0.1:19092"),
		Topic:   env("MC_KAFKA_TOPIC", "console.events"),
		Group:   env("MC_KAFKA_GROUP", "console-audit"),
	}, logger)
	// 消费组名同时用作去重台账的分组键：同一个事件在不同消费组里
	// 应当各被处理一次，所以去重必须以 (事件, 消费组) 为维度。
	group := env("MC_KAFKA_GROUP", "console-audit")

	go func() {
		if err := consumer.Run(ctx, func(ctx context.Context, evt mq.Event) error {
			// 只处理自己关心的事件类型。其他类型返回 nil（= 处理成功），
			// 否则会被当成失败重试，白白污染指标。
			if evt.Type != "trade.checkout.completed" && evt.Type != "benefit.rule.expired" {
				return nil
			}
			var p struct {
				Target string `json:"target_kind"`
				Ref    string `json:"target_ref"`
				Action string `json:"action_kind"`
				Op     string `json:"operator_ref"`
			}
			if err := json.Unmarshal(evt.Payload, &p); err != nil {
				// 载荷坏了没有重试价值，返回 nil 让它提交 offset。
				// 注意这与「处理逻辑失败」的区分 —— 后者才应该重试。
				logger.Warn("audit_event_bad_payload", "event_id", evt.ID, "err", err.Error())
				return nil
			}
			_, err := trade.RecordAuditOnce(ctx, evt.ID, group, p.Target, p.Ref, p.Action, p.Op, map[string]any{
				"source": "kafka",
			})
			// 重复投递是 Kafka 的正常行为，不是错误：去重表已经在事务里
			// 拦下了它，这里只需要当作处理成功继续消费。
			if errors.Is(err, domain.ErrDuplicateEvent) {
				logger.Debug("audit_event_skipped", "event_id", evt.ID, "reason", "duplicate")
				return nil
			}
			return err
		}); err != nil {
			logger.Error("audit_consumer_stopped", "err", err.Error())
		}
	}()
}

// startExpiryConsumer 消费「权益到期」延迟任务。
//
// 这里演示三件事配合：
//
//	RabbitMQ 延迟队列 —— 决定「什么时候执行」
//	分布式锁          —— 保证「同一规则只有一个实例在改」
//	幂等 SQL          —— 保证「重复执行不产生额外副作用」
//
// 三者是递进关系，缺一个都能出问题：
//
//	只有延迟队列  → 多个实例同时消费不同分片，可能同时处理同一规则
//	+ 锁          → 串行化了，但消费失败重投会重复执行
//	+ 幂等 SQL    → 重复执行 Affected=0，无副作用
func startExpiryConsumer(ctx context.Context, in *infra, benefit *domain.BenefitService, logger *slog.Logger) {
	if in.rabbit == nil {
		return
	}
	go func() {
		if err := in.rabbit.Consume(ctx, mq.QueueExpiry, func(ctx context.Context, t mq.Task) error {
			var p domain.ExpireTask
			if err := json.Unmarshal(t.Payload, &p); err != nil {
				logger.Warn("expiry_task_bad_payload", "task", t.ID, "err", err.Error())
				return nil // 载荷错误不重试
			}
			// 真正执行的逻辑抽成一个闭包，下面两条路径（有锁/无锁）共用
			doExpire := func(ctx context.Context) error {
				n, err := benefit.ExpireBenefitRule(ctx, p.RuleUID,
					func(ctx context.Context, tx *sql.Tx, affected int64) error {
						// 在业务事务内登记事件。Kafka 此刻挂了也不影响：
						// 记录留在 msg_outbox 里，补发协程会一直重试。
						if in.outbox == nil {
							return nil
						}
						evt := benefitExpiredEvent(t.TraceID, p, affected)
						return in.outbox.Enqueue(ctx, tx, env("MC_KAFKA_TOPIC", "console.events"), evt.ID, evt)
					})
				if err != nil {
					return err
				}
				logger.Info("expiry_processed", "rule", p.RuleUID, "affected", n, "trace_id", t.TraceID)
				return nil
			}

			// 无锁分支：Redis 不可用时降级为直接执行。
			// 这是有意的取舍 —— 宁可偶发重复（幂等 SQL 兜底），
			// 也不要因为 Redis 挂了就完全停止到期回收。
			if in.locker == nil {
				return doExpire(ctx)
			}
			return in.locker.Do(ctx, fmt.Sprintf("benefit.expire.rule:%d", p.RuleUID), doExpire)
		}); err != nil {
			logger.Error("expiry_consumer_stopped", "err", err.Error())
		}
	}()
}

// benefitExpiredEvent 构造「权益已到期」领域事件。
//
// 事件 ID 用 traceID + 规则号：同一条任务重投时 ID 相同，消费侧按 ID
// 去重就能避免重复审计；同时它也是发件箱的唯一约束键，同一次处理
// 不会被登记两次。
//
// 用 Kafka 而不是 RabbitMQ：这是「已发生的事实」，一对多广播 ——
// 将来加了风控、加了报表，各自起消费组即可，不用改这里。
func benefitExpiredEvent(traceID string, p domain.ExpireTask, affected int64) mq.Event {
	payload, _ := json.Marshal(map[string]any{
		"target_kind":  "benefit.rule",
		"target_ref":   fmt.Sprintf("%d", p.RuleUID),
		"action_kind":  "benefit.expired",
		"operator_ref": "system",
		"affected":     affected,
	})
	return mq.Event{
		ID:         traceID + "-" + fmt.Sprintf("%d", p.RuleUID),
		Type:       "benefit.rule.expired",
		Version:    1,
		TraceID:    traceID,
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}
}

// publishCheckoutEvent 交易完成后发事件。
//
// 调用点在 checkout 接口里，用 goroutine 异步发：
// 事件丢了比不上业务慢更严重吗？不是。所以这里**同步发**，
// 失败只记日志不阻断返回 —— 「下单成功但事件没发出去」是可接受的，
// 而「事件发出去了但订单其实没成」会引发下游对账混乱。
func publishCheckoutEvent(ctx context.Context, in *infra, traceID string, payload any) {
	if in.kafka == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	evt := mq.Event{
		ID:         traceID,
		Type:       "trade.checkout.completed",
		Version:    1,
		TraceID:    traceID,
		OccurredAt: time.Now().UTC(),
		Payload:    raw,
	}
	if err := in.kafka.Publish(ctx, traceID, evt); err != nil {
		slog.Default().Warn("checkout_event_publish_failed",
			"trace_id", traceID, "err", err.Error())
	}
}
