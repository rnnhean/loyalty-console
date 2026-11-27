/**
 * loyalty-console 全流程（有头浏览器）。
 *
 * 覆盖：登录 → 22 个业务页面遍历 → 数据核对 → 中间件链路验证。
 *
 * 这个项目的价值不在页面本身，在**中间件真的在干活**：
 *   Redis 缓存/锁、Kafka 审计、RabbitMQ 延迟队列、MinIO 导出。
 * 所以除了页面能打开，还要验证 `/api/infra/health` 五个组件都是 ok。
 */
import { chromium } from 'file:///D:/20260916/.e2e-tools/node_modules/playwright/index.mjs';

const WEB = 'http://localhost:5181';
const API = 'http://127.0.0.1:8080';
const SHOTS = 'D:/20260916/loyalty-console/docs/shots';

let pass = 0, fail = 0;
const check = (n, ok, extra = '') => { ok ? pass++ : fail++; console.log(`  ${ok ? '✅' : '❌'} ${n}${extra ? ' — ' + extra : ''}`); };

const browser = await chromium.launch({ headless: false, args: ['--window-size=1440,960'] });
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
const errors = [];
page.on('pageerror', (e) => errors.push(String(e)));

const isBlank = async () => {
    const t = (await page.locator('body').innerText().catch(() => '')).trim();
    return t.length < 20;
};

try {
    console.log('\n=== 1. 登录页 ===');
    await page.goto(WEB, { waitUntil: 'networkidle', timeout: 60000 });
    await page.waitForTimeout(2000);
    const loginText = await page.locator('body').innerText();
    check('登录页可访问', !(await isBlank()), loginText.slice(0, 50).replace(/\n/g, ' '));
    await page.screenshot({ path: `${SHOTS}/lc-1-登录.png` });

    console.log('\n=== 2. 登录 ===');
    const inputs = page.locator('input');
    const cnt = await inputs.count();
    console.log(`  输入框数: ${cnt}`);
    if (cnt >= 2) {
        await inputs.nth(0).fill('admin');
        await inputs.nth(1).fill('demo1234');
        const btn = page.locator('button').filter({ hasText: /登\s*录/ }).first();
        if (await btn.count()) await btn.click(); else await inputs.nth(1).press('Enter');
        await page.waitForTimeout(4000);
    }
    const afterLogin = await page.locator('body').innerText();
    check('登录成功', afterLogin.length > 100, afterLogin.slice(0, 60).replace(/\n/g, ' '));
    await page.screenshot({ path: `${SHOTS}/lc-2-登录后.png` });

    console.log('\n=== 3. 遍历全部业务页面 ===');
    // **直接按路由访问，而不是点菜单**：菜单是「六个域」分组折叠的，
    // 点一次只展开当前域；要遍历 22 页得处理展开/收起状态，
    // 那是在测导航交互而不是测页面本身。这里用 `console.ts` 声明的路由直接跳，
    // 每个页面都会重新挂载、重新请求数据，白屏问题一样能暴露。
    const ROUTES = [
        '/catalog/spu', '/catalog/sku', '/catalog/cards', '/catalog/pricing', '/catalog/binds',
        '/benefit/tiers', '/benefit/groups', '/benefit/rules', '/benefit/tasks',
        '/trade/checkouts', '/trade/debits', '/trade/promos',
        '/channel/merchants', '/channel/nodes', '/channel/attribution',
        '/operation/campaigns', '/operation/vouchers', '/operation/slots',
        '/setting/products', '/setting/dict', '/setting/modules', '/setting/toggles',
    ];
    console.log(`  共 ${ROUTES.length} 个业务页面`);

    let visited = 0;
    const blanks = [];
    for (const route of ROUTES) {
        await page.goto(WEB + route, { waitUntil: 'networkidle', timeout: 30000 }).catch(() => {});
        await page.waitForTimeout(1200);
        if (await isBlank()) {
            blanks.push(route);
            await page.screenshot({ path: `${SHOTS}/lc-blank-${route.replace(/[^a-z0-9]/gi, '_')}.png` });
        } else {
            visited++;
        }
    }
    check(`${visited}/${ROUTES.length} 个业务页面可渲染`, blanks.length === 0,
        blanks.length ? `白屏: ${blanks.join(', ')}` : '全部正常');

    // 存一张有代表性的页面截图（商品域，数据最全）
    await page.goto(WEB + '/catalog/spu', { waitUntil: 'networkidle' }).catch(() => {});
    await page.waitForTimeout(1500);
    await page.screenshot({ path: `${SHOTS}/lc-3-商品列表.png` });

    console.log('\n=== 4. 中间件健康检查 ===');
    const loginRes = await fetch(`${API}/api/auth/login`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username: 'admin', password: 'demo1234' }),
    }).then(r => r.json()).catch(() => null);
    const token = loginRes?.data?.token || loginRes?.token;
    check('拿到 API token', Boolean(token));

    if (token) {
        const infra = await fetch(`${API}/api/infra/health`, {
            headers: { Authorization: `Bearer ${token}` },
        }).then(r => r.json()).catch(() => null);
        // 响应形状是 {code:0, infra:{ mysql:{ok}, redis:{ok}, ... }}，
        // 之前写成 infra?.data 导致取不到任何组件，断言静默跳过——
        // 那种"没有断言失败但也没验证"的情况比直接失败更危险。
        const comps = infra?.infra || infra?.data?.infra || {};
        check('中间件健康接口返回 code=0', infra?.code === 0, `code=${infra?.code}`);
        const keys = Object.keys(comps);
        check('拿到中间件清单', keys.length > 0, keys.join(', '));
        for (const k of keys) {
            const v = comps[k];
            check(`中间件 ${k}`, v?.ok === true, v?.detail ? String(v.detail).slice(0, 50) : '');
        }
    }

    console.log('\n=== 5. 业务接口抽样 ===');
    if (token) {
        for (const [path, desc] of [
            ['/api/bff/catalog/overview?product=超级会员', 'BFF 聚合'],
            ['/api/catalog/spu', '商品 SPU 列表'],
            ['/api/benefit/tiers?product=超级会员', '会员层级'],
        ]) {
            const r = await fetch(API + path, { headers: { Authorization: `Bearer ${token}` } });
            const j = await r.json().catch(() => null);
            check(`${desc} (${path.split('?')[0]})`, r.status === 200 && j?.code === 0,
                `HTTP ${r.status} code=${j?.code}`);
        }
    }

    console.log('\n=== 6. JS 错误 ===');
    const real = errors.filter((e) => !/favicon|ResizeObserver/i.test(e));
    check('无致命 JS 错误', real.length === 0, real.slice(0, 2).join(' | '));

    console.log('\n浏览器保持打开 25 秒供查看…');
    await page.waitForTimeout(25000);
} catch (e) {
    check('流程执行', false, e.message);
    await page.screenshot({ path: `${SHOTS}/lc-error.png` }).catch(() => {});
} finally {
    console.log(`\n===== 结果: ${pass} 通过 / ${fail} 失败 =====`);
    await browser.close();
    process.exit(fail ? 1 : 0);
}
