/**
 * agent loop：四态状态机（collect → act → collect → report → done）。
 *
 * 为什么手写而不是上图框架：只有一条控制流，没有分支编排、没有人工中断、
 * 没有多 agent 交接。图框架的价值在这些地方，这里用不上。
 *
 * 三条不能省的东西：
 *   - 总时长必须落成 AbortSignal，不能只写在循环条件里（挂住的连接会让条件执行不到）
 *   - 校验失败先回灌重试一次，不要第一次就抛给用户
 *   - 「查了但结果为空」和「没查成」是两种状态，不能混
 *
 * **所有状态都在 runAgent 的闭包里。** 模块级变量会让两个并发请求互相看到
 * 对方的报告候选和工具回包——这类 bug 在单测里不出现，上线后在高峰期出现。
 */

import type {
    AgentEvent,
    CheckResult,
    ErrorReason,
    Report,
    StopReason,
} from '../../shared/contract.js';
import { costOf, type Price } from './config.js';
import { Ledger } from './ledger.js';
import {
    buildSystemPrompt,
    reportInstruction,
    type CheckInfo,
    type ContextHint,
} from './prompt.js';
import type {
    ChatMessage,
    CompleteRequest,
    CompleteResponse,
    Provider,
    ToolCall,
} from './provider.js';
import { asCheckResults, TOOL_SPECS, type ToolExecutor } from './tools.js';
import { validateReport, type ValidationIssue } from './validate.js';

export interface LoopLimits {
    maxTurns: number;
    totalTimeoutMs: number;
    toolTimeoutMs: number;
    /** 单轮工具调用上限。模型一次给 8 个也照发，会把后端正常业务打出去。 */
    maxParallelTools: number;
    /** 单次诊断的金额上限（测试用；生产用的是 guard 的日预算） */
    maxCostCny?: number;
    price: Price;
}

export interface LoopDeps {
    provider: Provider;
    executor: ToolExecutor;
    checks: CheckInfo[];
    limits: LoopLimits;
    traceId: string;
    onEvent?: (e: AgentEvent) => void;
    signal?: AbortSignal;
    /** 可注入，测试里避免真实计时 */
    now?: () => number;
}

export interface RunOutcome {
    report: Report;
    stopReason: StopReason;
    ledger: Ledger;
    costCny: number;
    /** 累计 token 用量，留痕用 */
    usage: { inTokens: number; outTokens: number; cacheHitTokens: number };
    turns: number;
    issues: ValidationIssue[];
    /** 没有发出 final 事件时，server 用它转成 SSE error */
    errorReason?: ErrorReason;
}

export async function runAgent(
    question: string,
    ctx: ContextHint,
    deps: LoopDeps,
): Promise<RunOutcome> {
    // AbortSignal.timeout 内部用 unref 的定时器，不需要手动 clear；
    // AbortSignal.any 让「用户断开」和「总时长」合成一个信号，下游只认一个。
    const signal = deps.signal
        ? AbortSignal.any([deps.signal, AbortSignal.timeout(deps.limits.totalTimeoutMs)])
        : AbortSignal.timeout(deps.limits.totalTimeoutMs);
    const external = deps.signal;
    const ledger = new Ledger();
    const messages: ChatMessage[] = [
        { role: 'system', content: buildSystemPrompt(deps.checks, ctx) },
        { role: 'user', content: question },
    ];

    let cost = 0;
    const usage = { inTokens: 0, outTokens: 0, cacheHitTokens: 0 };
    let turns = 0;
    let stepIndex = 0;
    let staleStreak = 0;
    let stopReason: StopReason | undefined;
    let reportCandidate: unknown;
    let issues: ValidationIssue[] = [];

    const emit = (e: AgentEvent): void => deps.onEvent?.(e);

    try {
        for (turns = 1; turns <= deps.limits.maxTurns; turns++) {
            if (signal.aborted) {
                stopReason = abortReason();
                break;
            }
            if (budgetHit()) {
                stopReason = 'budget';
                break;
            }

            const resp = await callModel({ messages, signal });

            if (resp.toolCalls.length > 0) {
                messages.push({
                    role: 'assistant',
                    content: resp.text,
                    toolCalls: resp.toolCalls,
                });
                const { added, toolMessages } = await act(resp.toolCalls, turns);
                messages.push(...toolMessages);

                // 连续两轮没有新增 finding，说明模型在重复查同样的东西
                staleStreak = added === 0 ? staleStreak + 1 : 0;
                if (staleStreak >= 2) {
                    stopReason = 'no_new_evidence';
                    break;
                }
                continue;
            }

            // 没有工具调用：先把这段文本当报告试试
            const inline = extractReport(parseJson(resp.text));
            if (inline !== undefined) {
                reportCandidate = inline;
                stopReason = 'completed';
                break;
            }

            // 不是报告，就明确要一次（不计入工具轮）
            messages.push({ role: 'assistant', content: resp.text || '（空）' });
            messages.push({ role: 'user', content: reportInstruction(ledger.snapshot()) });
            emit({ type: 'notice', level: 'info', text: '正在整理结论' });
            const closing = await callModel({ messages, signal, json: true });
            reportCandidate = extractReport(parseJson(closing.text));
            stopReason = 'completed';
            break;
        }

        if (stopReason === undefined) {
            stopReason = 'max_turns';
            // 循环变量在这里已经自增到 maxTurns+1，对外报告的是「实际用了几轮」
            turns = deps.limits.maxTurns;
        }
    } catch (err) {
        // 模型彻底不可用：不抛给调用方，先把已查到的东西交代清楚
        if (signal.aborted || isAbortError(err)) {
            stopReason = abortReason();
        } else {
            stopReason = 'degraded';
            emit({
                type: 'notice',
                level: 'warn',
                text: '模型服务暂时不可用，只汇报已经查到的结果',
            });
        }
    }

    const composed = await compose();
    issues = composed.issues;

    const finalStop: StopReason =
        composed.fatal && stopReason === 'completed' ? 'degraded' : stopReason;

    const finalReport: Report = {
        ...composed.report,
        // 降级报告不自称有置信度；正常报告以服务端聚合为准
        confidence: composed.fatal ? composed.report.confidence : ledger.confidence(),
        checks: ledger.checks,
    };

    const outcome: RunOutcome = {
        report: finalReport,
        stopReason: finalStop,
        ledger,
        costCny: cost,
        usage,
        turns,
        issues,
    };

    // 用户主动停止 / 超时：连接已经断了，发事件没有意义，也不该再占用额度
    if (finalStop === 'aborted_by_client' || finalStop === 'total_timeout') {
        return {
            ...outcome,
            errorReason: finalStop === 'aborted_by_client' ? 'aborted' : 'timeout',
        };
    }

    // 既没有报告候选，也没有任何证据：这是服务故障，不是「结论是不完整」
    if (reportCandidate === undefined && ledger.recordsList.length === 0) {
        emit({
            type: 'error',
            reason: finalStop === 'degraded' ? 'unavailable' : 'server',
            message: '这次没能给出结论，请稍后重试',
        });
        return {
            ...outcome,
            errorReason: finalStop === 'degraded' ? 'unavailable' : 'server',
        };
    }

    emit({
        type: 'final',
        report: finalReport,
        stop_reason: finalStop,
        trace_id: deps.traceId,
        checks: ledger.checks,
    });

    return outcome;

    // ── 闭包内的辅助函数 ────────────────────────────────────

    function abortReason(): StopReason {
        return external?.aborted ? 'aborted_by_client' : 'total_timeout';
    }

    function budgetHit(): boolean {
        const cap = deps.limits.maxCostCny;
        return cap !== undefined && cost >= cap;
    }

    async function callModel(req: CompleteRequest): Promise<CompleteResponse> {
        const resp = await deps.provider.complete({ tools: TOOL_SPECS, ...req });
        cost += costOf(deps.limits.price, resp.usage);
        usage.inTokens += resp.usage.inTokens;
        usage.outTokens += resp.usage.outTokens;
        usage.cacheHitTokens += resp.usage.cacheHitTokens;
        return resp;
    }

    interface ToolOutcome {
        call: ToolCall;
        ok: boolean;
        payload: unknown;
    }

    /**
     * 执行一轮工具调用，返回新增 finding 数与**按调用顺序**排列的 tool 回包。
     *
     * 超过单轮上限的调用也必须回包：OpenAI 协议要求 assistant 消息里每个
     * tool_call 都有对应的 tool 消息，少一条下一次请求直接 400。
     */
    async function act(
        calls: ToolCall[],
        round: number,
    ): Promise<{ added: number; toolMessages: ChatMessage[] }> {
        const limit = Math.max(1, deps.limits.maxParallelTools);
        const executed = calls.slice(0, limit);
        const dropped = calls.slice(limit);

        // running 和 done 共用同一个 index，前端才能把同一行就地更新，
        // 而不是先看到一行「运行中」、再看到下面多出一行「已完成」。
        const indices = executed.map(() => stepIndex++);
        executed.forEach((call, i) => {
            emit({
                type: 'step',
                index: indices[i] ?? 0,
                label: labelOf(call),
                status: 'running',
            });
        });

        const results = await Promise.all(
            executed.map(async (call): Promise<ToolOutcome> => {
                const res = await deps.executor.execute({
                    name: call.name,
                    args: parseJson(call.arguments),
                    timeoutMs: deps.limits.toolTimeoutMs,
                });
                return { call, ok: res.ok, payload: res };
            }),
        );

        let added = 0;
        const toolMessages: ChatMessage[] = [];

        results.forEach((r, i) => {
            emit({
                type: 'step',
                index: indices[i] ?? 0,
                label: labelOf(r.call),
                status: r.ok ? 'done' : 'failed',
            });

            if (r.ok) {
                const data = (r.payload as { data?: unknown }).data;
                added += recordSuccess(r.call, data, round);
            } else {
                const reason = (r.payload as { reason?: string }).reason ?? 'unknown';
                recordFailure(r.call, reason, round);
            }

            toolMessages.push({
                role: 'tool',
                toolCallId: r.call.id,
                content: JSON.stringify(r.payload),
            });
        });

        for (const call of dropped) {
            emit({
                type: 'step',
                index: stepIndex++,
                label: labelOf(call),
                status: 'failed',
            });
            toolMessages.push({
                role: 'tool',
                toolCallId: call.id,
                content: JSON.stringify({
                    ok: false,
                    reason: 'too_many_tools',
                    message: `单轮最多 ${limit} 个工具调用，这一条没有执行`,
                }),
            });
        }

        return { added, toolMessages };
    }

    function recordSuccess(call: ToolCall, data: unknown, round: number): number {
        const params = paramsOf(call);

        if (call.name === 'run_check') {
            if (!data || typeof data !== 'object') return 0;
            return ledger.append(data as CheckResult, round, params);
        }

        if (call.name === 'run_checks') {
            let added = 0;
            for (const result of asCheckResults(data)) {
                if (!result || typeof result !== 'object') continue;
                // batch 里单条失败会带 error 且 verdict 为空——照样记账，
                // 否则「这条没查成」在最终置信度里消失了
                if (result.error) {
                    ledger.append(syntheticInconclusive(result.check, result.error), round, params);
                    continue;
                }
                added += ledger.append(result, round, params);
            }
            return added;
        }

        return 0;
    }

    function recordFailure(call: ToolCall, reason: string, round: number): void {
        if (call.name !== 'run_check' && call.name !== 'run_checks') return;
        const params = paramsOf(call);
        for (const name of checkedNames(call)) {
            ledger.append(syntheticInconclusive(name, reason), round, params);
        }
    }

    /**
     * 组织最终报告。
     *
     * 致命校验失败回灌一次——模型第二次通常能改对（它现在知道白名单了）。
     * 再失败就出降级报告：把「为什么没结论」交给用户，不编一个结论。
     */
    async function compose(): Promise<{
        report: Report;
        fatal: boolean;
        issues: ValidationIssue[];
    }> {
        if (reportCandidate !== undefined) {
            const first = validateReport(reportCandidate, ledger);
            if (!first.fatal) {
                return { report: first.report, fatal: false, issues: first.issues };
            }
            if (signal.aborted) {
                return { report: degraded(first.issues), fatal: true, issues: first.issues };
            }
            try {
                messages.push({ role: 'user', content: retryPrompt(first.issues) });
                const again = await callModel({ messages, signal, json: true });
                const second = validateReport(extractReport(parseJson(again.text)), ledger);
                if (!second.fatal) {
                    return { report: second.report, fatal: false, issues: second.issues };
                }
                return { report: degraded(second.issues), fatal: true, issues: second.issues };
            } catch {
                return { report: degraded(first.issues), fatal: true, issues: first.issues };
            }
        }

        const shapeIssue: ValidationIssue = {
            kind: 'bad_shape',
            detail: '模型没有产出可解析的报告',
        };
        const combo = [...issues, shapeIssue];
        return { report: degraded(combo), fatal: true, issues: combo };
    }

    function degraded(found: ValidationIssue[]): Report {
        const reasons = found
            .map((i) => i.detail)
            .slice(0, 3)
            .join('；');
        return {
            kind: 'diagnosis',
            title: '这次没能给出可核验的结论',
            phenomenon: `诊断在「${stopLabel(stopReason ?? 'degraded')}」状态下结束。`,
            judgement: reasons
                ? `原始输出未通过校验：${reasons}`
                : '没有拿到可用的模型输出。',
            advice: [],
            finding_ids: ledger.findingIds(),
            confidence: 0,
            checks: ledger.checks,
        };
    }

    function retryPrompt(found: ValidationIssue[]): string {
        return `上一次输出没有通过校验：${found.map((i) => i.detail).join('；')}。
请只输出符合契约的 JSON。finding_ids 只能填下面这些 id：
${ledger.snapshot()}`;
    }
}

// ── 纯函数 ──────────────────────────────────────────────────

function parseJson(text: string): unknown {
    const t = text.trim();
    if (!t) return undefined;
    try {
        return JSON.parse(t);
    } catch {
        // 模型常把 JSON 包在 ```json 里或前后带解释，取最外层一对花括号再试
        const s = t.indexOf('{');
        const e = t.lastIndexOf('}');
        if (s >= 0 && e > s) {
            try {
                return JSON.parse(t.slice(s, e + 1));
            } catch {
                return undefined;
            }
        }
        return undefined;
    }
}

/** 兼容两种形态：{"report": {...}} 和直接给报告对象。 */
function extractReport(parsed: unknown): unknown {
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return undefined;
    const obj = parsed as Record<string, unknown>;
    const candidate = 'report' in obj ? obj.report : obj;
    if (candidate && typeof candidate === 'object' && 'kind' in (candidate as object)) {
        return candidate;
    }
    return undefined;
}

function paramsOf(call: ToolCall): Record<string, string> | undefined {
    const args = parseJson(call.arguments);
    if (!args || typeof args !== 'object') return undefined;
    const params = (args as { params?: unknown }).params;
    if (!params || typeof params !== 'object' || Array.isArray(params)) return undefined;
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(params as Record<string, unknown>)) {
        if (typeof v === 'string' || typeof v === 'number') out[k] = String(v);
    }
    return Object.keys(out).length > 0 ? out : undefined;
}

function checkedNames(call: ToolCall): string[] {
    const args = parseJson(call.arguments);
    if (!args || typeof args !== 'object') return [];
    const obj = args as { name?: unknown; checks?: unknown };
    if (call.name === 'run_check') return typeof obj.name === 'string' ? [obj.name] : [];
    if (call.name === 'run_checks' && Array.isArray(obj.checks)) {
        return obj.checks.filter((c): c is string => typeof c === 'string');
    }
    return [];
}

function labelOf(call: ToolCall): string {
    const args = parseJson(call.arguments) as
        | { name?: string; checks?: unknown[]; entity?: string }
        | undefined;
    if (call.name === 'run_check' && args?.name) return `检查「${args.name}」`;
    if (call.name === 'run_checks') return `体检 ${args?.checks?.length ?? 0} 项`;
    if (call.name === 'search_records') return `查询 ${args?.entity ?? ''}`;
    if (call.name === 'pick_product') return '匹配产品名';
    return call.name;
}

function stopLabel(r: StopReason): string {
    switch (r) {
        case 'max_turns':
            return '轮数上限';
        case 'total_timeout':
            return '总时长上限';
        case 'budget':
            return '预算上限';
        case 'no_new_evidence':
            return '连续两轮没有新证据';
        case 'aborted_by_client':
            return '用户停止';
        case 'context_limit':
            return '上下文上限';
        case 'repeated_violation':
            return '反复违反输出契约';
        default:
            return '异常';
    }
}

function isAbortError(err: unknown): boolean {
    const e = err as { name?: string };
    return e?.name === 'AbortError' || e?.name === 'TimeoutError';
}

function syntheticInconclusive(check: string, reason: string): CheckResult {
    return {
        check,
        verdict: 'inconclusive',
        confidence: 0.5,
        findings: [],
        coverage: { checked: 0, skipped: 0, missing: [`调用失败：${reason}`] },
        error: reason,
    };
}
