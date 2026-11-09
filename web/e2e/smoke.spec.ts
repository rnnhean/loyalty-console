import { expect, test, type Page } from '@playwright/test';
// 直接复用业务配置：页面清单只有一处定义，
// 新增页面时这里自动覆盖到，不会出现「测试里漏了一个路由」。
import { DOMAINS } from '../src/config/console';

const USER = process.env.E2E_USER || 'admin';
const PASS = process.env.E2E_PASS || 'demo1234';

/** 登录。每个用例独立上下文，所以不做 storageState 复用 —— 少一层缓存状态，失败更好复现。 */
async function login(page: Page) {
    await page.goto('/login');
    await page.getByLabel('用户名').fill(USER);
    await page.getByLabel('密码').fill(PASS);
    await page.getByRole('button', { name: '登 录' }).click();
    await expect(page).toHaveURL(/\/($|\?)/, { timeout: 15_000 });
}

test.describe('鉴权', () => {
    test('未登录直接访问业务页会回到登录页', async ({ page }) => {
        await page.goto('/catalog/spu');
        await expect(page).toHaveURL(/\/login/);
        await expect(page.getByRole('button', { name: '登 录' })).toBeVisible();
    });

    test('密码错误时停留在登录页并提示', async ({ page }) => {
        await page.goto('/login');
        await page.getByLabel('用户名').fill(USER);
        await page.getByLabel('密码').fill('wrong-password');
        await page.getByRole('button', { name: '登 录' }).click();
        await expect(page.locator('.ant-message-error')).toBeVisible();
        await expect(page).toHaveURL(/\/login/);
    });
});

test.describe('工作台', () => {
    test('登录后进入工作台且指标卡有数', async ({ page }) => {
        await login(page);
        // 四格指标卡是这个页面的核心信号，空数组或接口失败时这里会先炸
        await expect(page.locator('.ant-statistic')).toHaveCount(4);
        await expect(page.locator('.ant-statistic').first()).toContainText(/\d/);
    });
});

test.describe('业务页', () => {
    // 逐个域跑，失败时能直接看出是哪个模块
    for (const domain of DOMAINS) {
        test(`${domain.title}：${domain.pages.length} 个页面都能出数`, async ({ page }) => {
            await login(page);

            for (const p of domain.pages) {
                await page.goto(p.path);

                // 1) 页面标题渲染了（说明配置与路由对得上）
                await expect(page.locator('.console__content')).toContainText(p.title);

                // 2) 表格出现在预期状态：要么有数据行，要么明确为空态。
                //    这里不断言「必须有数据」—— 空态也是正确行为，
                //    真正的 bug 是「一直转圈」或「红了报错」。
                const table = page.locator('.ant-table');
                await expect(table).toBeVisible();

                const rows = page.locator('.ant-table-tbody tr.ant-table-row');
                const empty = page.locator('.ant-table-placeholder');
                await expect
                    .poll(async () => (await rows.count()) > 0 || (await empty.count()) > 0, {
                        timeout: 15_000,
                        message: `${p.title} 既没有数据行也没有空态，可能卡在 loading`,
                    })
                    .toBe(true);

                // 3) 页面没有落到错误提示
                await expect(page.locator('.data-page__alert')).toHaveCount(0);
            }
        });
    }
});

test.describe('交互', () => {
    test('关键词搜索与清空能回到全量', async ({ page }) => {
        await login(page);
        await page.goto('/catalog/spu');

        const search = page.getByPlaceholder(/关键词|名称/);
        await search.fill('__不存在的关键词__');
        // antd 会在两个汉字之间自动插一个空格，实际无障碍名是「查 询」，
        // 所以用正则而不是精确串 —— 这条坑踩过一次，写在这里免得下次再踩。
        await page.getByRole('button', { name: /查\s*询/ }).click();

        await expect(page.locator('.ant-table-placeholder')).toBeVisible({ timeout: 15_000 });

        await search.fill('');
        await page.getByRole('button', { name: /查\s*询/ }).click();
        await expect(page.locator('.ant-table-tbody tr.ant-table-row').first()).toBeVisible({
            timeout: 15_000,
        });
    });

    test('退出登录后回到登录页', async ({ page }) => {
        await login(page);
        await page.getByRole('button', { name: '管理员' }).click();
        await page.getByText('退出登录').click();
        await expect(page).toHaveURL(/\/login/);
    });
});
