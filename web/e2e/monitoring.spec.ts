import { expect, test } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * 监控栈的 E2E。
 *
 * 这一组不测业务，测的是「出事的时候能不能看到东西」：
 *   - Prometheus 是否真的抓到网关（up == 1）；
 *   - 本轮新加的指标是否真的被采集到；
 *   - Grafana 的面板和告警规则是否加载成功。
 *
 * 跳过而不是失败的条件：监控栈没起。业务 E2E 不该因为
 * 本地没开 Grafana 就变红，所以用 test.skip 显式跳过并打印原因。
 */

const PROM = process.env.E2E_PROM_URL || 'http://127.0.0.1:19090';
const GRAFANA = process.env.E2E_GRAFANA_URL || 'http://127.0.0.1:13000';
const GW = process.env.E2E_GATEWAY_URL || 'http://127.0.0.1:8080';

async function probe(url: string): Promise<boolean> {
    try {
        const res = await fetch(url, { signal: AbortSignal.timeout(3000) });
        return res.ok;
    } catch {
        return false;
    }
}

test.describe('监控栈', () => {
    test('Prometheus 抓到了网关且新指标在采集', async ({ request }) => {
        test.skip(!(await probe(`${PROM}/-/healthy`)), `Prometheus 未启动（${PROM}），跳过`);

        // 0) 先制造一次真实调用。
        //    Counter/Gauge 只有在被观测过之后才会出现序列 ——
        //    进程刚重启时这些指标根本不存在，直接断言会误判成「指标写错了」。
        const login = await request.post(`${GW}/api/auth/login`, {
            data: { username: 'admin', password: process.env.E2E_PASS || 'demo1234' },
        });
        const ticket = login.ok() ? (await login.json()).token : '';
        if (ticket) {
            // 幂等 + 分布式锁会在这一次调用里各产生一次指标。
            // 用一条已失效的规则（affected=0）即可，不改动业务数据。
            await request.post(`${GW}/api/benefit/expire-rule`, {
                headers: { Authorization: `Bearer ${ticket}` },
                data: { rule_uid: 10001 },
            });
        }

        // 等一个抓取周期（本地 5s）让指标进 Prometheus
        await new Promise((r) => setTimeout(r, 6500));

        // 1) 抓取目标存活：up == 1 是「Prometheus 认为这个实例健康」的唯一依据。
        //    看 /metrics 有没有输出不算 —— 那只能证明进程活着，不能证明被采集。
        const up = await request.get(`${PROM}/api/v1/query`, {
            params: { query: 'up{job=~".*gateway.*"}' },
        });
        expect(up.ok()).toBeTruthy();
        const upBody = await up.json();
        expect(upBody.status).toBe('success');
        expect(upBody.data.result.length, 'Prometheus 里没有 gateway 目标').toBeGreaterThan(0);
        for (const r of upBody.data.result) {
            expect(r.value[1], `target ${r.metric.instance} 不是 up`).toBe('1');
        }

        // 2) 本轮新增的可靠性指标必须真实存在。
        //    指标名写错时 Prometheus 会返回空结果而不是报错，
        //    所以这里断言 result 非空 —— 否则这条用例毫无意义。
        for (const metric of [
            'mc_outbox_backlog',
            'mc_lock_acquire_total',
            'mc_idempotency_outcome_total',
            'mc_http_requests_total',
        ]) {
            const res = await request.get(`${PROM}/api/v1/query`, { params: { query: metric } });
            const body = await res.json();
            expect(body.data.result.length, `指标 ${metric} 在 Prometheus 里查不到`).toBeGreaterThan(0);
        }
    });

    test('Prometheus 的告警规则全部加载成功', async ({ request }) => {
        test.skip(!(await probe(`${PROM}/-/healthy`)), `Prometheus 未启动（${PROM}），跳过`);

        const res = await request.get(`${PROM}/api/v1/rules`);
        const body = await res.json();
        const rules = body.data.groups.flatMap((g: { rules: unknown[] }) => g.rules);

        // 本地必须加载规则文件。历史上这里以「规则依赖 node-exporter」为由跳过，
        // 结果是 14 条告警从未被验证过，其中 2 条因为埋点缺失永久不会触发。
        // 现在 prometheus.local.yml 已挂载 alerts.yml，所以 0 条规则就是真故障。
        expect(rules.length, '告警规则一条都没加载，检查 prometheus.local.yml 的 rule_files').toBeGreaterThan(0);

        // 规则文件写错时 Prometheus 会整体拒绝加载，
        // 所以「数量对得上」本身就是一次语法校验。
        const bad = rules.filter((r: { health?: string }) => r.health && r.health !== 'ok');
        expect(bad, `有规则加载异常：${JSON.stringify(bad)}`).toHaveLength(0);
    });

    /**
     * 告警规则引用的指标必须真实存在于代码里。
     *
     * 这是监控里最隐蔽的一类失效：规则引用了不存在的指标名，
     * Prometheus 不会报错，规则状态照样是 ok（no data 也算 ok），
     * 于是这条告警永远不会触发，也永远不会有人发现。
     *
     * 为什么不能只查 /metrics：CounterVec / HistogramVec 在出现第一个标签值
     * 之前**完全不导出**。所以「/metrics 里没有」可能是两种情况：
     *   a) 定义了但本地还没触发过 —— 正常；
     *   b) 压根没定义或拼错了名字 —— 缺陷。
     * 只有读源码才能区分。这里直接扫 observ 包里的指标声明。
     */
    test('告警规则引用的每个指标都在代码里有定义', async ({ request }) => {
        test.skip(!(await probe(`${PROM}/-/healthy`)), `Prometheus 未启动（${PROM}），跳过`);

        const rulesRes = await request.get(`${PROM}/api/v1/rules`);
        const rulesBody = await rulesRes.json();
        const exprs: string[] = rulesBody.data.groups
            .flatMap((g: { rules: { name: string; query?: string }[] }) => g.rules)
            .map((r: { name: string; query?: string }) => r.query || '')
            .filter(Boolean);
        test.skip(exprs.length === 0, '没有告警规则可校验');

        // 收集规则引用的指标名
        const referenced = new Set<string>();
        for (const expr of exprs) {
            for (const m of expr.match(/\bmc_[a-z0-9_]+/g) || []) {
                referenced.add(m.replace(/_(bucket|sum|count)$/, ''));
            }
        }
        expect(referenced.size, '告警规则里没有解析出任何 mc_ 指标').toBeGreaterThan(0);

        // 源码侧：把 observ 包里所有指标拼成完整名（namespace_subsystem_name）
        // Playwright 以 ESM 运行，没有 __dirname，用 import.meta.url 反推。
        const here = path.dirname(fileURLToPath(import.meta.url));
        const obsPath = path.resolve(here, '../../server/internal/observ/observ.go');
        const src = fs.readFileSync(obsPath, 'utf8');
        const defined = new Set<string>();
        const blockRe = /Namespace:\s*"(mc)"[\s\S]{0,160}?Subsystem:\s*"([a-z_]+)"[\s\S]{0,160}?Name:\s*"([a-z_]+)"/g;
        for (const m of src.matchAll(blockRe)) {
            defined.add(`${m[1]}_${m[2]}_${m[3]}`);
        }
        expect(defined.size, '没能从 observ.go 解析出任何指标定义').toBeGreaterThan(0);

        const missing = [...referenced].filter((m) => !defined.has(m));
        expect(
            missing,
            `以下指标被告警规则引用但代码里没有定义（这些告警会永久 no data）：${missing.join(', ')}`,
        ).toHaveLength(0);
    });

    test('Grafana 总览面板能打开并渲染出面板', async ({ page }) => {
        test.skip(!(await probe(`${GRAFANA}/api/health`)), `Grafana 未启动（${GRAFANA}），跳过`);

        await page.goto(`${GRAFANA}/d/mc-console-overview`);
        await expect(page.getByText('会员运营台 · 总览').first()).toBeVisible({ timeout: 20_000 });

        // 面板容器渲染出来就说明 provisioned dashboard 生效了；
        // 具体数值依赖业务流量，不适合在 E2E 里断言。
        await expect(page.locator('[data-panelid]').first()).toBeVisible({ timeout: 20_000 });
        const panels = await page.locator('[data-panelid]').count();
        expect(panels, '面板数量少于预期，可能 provisioning 没加载').toBeGreaterThan(10);

        // 本轮新增的「消息与一致性」这一行
        await expect(page.getByText('消息与一致性').first()).toBeVisible();
    });

    test('网关 /metrics 不对外暴露内部路由明细以外的东西', async ({ request }) => {
        const res = await request.get(`${GW}/metrics`);
        test.skip(!res.ok(), `网关未启动（${GW}），跳过`);

        const text = await res.text();
        // route label 必须是模板化的，出现真实 ID 就说明有人写了 c.FullPath 以外的值
        expect(text).not.toMatch(/route="\/api\/[a-z-]+\/\d+/);
        // 凭据类字样不该出现在指标里
        expect(text.toLowerCase()).not.toContain('password');
        expect(text.toLowerCase()).not.toContain('token_secret');
    });
});
