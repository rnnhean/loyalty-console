import { expect, test } from '@playwright/test';

/**
 * 运营诊断助手的端到端。
 *
 * 这一组覆盖的是「前端 SSE 客户端 ↔ agent 服务 ↔ 网关工具」这条链路：
 * 单测能保证每条协议各自正确，但只有真跑一次才知道
 * 事件顺序、落库的 finding 和卡片渲染有没有对上。
 *
 * **agent 没起就跳过**，和 monitoring 那组同一个原则：
 * 业务 E2E 不该因为本地没开某个可选服务就变红。起法：
 *
 *   cd agent && AGENT_FAKE_MODEL=1 AGENT_FAKE_SCRIPT=testdata/fake_script.json \
 *     AGENT_BACKEND_URL=http://127.0.0.1:8080 node --import tsx src/server.ts
 */

const AGENT = process.env.E2E_AGENT_URL || 'http://127.0.0.1:3001';
const USER = process.env.E2E_USER || 'admin';
const PASS = process.env.E2E_PASS || 'demo1234';

async function login(page: import('@playwright/test').Page) {
    await page.goto('/login');
    await page.getByLabel('用户名').fill(USER);
    await page.getByLabel('密码').fill(PASS);
    await page.getByRole('button', { name: '登 录' }).click();
    await expect(page).toHaveURL(/\/($|\?)/, { timeout: 15_000 });
}

test.describe('诊断助手', () => {
    // 在 hook 里探测而不是用 test.skip(condition)：后者只接受同步条件，
    // 而这里要发一个 HTTP 请求才知道 agent 在不在。
    test.beforeEach(async () => {
        let enabled = false;
        try {
            const res = await fetch(`${AGENT}/healthz`, { signal: AbortSignal.timeout(2000) });
            enabled = Boolean((await res.json()).enabled);
        } catch {
            enabled = false;
        }
        test.skip(!enabled, 'agent 服务未启用（本地没起或 AGENT_ENABLED=0）');
    });

    test('提问后能拿到完整报告，且建议与检查步骤可见', async ({ page }) => {
        await login(page);

        await page.getByRole('button', { name: '打开运营诊断助手' }).click();
        await expect(page.getByText('运营诊断助手')).toBeVisible();

        const input = page.getByPlaceholder(/描述你看到的现象/);
        await input.fill('为什么有些权益配好了用户却领不到');
        await page.getByRole('button', { name: '发送' }).click();

        // 报告卡片出现即代表 SSE 收到了 final 事件并通过了服务端校验；
        // 校验不过时会落到降级分支，kind 仍是 diagnosis 但没有 advice。
        const report = page.locator('.agent-report');
        await expect(report).toBeVisible({ timeout: 30_000 });
        await expect(report).toHaveClass(/agent-report--diagnosis/);

        // 标题与判定必须有字，空字符串是「模型说了但没被解析出来」
        await expect(report.locator('.agent-report__title')).not.toBeEmpty();
        await expect(report.locator('.agent-report__text').first()).not.toBeEmpty();

        // 至少一条建议，且能点进对应页面
        await expect(report.locator('.agent-report__advice li')).toHaveCount(1);
        await expect(report.locator('.agent-report__advice li')).not.toBeEmpty();

        // 步骤列表记录了真实跑过的规则名，不能是空壳
        await expect(page.locator('.agent-steps__item')).toHaveCount(1);

        // 不能出现错误横幅 —— 有报告又同时报错，说明错误处理写串了
        await expect(page.locator('.ant-alert-error')).toHaveCount(0);
    });
});
