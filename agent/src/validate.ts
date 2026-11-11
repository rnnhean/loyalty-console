/**
 * 模型输出校验。
 *
 * 这是整份设计里最不能省的一层：模型的输出不被信任，
 * 只有和工具结果对得上的部分才被接受。
 *
 * 校验分两类：
 *   - 可修复（剔除未知引用、丢弃越界建议）→ 直接修，记进 issues
 *   - 致命（诊断型报告一条证据都没有）→ 交给 loop 回灌重试一次
 */

import type { Advice, Report } from '../../shared/contract.js';
import { Ledger } from './ledger.js';
import { ENTITIES } from './tools.js';

export type ValidationIssueKind =
    | 'unknown_finding'
    | 'empty_citation'
    | 'bad_advice_ref'
    | 'ambiguous_evidence'
    | 'number_not_in_evidence'
    | 'bad_shape';

export interface ValidationIssue {
    kind: ValidationIssueKind;
    detail: string;
}

export interface ValidationResult {
    report: Report;
    issues: ValidationIssue[];
    /** 只有这一类问题值得回灌重试 */
    fatal: boolean;
}

/** 建议里允许出现的页面路径：来自实体表，不允许模型自己拼。 */
const PAGE_WHITELIST = new Set(Object.values(ENTITIES).map((e) => e.path.replace(/^\/api/, '')));

export function validateReport(raw: unknown, ledger: Ledger): ValidationResult {
    const issues: ValidationIssue[] = [];

    if (!raw || typeof raw !== 'object') {
        return {
            report: emptyReport('模型没有产出结构化结果'),
            issues: [{ kind: 'bad_shape', detail: '报告不是对象' }],
            fatal: true,
        };
    }

    const obj = raw as Record<string, unknown>;
    const kind = obj.kind;
    if (kind !== 'diagnosis' && kind !== 'query' && kind !== 'out_of_scope') {
        return {
            report: emptyReport('报告的 kind 不是三类之一'),
            issues: [{ kind: 'bad_shape', detail: `未知 kind: ${String(kind)}` }],
            fatal: true,
        };
    }

    const knownIds = new Set(ledger.findingIds());
    const claimed = Array.isArray(obj.finding_ids) ? obj.finding_ids.map(String) : [];
    const findingIds = claimed.filter((id) => knownIds.has(id));
    for (const id of claimed) {
        if (!knownIds.has(id)) {
            issues.push({ kind: 'unknown_finding', detail: `引用了不存在的证据 ${id}` });
        }
    }

    const advice = sanitizeAdvice(obj.advice, ledger, issues);

    const phenomenon = str(obj.phenomenon);
    const judgement = str(obj.judgement);

    // 现象描述里的数字必须能在工具结果里找到。只查 2 位以上的数字，
    // 避免「1 条」「3 个」这类量词把正常报告判死。
    const evidenceText = JSON.stringify(ledger.all.map((f) => f.finding.evidence));
    for (const n of phenomenon.match(/\d{2,}/g) ?? []) {
        if (!evidenceText.includes(n)) {
            issues.push({ kind: 'number_not_in_evidence', detail: `数字 ${n} 在证据里找不到` });
        }
    }

    const fatal =
        (kind === 'diagnosis' && findingIds.length === 0) ||
        issues.some((i) => i.kind === 'bad_shape');

    return {
        report: {
            kind,
            title: str(obj.title) || '未命名结论',
            phenomenon,
            judgement,
            advice,
            finding_ids: findingIds,
            // 置信度以服务端聚合为准，模型写的那个值直接覆盖
            confidence: ledger.confidence(),
            checks: ledger.checks,
        },
        issues,
        fatal,
    };
}

function sanitizeAdvice(
    raw: unknown,
    ledger: Ledger,
    issues: ValidationIssue[],
): Advice[] {
    if (!Array.isArray(raw)) return [];
    const out: Advice[] = [];

    for (const item of raw) {
        if (!item || typeof item !== 'object') continue;
        const a = item as Record<string, unknown>;
        const text = str(a.text);
        if (!text) continue;

        const refObj = a.ref as Record<string, unknown> | undefined;
        if (refObj && typeof refObj.path === 'string') {
            const path = refObj.path;
            if (!PAGE_WHITELIST.has(path)) {
                issues.push({ kind: 'bad_advice_ref', detail: `建议指向了白名单外的路径 ${path}` });
                continue;
            }
        }

        // 设计 §6.8：advice 指向的证据必须唯一命中
        if (typeof a.evidence_ref === 'string' && a.evidence_ref) {
            const hits = ledger.matchEvidence(a.evidence_ref);
            if (hits.length !== 1) {
                issues.push({
                    kind: hits.length === 0 ? 'unknown_finding' : 'ambiguous_evidence',
                    detail: `建议引用的证据 ${a.evidence_ref} 命中 ${hits.length} 条`,
                });
                continue;
            }
        }

        out.push({
            text,
            ...(refObj && typeof refObj.path === 'string'
                ? {
                      ref: {
                          path: refObj.path,
                          query: isStringMap(refObj.query) ? refObj.query : undefined,
                      },
                  }
                : {}),
        });
    }

    return out;
}

function isStringMap(v: unknown): v is Record<string, string> {
    if (!v || typeof v !== 'object' || Array.isArray(v)) return false;
    return Object.values(v as Record<string, unknown>).every((x) => typeof x === 'string');
}

function str(v: unknown): string {
    return typeof v === 'string' ? v.trim() : '';
}

function emptyReport(reason: string): Report {
    return {
        kind: 'diagnosis',
        title: '无法给出结论',
        phenomenon: reason,
        judgement: '这次没能得到可核验的结论。',
        advice: [],
        finding_ids: [],
        confidence: 0,
    };
}
