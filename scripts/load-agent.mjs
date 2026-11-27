/**
 * 过载保护验证：RPM 闸门 + 并发闸门。
 *
 * 两组独立实验：
 *   A. 同一个 token 打 10 并发 —— 单用户 5 RPM，应该只有 5 个被放行；
 *   B. 30 个不同 token 同时打 —— 每个都是独立用户（hashUser(token)），
 *      RPM 不再是瓶颈，此时应该由 maxInflight=8 兜住。
 *
 * 不计入 CDN/推送，只打 /api/agent/diagnose，按 SSE 读完再统计。
 */

const ORIGIN = process.env.ORIGIN || 'http://127.0.0.1:8080';
const AGENT = process.env.AGENT || 'http://127.0.0.1:3001';
const USER = process.env.MC_ADMIN_USER || 'admin';
const PASS = process.env.MC_ADMIN_PASS || '';

async function login() {
    const res = await fetch(`${ORIGIN}/api/auth/login`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ username: USER, password: PASS }),
    });
    const json = await res.json();
    if (!json.token) throw new Error(`登录失败: ${JSON.stringify(json).slice(0, 120)}`);
    return json.token;
}

/** 发一次诊断，返回 { outcome, ms, detail }。outcome ∈ ok / rpm / concurrency / budget / other */
async function diagnose(token, question) {
    const startedAt = Date.now();
    try {
        const res = await fetch(`${AGENT}/api/agent/diagnose`, {
            method: 'POST',
            headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
            body: JSON.stringify({ question }),
        });

        // 被闸门拒绝时不是 SSE，直接是 JSON
        const ct = res.headers.get('content-type') || '';
        if (!ct.includes('text/event-stream')) {
            const text = await res.text();
            let reason = 'other';
            try {
                const json = JSON.parse(text);
                reason = json.reason || json.error || `http${res.status}`;
            } catch {
                reason = `http${res.status}`;
            }
            return { outcome: reason, ms: Date.now() - startedAt, status: res.status };
        }

        const reader = res.body.getReader();
        const dec = new TextDecoder();
        let buf = '';
        let final = null;
        let err = null;
        for (;;) {
            const { done, value } = await reader.read();
            if (done) break;
            buf += dec.decode(value, { stream: true });
            let i;
            while ((i = buf.indexOf('\n\n')) >= 0) {
                const raw = buf.slice(0, i);
                buf = buf.slice(i + 2);
                let data = '';
                for (const line of raw.split('\n')) {
                    if (line.startsWith('data:')) data += line.slice(5).trim();
                }
                try {
                    const o = JSON.parse(data);
                    if (o.type === 'final') final = o;
                    if (o.type === 'error') err = o;
                } catch {
                    /* 心跳帧等，忽略 */
                }
            }
        }
        return {
            outcome: final ? 'ok' : err ? `err:${err.reason}` : 'no_final',
            ms: Date.now() - startedAt,
            title: final?.report?.title,
        };
    } catch (e) {
        return { outcome: 'network', ms: Date.now() - startedAt, detail: String(e.message).slice(0, 80) };
    }
}

function tally(results) {
    const by = {};
    for (const r of results) by[r.outcome] = (by[r.outcome] || 0) + 1;
    return by;
}

async function main() {
    if (!PASS) throw new Error('MC_ADMIN_PASS 未设置');

    // 注意：Guard 的判定顺序是 全局并发 → 单用户并发 → 单用户 RPM。
    // 同一个 token 并发打，会先撞上「单用户并发」（一个人只允许同时跑一个诊断），
    // 压根走不到 RPM。所以测 RPM 必须**串行**发，测并发必须用**不同用户**。

    console.log('=== 实验 A：同一 token 串行发 6 次（RPM=5 → 期望 5 过 1 拒）===');
    const tokenA = await login();
    const resA = [];
    for (let i = 0; i < 6; i++) {
        const r = await diagnose(tokenA, `推广位有没有绑错？(${i})`);
        resA.push(r);
        console.log(`  第 ${i + 1} 次: ${r.outcome} ${r.ms}ms${r.detail ? ' | ' + r.detail : ''}`);
    }
    console.log('  A 分布', JSON.stringify(tally(resA)));

    // 网关登录限流是 2 rps / burst 5，30 个并发登录会被 429 打回（这本身是对的）。
    // 逐个登录、间隔 700ms，把 token 拿齐。
    console.log('\n=== 实验 B：10 个独立用户同时打（maxInflight=8 → 期望 8 过 2 拒）===');
    const tokens = [];
    for (let i = 0; i < 10; i++) {
        tokens.push(await login());
        await new Promise((r) => setTimeout(r, 700));
    }
    const unique = new Set(tokens).size;
    console.log('  拿到 token', tokens.length, '个，去重后', unique, '个');

    const t1 = Date.now();
    const resB = await Promise.all(
        tokens.map((tk, i) => diagnose(tk, `券批次有没有问题？(${i})`)),
    );
    const msB = Date.now() - t1;
    console.log('  耗时', (msB / 1000).toFixed(1) + 's', '| 分布', JSON.stringify(tally(resB)));
    const okB = resB.filter((r) => r.outcome === 'ok');
    if (okB.length) {
        const lat = okB.map((r) => r.ms).sort((a, b) => a - b);
        console.log(
            '  放行者延迟: min',
            lat[0] + 'ms',
            'p50',
            lat[Math.floor(lat.length / 2)] + 'ms',
            'max',
            lat[lat.length - 1] + 'ms',
        );
    }

    const health = await (await fetch(`${AGENT}/healthz`)).json();
    console.log('\n=== 结束后 ===');
    console.log('  累计花费', health.spent_cny, '元 | 在途', health.active);
    const okA = resA.filter((r) => r.outcome === 'ok');
    console.log('  A 放行', okA.length, '/ B 放行', okB.length, '| 总真实调用', okA.length + okB.length);
}

main().catch((e) => {
    console.error('FAIL:', e.message);
    process.exit(1);
});
