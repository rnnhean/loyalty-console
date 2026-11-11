import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
    createFakeProvider,
    createHttpProvider,
    ProviderError,
    type ChatMessage,
} from './provider.js';

const USER: ChatMessage[] = [{ role: 'user', content: '为什么兑换不了' }];

function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
        status,
        headers: { 'content-type': 'application/json' },
    });
}

test('http provider 解析 tool_calls 与缓存命中 token', async () => {
    let sent: Record<string, unknown> | undefined;

    const provider = createHttpProvider({
        baseUrl: 'http://model.local',
        apiKey: 'test-key',
        model: 'M1',
        fetchImpl: async (_url, init) => {
            sent = JSON.parse(String(init?.body)) as Record<string, unknown>;
            return jsonResponse({
                choices: [
                    {
                        message: {
                            content: '',
                            tool_calls: [
                                {
                                    id: 'c1',
                                    function: {
                                        name: 'run_check',
                                        arguments: '{"name":"orphan_rules"}',
                                    },
                                },
                            ],
                        },
                    },
                ],
                usage: {
                    prompt_tokens: 120,
                    completion_tokens: 30,
                    prompt_tokens_details: { cached_tokens: 100 },
                },
            });
        },
    });

    const resp = await provider.complete({ messages: USER });

    assert.equal(resp.toolCalls.length, 1);
    assert.equal(resp.toolCalls[0]?.name, 'run_check');
    assert.equal(resp.toolCalls[0]?.id, 'c1');
    assert.deepEqual(resp.usage, { inTokens: 120, outTokens: 30, cacheHitTokens: 100 });
    assert.equal(sent?.model, 'M1');
});

test('5xx 降级到 fallback 模型', async () => {
    const models: string[] = [];

    const provider = createHttpProvider({
        baseUrl: 'http://model.local',
        apiKey: 'test-key',
        model: 'M1',
        fallbackModel: 'M11',
        fetchImpl: async (_url, init) => {
            const body = JSON.parse(String(init?.body)) as { model: string };
            models.push(body.model);
            if (models.length === 1) return new Response('upstream boom', { status: 502 });
            return jsonResponse({ choices: [{ message: { content: '{"kind":"query"}' } }] });
        },
    });

    const resp = await provider.complete({ messages: USER });

    assert.deepEqual(models, ['M1', 'M11']);
    assert.match(resp.text, /kind/);
});

test('4xx 不降级：参数错换模型也不会变对', async () => {
    let calls = 0;

    const provider = createHttpProvider({
        baseUrl: 'http://model.local',
        apiKey: 'test-key',
        model: 'M1',
        fallbackModel: 'M11',
        fetchImpl: async () => {
            calls++;
            return new Response('bad request', { status: 400 });
        },
    });

    await assert.rejects(
        () => provider.complete({ messages: USER }),
        (err: unknown) => err instanceof ProviderError && err.status === 400,
    );
    assert.equal(calls, 1);
});

test('fake provider 把脚本里的 tool_calls 转成工具调用', async () => {
    const provider = createFakeProvider({
        script: ['{"tool_calls":[{"id":"c1","name":"run_checks","arguments":{"checks":["a"]}}]}'],
    });

    const resp = await provider.complete({ messages: USER });

    assert.equal(resp.toolCalls[0]?.name, 'run_checks');
    assert.equal(resp.toolCalls[0]?.arguments, '{"checks":["a"]}');
    assert.equal(resp.text, '');
});

test('fake provider 脚本耗尽时抛错，而不是静默返回空', async () => {
    const provider = createFakeProvider({ script: [] });
    await assert.rejects(() => provider.complete({ messages: USER }), /script exhausted/);
});

test('fake provider 已经取消的信号立即失败', async () => {
    const provider = createFakeProvider({ script: ['{}'] });
    const controller = new AbortController();
    controller.abort();

    await assert.rejects(
        () => provider.complete({ messages: USER, signal: controller.signal }),
        (err: unknown) => (err as Error).name === 'AbortError',
    );
});
