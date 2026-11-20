/**
 * 模型适配层。
 *
 * 只暴露一个方法 `complete`，返回「文本 + 工具调用 + token 用量」。
 * 这里不做任何判定——判定在 Go 规则层，组织语言在 loop/report。
 *
 * 两个实现：
 *   createHttpProvider：真实供应商，兼容 OpenAI 的 /chat/completions
 *   createFakeProvider：按脚本回放，测试与本地演练用，不联网
 */

export interface ToolCall {
    id: string;
    name: string;
    /** 原始 JSON 字符串。解析放在 executor 里，解析失败要能回给模型而不是崩掉。 */
    arguments: string;
}

export type ChatRole = 'system' | 'user' | 'assistant' | 'tool';

export interface ChatMessage {
    role: ChatRole;
    content: string;
    /** role=tool 时必填：对应哪一次工具调用 */
    toolCallId?: string;
    /** role=assistant 时可以带工具调用 */
    toolCalls?: ToolCall[];
}

export interface ToolSpec {
    name: string;
    description: string;
    parameters: Record<string, unknown>;
}

export interface Usage {
    inTokens: number;
    outTokens: number;
    cacheHitTokens: number;
}

export interface CompleteRequest {
    messages: ChatMessage[];
    tools?: ToolSpec[];
    signal?: AbortSignal;
    temperature?: number;
    /** true 时要求模型输出纯 JSON（服务端用 response_format 表达） */
    json?: boolean;
    /** 覆盖默认模型（降级时用） */
    model?: string;
}

export interface CompleteResponse {
    text: string;
    toolCalls: ToolCall[];
    usage: Usage;
}

export interface Provider {
    readonly model: string;
    complete(req: CompleteRequest): Promise<CompleteResponse>;
}

export class ProviderError extends Error {
    constructor(
        message: string,
        readonly status: number,
    ) {
        super(message);
        this.name = 'ProviderError';
    }
}

export interface HttpProviderOptions {
    baseUrl: string;
    apiKey: string;
    model: string;
    fallbackModel?: string;
    /** 供测试注入 */
    fetchImpl?: typeof fetch;
}

const ZERO_USAGE: Usage = { inTokens: 0, outTokens: 0, cacheHitTokens: 0 };

export function createHttpProvider(opts: HttpProviderOptions): Provider {
    const doFetch = opts.fetchImpl ?? fetch;

    // 档位代号 → 上游真实模型名。白名单（M1/M2/M7/M8）是业务侧的抽象占位，
    // 换供应商时只调这张表、不动业务代码。从 env 读 AGENT_MODEL_ALIAS_<代号>。
    // 没配别名时按原样下发（部分转售网关直接认 M1 这类代号）。
    function resolveModel(code: string): string {
        return process.env[`AGENT_MODEL_ALIAS_${code}`] || code;
    }

    async function once(req: CompleteRequest, model: string): Promise<CompleteResponse> {
        const body: Record<string, unknown> = {
            model: resolveModel(model),
            messages: toOpenAIMessages(req.messages),
            stream: false,
        };
        if (req.tools?.length) body.tools = toOpenAITools(req.tools);
        if (req.temperature !== undefined) body.temperature = req.temperature;
        if (req.json) body.response_format = { type: 'json_object' };

        const res = await doFetch(`${opts.baseUrl}/chat/completions`, {
            method: 'POST',
            headers: {
                'content-type': 'application/json',
                authorization: `Bearer ${opts.apiKey}`,
            },
            body: JSON.stringify(body),
            signal: req.signal,
        });

        if (!res.ok) {
            // 只带状态码和一小段正文，绝不回显请求头（里面有 key）
            const detail = (await res.text().catch(() => '')).slice(0, 200);
            throw new ProviderError(`模型返回 ${res.status}: ${detail}`, res.status);
        }

        const json = (await res.json()) as OpenAIChatResponse;
        const choice = json.choices?.[0];
        const message = choice?.message;
        const toolCalls = (message?.tool_calls ?? []).map((c) => ({
            id: c.id,
            name: c.function.name,
            arguments: c.function.arguments ?? '{}',
        }));

        return {
            text: message?.content ?? '',
            toolCalls,
            usage: {
                inTokens: json.usage?.prompt_tokens ?? 0,
                outTokens: json.usage?.completion_tokens ?? 0,
                cacheHitTokens: json.usage?.prompt_tokens_details?.cached_tokens ?? 0,
            },
        };
    }

    return {
        model: opts.model,
        async complete(req) {
            try {
                return await once(req, req.model ?? opts.model);
            } catch (err) {
                // 降级只对「服务端故障 / 网络」做，不对 4xx 做——
                // 参数错换模型也不会变对，重试只是多烧一次钱。
                const retryable =
                    !(err instanceof ProviderError) || err.status >= 500 || err.status === 429;
                if (!retryable || !opts.fallbackModel) throw err;
                return await once(req, opts.fallbackModel);
            }
        },
    };
}

// ── 假 provider ─────────────────────────────────────────────

export interface FakeProviderOptions {
    /** 每一项是一次 `complete` 的返回。JSON 里带 tool_calls 就按工具调用解析。 */
    script: string[];
    model?: string;
    usage?: Partial<Usage>;
}

/**
 * 按脚本顺序回放。
 *
 * 脚本耗尽后**抛错而不是返回空**：返回空会让 loop 以为模型没话说了，
 * 静默走完一轮，测试就变成「什么都没验证但全部通过」。
 */
export function createFakeProvider(opts: FakeProviderOptions): Provider {
    let cursor = 0;
    return {
        model: opts.model ?? 'M1',
        async complete(req) {
            if (req.signal?.aborted) throw abortError();
            const entry = opts.script[cursor++];
            if (entry === undefined) throw new Error('fake provider: script exhausted');

            const usage: Usage = {
                inTokens: opts.usage?.inTokens ?? 100,
                outTokens: opts.usage?.outTokens ?? 50,
                cacheHitTokens: opts.usage?.cacheHitTokens ?? 0,
            };

            let parsed: unknown;
            try {
                parsed = JSON.parse(entry);
            } catch {
                return { text: entry, toolCalls: [], usage: ZERO_USAGE };
            }

            const rawCalls = (parsed as { tool_calls?: unknown }).tool_calls;
            if (Array.isArray(rawCalls)) {
                return {
                    text: '',
                    toolCalls: rawCalls.map((c, i) => {
                        const call = c as { id?: string; name?: string; arguments?: unknown };
                        return {
                            id: call.id ?? `call_${i}`,
                            name: call.name ?? '',
                            arguments:
                                typeof call.arguments === 'string'
                                    ? call.arguments
                                    : JSON.stringify(call.arguments ?? {}),
                        };
                    }),
                    usage,
                };
            }

            // 其余情况原样交给 loop 解析：可能是 {"report":{...}}，也可能是别的
            return { text: entry, toolCalls: [], usage };
        },
    };
}

function abortError(): Error {
    const e = new Error('aborted');
    e.name = 'AbortError';
    return e;
}

// ── OpenAI 协议映射 ─────────────────────────────────────────

interface OpenAIChatResponse {
    choices?: {
        message?: {
            content?: string | null;
            tool_calls?: { id: string; function: { name: string; arguments?: string } }[];
        };
    }[];
    usage?: {
        prompt_tokens?: number;
        completion_tokens?: number;
        prompt_tokens_details?: { cached_tokens?: number };
    };
}

function toOpenAIMessages(messages: ChatMessage[]): unknown[] {
    return messages.map((m) => {
        if (m.role === 'tool') {
            return { role: 'tool', tool_call_id: m.toolCallId, content: m.content };
        }
        if (m.role === 'assistant' && m.toolCalls?.length) {
            return {
                role: 'assistant',
                content: m.content || null,
                tool_calls: m.toolCalls.map((c) => ({
                    id: c.id,
                    type: 'function',
                    function: { name: c.name, arguments: c.arguments },
                })),
            };
        }
        return { role: m.role, content: m.content };
    });
}

function toOpenAITools(tools: ToolSpec[]): unknown[] {
    return tools.map((t) => ({
        type: 'function',
        function: { name: t.name, description: t.description, parameters: t.parameters },
    }));
}
