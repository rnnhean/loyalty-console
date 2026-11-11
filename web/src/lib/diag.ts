/**
 * 诊断相关的类型与请求封装。
 *
 * 这一层只做两件事：把后端契约翻译成 TS 类型，以及把「一次体检」拆成
 * 可被页面直接消费的形态。判定逻辑一条都不在这里——它在 Go 里。
 */

import { http } from '@/lib/http';

// ── 后端契约 ───────────────────────────────────────────────

export type Verdict = 'ok' | 'issue' | 'inconclusive';
export type Severity = 'high' | 'mid' | 'low';

export interface Finding {
    id: string;
    severity: Severity;
    title: string;
    detail: string;
    evidence: Record<string, unknown>;
    /** 能直接落到具体行的链接；空串表示这条没有对应页面 */
    link: string;
}

export interface Coverage {
    checked: number;
    skipped: number;
    /** 非空表示「这次没查全」，必须展示给用户看 */
    missing: string[] | null;
}

export interface RunResult {
    check: string;
    /** 空串 = 没跑成（配合 error 字段） */
    verdict: Verdict | '';
    confidence: number;
    findings: Finding[];
    coverage: Coverage;
    error?: string;
}

export interface CheckInfo {
    name: string;
    title: string;
    domain: string;
    cost_hint: 'low' | 'mid' | 'high';
    timeout_ms: number;
    params?: Record<string, { type: string; required: boolean }>;
}

interface ChecksPayload {
    code: number;
    total: number;
    items: CheckInfo[];
}

interface BatchPayload {
    code: number;
    data: {
        partial: boolean;
        results: RunResult[];
    };
}

// ── 接口调用 ───────────────────────────────────────────────

/** 规则目录。页面用它补上规则的标题——结果里只有 name。 */
export function fetchChecks(): Promise<CheckInfo[]> {
    return http.get<ChecksPayload>('/api/diag/checks').then((r) => r.items ?? []);
}

/**
 * 跑一次体检。
 *
 * 只传无参数规则：带参数的（benefit_chain / change_impact）在体检场景下
 * 拿不到 product / toggle_key，跑出来必然是 inconclusive——那只会让报告里
 * 多两条「查不了」，不如交给用户在对话框里按需触发。
 */
export function runAudit(checkNames: string[]): Promise<BatchPayload['data']> {
    return http.post<BatchPayload>('/api/diag/batch', { checks: checkNames }).then((r) => r.data);
}

// ── 展示用的派生计算 ───────────────────────────────────────

/** 体检页只跑这些：其余规则需要参数，进了体检只会是噪音。 */
export const AUDIT_CHECKS = [
    'orphan_rules',
    'voucher_health',
    'slot_bindings',
    'pricing_visibility',
    'test_data_scan',
] as const;

export interface AuditSummary {
    total: number;
    ok: number;
    issue: number;
    inconclusive: number;
    failed: number;
    findings: Finding[];
    /** 所有「没查全」的原因，去重后展示 */
    gaps: string[];
}

/**
 * 把批量结果压成一份摘要。
 *
 * 注意 ok/issue/inconclusive/failed 四类之和必须等于 total——
 * 任何一个结果都必须落进某一类，不能有「不知道算哪类」的灰色地带，
 * 否则页面上的数字加起来对不上，用户第一时间就会不信这份报告。
 */
export function summarize(results: RunResult[]): AuditSummary {
    const s: AuditSummary = {
        total: results.length,
        ok: 0,
        issue: 0,
        inconclusive: 0,
        failed: 0,
        findings: [],
        gaps: [],
    };

    const gapSet = new Set<string>();

    for (const r of results) {
        if (r.error) {
            s.failed += 1;
            gapSet.add(`「${r.check}」没能执行（${r.error}）`);
            continue;
        }
        if (r.verdict === 'ok') s.ok += 1;
        else if (r.verdict === 'issue') s.issue += 1;
        else if (r.verdict === 'inconclusive') s.inconclusive += 1;
        else s.failed += 1;

        for (const f of r.findings ?? []) {
            s.findings.push(f);
        }
        for (const m of r.coverage?.missing ?? []) {
            if (m) gapSet.add(m);
        }
    }

    s.gaps = Array.from(gapSet);

    // 高危在前：运营第一眼该看到的是「必须处理」的那几条。
    const order: Record<Severity, number> = { high: 0, mid: 1, low: 2 };
    s.findings.sort((a, b) => (order[a.severity] ?? 9) - (order[b.severity] ?? 9));

    return s;
}
