/**
 * 业务埋点：把「一次诊断发生了什么」变成可聚合的数字。
 *
 * 这里只定义指标和采集点，不做任何判定。所有 label 值都来自**闭集**
 * （终止原因枚举、工具名白名单、报告类型枚举），没有一处拿用户输入当 label——
 * 那会让外部输入把时间序列撑爆。
 *
 * 与 trace.ts 的分工：
 *   trace  —— 一次调用一行，能追到具体那次（排查「刚才那次怎么了」）
 *   metrics —— 跨请求聚合，能被 PromQL 查询和告警（回答「最近怎么样」）
 * 两者都要：只有日志没法告警，只有指标没法复盘单次。
 */

import { monitorEventLoopDelay } from 'node:perf_hooks';
import type { IntervalHistogram } from 'node:perf_hooks';

import type { RunOutcome } from './loop.js';
import { TOOL_SPECS, type ToolExecutor } from './tools.js';
import type { ValidationIssueKind } from './validate.js';
import { Counter, Gauge, Histogram as H } from './metrics.js';

// ── 指标定义 ────────────────────────────────────────────────

/** result 是终止原因的直方图化：completed / degraded / max_turns / budget …，
 *  外加三个非模型结果（denied / unauthorized / bad_request）。
 *  看 `no_new_evidence` 的占比就知道提示词是不是在绕圈。 */
export const RequestTotal = new Counter(
    'agent_request_total',
    '诊断请求数，按结果切分',
    ['result'],
);

export const RequestDuration = new H(
    'agent_request_duration_seconds',
    '一次诊断的总耗时（含模型调用与工具执行）',
    [],
    [0.5, 1, 2, 5, 10, 20, 30, 45, 60],
);

export const ReportKindTotal = new Counter(
    'agent_report_kind_total',
    '报告类型分布（diagnosis / query / out_of_scope）',
    ['kind'],
);

export const DeniedTotal = new Counter(
    'agent_denied_total',
    '被准入闸门拒绝的请求，按原因切分（disabled / concurrency / rpm / budget）',
    ['reason'],
);

export const ToolTotal = new Counter(
    'agent_tool_total',
    '工具调用次数，按 工具/是否成功 切分',
    ['tool', 'ok'],
);

export const ToolDuration = new H(
    'agent_tool_duration_seconds',
    '单次工具调用耗时',
    ['tool'],
    [0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8],
);

/** 这个数不为 0 就是质量问题：模型引用了台账里不存在的证据。
 *  按 kind 切分能区分「编造 finding id」和「数字和证据对不上」——
 *  前者是提示词没约束住，后者通常是工具结果被截断了。 */
export const CitationDropped = new Counter(
    'agent_citation_dropped_total',
    '被校验剔除的引用，按问题类型切分（>0 说明模型在编）',
    ['kind'],
);

export const TokensTotal = new Counter(
    'agent_tokens_total',
    '模型 token 用量，按 direction 切分（input / output / cache_read）',
    ['direction'],
);

export const InflightGauge = new Gauge('agent_inflight', '当前在途的诊断请求数');

/** **进程内**当日累计金额（元）。
 *  不是真实账单：进程重启就归零，多实例也各算各的。
 *  它只回答「这个实例离闸门还有多远」，真实花费看 trace 汇总。 */
export const DailyCostGauge = new Gauge(
    'agent_daily_cost_cny',
    '本进程累计的诊断花费（元）。重启归零，是闸门口径，不是账单',
);

export const EventLoopLagGauge = new Gauge(
    'agent_event_loop_lag_seconds',
    '事件循环延迟 p99（20ms 采样，上次抓取后重置）。超过 0.2 说明请求路径里有同步阻塞',
);

export const UptimeGauge = new Gauge('agent_uptime_seconds', '进程已运行秒数');

export const HeapGauge = new Gauge('agent_heap_used_bytes', 'V8 堆已用字节数');

// ── 采集点 ──────────────────────────────────────────────────

/** 引用类问题：都是「模型说了证据不支持的话」，和 schema 不符分开算。 */
const CITATION_ISSUES = new Set<ValidationIssueKind>([
    'unknown_finding',
    'empty_citation',
    'bad_advice_ref',
    'ambiguous_evidence',
    'number_not_in_evidence',
]);

const KNOWN_TOOLS = new Set<string>(TOOL_SPECS.map((t) => t.name));

/**
 * 给工具执行器套一层计时与计数。
 *
 * 放在装饰器里而不是 loop 里：loop 是纯状态机、被 11 条单测直接驱动，
 * 埋点混进去会让单测之间互相污染全局指标。
 */
export function withToolMetrics(executor: ToolExecutor): ToolExecutor {
    return {
        async execute(req) {
            // 工具名来自模型输出，必须过白名单再当 label
            const tool = KNOWN_TOOLS.has(req.name) ? req.name : 'unknown';
            const started = performance.now();
            try {
                const res = await executor.execute(req);
                ToolTotal.inc({ tool, ok: res.ok ? 'true' : 'false' });
                ToolDuration.observe({ tool }, (performance.now() - started) / 1000);
                return res;
            } catch (err) {
                // 执行器正常不抛（失败用 {ok:false} 表达），真抛了也不能让它绕过计数
                ToolTotal.inc({ tool, ok: 'false' });
                ToolDuration.observe({ tool }, (performance.now() - started) / 1000);
                throw err;
            }
        },
    };
}

/** 正常跑完（含 degraded）后记一次。被拒绝的请求走 DeniedTotal，不进这里。 */
export function recordOutcome(outcome: RunOutcome, durationMs: number): void {
    RequestTotal.inc({ result: outcome.stopReason });
    RequestDuration.observe(undefined, durationMs / 1000);
    ReportKindTotal.inc({ kind: outcome.report.kind });

    for (const issue of outcome.issues) {
        if (CITATION_ISSUES.has(issue.kind)) CitationDropped.inc({ kind: issue.kind });
    }

    TokensTotal.inc({ direction: 'input' }, outcome.usage.inTokens);
    TokensTotal.inc({ direction: 'output' }, outcome.usage.outTokens);
    TokensTotal.inc({ direction: 'cache_read' }, outcome.usage.cacheHitTokens);
}

let lagHistogram: IntervalHistogram | null = null;

/** 启动时调一次。分辨率 20ms：比抓取间隔(15s)细得多，又不至于占 CPU。 */
export function startRuntimeSampling(): void {
    if (lagHistogram) return;
    lagHistogram = monitorEventLoopDelay({ resolution: 20 });
    lagHistogram.enable();
}

/**
 * 抓取时现采样一次进程级指标。
 *
 * 不用 setInterval 定时刷：这个服务大多数时间没有请求，定时器只是白白唤醒
 * 事件循环——而「事件循环延迟」正是它要衡量的东西。
 */
export function syncRuntime(state: { inflight: number; spentCny: number }): void {
    InflightGauge.set(undefined, state.inflight);
    DailyCostGauge.set(undefined, state.spentCny);
    UptimeGauge.set(undefined, process.uptime());
    HeapGauge.set(undefined, process.memoryUsage().heapUsed);

    if (lagHistogram) {
        // 单位是纳秒；reset 让每个抓取周期各自统计，避免 p99 被历史拖住
        EventLoopLagGauge.set(undefined, lagHistogram.percentile(99) / 1e9);
        lagHistogram.reset();
    }
}
