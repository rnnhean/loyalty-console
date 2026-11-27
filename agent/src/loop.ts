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
import type { LangfuseTracer } from './langfuse.js';
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
    /** 有值时才上报 Langfuse；测试与本地关掉埋点时不传 */
    langfuse?: LangfuseTracer;
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
    let lastCallSig = '';
    let toolsCalled = 0;
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

            // 工具调用有两种到达方式，都要认：
            //   1) 标准 function calling —— 供应商返回 choices[].message.tool_calls；
            //   2) 文本 JSON —— OUTPUT_CONTRACT 明确要求模型「需要更多信息时只输出
            //      {"tool_calls":[...]}」。小模型/网关经常只会第 2 种。
            // 只认第 1 种时，模型老老实实按提示词输出文本工具调用，loop 却当成
            // 「没有工具调用」，直接把空账本交给报告分支——实测就是这么丢掉整个查询的。
            const calls =
                resp.toolCalls.length > 0
                    ? resp.toolCalls
                    : extractTextToolCalls(parseJson(resp.text));

            if (calls.length > 0) {
                messages.push({
                    role: 'assistant',
                    content: resp.text,
                    toolCalls: calls,
                });
                const { added, toolMessages } = await act(calls, turns);
                messages.push(...toolMessages);
                toolsCalled += calls.length;

                // 判断「原地打转」用调用签名，而不是 finding 数。
                //
                // 查询类工具（列产品、搜记录、挑候选）压根不产生 finding，
                // 按 finding 计数会把一次正常查询判成重复；而按工具类型豁免，
                // 又会对「连调 5 次 list_products」这种真正的原地打转视而不见，
                // 一路烧到轮数上限。按「这轮调的（工具+参数）跟上一轮一模一样」来判最准。
                const sig = calls
                    .map((c) => `${c.name}:${c.arguments}`)
                    .sort()
                    .join('|');
                staleStreak = sig === lastCallSig && added === 0 ? staleStreak + 1 : 0;
                lastCallSig = sig;
                if (staleStreak >= 2) {
                    stopReason = 'no_new_evidence';
                    break;
                }
                continue;
            }

            // 没有工具调用：先把这段文本当报告试试
            const inline = extractReport(parseJson(resp.text));

            // 账本为空时收到 diagnosis/query 结论，一律不采纳。
            //
            // 提示词里写着「工具没返回的，你不知道」，但实测模型面对模糊提问
            // （「有没有配错的券」）会第一轮直接回一条 inconclusive 就收尾——
            // 它等于在没有任何数据的情况下宣布「判不了」，用户花了钱什么也没查到。
            // 这里把它顶回去，要求先取数。out_of_scope 例外：它本来就不依赖数据。
            // 顶回去也计入轮次，模型若始终不调工具，最终会落到 max_turns。
            const emptyLedger = ledger.recordsList.length === 0;
            if (
                inline !== undefined &&
                emptyLedger &&
                (inline as { kind?: unknown }).kind !== 'out_of_scope'
            ) {
                messages.push({ role: 'assistant', content: resp.text || '（空）' });
                messages.push({
                    role: 'user',
                    content:
                        '你还没有执行任何检查，当前没有任何工具返回的数据。先调用工具取数，再下结论。',
                });
                emit({ type: 'notice', level: 'info', text: '正在执行检查' });
                continue;
            }

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

        // 循环是因轮数上限 / 原地打转停的，但模型已经查到了东西——
        // 这时候不能直接判「服务故障」。补一次收尾调用，把已有结果落成报告。
        // 典型场景：「一共有多少个产品」，模型反复调 list_products 直到被叫停，
        // 数据其实早就拿到了，只是没人替它写成结论。
        if (
            reportCandidate === undefined &&
            toolsCalled > 0 &&
            stopReason !== 'degraded' &&
            // 预算已经烧完，再调一次就是超支；调用方等的就是「到预算了」这个信号
            stopReason !== 'budget' &&
            !signal.aborted
        ) {
            try {
                // 纯查询（账本为空）不能套 reportInstruction——那段是给「按证据下结论」用的，
                // 会明确告诉模型「只能用这些 id」，而这里根本没有 id 可用。
                messages.push({
                    role: 'user',
                    content:
                        ledger.recordsList.length === 0
                            ? '现在给出最终报告，只输出 JSON。你没有跑过诊断规则，' +
                              '请直接根据前面工具返回的查询结果作答，kind 用 query，finding_ids 留空，' +
                              'advice 可以为空。'
                            : reportInstruction(ledger.snapshot()),
                });
                const closing = await callModel({ messages, signal, json: true });
                const rep = extractReport(parseJson(closing.text));
                // 只有真拿到报告才改判 completed；否则保留原停止原因
                // （no_new_evidence / max_turns），别把「打转停下」粉饰成「正常结束」。
                if (rep !== undefined) {
                    reportCandidate = rep;
                    stopReason = 'completed';
                }
            } catch {
                // 收尾也失败就维持原判，走下面的降级路径
            }
        }
    } catch (err) {
        // 模型彻底不可用：不抛给调用方，先把已查到的东西交代清楚
        if (signal.aborted || isAbortError(err)) {
            stopReason = abortReason();
        } else {
            stopReason = 'degraded';
            // 降级必须留下可排查的线索：ProviderError 的 message 只有状态码和
            // 响应正文片段（不含请求头/key），截断后写日志是安全的。
            console.error('[loop] model call failed:', String((err as Error)?.message ?? err).slice(0, 300));
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
    //
    // 例外：调用过工具（比如纯查询走 list_products/search_records）时，
    // 答案在工具结果里而不在账本里——这条路不能报「请稍后重试」，
    // 那样用户明明问的是「一共有多少个产品」，拿到的却是一条服务故障。
    if (
        reportCandidate === undefined &&
        ledger.recordsList.length === 0 &&
        (toolsCalled === 0 || finalStop === 'degraded')
    ) {
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
        const startedAt = new Date();
        const resp = await deps.provider.complete({ tools: TOOL_SPECS, ...req });
        const endedAt = new Date();
        cost += costOf(deps.limits.price, resp.usage);
        usage.inTokens += resp.usage.inTokens;
        usage.outTokens += resp.usage.outTokens;
        usage.cacheHitTokens += resp.usage.cacheHitTokens;

        // 每一次模型调用都是一条 generation，Langfuse 面板上才看得到
        // 轮次、token、耗时、是否走了工具——没有这个，trace 只是一个空壳。
        deps.langfuse?.recordGeneration({
            name: req.json ? 'llm.report' : 'llm.chat',
            model: req.model ?? deps.provider.model,
            startTime: startedAt,
            endTime: endedAt,
            inputTokens: resp.usage.inTokens,
            outputTokens: resp.usage.outTokens,
            inputSummary: summarizeMessages(req.messages),
            outputSummary:
                resp.text.slice(0, 2000) ||
                (resp.toolCalls.length > 0
                    ? `[tool_calls] ${resp.toolCalls.map((c) => c.name).join(', ')}`
                    : ''),
            toolCallCount: resp.toolCalls.length,
        });

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

/** 从模型文本里取 {"tool_calls":[...]}——OUTPUT_CONTRACT 约定的第一种形态。 */
function extractTextToolCalls(parsed: unknown): ToolCall[] {
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return [];
    const arr = (parsed as { tool_calls?: unknown }).tool_calls;
    if (!Array.isArray(arr)) return [];
    const out: ToolCall[] = [];
    for (const [i, raw] of arr.entries()) {
        if (!raw || typeof raw !== 'object') continue;
        const c = raw as { id?: unknown; name?: unknown; arguments?: unknown };
        const name = typeof c.name === 'string' ? c.name.trim() : '';
        if (!name) continue;
        const args =
            typeof c.arguments === 'string'
                ? c.arguments
                : JSON.stringify(c.arguments ?? {});
        out.push({ id: typeof c.id === 'string' && c.id ? c.id : `txt${i + 1}`, name, arguments: args });
    }
    return out;
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
        | { name?: string; checks?: unknown[]; entity?: string; rule_id?: number }
        | undefined;
    if (call.name === 'run_check' && args?.name) return `检查「${args.name}」`;
    if (call.name === 'run_checks') return `体检 ${args?.checks?.length ?? 0} 项`;
    if (call.name === 'search_records') return `查询 ${args?.entity ?? ''}`;
    if (call.name === 'pick_product') return '匹配产品名';
    // 规则工具：用户在抽屉里看到的是「解释规则 #7」而不是 explain_rule，
    // 否则步骤列表会中英混杂，不像一个成品。
    if (call.name === 'list_rules') return '列出可配置规则';
    if (call.name === 'explain_rule') {
        return args?.rule_id ? `解释规则 #${args.rule_id}` : '解释规则';
    }
    if (call.name === 'simulate_rules') return '试跑全部规则';
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

/**
 * Langfuse 上展示用的输入摘要。
 *
 * 只放消息条数和最后一条用户输入——完整上下文动辄上万 token，
 * 全塞进 Langfuse 既贵又没法读。要复现问题就按 trace_id 查本地日志。
 */
function summarizeMessages(messages: ChatMessage[]): string {
    const lastUser = [...messages].reverse().find((m) => m.role === 'user');
    return `[${messages.length} 条消息] ${(lastUser?.content ?? '').slice(0, 1500)}`;
}
