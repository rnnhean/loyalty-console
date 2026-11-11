/**
 * 测试替身。
 *
 * 不放在 *.test.ts 里：这些工具被多个测试文件共用，放进去会被
 * `--test` 当测试文件执行一遍（副作用是没有任何测试）。
 */

import type { AgentEvent, CheckResult, Finding } from '../../shared/contract.js';
import type { LoopLimits } from './loop.js';
import type { ToolExecutor, ToolRequest, ToolResult } from './tools.js';

export const ZERO_PRICE = { input: 0, output: 0, cacheRead: 0 };

export function baseLimits(over: Partial<LoopLimits> = {}): LoopLimits {
    return {
        maxTurns: 6,
        totalTimeoutMs: 30_000,
        toolTimeoutMs: 5_000,
        maxParallelTools: 4,
        price: ZERO_PRICE,
        ...over,
    };
}

export function finding(id: string, over: Partial<Finding> = {}): Finding {
    return {
        id,
        severity: 'mid',
        title: `问题 ${id}`,
        detail: `证据细节 ${id}`,
        evidence: { id, amount: 100 },
        link: '/benefit/rules',
        ...over,
    };
}

export function result(
    check: string,
    over: Partial<CheckResult> = {},
): CheckResult {
    return {
        check,
        verdict: 'ok',
        confidence: 1,
        findings: [],
        coverage: { checked: 10, skipped: 0, missing: null },
        ...over,
    };
}

export interface FakeExecutorOptions {
    /** check 名 → 结果 */
    checks?: Record<string, CheckResult>;
    /** check 名 → 失败短标签 */
    failures?: Record<string, string>;
}

export interface FakeExecutor extends ToolExecutor {
    readonly calls: ToolRequest[];
}

export function fakeExecutor(opts: FakeExecutorOptions = {}): FakeExecutor {
    const calls: ToolRequest[] = [];

    function one(name: string): ToolResult {
        const failed = opts.failures?.[name];
        if (failed) return { ok: false, reason: failed, message: `后端 ${failed}` };
        return { ok: true, data: opts.checks?.[name] ?? result(name) };
    }

    return {
        calls,
        async execute(req: ToolRequest): Promise<ToolResult> {
            calls.push(req);

            if (req.name === 'run_check') {
                const name = (req.args as { name?: string } | undefined)?.name;
                if (!name) return { ok: false, reason: 'invalid_param', message: '缺少 name' };
                return one(name);
            }

            if (req.name === 'run_checks') {
                const names = (req.args as { checks?: string[] } | undefined)?.checks ?? [];
                return { ok: true, data: { results: names.map((n) => one(n).ok ? (opts.checks?.[n] ?? result(n)) : { ...result(n), verdict: '', error: opts.failures?.[n] }) } };
            }

            return { ok: false, reason: 'unknown_tool', message: `没有这个工具：${req.name}` };
        },
    };
}

export function collectEvents(): { events: AgentEvent[]; onEvent: (e: AgentEvent) => void } {
    const events: AgentEvent[] = [];
    return { events, onEvent: (e) => events.push(e) };
}
