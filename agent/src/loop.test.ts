import assert from 'node:assert/strict';
import { test } from 'node:test';

import type { AgentEvent, FinalEvent } from '../../shared/contract.js';
import { runAgent, type LoopDeps } from './loop.js';
import type { CompleteRequest, CompleteResponse, Provider } from './provider.js';
import { baseLimits, collectEvents, fakeExecutor, finding, result } from './testkit.js';

const USAGE = { inTokens: 10, outTokens: 5, cacheHitTokens: 0 };

/** 按脚本回放，同时把每次请求记下来（要断言消息序列就靠它）。 */
function scripted(script: string[]): { provider: Provider; calls: CompleteRequest[] } {
    const calls: CompleteRequest[] = [];
    let cursor = 0;

    return {
        calls,
        provider: {
            model: 'test',
            async complete(req: CompleteRequest): Promise<CompleteResponse> {
                calls.push(req);
                const entry = script[cursor++];
                if (entry === undefined) throw new Error('script exhausted');

                const parsed = JSON.parse(entry) as {
                    tool_calls?: { id: string; name: string; arguments?: unknown }[];
                };
                if (Array.isArray(parsed.tool_calls)) {
                    return {
                        text: '',
                        toolCalls: parsed.tool_calls.map((c) => ({
                            id: c.id,
                            name: c.name,
                            arguments:
                                typeof c.arguments === 'string'
                                    ? c.arguments
                                    : JSON.stringify(c.arguments ?? {}),
                        })),
                        usage: USAGE,
                    };
                }
                return { text: entry, toolCalls: [], usage: USAGE };
            },
        },
    };
}

function deps(provider: Provider, over: Partial<LoopDeps> = {}): LoopDeps {
    return {
        provider,
        executor: fakeExecutor(),
        checks: [],
        limits: baseLimits(),
        traceId: 'trace-test',
        ...over,
    };
}

function finalOf(events: AgentEvent[]): FinalEvent {
    const e = events.find((x): x is FinalEvent => x.type === 'final');
    assert.ok(e, '没有发出 final 事件');
    return e;
}

const GOOD_REPORT = JSON.stringify({
    kind: 'diagnosis',
    title: '有规则没有挂到权益包',
    phenomenon: '扫到一条规则没有权益包',
    judgement: '这些规则永远不会被触发',
    advice: [],
    finding_ids: ['orphan_rules/rule:101'],
    confidence: 1,
});

test('两轮正常：一次工具调用 + 一份报告', async () => {
    const { provider } = scripted([
        JSON.stringify({
            tool_calls: [{ id: 'c1', name: 'run_checks', arguments: { checks: ['orphan_rules'] } }],
        }),
        GOOD_REPORT,
    ]);
    const { events, onEvent } = collectEvents();

    const out = await runAgent(
        '为什么兑换不了',
        {},
        deps(provider, {
            onEvent,
            executor: fakeExecutor({
                checks: {
                    orphan_rules: result('orphan_rules', {
                        verdict: 'issue',
                        confidence: 0.9,
                        findings: [finding('rule:101')],
                    }),
                },
            }),
        }),
    );

    const final = finalOf(events);
    assert.equal(final.stop_reason, 'completed');
    assert.equal(final.trace_id, 'trace-test');
    assert.deepEqual(final.report.finding_ids, ['orphan_rules/rule:101']);
    assert.equal(final.report.confidence, 0.9);
    assert.deepEqual(final.checks, ['orphan_rules']);

    // 工具调用产生 running + done 两个 step
    const steps = events.filter((e) => e.type === 'step');
    assert.deepEqual(
        steps.map((s) => (s.type === 'step' ? s.status : '')),
        ['running', 'done'],
    );

    assert.equal(out.stopReason, 'completed');
});

test('轮数上限：模型一直不给报告', async () => {
    const { provider } = scripted([
        JSON.stringify({
            tool_calls: [{ id: 'c1', name: 'run_check', arguments: { name: 'tier_gap' } }],
        }),
    ]);
    const { events, onEvent } = collectEvents();

    const out = await runAgent(
        'x',
        {},
        deps(provider, {
            onEvent,
            limits: baseLimits({ maxTurns: 1 }),
            executor: fakeExecutor({
                checks: { tier_gap: result('tier_gap', { findings: [finding('tier:5')] }) },
            }),
        }),
    );

    assert.equal(out.stopReason, 'max_turns');
    assert.equal(out.turns, 1);
    // 有证据但没报告 → 降级报告，仍然发 final
    assert.equal(finalOf(events).stop_reason, 'max_turns');
    assert.deepEqual(finalOf(events).report.finding_ids, ['tier_gap/tier:5']);
});

test('连续两轮没有新证据就停，不再烧钱', async () => {
    const call = JSON.stringify({
        tool_calls: [{ id: 'c1', name: 'run_check', arguments: { name: 'orphan_rules' } }],
    });
    const { provider, calls } = scripted([call, call, call, call]);
    const { onEvent } = collectEvents();

    const out = await runAgent(
        'x',
        {},
        deps(provider, {
            onEvent,
            executor: fakeExecutor({
                checks: {
                    orphan_rules: result('orphan_rules', {
                        verdict: 'issue',
                        findings: [finding('rule:101')],
                    }),
                },
            }),
        }),
    );

    assert.equal(out.stopReason, 'no_new_evidence');
    // 第 1 轮有 finding，第 2、3 轮重复 → stale 到 2 停止。
    // 停止后 loop 会补一次收尾调用，把已查到的结果落成报告（共 4 次）。
    // 关键是「只补一次」：不是无限重试。
    assert.equal(calls.length, 4);
    assert.equal(out.report.finding_ids.length, 1);
});

test('模型连续两次给不出合法报告 → 降级，置信度为 0', async () => {
    const { provider } = scripted([
        JSON.stringify({
            tool_calls: [{ id: 'c1', name: 'run_check', arguments: { name: 'orphan_rules' } }],
        }),
        '不是 JSON',
        '还是不是 JSON',
        '第三次也不对',
    ]);
    const { events, onEvent } = collectEvents();

    const out = await runAgent(
        'x',
        {},
        deps(provider, {
            onEvent,
            executor: fakeExecutor({
                checks: { orphan_rules: result('orphan_rules', { findings: [finding('rule:101')] }) },
            }),
        }),
    );

    assert.equal(out.stopReason, 'degraded');
    assert.equal(out.report.confidence, 0);
    assert.match(out.report.title, /没能给出/);
    assert.ok(out.issues.some((i) => i.kind === 'bad_shape'));
    assert.equal(finalOf(events).stop_reason, 'degraded');
});

test('单条规则失败记成 inconclusive，不影响其他规则', async () => {
    const { provider } = scripted([
        JSON.stringify({
            tool_calls: [
                {
                    id: 'c1',
                    name: 'run_checks',
                    arguments: { checks: ['orphan_rules', 'tier_gap'] },
                },
            ],
        }),
        JSON.stringify({
            kind: 'diagnosis',
            title: '等级阈值有问题',
            phenomenon: '扫到一条阈值异常',
            judgement: 'issue',
            advice: [],
            finding_ids: ['tier_gap/tier:5'],
            confidence: 1,
        }),
    ]);
    const { onEvent } = collectEvents();

    const out = await runAgent(
        'x',
        {},
        deps(provider, {
            onEvent,
            executor: fakeExecutor({
                checks: {
                    tier_gap: result('tier_gap', { findings: [finding('tier:5')] }),
                },
                failures: { orphan_rules: 'server_error' },
            }),
        }),
    );

    assert.deepEqual(out.ledger.inconclusiveChecks(), ['orphan_rules']);
    // 失败那条按 0.5 记账，聚合置信度被拉下来
    assert.equal(out.report.confidence, 0.5);
    assert.deepEqual(out.report.finding_ids, ['tier_gap/tier:5']);
    assert.equal(out.stopReason, 'completed');
});

test('预算耗尽在调用模型之前就停', async () => {
    const { provider, calls } = scripted([
        JSON.stringify({
            tool_calls: [{ id: 'c1', name: 'run_check', arguments: { name: 'orphan_rules' } }],
        }),
        GOOD_REPORT,
    ]);
    const { onEvent } = collectEvents();

    const out = await runAgent(
        'x',
        {},
        deps(provider, {
            onEvent,
            limits: baseLimits({
                maxCostCny: 0.01,
                price: { input: 1000, output: 1000, cacheRead: 1000 },
            }),
        }),
    );

    assert.equal(out.stopReason, 'budget');
    assert.equal(calls.length, 1, '预算用完后不应该再调用模型');
});

test('外部取消：立即停，不再调用模型', async () => {
    const ac = new AbortController();
    let calls = 0;

    const provider: Provider = {
        model: 'test',
        async complete() {
            calls++;
            ac.abort();
            return {
                text: '',
                toolCalls: [{ id: 'c1', name: 'run_check', arguments: '{"name":"orphan_rules"}' }],
                usage: USAGE,
            };
        },
    };

    const { events, onEvent } = collectEvents();
    const out = await runAgent('x', {}, deps(provider, { signal: ac.signal, onEvent }));

    assert.equal(calls, 1);
    assert.equal(out.stopReason, 'aborted_by_client');
    assert.equal(out.errorReason, 'aborted');
    // 连接已断，不发 final
    assert.equal(events.some((e) => e.type === 'final'), false);
});

test('未知工具返回 unknown_tool，步骤标 failed，loop 继续', async () => {
    const { provider } = scripted([
        JSON.stringify({
            tool_calls: [{ id: 'c1', name: 'delete_everything', arguments: {} }],
        }),
        JSON.stringify({
            kind: 'out_of_scope',
            title: '不在服务范围',
            phenomenon: '这个问题和会员体系无关',
            judgement: '无法回答',
            advice: [],
            finding_ids: [],
            confidence: 1,
        }),
    ]);
    const { events, onEvent } = collectEvents();

    const out = await runAgent('x', {}, deps(provider, { onEvent }));

    const failed = events.filter((e) => e.type === 'step' && e.status === 'failed');
    assert.equal(failed.length, 1);
    assert.equal(out.stopReason, 'completed');
    assert.equal(out.report.kind, 'out_of_scope');
});

test('单轮超过上限的工具调用也要有回包，否则下一次请求会被模型侧 400', async () => {
    const many = Array.from({ length: 6 }, (_, i) => ({
        id: `c${i}`,
        name: 'run_check',
        arguments: { name: `check_${i}` },
    }));

    const { provider, calls } = scripted([
        JSON.stringify({ tool_calls: many }),
        JSON.stringify({
            kind: 'query',
            title: '查询',
            phenomenon: '没有异常',
            judgement: '无',
            advice: [],
            finding_ids: [],
            confidence: 1,
        }),
    ]);

    const executor = fakeExecutor();
    const out = await runAgent(
        'x',
        {},
        deps(provider, { executor, limits: baseLimits({ maxParallelTools: 4 }) }),
    );

    assert.equal(executor.calls.length, 4, '只执行上限内的 4 个');
    const second = calls[1];
    const toolMessages = second?.messages.filter((m) => m.role === 'tool') ?? [];
    assert.equal(toolMessages.length, 6, '6 个调用都必须有 tool 回包');
    assert.equal(out.stopReason, 'completed');
});

test('报告引用了不存在的证据 → 回灌重试一次后通过', async () => {
    const bad = JSON.stringify({
        kind: 'diagnosis',
        title: '有规则没挂包',
        phenomenon: '扫到一条规则没挂包',
        judgement: 'issue',
        advice: [],
        finding_ids: ['orphan_rules/rule:999'],
        confidence: 1,
    });
    const { provider, calls } = scripted([
        JSON.stringify({
            tool_calls: [{ id: 'c1', name: 'run_check', arguments: { name: 'orphan_rules' } }],
        }),
        bad,
        GOOD_REPORT,
    ]);
    const { onEvent } = collectEvents();

    const out = await runAgent(
        'x',
        {},
        deps(provider, {
            onEvent,
            executor: fakeExecutor({
                checks: {
                    orphan_rules: result('orphan_rules', {
                        verdict: 'issue',
                        findings: [finding('rule:101')],
                    }),
                },
            }),
        }),
    );

    assert.equal(calls.length, 3, '工具一轮 + 坏报告 + 重试');
    assert.deepEqual(out.report.finding_ids, ['orphan_rules/rule:101']);
    assert.equal(out.stopReason, 'completed');
    // 回灌提示里必须带上白名单
    const retry = calls[2]?.messages[calls[2].messages.length - 1];
    assert.match(retry?.content ?? '', /orphan_rules\/rule:101/);
});
