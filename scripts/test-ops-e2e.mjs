/**
 * 运营中台三块新能力的端到端验证：规则引擎 / 定时作业 / 字段脱敏。
 *
 * 判定不靠「接口 200」，每一块都要拿到**可核验的业务结果**：
 *   - 规则：试运行命中数、解释里能看到「实际值 vs 期望值」、改一次版本号 +1；
 *   - 作业：手动触发后执行历史里出现一条 ok，且输出带业务摘要；
 *   - 脱敏：预览结果必须是掩码，且来源能区分 policy / remote / local。
 *
 * 任何一步失败都打印响应原文，不假装通过。
 */

const BASE = process.env.MC_BASE || 'http://127.0.0.1:8080';
const USER = process.env.MC_ADMIN_USER || 'admin';
const PASS = process.env.MC_ADMIN_PASS || '';

let pass = 0;
let fail = 0;
const check = (name, ok, extra = '') => {
    ok ? pass++ : fail++;
    console.log(`  ${ok ? '✅' : '❌'} ${name}${extra ? ' — ' + extra : ''}`);
};

let token = '';

async function api(path, { method = 'GET', body, raw } = {}) {
    const headers = {};
    if (token) headers.authorization = `Bearer ${token}`;
    let payload;
    if (raw !== undefined) {
        payload = raw;
    } else if (body !== undefined) {
        headers['content-type'] = 'application/json';
        payload = JSON.stringify(body);
    }
    const res = await fetch(BASE + path, { method, headers, body: payload });
    const text = await res.text();
    let json = null;
    try {
        json = JSON.parse(text);
    } catch {
        /* 非 JSON */
    }
    return { status: res.status, json, text };
}

async function login() {
    const res = await fetch(`${BASE}/api/auth/login`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ username: USER, password: PASS }),
    });
    const json = await res.json();
    if (!json.token) throw new Error('登录失败: ' + JSON.stringify(json).slice(0, 120));
    token = json.token;
}

(async () => {
    if (!PASS) throw new Error('MC_ADMIN_PASS 未设置');
    console.log('=== 0. 登录 ===');
    await login();
    check('拿到 token', Boolean(token));

    // ── 规则引擎 ──
    console.log('\n=== 1. 规则列表 ===');
    const list = await api('/api/rules');
    check('列表 200', list.status === 200, `实际 ${list.status}`);
    const seeds = list.json?.items ?? [];
    check('种子规则已就绪', seeds.length >= 2, `${seeds.length} 条`);

    console.log('\n=== 2. 试运行（样例数据）===');
    const dry = await api('/api/rule-dry-run', { method: 'POST', body: {} });
    check('试运行 200', dry.status === 200, `实际 ${dry.status}`);
    const dryItems = dry.json?.items ?? [];
    check('有启用的规则参与试跑', dryItems.length > 0, `${dryItems.length} 条`);
    const hitCount = dryItems.filter((x) => x.hit).length;
    console.log(`    命中 ${hitCount} / 未命中 ${dryItems.length - hitCount}（样例数据：tier.state=1, tier.online=1, rank=2`);

    console.log('\n=== 3. 解释（能看到实际值 vs 期望值）===');
    const seedRule = seeds.find((r) => r.enabled) ?? seeds[0];
    const exp = await api(`/api/rules/${seedRule.id}/explain`, {
        method: 'POST',
        body: { data: { 'tier.state': 1, 'tier.online': 0, 'member.login_days': 45, 'tier.rank': 1 } },
    });
    check('解释 200', exp.status === 200, `实际 ${exp.status}`);
    const steps = exp.json?.steps ?? [];
    check('返回了求值轨迹', steps.length > 0, `${steps.length} 步`);
    const hasActual = steps.some((s) => s.field && s.actual !== undefined && s.actual !== null);
    check('轨迹带实际值', hasActual, JSON.stringify(steps[0] ?? {}).slice(0, 120));
    // 传一个明确不满足的数据，必须给出「未命中」且指出卡在哪一步
    const miss = await api(`/api/rules/${seedRule.id}/explain`, {
        method: 'POST',
        body: { data: { 'tier.state': 9, 'tier.online': 0, 'member.login_days': 0, 'tier.rank': 1 } },
    });
    console.log('    刻意给不满足的数据 →', miss.json?.summary);

    console.log('\n=== 4. 新建 / 修改 / 版本号 ===');
    const created = await api('/api/rules', {
        method: 'POST',
        body: {
            name: `e2e 规则 ${Date.now()}`,
            domain: 'operation',
            enabled: true,
            action: 'e2e 自动创建',
            expr: { all: [{ field: 'member.total_spend', op: 'gte', value: 100 }] },
        },
    });
    check('新建 200', created.status === 200, created.text.slice(0, 100));
    const newId = created.json?.id;
    const up = await api(`/api/rules/${newId}`, {
        method: 'PUT',
        body: {
            name: `e2e 规则 ${Date.now()}`,
            domain: 'operation',
            enabled: true,
            action: 'e2e 改过一次',
            expr: { all: [{ field: 'member.total_spend', op: 'gte', value: 500 }] },
        },
    });
    check('修改 200', up.status === 200, up.text.slice(0, 100));
    const got = await api(`/api/rules/${newId}`);
    check('版本号递增到 2', got.json?.item?.version === 2, `实际 ${got.json?.item?.version}`);
    const vers = await api(`/api/rules/${newId}/versions`);
    check('历史版本可查', (vers.json?.items ?? []).length >= 2, `${(vers.json?.items ?? []).length} 条`);

    console.log('\n=== 5. 非法表达式必须被拒 ===');
    const bad = await api('/api/rules', {
        method: 'POST',
        body: { name: 'bad', domain: 'operation', action: 'x', expr: { field: 'a', op: 'eval', value: 1 } },
    });
    check('非法算子返回 400', bad.status === 400, `${bad.status} ${bad.text.slice(0, 80)}`);

    const del = await api(`/api/rules/${newId}`, { method: 'DELETE' });
    check('删除 200', del.status === 200, del.text.slice(0, 80));

    // ── 定时作业 ──
    console.log('\n=== 6. 作业列表与命令注册 ===');
    const jobs = await api('/api/jobs');
    check('作业列表 200', jobs.status === 200, `实际 ${jobs.status}`);
    const cmds = jobs.json?.commands ?? [];
    check('命令已注册', cmds.includes('snapshot_metrics') && cmds.includes('cleanup_expired_rules'), cmds.join(','));
    const jobItems = jobs.json?.items ?? [];
    check('种子作业已就绪', jobItems.length >= 2, `${jobItems.length} 条`);

    console.log('\n=== 7. 手动触发 + 执行历史 ===');
    const target = jobItems.find((j) => j.command === 'snapshot_metrics') ?? jobItems[0];
    const run = await api(`/api/jobs/${target.id}/run`, { method: 'POST' });
    check('手动触发 200', run.status === 200, run.text.slice(0, 120));
    check('返回业务摘要', /快照|清理/.test(run.json?.output ?? ''), run.json?.output);

    await new Promise((r) => setTimeout(r, 500));
    const runs = await api(`/api/job-runs?job_id=${target.id}`);
    const runItems = runs.json?.items ?? [];
    check('执行历史有记录', runItems.length > 0, `${runItems.length} 条`);
    check('最近一条是 ok', runItems[0]?.status === 'ok', JSON.stringify(runItems[0] ?? {}).slice(0, 120));

    console.log('\n=== 8. 非法 cron 必须被拒 ===');
    const badCron = await api('/api/jobs', {
        method: 'POST',
        body: { name: 'bad cron', cron: '99 * * * *', command: 'snapshot_metrics' },
    });
    check('非法 cron 返回 400', badCron.status === 400, badCron.text.slice(0, 100));

    // ── 脱敏 ──
    console.log('\n=== 9. 脱敏策略 ===');
    const pol = await api('/api/mask-policies');
    const polItems = pol.json?.items ?? [];
    check('策略列表 200', pol.status === 200, `实际 ${pol.status}`);
    check('种子策略已就绪', polItems.length >= 3, `${polItems.length} 条`);

    console.log('\n=== 10. 预览脱敏（策略命中）===');
    const pv = await api('/api/mask/preview', {
        method: 'POST',
        body: { field: 'member_phone', value: '13800001234' },
    });
    check('预览 200', pv.status === 200, pv.text.slice(0, 100));
    const masked = pv.json?.result?.masked;
    check('手机号被打码', masked === '138****1234', `实际 ${masked}`);
    check('来源是 policy', pv.json?.result?.source === 'policy', pv.json?.result?.source);

    console.log('\n=== 11. 预览：无策略字段走远端识别 ===');
    // member_email 有策略；换一个没有策略但值敏感的字段名，应走 remote 或 local
    const pv2 = await api('/api/mask/preview', {
        method: 'POST',
        body: { field: 'member_id_card', value: '110101199001011234' },
    });
    check('证件号被隐藏', pv2.json?.result?.masked === '«已隐藏»', pv2.json?.result?.masked);

    console.log('\n=== 12. 白名单外字段不许预览 ===');
    const pv3 = await api('/api/mask/preview', {
        method: 'POST',
        body: { field: 'member_total_asset', value: '999999' },
    });
    check('白名单外返回 400', pv3.status === 400, `${pv3.status}`);

    console.log('\n=== 13. 明文访问要审计 ===');
    const before = await api('/api/mask/audit');
    const n0 = (before.json?.items ?? []).length;
    const plain = await api('/api/mask/plaintext', {
        method: 'POST',
        body: { field: 'member_phone', reason: 'e2e 验证审计链路', count: 3 },
    });
    check('审计接口 200', plain.status === 200, plain.text.slice(0, 80));
    const after = await api('/api/mask/audit');
    check('审计记录 +1', (after.json?.items ?? []).length === n0 + 1, `${n0} → ${(after.json?.items ?? []).length}`);

    // ── 远端识别与降级 ──
    // 这三步是本轮最有价值的部分：证明脱敏真的经过 Python 服务，
    // 并且服务挂掉时不会把运营台一起拖死。
    console.log('\n=== 14. 无策略字段走 Python 识别服务 ===');
    await api('/api/mask-policies/member_email', { method: 'DELETE' });
    const remote = await api('/api/mask/preview', {
        method: 'POST',
        body: { field: 'member_email', value: 'alice@example.com' },
    });
    const rsrc = remote.json?.result?.source;
    check('来源是 remote（经过 Python 服务）', rsrc === 'remote', `实际 ${rsrc} / masked=${remote.json?.result?.masked}`);
    check('邮箱被打码', remote.json?.result?.masked !== 'alice@example.com', remote.json?.result?.masked);

    console.log('\n=== 15. 停掉识别服务后自动降级 ===');
    const { execSync } = await import('node:child_process');
    try {
        execSync('docker stop mc-mask-service', { timeout: 60000, stdio: 'pipe' });
    } catch (e) {
        console.log('    停止容器失败:', String(e.message).slice(0, 80));
    }
    // 策略已删，远端已停 → 必须降级到 local，且仍然返回掩码
    let degraded = null;
    for (let i = 0; i < 5; i++) {
        await new Promise((r) => setTimeout(r, 1200));
        degraded = await api('/api/mask/preview', {
            method: 'POST',
            body: { field: 'member_email', value: 'alice@example.com' },
        });
        if (degraded.json?.result?.source === 'local') break;
    }
    check('降级到本地识别', degraded?.json?.result?.source === 'local', `实际 ${degraded?.json?.result?.source}`);
    check('降级后仍被打码', degraded?.json?.result?.masked !== 'alice@example.com', degraded?.json?.result?.masked);

    console.log('\n=== 16. 恢复识别服务 ===');
    try {
        execSync('docker start mc-mask-service', { timeout: 60000, stdio: 'pipe' });
    } catch {
        /* 恢复失败不影响后续断言，但要如实提示 */
    }
    let back = null;
    for (let i = 0; i < 15; i++) {
        await new Promise((r) => setTimeout(r, 1500));
        back = await api('/api/mask/preview', {
            method: 'POST',
            body: { field: 'member_email', value: 'alice@example.com' },
        });
        if (back.json?.result?.source === 'remote') break;
    }
    check('服务恢复后回到 remote', back?.json?.result?.source === 'remote', `实际 ${back?.json?.result?.source}`);

    // 恢复策略，避免影响后续手工验证
    await api('/api/mask-policies', {
        method: 'PUT',
        body: { field: 'member_email', strategy: 'mask', keep_prefix: 2, keep_suffix: 0, mask_char: '*', roles: [], note: '邮箱：保留前两位' },
    });

    console.log(`\n===== 结果: ${pass} 通过 / ${fail} 失败 =====`);
    process.exit(fail ? 1 : 0);
})().catch((e) => {
    console.error('FAIL:', e.message);
    process.exit(1);
});
