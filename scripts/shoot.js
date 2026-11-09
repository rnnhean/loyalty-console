/**
 * 用真实浏览器验证前端（登录 + 工作台 + 各域列表页），并截图存档。
 *
 * 跑法：node scripts/shoot.js
 * 依赖：d:/projects/e2e-lib 里的 playwright（本工程不重复装一份浏览器）
 */
'use strict';

const { chromium } = require('d:/projects/e2e-lib/node_modules/playwright');
const fs = require('fs');
const path = require('path');

const BASE = process.env.WEB_BASE || 'http://localhost:5173';
const USER = process.env.MC_ADMIN_USER || 'admin';
const PASS = process.env.MC_ADMIN_PASS || 'test123';
const OUT = path.join(__dirname, '..', 'docs', 'shots');

const PAGES = [
    ['dashboard', '/'],
    ['catalog-spu', '/catalog/spu'],
    ['catalog-cards', '/catalog/cards'],
    ['catalog-pricing', '/catalog/pricing'],
    ['benefit-tiers', '/benefit/tiers'],
    ['benefit-groups', '/benefit/groups'],
    ['channel-merchants', '/channel/merchants'],
    ['setting-products', '/setting/products'],
];

(async () => {
    fs.mkdirSync(OUT, { recursive: true });
    // 本机已装的 chromium 版本与 e2e-lib 的 playwright 期望版本不一致，
    // 直接指定可执行文件，避免为一个版本差再下一份浏览器。
    const exe = path.join(
        process.env.LOCALAPPDATA || 'C:/Users/v-renhan/AppData/Local',
        'ms-playwright/chromium-1140/chrome-win/chrome.exe',
    );
    const browser = await chromium.launch(fs.existsSync(exe) ? { executablePath: exe } : {});
    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    const page = await ctx.newPage();

    const errors = [];
    page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));
    page.on('console', (m) => {
        if (m.type() === 'error') errors.push(`console: ${m.text()}`.slice(0, 300));
    });

    // 1. 登录页
    await page.goto(`${BASE}/login`, { waitUntil: 'networkidle' });
    await page.screenshot({ path: path.join(OUT, '00-login.png'), fullPage: true });
    console.log('✓ 登录页');

    // 2. 登录
    await page.fill('input[autocomplete="username"]', USER);
    await page.fill('input[autocomplete="current-password"]', PASS);
    await page.click('button[type="submit"]');
    await page.waitForURL(`${BASE}/`, { timeout: 10000 });
    await page.waitForLoadState('networkidle');
    console.log('✓ 登录成功');

    // 3. 逐页截图。产品下拉类页面要多等一会儿——它要先试几个产品才定下来。
    for (const [name, route] of PAGES) {
        await page.goto(`${BASE}${route}`, { waitUntil: 'networkidle' });
        if (route !== '/') {
            await page.waitForSelector('.ant-table', { timeout: 10000 }).catch(() => {});
        }
        await page.waitForTimeout(route.includes('pricing') || route.includes('tiers') ? 3500 : 800);

        const rows = await page.locator('.ant-table-tbody tr.ant-table-row').count();
        const empty = await page.locator('.ant-table-placeholder').count();
        const selected = await page
            .locator('.ant-select-selection-item')
            .first()
            .textContent()
            .catch(() => '');
        console.log(`  ${name.padEnd(18)} 行数=${rows} 空态=${empty} 产品=${(selected || '').trim()}`);

        await page.screenshot({ path: path.join(OUT, `${name}.png`), fullPage: true });
    }

    await browser.close();

    if (errors.length) {
        console.log('\n=== 页面错误 ===');
        [...new Set(errors)].slice(0, 20).forEach((e) => console.log('  ' + e));
    } else {
        console.log('\n无 JS 错误');
    }
})().catch((e) => {
    console.error('FAILED:', e.message);
    process.exit(1);
});
