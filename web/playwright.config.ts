import { defineConfig, devices } from '@playwright/test';

/**
 * E2E 只覆盖「真实浏览器能不能把页面跑起来」这一层：
 * 登录、导航、表格出数、接口报错时的降级表现。
 *
 * 为什么不用单元测试替代：这个项目的风险集中在
 * 「前端声明式配置 ↔ 后端接口字段」这条缝上，
 * 类型系统管不到跨进程的字段名，只有真跑一遍才知道对不对。
 */
export default defineConfig({
    testDir: './e2e',
    timeout: 30_000,
    expect: { timeout: 10_000 },
    fullyParallel: false, // 共享同一个本地会话，串行更稳
    workers: 1,
    retries: 0,
    reporter: [['list']],
    use: {
        baseURL: process.env.E2E_BASE_URL || 'http://localhost:5173',
        trace: 'retain-on-failure',
        screenshot: 'only-on-failure',
        locale: 'zh-CN',
    },
    projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
    // 本地已经手动起了前后端就用现成的，没有则自己拉起来。
    webServer: {
        command: 'pnpm run dev',
        url: 'http://localhost:5173',
        reuseExistingServer: true,
        timeout: 60_000,
    },
});
