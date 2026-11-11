/**
 * 诊断助手前端客户端。
 *
 * 为什么不用 EventSource：
 *   它只能发 GET、不能带 Authorization 头，还要把问题塞进 URL。
 *   这里必须 POST + Bearer，所以走 fetch + ReadableStream，自己切 SSE 帧。
 *
 * 解析器的原则是**不抛异常**：
 *   服务端发来一个不认识的事件，最坏结果应该是「抽屉里多一条错误提示」，
 *   而不是整个页面白屏。所以未知 type 一律降级成 error/contract。
 */

import type { Advice, AgentEvent, Report } from '@shared/contract';

import { ticketStore } from '@/lib/http';

export interface DiagnoseOptions {
    question: string;
    context?: { product?: string; route?: string };
    signal?: AbortSignal;
    onEvent: (e: AgentEvent) => void;
}

/**
 * 把缓冲区切成事件。
 *
 * SSE 帧以空行分隔；`:` 开头的是注释（服务端每 15 秒发一次 ping 保活），
 * 直接跳过。返回的 `rest` 是还没收到完整帧的尾部，下次接着拼。
 */
export function parseSSEChunk(buffer: string): { events: AgentEvent[]; rest: string } {
    const normalized = buffer.replace(/\r\n/g, '\n');
    const blocks = normalized.split('\n\n');
    const rest = blocks.pop() ?? '';
    const events: AgentEvent[] = [];

    for (const block of blocks) {
        const data = block
            .split('\n')
            .filter((line) => line.startsWith('data:'))
            .map((line) => line.slice(5).replace(/^ /, ''))
            .join('\n');

        if (!data) continue; // ping / 注释帧
        events.push(parseEvent(data));
    }

    return { events, rest };
}

function parseEvent(raw: string): AgentEvent {
    let parsed: unknown;
    try {
        parsed = JSON.parse(raw);
    } catch {
        return contractError('收到一个不是合法 JSON 的事件');
    }

    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        return contractError('事件格式不符合约定');
    }

    const o = parsed as Record<string, unknown>;
    switch (o.type) {
        case 'step':
            return {
                type: 'step',
                index: typeof o.index === 'number' ? o.index : 0,
                label: typeof o.label === 'string' ? o.label : '',
                status:
                    o.status === 'done' || o.status === 'failed'
                        ? o.status
                        : 'running',
            };

        case 'notice':
            return {
                type: 'notice',
                level: o.level === 'warn' ? 'warn' : 'info',
                text: typeof o.text === 'string' ? o.text : '',
            };

        case 'final': {
            const report = asReport(o.report);
            if (!report) return contractError('结论缺少必要字段');
            return {
                type: 'final',
                report,
                stop_reason: typeof o.stop_reason === 'string' ? (o.stop_reason as never) : 'degraded',
                trace_id: typeof o.trace_id === 'string' ? o.trace_id : '',
                checks: Array.isArray(o.checks)
                    ? o.checks.filter((c): c is string => typeof c === 'string')
                    : [],
            };
        }

        case 'error':
            return {
                type: 'error',
                reason: typeof o.reason === 'string' ? (o.reason as never) : 'server',
                message: typeof o.message === 'string' ? o.message : '诊断失败',
            };

        default:
            return contractError(`不认识的事件类型：${String(o.type)}`);
    }
}

function contractError(message: string): AgentEvent {
    return { type: 'error', reason: 'contract', message };
}

function asReport(raw: unknown): Report | null {
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null;
    const r = raw as Record<string, unknown>;

    const kind = r.kind;
    if (kind !== 'diagnosis' && kind !== 'query' && kind !== 'out_of_scope') return null;

    return {
        kind,
        title: str(r.title),
        phenomenon: str(r.phenomenon),
        judgement: str(r.judgement),
        advice: Array.isArray(r.advice)
            ? r.advice.map(asAdvice).filter((a): a is Advice => a !== null)
            : [],
        finding_ids: Array.isArray(r.finding_ids)
            ? r.finding_ids.filter((x): x is string => typeof x === 'string')
            : [],
        confidence: typeof r.confidence === 'number' ? r.confidence : 0,
        ...(Array.isArray(r.checks)
            ? { checks: r.checks.filter((x): x is string => typeof x === 'string') }
            : {}),
    };
}

function asAdvice(raw: unknown): Advice | null {
    if (!raw || typeof raw !== 'object') return null;
    const a = raw as Record<string, unknown>;
    const text = str(a.text);
    if (!text) return null;

    const ref = a.ref;
    if (ref && typeof ref === 'object' && typeof (ref as { path?: unknown }).path === 'string') {
        const r = ref as { path: string; query?: unknown };
        return {
            text,
            ref: {
                path: r.path,
                ...(r.query && typeof r.query === 'object'
                    ? { query: r.query as Record<string, string> }
                    : {}),
            },
        };
    }

    return { text };
}

function str(v: unknown): string {
    return typeof v === 'string' ? v : '';
}

/**
 * 发起一次诊断，事件按到达顺序回调。
 *
 * 不返回 Promise<Report>：用户在中途就要看到步骤条动起来，
 * 等全部结束再渲染会让一次 20 秒的诊断看起来像卡死。
 */
export async function diagnose(opts: DiagnoseOptions): Promise<void> {
    let res: Response;
    try {
        res = await fetch('/api/agent/diagnose', {
            method: 'POST',
            headers: {
                'content-type': 'application/json',
                accept: 'text/event-stream',
                authorization: `Bearer ${ticketStore.get()}`,
            },
            body: JSON.stringify({ question: opts.question, context: opts.context }),
            signal: opts.signal,
        });
    } catch (err) {
        if (isAbort(err)) return;
        opts.onEvent({ type: 'error', reason: 'network', message: '网络异常，诊断没有发出去' });
        return;
    }

    if (res.status === 401) {
        ticketStore.clear();
        opts.onEvent({
            type: 'error',
            reason: 'unavailable',
            message: '登录已失效，请刷新页面重新登录',
        });
        return;
    }

    if (!res.ok) {
        const detail = await res.text().catch(() => '');
        opts.onEvent({
            type: 'error',
            reason: 'server',
            message: detail || `诊断服务不可用（${res.status}）`,
        });
        return;
    }

    if (!res.body) {
        opts.onEvent({ type: 'error', reason: 'server', message: '浏览器不支持流式响应' });
        return;
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    let terminated = false;

    const dispatch = (events: AgentEvent[]): void => {
        for (const e of events) {
            if (e.type === 'final' || e.type === 'error') terminated = true;
            opts.onEvent(e);
        }
    };

    try {
        for (;;) {
            const { done, value } = await reader.read();
            if (done) break;

            buffer += decoder.decode(value, { stream: true });
            const { events, rest } = parseSSEChunk(buffer);
            buffer = rest;
            dispatch(events);
        }

        // 服务端最后一段可能没带结尾空行
        if (buffer.trim()) {
            dispatch(parseSSEChunk(buffer + '\n\n').events);
        }
    } catch (err) {
        if (isAbort(err)) return;
        opts.onEvent({ type: 'error', reason: 'network', message: '连接中断，结论没有收全' });
        return;
    }

    // 连接正常结束但没收到终止事件：不能当作「成功但没结论」
    if (!terminated) {
        opts.onEvent({
            type: 'error',
            reason: 'contract',
            message: '连接提前结束，没有收到结论',
        });
    }
}

function isAbort(err: unknown): boolean {
    const e = err as { name?: string };
    return e?.name === 'AbortError' || e?.name === 'CanceledError';
}
