import { expect, test } from '@playwright/test';
import path from 'node:path';

/**
 * 生成交付用的截图。
 *
 * 单独一个文件而不是塞进 smoke：截图是「给人看」的产物，
 * 和断言是两件事 —— 混在一起时，改截图逻辑会动到断言文件，
 * 评审时看不出哪些是回归风险。
 *
 * 输出到 docs/shots/，和文档里的引用路径一致。
 */

// 项目是 ESM（package.json type: module），没有 __dirname，用 cwd 拼。
// Playwright 的 cwd 固定是 web/，所以往上一级就是仓库根的 docs/shots。
const OUT = path.resolve(process.cwd(), '../docs/shots');

test.describe('截图', () => {
    test('登录页', async ({ page }) => {
        await page.goto('/login');
        await expect(page.getByRole('button', { name: '登 录' })).toBeVisible();
        await page.screenshot({ path: path.join(OUT, 'e2e-login.png'), fullPage: true });
    });

    test('工作台', async ({ page }) => {
        await page.goto('/login');
        await page.getByLabel('用户名').fill(process.env.E2E_USER || 'admin');
        await page.getByLabel('密码').fill(process.env.E2E_PASS || 'demo1234');
        await page.getByRole('button', { name: '登 录' }).click();
        await expect(page.locator('.ant-statistic')).toHaveCount(4);
        // 指标卡是异步拉的，等两拍再截，否则会截到 0
        await page.waitForTimeout(1200);
        await page.screenshot({ path: path.join(OUT, 'e2e-dashboard.png'), fullPage: true });
    });

    test('商品档案（带筛选）', async ({ page }) => {
        await page.goto('/login');
        await page.getByLabel('用户名').fill(process.env.E2E_USER || 'admin');
        await page.getByLabel('密码').fill(process.env.E2E_PASS || 'demo1234');
        await page.getByRole('button', { name: '登 录' }).click();
        await page.goto('/catalog/spu');
        await expect(page.locator('.ant-table-tbody tr.ant-table-row').first()).toBeVisible({
            timeout: 15_000,
        });
        await page.screenshot({ path: path.join(OUT, 'e2e-catalog-spu.png'), fullPage: true });
    });

    test('Grafana 总览', async ({ page }) => {
        await page.goto('http://127.0.0.1:13000/d/mc-console-overview');
        await expect(page.locator('[data-panelid]').first()).toBeVisible({ timeout: 20_000 });
        // 面板里的折线要等一次 ranger 查询，等久一点
        await page.waitForTimeout(3000);
        await page.screenshot({ path: path.join(OUT, 'e2e-grafana.png'), fullPage: true });
    });

    test('Prometheus 抓取目标', async ({ page }) => {
        await page.goto('http://127.0.0.1:19090/targets');
        // 「gateway (1/1 up)」是抓取池标题，用角色+正则定位；
        // 直接 getByText('gateway') 会命中 labels 里的 job="gateway"，不是同一个元素。
        await expect(page.getByRole('link', { name: /gateway \(\d+\/\d+ up\)/ })).toBeVisible({
            timeout: 20_000,
        });
        await page.screenshot({ path: path.join(OUT, 'e2e-prometheus-targets.png'), fullPage: true });
    });
});
