/**
 * 全量截图：前端所有页面 + 交互流程 + 前端监控页 + 后端监控页。
 *
 * 跑法：MC_ADMIN_PASS=demo1234 node scripts/shoot-all.js
 *
 * 输出到 docs/shots/，文件名按「序号-分类-名称」排列，方便 README 引用。
 *
 * 设计说明：
 *   - 用真实浏览器走真实登录、真实接口，不使用截图或固定 HTML
 *   - 每张截图前记录行数/空态，用来发现「页面打开了但没数据」这种假通过
 *   - 监控页（Grafana/Prometheus/Sentry）单独一段，因为它们要等图表渲染
 */
'use strict';

const { chromium } = require('d:/projects/e2e-lib/node_modules/playwright');
const fs = require('fs');
const path = require('path');

const BASE = process.env.WEB_BASE || 'http://localhost:5173';
const USER = process.env.MC_ADMIN_USER || 'admin';
const PASS = process.env.MC_ADMIN_PASS || 'demo1234';

const GRAFANA = process.env.GRAFANA_BASE || 'http://localhost:13000';
const PROM = process.env.PROM_BASE || 'http://localhost:19090';

const OUT = path.join(__dirname, '..', 'docs', 'shots');

// 22 个业务页 + 登录 + 工作台
const PAGES = [
    ['01-login', '/login'],

    ['10-dashboard', '/'],

    ['20-catalog-spu', '/catalog/spu'],
    ['21-catalog-sku', '/catalog/sku'],
    ['22-catalog-cards', '/catalog/cards'],
    ['23-catalog-pricing', '/catalog/pricing'],
    ['24-catalog-binds', '/catalog/binds'],

    ['30-benefit-tiers', '/benefit/tiers'],
    ['31-benefit-groups', '/benefit/groups'],
    ['32-benefit-rules', '/benefit/rules'],
    ['33-benefit-tasks', '/benefit/tasks'],

    ['40-trade-checkouts', '/trade/checkouts'],
    ['41-trade-debits', '/trade/debits'],
    ['42-trade-promos', '/trade/promos'],

    ['50-channel-merchants', '/channel/merchants'],
    ['51-channel-nodes', '/channel/nodes'],
    ['52-channel-attribution', '/channel/attribution'],

    ['60-operation-campaigns', '/operation/campaigns'],
    ['61-operation-vouchers', '/operation/vouchers'],
    ['62-operation-slots', '/operation/slots'],

    ['70-setting-products', '/setting/products'],
    ['71-setting-dict', '/setting/dict'],
    ['72-setting-modules', '/setting/modules'],
    ['73-setting-toggles', '/setting/toggles'],
];

const results = [];
const errors = [];

function note(name, extra) {
    results.push(`${name.padEnd(28)} ${extra}`);
}

async function snap(page, name, opts = {}) {
    await page.screenshot({ path: path.join(OUT, `${name}.png`), fullPage: opts.fullPage !== false });
}

(async () => {
    fs.mkdirSync(OUT, { recursive: true });

    const exe = path.join(
        process.env.LOCALAPPDATA || 'C:/Users/v-renhan/AppData/Local',
        'ms-playwright/chromium-1140/chrome-win/chrome.exe',
    );
    const browser = await chromium.launch(fs.existsSync(exe) ? { executablePath: exe } : {});
    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    const page = await ctx.newPage();

    page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));
    page.on('console', (m) => {
        if (m.type() === 'error') errors.push(`console: ${m.text()}`.slice(0, 200));
    });

    // ── 1. 登录页 + 登录流程 ──
    await page.goto(`${BASE}/login`, { waitUntil: 'networkidle' });
    await snap(page, '01-login');
    note('01-login', '未登录状态');

    // 故意输错密码，截「登录失败」提示
    await page.fill('input[autocomplete="username"]', USER);
    await page.fill('input[autocomplete="current-password"]', 'wrong-password');
    await page.click('button[type="submit"]');
    await page.waitForTimeout(1500);
    await snap(page, '02-login-error');
    note('02-login-error', '错误密码的提示');

    // 正确登录
    await page.fill('input[autocomplete="current-password"]', PASS);
    await page.click('button[type="submit"]');
    await page.waitForURL(`${BASE}/`, { timeout: 15000 });
    await page.waitForLoadState('networkidle');
    note('login', '登录成功');

    // ── 2. 全部业务页 ──
    for (const [name, route] of PAGES) {
        if (name === '01-login') continue;
        await page.goto(`${BASE}${route}`, { waitUntil: 'networkidle' });
        if (route !== '/') {
            await page.waitForSelector('.ant-table', { timeout: 10000 }).catch(() => {});
        }
        // 产品下拉类页面要先试几个产品才定下来，多等一会儿
        await page.waitForTimeout(route.includes('pricing') || route.includes('tiers') ? 3000 : 900);

        const rows = await page.locator('.ant-table-tbody tr.ant-table-row').count();
        const empty = await page.locator('.ant-table-placeholder').count();
        await snap(page, name);
        note(name, `行数=${rows} 空态=${empty}`);
    }

    // ── 3. 交互流程 ──
    // 3.1 表格分页
    await page.goto(`${BASE}/catalog/spu`, { waitUntil: 'networkidle' });
    await page.waitForSelector('.ant-table-tbody tr.ant-table-row', { timeout: 10000 });
    await page.waitForTimeout(800);
    const pager = page.locator('.ant-pagination-item-2').first();
    if (await pager.count()) {
        await pager.click();
        await page.waitForTimeout(1200);
        await snap(page, '90-interact-pagination');
        note('90-interact-pagination', '点击第 2 页');
    }

    // 3.2 搜索框输入
    const searchInput = page.locator('input[placeholder*="搜索"], input[placeholder*="关键字"]').first();
    if (await searchInput.count()) {
        await searchInput.fill('test');
        await page.keyboard.press('Enter');
        await page.waitForTimeout(1200);
        await snap(page, '91-interact-search');
        note('91-interact-search', '搜索 test');
    }

    // 3.3 产品下拉展开
    const productSelect = page.locator('.ant-select').first();
    if (await productSelect.count()) {
        await productSelect.click();
        await page.waitForTimeout(900);
        await snap(page, '92-interact-product-dropdown', { fullPage: false });
        note('92-interact-product-dropdown', '产品下拉展开');
        await page.keyboard.press('Escape');
    }

    // 3.4 侧边栏折叠/展开
    await page.goto(`${BASE}/`, { waitUntil: 'networkidle' });
    const collapseBtn = page.locator('.ant-layout-sider-trigger').first();
    if (await collapseBtn.count()) {
        await collapseBtn.click();
        await page.waitForTimeout(900);
        await snap(page, '93-interact-sidebar-collapsed', { fullPage: false });
        note('93-interact-sidebar-collapsed', '侧边栏折叠');

        await collapseBtn.click();
        await page.waitForTimeout(700);
    }

    // 3.5 退出登录
    await page.goto(`${BASE}/`, { waitUntil: 'networkidle' });
    const avatar = page.locator('.ant-avatar, .ant-dropdown-trigger').first();
    if (await avatar.count()) {
        await avatar.hover();
        await page.waitForTimeout(800);
        await snap(page, '94-interact-user-menu', { fullPage: false });
        note('94-interact-user-menu', '用户菜单');
    }

    // ── 4. 后端监控：Prometheus ──
    const promPages = [
        ['80-mon-prom-graph', `${PROM}/graph`, 5000],
        ['81-mon-prom-targets', `${PROM}/targets`, 3000],
        ['82-mon-prom-rules', `${PROM}/rules`, 3000],
        ['83-mon-prom-alerts', `${PROM}/alerts`, 3000],
    ];
    for (const [name, url, wait] of promPages) {
        try {
            await page.goto(url, { waitUntil: 'networkidle', timeout: 20000 });
            await page.waitForTimeout(wait);
            await snap(page, name);
            note(name, 'ok');
        } catch (e) {
            note(name, `失败: ${e.message.slice(0, 60)}`);
        }
    }

    // 指标查询：打一个真实 query 出图
    try {
        await page.goto(`${PROM}/graph?g0.expr=mc_gateway_request_total&g0.tab=0`, {
            waitUntil: 'networkidle',
            timeout: 20000,
        });
        await page.waitForTimeout(6000);
        await snap(page, '84-mon-prom-query');
        note('84-mon-prom-query', 'mc_gateway_request_total 曲线');
    } catch (e) {
        note('84-mon-prom-query', `失败: ${e.message.slice(0, 60)}`);
    }

    // ── 5. 后端监控：Grafana ──
    const grafPages = [
        ['85-mon-grafana-dashboard', `${GRAFANA}/d/mc-console-overview`, 8000],
        ['86-mon-grafana-home', `${GRAFANA}/`, 5000],
    ];
    for (const [name, url, wait] of grafPages) {
        try {
            await page.goto(url, { waitUntil: 'networkidle', timeout: 30000 });
            await page.waitForTimeout(wait);
            // Grafana 首次访问可能有引导弹窗，尽量关掉
            await page.keyboard.press('Escape').catch(() => {});
            await page.waitForTimeout(1500);
            await snap(page, name);
            note(name, 'ok');
        } catch (e) {
            note(name, `失败: ${e.message.slice(0, 60)}`);
        }
    }

    await browser.close();

    // ── 汇总 ──
    console.log('\n=== 截图结果 ===');
    results.forEach((r) => console.log('  ' + r));

    if (errors.length) {
        console.log('\n=== 页面错误（去重）===');
        [...new Set(errors)].slice(0, 15).forEach((e) => console.log('  ' + e));
    } else {
        console.log('\n无 JS 错误');
    }
})().catch((e) => {
    console.error('FAILED:', e.message);
    process.exit(1);
});
