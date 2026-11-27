/**
 * Langfuse 上报。
 *
 * **为什么不用旧的事件接口**：Langfuse Cloud 的 v3 ingestion API 已按官方公告
 * 在 2026-11-16 关停（`trace-create` / `generation-create` / `span-create`
 * 全部返回 410），只剩 OTel 通道可用。所以这里直接打 OTLP/HTTP JSON：
 * `POST /api/public/otel/v1/traces`。
 *
 * 不引 SDK 的理由同前：agent 的依赖表是空的，为埋点栽一棵依赖树不划算，
 * OTLP JSON 就是一层可读的 JSON 结构而已。
 *
 * 设计取舍：
 *   - **fire-and-forget**：上报失败绝不能影响诊断本身。
 *   - **不落本地**：本地 trace 文件按约定不存问题原文（见 trace.ts），
 *     但 Langfuse 是受控内部观测平台、有 project 级隔离，这里保留原文。
 *   - **一次成批**：root span + N 个 generation 一起提交，不会出现
 *     半个 trace（有根没子）的情况。
 */

import { createHash, randomBytes } from 'node:crypto';

export interface LangfuseConfig {
    publicKey: string;
    secretKey: string;
    /** 例如 https://cloud.langfuse.com */
    host: string;
}

export interface LlmCallRecord {
    name: string;
    model: string;
    startTime: Date;
    endTime: Date;
    inputTokens: number;
    outputTokens: number;
    /** 模型可见的输入摘要，不塞完整上下文 */
    inputSummary: string;
    outputSummary: string;
    /** 本次响应的工具调用数；0 表示这是纯文本回合 */
    toolCallCount: number;
}

export interface LangfuseTracer {
    recordGeneration(rec: LlmCallRecord): void;
    flush(summary: {
        name: string;
        input: string;
        output: string;
        metadata: Record<string, unknown>;
        startTime: Date;
        endTime: Date;
    }): Promise<void>;
}

/** OTel 的 traceId 固定 32 位十六进制；业务 id 不是，用哈希稳定映射。 */
function toOtelTraceId(businessId: string): string {
    return createHash('sha256').update(businessId).digest('hex').slice(0, 32);
}

function newSpanId(): string {
    return randomBytes(8).toString('hex');
}

/** OTLP JSON 的时间是纳秒字符串，直接 Date 换算。 */
function toNanos(d: Date): string {
    return (BigInt(d.getTime()) * 1_000_000n).toString();
}

type OtlpValue = { stringValue: string } | { intValue: string };

function attr(key: string, value: string | number): { key: string; value: OtlpValue } {
    return {
        key,
        value: typeof value === 'number' ? { intValue: String(Math.trunc(value)) } : { stringValue: value },
    };
}

export function langfuseConfigFrom(env: NodeJS.ProcessEnv): LangfuseConfig | null {
    const publicKey = env.LANGFUSE_PUBLIC_KEY ?? '';
    const secretKey = env.LANGFUSE_SECRET_KEY ?? '';
    const host = (env.LANGFUSE_HOST ?? 'https://cloud.langfuse.com').replace(/\/+$/, '');
    if (!publicKey || !secretKey) return null;
    return { publicKey, secretKey, host };
}

export function createLangfuseTracer(
    cfg: LangfuseConfig,
    businessTraceId: string,
    fetchImpl: typeof fetch = fetch,
): LangfuseTracer {
    const traceId = toOtelTraceId(businessTraceId);
    const rootSpanId = newSpanId();
    const childSpans: Record<string, unknown>[] = [];

    return {
        recordGeneration(rec) {
            childSpans.push({
                traceId,
                spanId: newSpanId(),
                parentSpanId: rootSpanId,
                name: rec.name,
                kind: 1, // SPAN_KIND_INTERNAL
                startTimeUnixNano: toNanos(rec.startTime),
                endTimeUnixNano: toNanos(rec.endTime),
                attributes: [
                    // Langfuse 靠这个属性把 span 归成 generation，才有 token/成本视图
                    attr('langfuse.observation.type', 'generation'),
                    // 两套都写：`langfuse.observation.*` 是 Langfuse 自家口径（v4 优先），
                    // `gen_ai.*` 是 OTel 标准口径。只写后者时实测 model/usage 都是空的。
                    attr('langfuse.observation.model.name', rec.model),
                    attr(
                        'langfuse.observation.usage_details',
                        JSON.stringify({ input: rec.inputTokens, output: rec.outputTokens }),
                    ),
                    attr('gen_ai.request.model', rec.model),
                    attr('gen_ai.usage.input_tokens', rec.inputTokens),
                    attr('gen_ai.usage.output_tokens', rec.outputTokens),
                    attr('langfuse.observation.input', rec.inputSummary),
                    attr('langfuse.observation.output', rec.outputSummary),
                    attr('agent.tool_call_count', rec.toolCallCount),
                ],
                status: { code: 1 },
            });
        },

        async flush(summary) {
            const rootSpan = {
                traceId,
                spanId: rootSpanId,
                name: summary.name,
                kind: 1,
                startTimeUnixNano: toNanos(summary.startTime),
                endTimeUnixNano: toNanos(summary.endTime),
                attributes: [
                    attr('langfuse.observation.type', 'span'),
                    attr('langfuse.trace.name', summary.name),
                    attr('langfuse.observation.input', summary.input),
                    attr('langfuse.observation.output', summary.output),
                    ...Object.entries(summary.metadata).map(([k, v]) =>
                        attr(`agent.${k}`, typeof v === 'number' ? v : JSON.stringify(v)),
                    ),
                ],
                status: { code: 1 },
            };

            const payload = {
                resourceSpans: [
                    {
                        resource: {
                            attributes: [attr('service.name', 'loyalty-console-agent')],
                        },
                        scopeSpans: [
                            {
                                scope: { name: 'loyalty-console-agent', version: '0.1.0' },
                                spans: [rootSpan, ...childSpans],
                            },
                        ],
                    },
                ],
            };

            const auth = Buffer.from(`${cfg.publicKey}:${cfg.secretKey}`).toString('base64');
            try {
                const res = await fetchImpl(`${cfg.host}/api/public/otel/v1/traces`, {
                    method: 'POST',
                    headers: {
                        'content-type': 'application/json',
                        authorization: `Basic ${auth}`,
                    },
                    body: JSON.stringify(payload),
                    signal: AbortSignal.timeout(8000),
                });
                if (!res.ok) {
                    // 只记状态码，绝不打请求体（auth 里是 key 的 base64）
                    console.error('[langfuse] otlp 上报返回', res.status);
                }
            } catch (err) {
                console.error(
                    '[langfuse] 上报失败:',
                    String((err as Error)?.message ?? err).slice(0, 120),
                );
            }
        },
    };
}
