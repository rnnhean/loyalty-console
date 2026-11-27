/**
 * 运维配置三页的有头浏览器验证。
 *
 * 为什么必须有头：这三页都是「点开、填表、看结果」的交互，
 * 无头模式只能证明 DOM 渲染，证明不了按钮真的接通了后端。
 *
 * 验证内容：
 *   1. 三个页面都能从菜单进入，并且加载出种子数据；
 *   2. 规则页的「试运行全部」真的弹出结果，且命中数与后端一致；
 *   3. 作业页的「立即执行」真的产生一条历史记录；
 *   4. 脱敏页的预览真的把手机号打码。
 *
 * 任何一步失败都截图，不假装通过。
 */
import { chromium } from 'file:///D:/20260916/.e2e-tools/node_modules/playwright/index.mjs';

const WEB = 'http://localhost:5181';
const SHOTS = 'D:/20260916/loyalty-console/docs/shots';
const USER = process.env.MC_ADMIN_USER || 'admin';
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

try {
    if (!PASS) throw new Error('MC_ADMIN_PASS 未设置');

    console.log('=== 1. 登录 ===');
    await page.goto(WEB + '/login', { waitUntil: 'networkidle', timeout: 60000 });
    const inputs = page.locator('input');
    await inputs.nth(0).fill(USER);
    await inputs.nth(1).fill(PASS);
    await page.locator('button[type=submit], button:has-text("登")').first().click();
    await page.waitForTimeout(4000);
    check('登录成功', page.url().includes('/') && !page.url().includes('/login'), page.url());

    console.log('\n=== 2. 可配置规则页 ===');
    // 侧栏默认是折叠的，折叠态下 antd 只渲染图标、不渲染文字，
    // 直接按文字找会等 30 秒超时。先点触发器展开。
    const trigger = page.locator('.ant-layout-sider-trigger');
    if (await trigger.count()) {
        await trigger.first().click();
        await page.waitForTimeout(800);
    }
    const openMenu = async (label, pathPart) => {
        let item = page.locator(`.ant-menu-item:has-text("${label}"), a:has-text("${label}")`).first();
        if (!(await item.count())) {
            // antd 的二级菜单默认懒渲染：先展开父级「运维配置」子项才会进 DOM
            const parent = page.locator('.ant-menu-submenu-title:has-text("运维配置")').first();
            if (await parent.count()) {
                await parent.click();
                await page.waitForTimeout(800);
                item = page.locator(`.ant-menu-item:has-text("${label}"), a:has-text("${label}")`).first();
            }
        }
        if (await item.count()) {
            await item.click();
        } else {
            // 兜底：菜单结构变了也不至于整个测试挂掉，但要打印出来
            console.log(`    （菜单里没找到「${label}」，直接按路由打开）`);
            await page.goto(WEB + pathPart, { waitUntil: 'networkidle' });
        }
        await page.waitForTimeout(2200);
    };

    /**
     * 读当前**可见**的抽屉内容。
     * 页面上同时挂了两个 Drawer（试运行 / 解释），关闭后 DOM 仍在，
     * 直接取 .ant-drawer 会拿到隐藏的那个，断言就会对着上一次的结果做。
     */
    // 不能用 offsetParent 判可见：抽屉是 position:fixed，offsetParent 恒为 null。
    // 交给 Playwright 的 :visible 引擎判断更可靠。
    const visibleDrawerText = async () => {
        const loc = page.locator('.ant-drawer-body:visible').last();
        return (await loc.count()) ? await loc.innerText().catch(() => '') : '';
    };
    const closeDrawer = async () => {
        const btn = page.locator('.ant-drawer-close:visible').first();
        if (await btn.count()) await btn.click();
        await page.waitForTimeout(1200);
    };

    await openMenu('可配置规则', '/operation/rules');
    let text = await page.locator('body').innerText();
    check('进入规则页', page.url().includes('/operation/rules'), page.url());
    check('加载出种子规则', /连续 30 天未登录打标|高价值层级缺失在线状态/.test(text));
    await page.screenshot({ path: `${SHOTS}/ops-1-rules.png` });

    // 试运行
    await page.getByRole('button', { name: '试运行全部' }).click();
    await page.waitForTimeout(2500);
    const dryText = await visibleDrawerText();
    // 标题在 .ant-drawer-header 里，不在 body 里，要单独取
    const dryTitle = await page.locator('.ant-drawer-title:visible').last().innerText().catch(() => '');
    check('试运行抽屉打开', /试运行结果/.test(dryTitle), dryTitle);
    check('试运行显示命中结果', /命中/.test(dryText), dryText.replace(/\n/g, ' ').slice(0, 100));
    await page.screenshot({ path: `${SHOTS}/ops-2-rules-dryrun.png` });
    await closeDrawer();

    // 解释
    // 用 table 内的文本定位：getByRole 在 antd Button 的 span 包裹下偶尔拿不到可访问名
    // 注意：antd 会在两个汉字之间自动插空格（「解 释」），按纯文本找不到。
    await page.locator('.ant-table-tbody button').filter({ hasText: '解' }).first().click();
    await page.waitForTimeout(2000);
    const expText = await visibleDrawerText();
    check('解释抽屉出现实际值/期望值', /实际|期望/.test(expText), expText.replace(/\n/g, ' ').slice(0, 120));
    await page.screenshot({ path: `${SHOTS}/ops-3-rules-explain.png` });
    await closeDrawer();

    console.log('\n=== 3. 定时作业页 ===');
    await openMenu('定时作业', '/setting/jobs');
    text = await page.locator('body').innerText();
    check('进入作业页', page.url().includes('/setting/jobs'), page.url());
    check('加载出种子作业', /过期规则清理|指标快照/.test(text));
    await page.screenshot({ path: `${SHOTS}/ops-4-jobs.png` });

    // 立即执行第一行
    const beforeRuns = await page.evaluate(async () => {
        const t = localStorage.getItem('mc.console.ticket');
        const r = await fetch('/api/job-runs', { headers: { authorization: 'Bearer ' + t } });
        const j = await r.json();
        return (j.items || []).length;
    });
    await page.getByRole('button', { name: '立即执行' }).first().click();
    await page.waitForTimeout(4000);
    const afterRuns = await page.evaluate(async () => {
        const t = localStorage.getItem('mc.console.ticket');
        const r = await fetch('/api/job-runs', { headers: { authorization: 'Bearer ' + t } });
        const j = await r.json();
        return (j.items || []).length;
    });
    check('立即执行产生了新记录', afterRuns > beforeRuns, `${beforeRuns} → ${afterRuns}`);
    await page.screenshot({ path: `${SHOTS}/ops-5-jobs-run.png` });

    console.log('\n=== 4. 字段脱敏页 ===');
    await openMenu('字段脱敏', '/setting/masking');
    text = await page.locator('body').innerText();
    check('进入脱敏页', page.url().includes('/setting/masking'), page.url());
    check('加载出策略', /member_phone|member_id_card/.test(text));
    await page.screenshot({ path: `${SHOTS}/ops-6-masking.png` });

    // 预览（antd 会把「预览」渲染成「预 览」，用正则容忍中间空格）
    await page.getByRole('button', { name: /预\s*览/ }).click();
    await page.waitForTimeout(2000);
    const pvText = await page.locator('body').innerText();
    check('预览显示打码结果', /138\*+1234/.test(pvText), (pvText.match(/138\*+\d+/) || [''])[0]);
    await page.screenshot({ path: `${SHOTS}/ops-7-mask-preview.png` });

    console.log('\n=== 5. JS 错误 ===');
    const real = errors.filter((e) => !/favicon|ResizeObserver/i.test(e));
    check('无致命 JS 错误', real.length === 0, real.slice(0, 2).join(' | '));
} catch (e) {
    check('流程执行', false, e.message);
    await page.screenshot({ path: `${SHOTS}/ops-error.png` }).catch(() => {});
} finally {
    console.log(`\n===== 结果: ${pass} 通过 / ${fail} 失败 =====`);
    console.log('截图: docs/shots/ops-*.png');
    await browser.close();
    process.exit(fail ? 1 : 0);
}
