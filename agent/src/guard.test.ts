import assert from 'node:assert/strict';
import { test } from 'node:test';

import { costOf } from './config.js';
import { Guard, type GuardOptions } from './guard.js';

function toggleFetch(enabled: boolean): typeof fetch {
    return async () =>
        new Response(JSON.stringify({ data: [{ toggle_key: 'agent_enabled', value: enabled }] }), {
            status: 200,
            headers: { 'content-type': 'application/json' },
        });
}

function makeGuard(over: Partial<GuardOptions> = {}): Guard {
    return new Guard({
        enabled: true,
        maxInflight: 8,
        userRpm: 5,
        dailyBudgetCny: 100,
        toggleTtlSec: 60,
        backendUrl: 'http://backend.local',
        fetchImpl: toggleFetch(true),
        ...over,
    });
}

test('环境变量总开关关闭时直接拒绝，不查远程 toggle', async () => {
    let fetched = 0;
    const guard = makeGuard({
        enabled: false,
        fetchImpl: async () => {
            fetched++;
            return toggleFetch(true)(new URL('http://x'), {});
        },
    });

    const d = await guard.check('u1', 'tok');
    assert.equal(d.ok, false);
    if (!d.ok) assert.equal(d.reason, 'disabled');
    assert.equal(fetched, 0);
});

test('远程 toggle 关闭时拒绝', async () => {
    const guard = makeGuard({ fetchImpl: toggleFetch(false) });
    const d = await guard.check('u1', 'tok');
    assert.equal(d.ok, false);
    if (!d.ok) assert.equal(d.reason, 'disabled');
});

test('同一用户并发 1：第二个请求被拒，release 后可以再进', async () => {
    const guard = makeGuard();

    const first = await guard.check('u1', 'tok');
    assert.equal(first.ok, true);

    const second = await guard.check('u1', 'tok');
    assert.equal(second.ok, false);
    if (!second.ok) assert.equal(second.reason, 'concurrency');

    if (first.ok) first.lease.release();

    const third = await guard.check('u1', 'tok');
    assert.equal(third.ok, true);
    if (third.ok) third.lease.release();
});

test('全局并发上限', async () => {
    const guard = makeGuard({ maxInflight: 2 });

    const a = await guard.check('u1', 'tok');
    const b = await guard.check('u2', 'tok');
    assert.equal(a.ok, true);
    assert.equal(b.ok, true);

    const c = await guard.check('u3', 'tok');
    assert.equal(c.ok, false);
    if (!c.ok) assert.equal(c.reason, 'concurrency');
});

test('单用户每分钟次数：第 userRpm+1 次被拒', async () => {
    const guard = makeGuard({ userRpm: 2 });

    for (let i = 0; i < 2; i++) {
        const d = await guard.check('u1', 'tok');
        assert.equal(d.ok, true);
        if (d.ok) d.lease.release();
    }

    const third = await guard.check('u1', 'tok');
    assert.equal(third.ok, false);
    if (!third.ok) {
        assert.equal(third.reason, 'rpm');
        assert.ok((third.retryAfterMs ?? 0) > 0);
    }
});

test('金额闸门看元不看 token：单价 5 倍时同样 token 更早被拒', async () => {
    const usage = { inTokens: 1_000_000, outTokens: 0 };
    const cheap = costOf({ input: 1, output: 1, cacheRead: 1 }, usage); // 1 元
    const pricey = costOf({ input: 5, output: 5, cacheRead: 5 }, usage); // 5 元

    async function spendUntilDenied(cost: number): Promise<number> {
        const guard = makeGuard({ dailyBudgetCny: 3 });
        let calls = 0;
        for (let i = 0; i < 5; i++) {
            const d = await guard.check('u1', 'tok');
            if (!d.ok) {
                assert.equal(d.reason, 'budget');
                return calls;
            }
            calls++;
            d.lease.addCost(cost);
            d.lease.release();
        }
        return calls;
    }

    assert.equal(await spendUntilDenied(cheap), 3); // 1 元跑 3 次刚好触顶
    assert.equal(await spendUntilDenied(pricey), 1); // 5 元跑 1 次就超
});

test('toggle 拉取失败时沿用上次成功值', async () => {
    let clock = 1_000;
    let fail = false;
    const guard = makeGuard({
        now: () => clock,
        toggleTtlSec: 1,
        fetchImpl: async () => {
            if (fail) throw new Error('network down');
            return toggleFetch(true)(new URL('http://x'), {});
        },
    });

    const first = await guard.check('u1', 'tok');
    assert.equal(first.ok, true);
    if (first.ok) first.lease.release();

    // 让缓存过期，且下一次拉取失败
    clock += 5_000;
    fail = true;

    const second = await guard.check('u1', 'tok');
    assert.equal(second.ok, true);
    if (second.ok) second.lease.release();
});

test('从没成功拉到 toggle 时按关闭处理（fail-closed）', async () => {
    const guard = makeGuard({
        fetchImpl: async () => {
            throw new Error('network down');
        },
    });

    const d = await guard.check('u1', 'tok');
    assert.equal(d.ok, false);
    if (!d.ok) assert.equal(d.reason, 'disabled');
});
