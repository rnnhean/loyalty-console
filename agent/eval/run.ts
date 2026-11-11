/**
 * 评测脚本。
 *
 * 它回答的问题不是「模型好不好」，而是「这套装配在真实模型下还能不能守住契约」：
 * 选没选对规则、有没有引用真证据、越界问题有没有拒答、被注入时有没有漏东西。
 *
 * 运行前提（缺一不可）：
 *   - 后端已启动，且 8 条诊断规则可用
 *   - AGENT_BASE_URL / AGENT_API_KEY / AGENT_MODEL / AGENT_PRICE_* 已配置
 *   - EVAL_TOKEN 是一个有效的用户 JWT（本脚本只透传，不打印、不落盘）
 *
 * 用法：
 *   node --import tsx eval/run.ts --dry          # 只校验用例文件，不联网
 *   node --import tsx eval/run.ts                # 全量跑
 *   node --import tsx eval/run.ts --only=a,b     # 只跑指定 id
 *
 * 退出码：有失败 = 1。可以直接挂到 CI 上。
 */

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import type { ReportKind } from '../../shared/contract.js';
import { loadConfig } from '../src/config.js';
import { runAgent, type LoopLimits, type RunOutcome } from '../src/loop.js';
import { createHttpProvider } from '../src/provider.js';
import { createToolExecutor, type ToolExecutor, type ToolRequest } from '../src/tools.js';

interface Expect {
    kind_any?: ReportKind[];
    checks_any?: string[];
    checks_all?: string[];
    must_cite?: boolean;
    tools_any?: string[];
    /** 正则，命中即失败 */
    forbid_text?: string[];
    max_turns?: number;
}

interface EvalCase {
    id: string;
    question: string;
    context?: { product?: string; route?: string };
    expect: Expect;
}

const here = dirname(fileURLToPath(import.meta.url));
const args = process.argv.slice(2);
const dry = args.includes('--dry');
const onlyArg = args.find((a) => a.startsWith('--only='));
const only = onlyArg ? new Set(onlyArg.slice('--only='.length).split(',').filter(Boolean)) : null;

const cases = loadCases();

if (dry) {
    const problems = checkCases(cases);
    if (problems.length > 0) {
        for (const p of problems) process.stderr.write(`✗ ${p}\n`);
        process.exit(1);
    }
    process.stdout.write(`✓ ${cases.length} 条用例格式正确\n`);
    process.exit(0);
}

const token = process.env.EVAL_TOKEN ?? '';
if (!token) {
    process.stderr.write('缺少 EVAL_TOKEN（有效用户 JWT）。脚本不会打印或保存它。\n');
    process.exit(2);
}

const cfg = loadConfig();
const selected = only ? cases.filter((c) => only.has(c.id)) : cases;

if (selected.length === 0) {
    process.stderr.write('没有匹配的用例 id\n');
    process.exit(2);
}

const provider = createHttpProvider({
    baseUrl: cfg.baseUrl,
    apiKey: cfg.apiKey,
    model: cfg.model,
    fallbackModel: cfg.fallbackModel,
});

const limits: LoopLimits = {
    maxTurns: cfg.maxTurns,
    totalTimeoutMs: cfg.totalTimeoutMs,
    toolTimeoutMs: cfg.toolTimeoutMs,
    maxParallelTools: 4,
    price: cfg.price,
};

interface Row {
    id: string;
    ok: boolean;
    fails: string[];
    kind: string;
    checks: string[];
    turns: number;
    cost: number;
    latency: number;
}

const rows: Row[] = [];

for (const c of selected) {
    const started = Date.now();
    const tools = new Set<string>();
    const errors: string[] = [];

    const executor = recording(createToolExecutor({ backendUrl: cfg.backendUrl, token }), tools);

    let out: RunOutcome;
    try {
        out = await runAgent(c.question, c.context ?? {}, {
            provider,
            executor,
            checks: [],
            limits,
            traceId: `eval-${c.id}`,
            onEvent: (e) => {
                if (e.type === 'error') errors.push(`${e.reason}: ${e.message}`);
            },
        });
    } catch (err) {
        rows.push({
            id: c.id,
            ok: false,
            fails: [`抛异常：${(err as Error).message}`],
            kind: '-',
            checks: [],
            turns: 0,
            cost: 0,
            latency: Date.now() - started,
        });
        continue;
    }

    const fails = score(c, out, tools);
    if (errors.length > 0) fails.push(...errors);

    rows.push({
        id: c.id,
        ok: fails.length === 0,
        fails,
        kind: out.report.kind,
        checks: out.ledger.checks,
        turns: out.turns,
        cost: out.costCny,
        latency: Date.now() - started,
    });
}

// ── 输出 ────────────────────────────────────────────────────

let failed = 0;
for (const r of rows) {
    const mark = r.ok ? 'PASS' : 'FAIL';
    if (!r.ok) failed++;
    process.stdout.write(
        `${mark}  ${r.id.padEnd(28)} kind=${r.kind.padEnd(12)} ` +
            `turns=${r.turns} cost=${r.cost.toFixed(4)}元 ${r.latency}ms ` +
            `checks=[${r.checks.join(',')}]\n`,
    );
    for (const f of r.fails) process.stdout.write(`      └─ ${f}\n`);
}

const total = rows.length;
const avg = (pick: (r: Row) => number): string =>
    total === 0 ? '-' : (rows.reduce((s, r) => s + pick(r), 0) / total).toFixed(4);

process.stdout.write(
    `\n合计 ${total} 条，通过 ${total - failed}，失败 ${failed}；` +
        `平均 ${avg((r) => r.turns)} 轮 / ${avg((r) => r.cost)} 元 / ${avg((r) => r.latency)}ms\n`,
);

process.exit(failed > 0 ? 1 : 0);

// ── 评分 ────────────────────────────────────────────────────

function score(c: EvalCase, out: RunOutcome, tools: Set<string>): string[] {
    const e = c.expect;
    const fails: string[] = [];
    const report = out.report;

    if (e.kind_any && !e.kind_any.includes(report.kind)) {
        fails.push(`kind=${report.kind}，期望 ${e.kind_any.join('/')}`);
    }

    if (e.checks_any && !e.checks_any.some((n) => out.ledger.checks.includes(n))) {
        fails.push(`没有跑期望的规则之一 [${e.checks_any.join(',')}]，实际 [${out.ledger.checks.join(',')}]`);
    }

    if (e.checks_all) {
        const missing = e.checks_all.filter((n) => !out.ledger.checks.includes(n));
        if (missing.length > 0) fails.push(`缺少规则 [${missing.join(',')}]`);
    }

    if (e.must_cite && report.finding_ids.length === 0) {
        fails.push('诊断报告没有引用任何 finding');
    }

    if (e.tools_any && !e.tools_any.some((n) => tools.has(n))) {
        fails.push(`没有调用期望的工具之一 [${e.tools_any.join(',')}]，实际 [${[...tools].join(',')}]`);
    }

    if (e.max_turns !== undefined && out.turns > e.max_turns) {
        fails.push(`用了 ${out.turns} 轮，超过上限 ${e.max_turns}`);
    }

    const text = [report.title, report.phenomenon, report.judgement, ...report.advice.map((a) => a.text)].join(
        '\n',
    );
    for (const pattern of e.forbid_text ?? []) {
        if (new RegExp(pattern, 'i').test(text)) {
            fails.push(`输出命中禁用模式 /${pattern}/`);
        }
    }

    return fails;
}

// ── 加载与自检 ──────────────────────────────────────────────

function loadCases(): EvalCase[] {
    const raw = readFileSync(join(here, 'cases.json'), 'utf8');
    const parsed = JSON.parse(raw) as unknown;
    if (!Array.isArray(parsed)) throw new Error('cases.json 必须是数组');
    return parsed as EvalCase[];
}

function checkCases(list: EvalCase[]): string[] {
    const problems: string[] = [];
    const ids = new Set<string>();

    for (const [i, c] of list.entries()) {
        const at = `第 ${i + 1} 条`;
        if (!c || typeof c !== 'object') {
            problems.push(`${at} 不是对象`);
            continue;
        }
        if (!c.id) problems.push(`${at} 缺少 id`);
        else if (ids.has(c.id)) problems.push(`id 重复：${c.id}`);
        else ids.add(c.id);

        if (!c.question || c.question.length > 500) {
            problems.push(`${c.id ?? at} 的 question 为空或超过 500 字`);
        }
        if (!c.expect || typeof c.expect !== 'object') {
            problems.push(`${c.id ?? at} 缺少 expect`);
        }
    }

    return problems;
}

// ── 工具调用记录 ────────────────────────────────────────────

function recording(inner: ToolExecutor, seen: Set<string>): ToolExecutor {
    return {
        execute(req: ToolRequest) {
            seen.add(req.name);
            return inner.execute(req);
        },
    };
}
