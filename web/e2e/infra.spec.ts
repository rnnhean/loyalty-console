import { expect, test, type APIRequestContext } from '@playwright/test';

/**
 * 中间件功能验证。
 *
 * 每个组件都要求「业务链路真的走过它」，而不是「容器起来了」：
 * 起着的 Kafka 不说明有事件；有事件不说明有人消费。
 * 所以断言一律落在**业务结果**上（数据变了、文件下来了、审计多了），
 * 中间件指标只用来确认「这次业务确实用到了它」。
 */

const GW = process.env.E2E_GATEWAY_URL || 'http://127.0.0.1:8080';
const USER = process.env.E2E_USER || 'admin';
const PASS = process.env.E2E_PASS || 'demo1234';

let ticket = '';

test.beforeAll(async ({ request }) => {
    const res = await request.post(`${GW}/api/auth/login`, {
        data: { username: USER, password: PASS },
    });
    expect(res.ok(), '登录失败，后面的用例都会连带失败').toBeTruthy();
    ticket = (await res.json()).token;
});

function auth() {
    return { Authorization: `Bearer ${ticket}` };
}

/**
 * 从 /metrics 取一个指标的和。
 *
 * 严格按「名字 + 可选标签过滤」匹配：`name` 必须是序列名本身，
 * 不能是前缀 —— 否则 `mc_export_rows` 会连带把
 * `mc_export_rows_bucket` / `_sum` / `_count` 全加进来，
 * 不同量纲的数字混在一起，断言就成了摆设。
 */
async function metric(request: APIRequestContext, name: string, label?: string): Promise<number> {
    const res = await request.get(`${GW}/metrics`);
    const text = await res.text();
    let sum = 0;
    for (const line of text.split('\n')) {
        if (!line || line.startsWith('#')) continue;
        const sp = line.lastIndexOf(' ');
        const series = line.slice(0, sp);
        const seriesName = series.startsWith(`${name}{`) ? name : series === name ? name : '';
        if (!seriesName) continue;
        if (label && !series.includes(label)) continue;
        const v = Number(line.slice(sp + 1));
        if (!Number.isNaN(v)) sum += v;
    }
    return sum;
}

test.describe('中间件', () => {
    test('健康检查：五个组件全部就绪', async ({ request }) => {
        const res = await request.get(`${GW}/api/infra/health`, { headers: auth() });
        expect(res.ok()).toBeTruthy();
        const body = await res.json();
        for (const [name, v] of Object.entries(body.infra as Record<string, { ok: boolean }>)) {
            expect(v.ok, `${name} 未就绪`).toBeTruthy();
        }
    });

    test('MySQL：分页查询返回一致的总数', async ({ request }) => {
        const p1 = await request.get(`${GW}/api/catalog/spu`, {
            headers: auth(),
            params: { page: 1, page_size: 5 },
        });
        const b1 = await p1.json();
        expect(b1.code).toBe(0);
        expect(b1.items.length).toBeGreaterThan(0);
        expect(b1.total).toBeGreaterThan(0);

        // 第二页不该出现第一页的数据 —— 这是 OFFSET 分页最容易写错的地方
        const p2 = await request.get(`${GW}/api/catalog/spu`, {
            headers: auth(),
            params: { page: 2, page_size: 5 },
        });
        const b2 = await p2.json();
        const ids1 = new Set(b1.items.map((r: { uid: number }) => r.uid));
        for (const r of b2.items) {
            expect(ids1.has(r.uid), `第二页出现了第一页的记录 ${r.uid}`).toBeFalsy();
        }
    });

    test('Redis 缓存：重复查询命中缓存，不再回源', async ({ request }) => {
        // 打的是产品下拉，它走 GetOrLoad 的两级缓存（本地 → Redis）。
        // 页面上下拉框到处都在拉这个接口，是全站最热的一个查询。
        const before = await metric(request, 'mc_cache_hits_total');
        const missBefore = await metric(request, 'mc_cache_misses_total');
        for (let i = 0; i < 3; i++) {
            const res = await request.get(`${GW}/api/setting/products`, { headers: auth() });
            expect(res.ok()).toBeTruthy();
            expect((await res.json()).items.length).toBeGreaterThan(0);
        }
        const after = await metric(request, 'mc_cache_hits_total');
        const missAfter = await metric(request, 'mc_cache_misses_total');
        // 命中数增加是必要条件，但不充分 —— 如果每次既回源又命中，说明
        // 缓存写了又被绕过。所以再要求「回源最多一次」（仅首次未命中）。
        expect(after, '连打三次同一查询，缓存命中数没有增加').toBeGreaterThan(before);
        expect(missAfter - missBefore, '三次查询都在回源，缓存没接上').toBeLessThanOrEqual(1);
    });

    test('Redis 锁 + 幂等：相同请求返回同一份结果', async ({ request }) => {
        const uid = 10001; // 已失效的规则，重复执行不会改动业务数据
        const body = { rule_uid: uid };

        const r1 = await request.post(`${GW}/api/benefit/expire-rule`, {
            headers: auth(),
            data: body,
        });
        const r2 = await request.post(`${GW}/api/benefit/expire-rule`, {
            headers: auth(),
            data: body,
        });
        expect(r1.ok() || r1.status() === 409).toBeTruthy();
        expect(r2.ok()).toBeTruthy();

        const b1 = await r1.json();
        const b2 = await r2.json();
        // 幂等命中时直接回放缓存的响应体，trace_id 会**与首次相同**。
        // 这个是回放最强的证据：重新执行一次必然生成新 trace_id。
        if (b2.code === 0) {
            expect(b2.trace_id, '两次请求的 trace_id 不同，说明第二次是重新执行的').toBe(b1.trace_id);
        }

        const replay = await metric(request, 'mc_idempotency_outcome_total', 'outcome="replay"');
        const proceed = await metric(request, 'mc_idempotency_outcome_total', 'outcome="proceed"');
        expect(replay + proceed, '幂等中间件没有记录到任何判定').toBeGreaterThan(0);
        // 连着打两次同一键，至少有一次必须是 replay；
        // 全 proceed 说明幂等键每次都不同（最常见的实现错误）。
        expect(replay, '两次相同的请求都走了 proceed，幂等键没生效').toBeGreaterThan(0);

        // 锁的验证必须**换一个幂等键**。
        //
        // 上面两次请求用的是同一个 rule_uid，第二次命中幂等直接回放缓存的响应，
        // 根本不会走到 locker.Do() —— 所以 replay 次数增加时，锁计数可能一动不动。
        //
        // 而且幂等缓存有 30s TTL，用固定的 uid 连跑两次测试，
        // 第二次必然撞上上一次留下的缓存、同样不会加锁。
        // 用时间戳造一个必然没被处理过的 uid，才能稳定验证到加锁。
        const uniqueUID = Date.now();
        const lockBefore = await metric(request, 'mc_lock_acquire_total', 'result="ok"');
        const r3 = await request.post(`${GW}/api/benefit/expire-rule`, {
            headers: auth(),
            data: { rule_uid: uniqueUID },
        });
        expect(r3.status(), '换一个 uid 的请求应当是正常响应（含 404/409 这类业务响应）').toBeLessThan(500);
        const lockAfter = await metric(request, 'mc_lock_acquire_total', 'result="ok"');
        expect(lockAfter, `锁指标没记录到成功获取（${lockBefore} → ${lockAfter}）`).toBeGreaterThan(lockBefore);
    });

    test('RabbitMQ → Kafka → MySQL：到期任务闭环', async ({ request }) => {
        // 找一条还在生效的规则；如果没有（被前面的用例用掉了），跳过而不是造假
        const list = await request.get(`${GW}/api/benefit/rules`, {
            headers: auth(),
            params: { page: 1, page_size: 100 },
        });
        const rules: { uid: number; state: number }[] = (await list.json()).items;
        const alive = rules.find((r) => r.state === 1);
        test.skip(!alive, '没有处于启用状态的权益规则可用于到期测试');
        const uid = alive!.uid;

        const task = await request.post(`${GW}/api/benefit/expiry-task`, {
            headers: auth(),
            data: { rule_uid: uid, delay_sec: 5 },
        });
        expect(task.ok(), '延迟任务投递失败').toBeTruthy();

        // 延时是 5s，但延迟队列按档位取整（5s 档正好命中），
        // 留 40s 给消费者 + Kafka + 审计消费。
        await expect
            .poll(
                async () => {
                    const res = await request.get(`${GW}/api/trade/audit`, {
                        headers: auth(),
                        params: { target: 'benefit.rule', ref: String(uid), page_size: 5 },
                    });
                    const rows = (await res.json()).items ?? [];
                    return rows.some((r: { action_kind: string }) => r.action_kind === 'benefit.expired');
                },
                { timeout: 40_000, intervals: [1000, 2000, 3000] },
            )
            .toBe(true);

        // 业务结果：规则确实失效了
        const after = await request.get(`${GW}/api/benefit/rules`, {
            headers: auth(),
            params: { page: 1, page_size: 100 },
        });
        const row = ((await after.json()).items as { uid: number; state: number }[]).find(
            (r) => r.uid === uid,
        );
        expect(row?.state, '审计有记录但规则没失效').toBe(0);
    });

    test('MinIO：导出文件真实可下载且内容正确', async ({ request }) => {
        const submit = await request.post(`${GW}/api/export/jobs`, {
            headers: auth(),
            params: { kind: 'catalog-spu' },
        });
        expect(submit.ok()).toBeTruthy();
        const job = (await submit.json()).job;

        const done = await expect
            .poll(
                async () => {
                    const res = await request.get(`${GW}/api/export/jobs/${job.id}`, {
                        headers: auth(),
                    });
                    return (await res.json()).job;
                },
                { timeout: 30_000, intervals: [500, 1000, 2000] },
            )
            .toMatchObject({ state: 'done' });
        void done;

        const final = await request.get(`${GW}/api/export/jobs/${job.id}`, { headers: auth() });
        const j = (await final.json()).job;
        expect(j.rows).toBeGreaterThan(0);
        expect(j.url, '没有返回预签名地址').toContain('X-Amz-Signature');

        // 预签名地址是给浏览器用的，这里直接用它下载 ——
        // 签名、Host、过期时间任意一处不对，这一步就会 403。
        const file = await request.get(j.url);
        expect(file.status(), '预签名地址下载失败').toBe(200);
        const csv = await file.text();
        expect(csv.split('\n')[0]).toContain('商品名称');
        expect(csv.split('\n').length).toBeGreaterThan(1);

        // 走到这一步说明对象存储的 put / presign 都被用过了
        const put = await metric(request, 'mc_blob_operations_total', 'op="put"');
        expect(put, 'MinIO 没有收到上传').toBeGreaterThan(0);
        const presign = await metric(request, 'mc_blob_operations_total', 'op="presign"');
        expect(presign, '没有生成预签名地址').toBeGreaterThan(0);
    });

    test('Kafka：事件被真实消费且没有进死信', async ({ request }) => {
        const consumed = await metric(request, 'mc_mq_kafka_consumed_total');
        expect(consumed, 'Kafka 消费指标为 0，说明事件链路没走通').toBeGreaterThan(0);

        const dlq = await metric(request, 'mc_mq_kafka_consumed_total', 'result="dlq"');
        expect(dlq, '有事件进了死信').toBe(0);

        const published = await metric(request, 'mc_mq_kafka_published_total');
        expect(published, '一条都没发布过').toBeGreaterThan(0);
    });

    test('发件箱：没有积压也没有被丢弃', async ({ request }) => {
        const backlog = await metric(request, 'mc_outbox_backlog');
        expect(backlog, '发件箱有积压').toBe(0);
        const dropped = await metric(request, 'mc_outbox_flushed_total', 'result="dropped"');
        expect(dropped, '有事件重试耗尽被丢弃').toBe(0);
    });

    test('RabbitMQ：延迟任务被消费且没有死信', async ({ request }) => {
        const dead = await metric(request, 'mc_mq_rabbit_dead_lettered_total');
        expect(dead, '有到期任务没处理成功，权益不会被回收').toBe(0);
        const consumed = await metric(request, 'mc_mq_rabbit_consumed_total', 'result="ok"');
        expect(consumed, '延迟任务没有被消费').toBeGreaterThan(0);
        const published = await metric(request, 'mc_mq_rabbit_published_total');
        expect(published, '延迟任务没有被投递').toBeGreaterThan(0);
    });
});

/**
 * 限流的两层。
 *
 * 这两条必须分开断言，因为它们防的是**不同的攻击**，任何一层失效都不会
 * 让另一层报错 —— 只做一层的话，另一种攻击照样打穿：
 *
 *   按来源（scope=caller）  防「一个 IP 狂刷」
 *   按业务 ID（scope=biz_key）防「大量 IP 一起刷同一条数据」
 *
 * 通用的"发很多请求看有没有 429"验不出第二条：如果只按来源限流，
 * 换 caller 就等于换了一个满额度的桶，永远打不到 429。
 */
test.describe('限流', () => {
    /** 打一次到期任务接口，返回状态码。ruleKey 决定刷的是哪条规则。 */
    async function hit(request: APIRequestContext, caller: string, ruleUid: number) {
        const res = await request.post(`${GW}/api/benefit/expiry-task`, {
            headers: { ...auth(), 'X-Caller-Id': caller },
            data: { rule_uid: ruleUid, delay_sec: 60 },
        });
        return res.status();
    }

    test('按业务 ID 限流：不同来源刷同一个 ID 会被合并计数', async ({ request }) => {
        // 用一个没人用过的 ID，避免和其它用例抢同一个桶。
        // 每次跑用不同基数：残留的桶会让结果不稳定。
        const ruleUid = 700000 + Math.floor(Date.now() / 1000) % 100000;

        // 30 个**不同**来源，同一时刻打同一个 ID。
        // 每个来源各自只发 1 次，按来源限流完全看不出异常；
        // 但按 ID 计数就是 30 次并发，超过 burst 的部分必须被打回。
        const codes = await Promise.all(
            Array.from({ length: 30 }, (_, i) => hit(request, `e2e-src-${i}-${ruleUid}`, ruleUid)),
        );
        const rejected = codes.filter((c) => c === 429).length;

        expect(rejected, '30 个不同来源并发刷同一 ID 却没有被限流，说明按业务 ID 这一层没生效').toBeGreaterThan(0);

        // 同时确认没打到 5xx：限流应该是「优雅拒绝」，不是「打崩后端」
        expect(codes.filter((c) => c >= 500), '限流把后端打出了 5xx').toHaveLength(0);
    });

    test('按业务 ID 限流：换一个 ID 立刻就放行', async ({ request }) => {
        // 桶是按 ID 分的，不是全局的。如果这条挂了，说明 key 拼错了
        // （比如漏了 ID 只用 route），那就会变成"刷一次就把全站打死"。
        const stamp = Math.floor(Date.now() / 1000) % 100000;
        const busyId = 800000 + stamp;
        const freshId = 900000 + stamp;

        // 先把 busyId 打满
        await Promise.all(
            Array.from({ length: 30 }, (_, i) => hit(request, `e2e-busy-${i}-${stamp}`, busyId)),
        );

        // 再用一个**全新**的 ID，同一个 caller 名字 —— 必须放行
        const code = await hit(request, `e2e-busy-0-${stamp}`, freshId);
        expect(code, '换了个业务 ID 仍被限流，说明桶没有按 ID 分开').toBe(200);
    });

    test('按来源限流仍然生效（第一层没被第二层替换掉）', async ({ request }) => {
        // 同一个来源并发打 70 个**不同** ID：
        // 第二层每个 ID 只算 1 次，全部放行；
        // 第一层按来源累计，超过 burst 的部分必须被打回。
        // 这条防的是「加了按 ID 限流就把按来源限流删了」这种回归。
        const stamp = Math.floor(Date.now() / 1000) % 100000;
        const caller = `e2e-layer1-${stamp}`;
        const codes = await Promise.all(
            Array.from({ length: 70 }, (_, i) => hit(request, caller, 600000 + stamp * 100 + i)),
        );
        const rejected = codes.filter((c) => c === 429).length;
        expect(rejected, '同一来源连打 70 个不同 ID 都没被限，按来源这一层失效了').toBeGreaterThan(0);
    });
});
