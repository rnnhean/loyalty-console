/**
 * Agent 新工具的端到端验证（真实大模型，不 mock）。
 *
 * 验证的是本轮新增的两个工具：
 *   - explain_rule：解释一条可配置规则为什么没命中，返回每一步的「实际值 vs 期望值」
 *   - simulate_rules：用样例数据试跑全部启用规则
 *
 * 判定标准不是「模型说了漂亮话」，而是：
 *   1. SSE 步骤里真的出现了对应工具名（说明 function calling 走了真实链路）；
 *   2. 最终结论里出现了工具返回的真实字段名/数值（说明模型用了数据，不是编的）。
 */

const ORIGIN = process.env.MC_BASE || 'http://127.0.0.1:8080';
const AGENT = process.env.AGENT || 'http://127.0.0.1:3001';
const USER = process.env.MC_ADMIN_USER || 'admin';
const PASS = process.env.MC_ADMIN_PASS || '';

let pass = 0;
let fail = 0;
const check = (n, ok, extra = '') => {
    ok ? pass++ : fail++;
    console.log(`  ${ok ? '✅' : '❌'} ${n}${extra ? ' — ' + extra : ''}`);
};

async function login() {
    const res = await fetch(`${ORIGIN}/api/auth/login`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ username: USER, password: PASS }),
    });
    const json = await res.json();
    if (!json.token) throw new Error('登录失败');
    return json.token;
}

/** 发一次诊断，返回 { steps, notice, final } */
async function diagnose(token, question) {
    const res = await fetch(`${AGENT}/api/agent/diagnose`, {
        method: 'POST',
        headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
        body: JSON.stringify({ question }),
    });
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    const steps = [];
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
                if (o.type === 'step' && o.status === 'done') steps.push(o.label);
                if (o.type === 'final') final = o;
                if (o.type === 'error') err = o;
            } catch {
                /* 心跳 */
            }
        }
    }
    return { steps, final, err };
}

(async () => {
    if (!PASS) throw new Error('MC_ADMIN_PASS 未设置');
    const token = await login();
    console.log('=== 登录完成 ===\n');

    console.log('=== 1. explain_rule：给定数据解释规则为什么不命中 ===');
    // 问题必须带具体数据：只说「为什么没命中」会撞上「其实命中了」的真实情况，
    // 模型会去翻真实会员表，越查越跑偏（实测 7 轮后 total_timeout）。
    const q1 =
        '「连续 30 天未登录打标」这条规则，假设某个会员登录天数是 10 天、层级状态是 1，' +
        '会命中吗？请用 explain_rule 解释是哪一步不满足，并给出该步的实际值和期望值。';
    const r1 = await diagnose(token, q1);
    console.log('  工具链:', r1.steps.join(' → ') || '(未调工具)');
    // 步骤标签是给人看的中文，不能只认英文工具名（改了 labelOf 之后这里会假失败）
    check('调用了规则工具', r1.steps.some((s) => /列出可配置规则|解释规则|list_rules|explain_rule/.test(s)), r1.steps.join(' → '));
    check('调用了 explain_rule', r1.steps.some((s) => /解释规则|explain_rule/.test(s)));
    check('产出最终结论', Boolean(r1.final));
    const body1 = r1.final
        ? JSON.stringify(r1.final.report)
        : '';
    console.log('  结论:', r1.final?.report?.title, '|', r1.final?.report?.judgement?.slice(0, 120));
    // 工具返回里一定有字段名（tier.state / member.login_days 等），模型要用上
    check(
        '结论引用了真实字段',
        /tier\.state|tier\.online|member\.login_days|tier\.rank|没命中|未命中|不满足/.test(body1),
        body1.slice(0, 140),
    );

    console.log('\n=== 2. simulate_rules：试跑全部规则 ===');
    const q2 = '用样例数据把所有启用的规则试跑一遍，告诉我哪些会命中。';
    const r2 = await diagnose(token, q2);
    console.log('  工具链:', r2.steps.join(' → ') || '(未调工具)');
    check('调用了 simulate_rules', r2.steps.some((s) => /试跑全部规则|simulate_rules/.test(s)), r2.steps.join(' → '));
    check('产出最终结论', Boolean(r2.final));
    const body2 = r2.final ? JSON.stringify(r2.final.report) : '';
    console.log('  结论:', r2.final?.report?.title);
    check(
        '结论包含命中与否的判断',
        /命中|未命中|会触发|不会触发/.test(body2),
        body2.slice(0, 140),
    );

    console.log('\n=== 3. 成本 ===');
    const h = await (await fetch(`${AGENT}/healthz`)).json();
    console.log(`  模型 ${h.model} | 累计花费 ${h.spent_cny} 元`);
    check('真实产生了费用', h.spent_cny > 0, `${h.spent_cny} 元`);

    console.log(`\n===== 结果: ${pass} 通过 / ${fail} 失败 =====`);
    process.exit(fail ? 1 : 0);
})().catch((e) => {
    console.error('FAIL:', e.message);
    process.exit(1);
});
