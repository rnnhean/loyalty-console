/**
 * 运维三页的**写操作**有头浏览器验证。
 *
 * 之前的 test-ops-ui.mjs 只点了只读部分（列表、试运行、解释、预览），
 * 三个页面的新建 / 编辑 / 删除一个都没点过 —— 这些恰恰是最容易接错的地方：
 * 表单字段名对不上、PUT 传错 id、删除确认框没绑定，都只在点下去那一刻才暴露。
 *
 * 本脚本补的就是这些「写」路径，并且每条都回查一次后端，确认真的落库了。
 */
import { chromium } from 'file:///D:/20260916/.e2e-tools/node_modules/playwright/index.mjs';

const WEB = 'http://localhost:5181';
const SHOTS = 'D:/20260916/loyalty-console/docs/shots';
const USER = 'admin';
const PASS = process.env.MC_ADMIN_PASS || '';

let pass = 0;
let fail = 0;
const check = (n, ok, extra = '') => {
    ok ? pass++ : fail++;
    console.log(`  ${ok ? '✅' : '❌'} ${n}${extra ? ' — ' + extra : ''}`);
};

const browser = await chromium.launch({ headless: false, args: ['--window-size=1500,1000'] });
const page = await browser.newPage({ viewport: { width: 1500, height: 950 } });
const errors = [];
page.on('pageerror', (e) => errors.push(String(e)));

/** 直接问后端，确认 UI 上的操作真的生效了（不只看前端提示）。 */
const apiGet = async (path) => page.evaluate(async (p) => {
    const t = localStorage.getItem('mc.console.ticket');
    const r = await fetch(p, { headers: { authorization: 'Bearer ' + t } });
    return r.json();
}, path);

try {
    if (!PASS) throw new Error('MC_ADMIN_PASS 未设置');

    console.log('=== 登录 ===');
    await page.goto(WEB + '/login', { waitUntil: 'networkidle', timeout: 60000 });
    const ins = page.locator('input');
    await ins.nth(0).fill(USER);
    await ins.nth(1).fill(PASS);
    await page.locator('button[type=submit]').first().click();
    await page.waitForTimeout(3500);

    // ── 规则页：新建 → 编辑 → 删除 ──
    console.log('\n=== 1. 规则页写操作 ===');
    await page.goto(WEB + '/operation/rules', { waitUntil: 'networkidle' });
    await page.waitForTimeout(2200);

    const ruleName = `UI规则${Date.now()}`;
    await page.getByRole('button', { name: '新建规则' }).click();
    await page.waitForTimeout(800);
    const modal = page.locator('.ant-modal-content');
    await modal.locator('input').nth(0).fill(ruleName);
    await modal.locator('input').nth(1).fill('operation');
    await modal.locator('input').nth(2).fill('UI 自动化创建');
    await modal.locator('textarea').fill('{"field":"member.total_spend","op":"gte","value":100}');
    await page.screenshot({ path: `${SHOTS}/ops-ui-1-rule-create.png` });
    await modal.getByRole('button', { name: /确\s*定|OK/ }).click();
    await page.waitForTimeout(2500);

    let rules = await apiGet('/api/rules');
    const created = (rules.items ?? []).find((r) => r.name === ruleName);
    check('新建规则已落库', Boolean(created), created ? `id=${created.id}` : '未找到');

    // 编辑刚建的规则
    if (created) {
        const row = page.locator('.ant-table-tbody tr').filter({ hasText: ruleName });
        await row.locator('button').filter({ hasText: '编' }).first().click();
        await page.waitForTimeout(900);
        const nameInput = page.locator('.ant-modal-content input').nth(0);
        await nameInput.fill(ruleName + '改名');
        await page.locator('.ant-modal-content').getByRole('button', { name: /确\s*定|OK/ }).click();
        await page.waitForTimeout(2200);
        rules = await apiGet('/api/rules');
        const renamed = (rules.items ?? []).find((r) => r.id === created.id);
        check('编辑规则已生效', renamed?.name === ruleName + '改名', renamed?.name);
        check('版本号递增', renamed?.version === 2, `v${renamed?.version}`);

        // 删除
        const row2 = page.locator('.ant-table-tbody tr').filter({ hasText: '改名' });
        await row2.locator('button').filter({ hasText: '删' }).first().click();
        await page.waitForTimeout(700);
        await page.locator('.ant-modal-confirm').getByRole('button', { name: /确\s*定|OK/ }).click();
        await page.waitForTimeout(2200);
        rules = await apiGet('/api/rules');
        check('删除规则已生效', !(rules.items ?? []).some((r) => r.id === created.id));
    }
    await page.screenshot({ path: `${SHOTS}/ops-ui-2-rule-after.png` });

    // ── 作业页：新建 → 编辑 → 删除 ──
    console.log('\n=== 2. 作业页写操作 ===');
    await page.goto(WEB + '/setting/jobs', { waitUntil: 'networkidle' });
    await page.waitForTimeout(2200);

    const jobName = `UI作业${Date.now()}`;
    await page.getByRole('button', { name: '新建作业' }).click();
    await page.waitForTimeout(900);
    const jm = page.locator('.ant-modal-content');
    await jm.locator('input').nth(0).fill(jobName);
    await jm.locator('input').nth(1).fill('*/11 * * * *');
    await page.screenshot({ path: `${SHOTS}/ops-ui-3-job-create.png` });
    await jm.getByRole('button', { name: /确\s*定|OK/ }).click();
    await page.waitForTimeout(2500);

    let jobs = await apiGet('/api/jobs');
    const job = (jobs.items ?? []).find((j) => j.name === jobName);
    check('新建作业已落库', Boolean(job), job ? `id=${job.id}` : '未找到');

    if (job) {
        const jrow = page.locator('.ant-table-tbody tr').filter({ hasText: jobName });
        await jrow.locator('button').filter({ hasText: '编' }).first().click();
        await page.waitForTimeout(900);
        await page.locator('.ant-modal-content input').nth(1).fill('*/13 * * * *');
        await page.locator('.ant-modal-content').getByRole('button', { name: /确\s*定|OK/ }).click();
        await page.waitForTimeout(2200);
        jobs = await apiGet('/api/jobs');
        const j2 = (jobs.items ?? []).find((x) => x.id === job.id);
        check('编辑作业已生效', j2?.cron === '*/13 * * * *', j2?.cron);

        // 历史抽屉（作业页第三个按钮）
        const jrow2 = page.locator('.ant-table-tbody tr').filter({ hasText: jobName });
        await jrow2.locator('button').filter({ hasText: '历' }).first().click();
        await page.waitForTimeout(1500);
        const drawerTitle = await page.locator('.ant-drawer-title:visible').last().innerText().catch(() => '');
        check('执行历史抽屉可打开', /执行历史/.test(drawerTitle), drawerTitle);
        await page.screenshot({ path: `${SHOTS}/ops-ui-4-job-runs.png` });
        const close = page.locator('.ant-drawer-close:visible').first();
        if (await close.count()) await close.click();
        await page.waitForTimeout(800);

        const jrow3 = page.locator('.ant-table-tbody tr').filter({ hasText: jobName });
        await jrow3.locator('button').filter({ hasText: '删' }).first().click();
        await page.waitForTimeout(700);
        await page.locator('.ant-modal-confirm').getByRole('button', { name: /确\s*定|OK/ }).click();
        await page.waitForTimeout(2200);
        jobs = await apiGet('/api/jobs');
        check('删除作业已生效', !(jobs.items ?? []).some((x) => x.id === job.id));
    }
    await page.screenshot({ path: `${SHOTS}/ops-ui-5-job-after.png` });

    // ── 脱敏页：新建策略 → 编辑 → 删除 ──
    console.log('\n=== 3. 脱敏页写操作 ===');
    await page.goto(WEB + '/setting/masking', { waitUntil: 'networkidle' });
    await page.waitForTimeout(2200);

    const field = 'member_e2e_field';
    await page.getByRole('button', { name: '新建策略' }).click();
    await page.waitForTimeout(900);
    const mm = page.locator('.ant-modal-content');
    await mm.locator('input').nth(0).fill(field);
    await page.screenshot({ path: `${SHOTS}/ops-ui-6-mask-create.png` });
    await mm.getByRole('button', { name: /确\s*定|OK/ }).click();
    await page.waitForTimeout(2500);

    let pol = await apiGet('/api/mask-policies');
    const p = (pol.items ?? []).find((x) => x.field === field);
    check('新建策略已落库', Boolean(p), p ? p.strategy : '未找到');

    if (p) {
        const prow = page.locator('.ant-table-tbody tr').filter({ hasText: field });
        await prow.locator('button').filter({ hasText: '编' }).first().click();
        await page.waitForTimeout(900);
        // 备注改成可辨识的值
        const noteInput = page.locator('.ant-modal-content input').last();
        await noteInput.fill('UI 自动化改过');
        await page.locator('.ant-modal-content').getByRole('button', { name: /确\s*定|OK/ }).click();
        await page.waitForTimeout(2200);
        pol = await apiGet('/api/mask-policies');
        const p2 = (pol.items ?? []).find((x) => x.field === field);
        check('编辑策略已生效', p2?.note === 'UI 自动化改过', p2?.note);

        const prow2 = page.locator('.ant-table-tbody tr').filter({ hasText: field });
        await prow2.locator('button').filter({ hasText: '删' }).first().click();
        await page.waitForTimeout(700);
        await page.locator('.ant-modal-confirm').getByRole('button', { name: /确\s*定|OK/ }).click();
        await page.waitForTimeout(2200);
        pol = await apiGet('/api/mask-policies');
        check('删除策略已生效', !(pol.items ?? []).some((x) => x.field === field));
    }
    await page.screenshot({ path: `${SHOTS}/ops-ui-7-mask-after.png` });

    console.log('\n=== 4. JS 错误 ===');
    const real = errors.filter((e) => !/favicon|ResizeObserver/i.test(e));
    check('无致命 JS 错误', real.length === 0, real.slice(0, 2).join(' | '));
} catch (e) {
    check('流程执行', false, e.message);
    await page.screenshot({ path: `${SHOTS}/ops-ui-error.png` }).catch(() => {});
} finally {
    console.log(`\n===== 结果: ${pass} 通过 / ${fail} 失败 =====`);
    await browser.close();
    process.exit(fail ? 1 : 0);
}
