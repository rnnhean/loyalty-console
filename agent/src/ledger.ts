/**
 * 事实层。
 *
 * 只追加，不修改、不删除。模型上下文里的工具结果会被截断，
 * 被截掉的那部分如果恰好是它的依据，它引用起来毫无障碍——
 * 所以校验时必须对一个**服务端维护、模型改不动**的集合比对。
 *
 * 不做摘要压缩。id 很短，全量注入成本可忽略；注入摘要，模型会拿摘要去编。
 */

import type { CheckResult, Finding } from '../../shared/contract.js';

export interface Fact {
    /** `${check}/${finding.id}`，跨规则唯一 */
    id: string;
    check: string;
    round: number;
    finding: Finding;
}

export interface CheckRecord {
    check: string;
    paramsKey: string;
    round: number;
    result: CheckResult;
}

export function paramKey(params: Record<string, string> | undefined): string {
    if (!params) return '';
    return Object.keys(params)
        .sort()
        .map((k) => `${k}=${params[k]}`)
        .join('&');
}

export class Ledger {
    private readonly facts: Fact[] = [];
    private readonly records: CheckRecord[] = [];
    private readonly seen = new Set<string>();

    /** 记一次规则结果，返回本次新增的 finding 条数（用于判断「没有新证据」）。 */
    append(result: CheckResult, round: number, params?: Record<string, string>): number {
        const key = `${result.check}|${paramKey(params)}`;
        this.seen.add(key);
        this.records.push({ check: result.check, paramsKey: paramKey(params), round, result });

        let added = 0;
        for (const f of result.findings ?? []) {
            const id = `${result.check}/${f.id}`;
            // 同一条 finding 重复出现时不重复记账，但记账次数不影响它是真的
            if (this.facts.some((x) => x.id === id)) continue;
            this.facts.push({ id, check: result.check, round, finding: { ...f, id } });
            added++;
        }
        return added;
    }

    hasChecked(check: string, params?: Record<string, string>): boolean {
        return this.seen.has(`${check}|${paramKey(params)}`);
    }

    get all(): readonly Fact[] {
        return this.facts;
    }

    get checks(): string[] {
        const out: string[] = [];
        for (const r of this.records) if (!out.includes(r.check)) out.push(r.check);
        return out;
    }

    get recordsList(): readonly CheckRecord[] {
        return this.records;
    }

    findingIds(): string[] {
        return this.facts.map((f) => f.id);
    }

    /** 注入提示词的白名单事实：id + 一句话，不含 evidence 原文。 */
    snapshot(): string {
        if (this.facts.length === 0) return '（本轮还没有产生任何 finding）';
        return this.facts
            .map((f) => `- ${f.id}｜${f.check}｜${f.finding.severity}｜${f.finding.title}`)
            .join('\n');
    }

    /**
     * 给 advice 用的「唯一匹配」：一条建议指向的证据必须有且只有一条对得上。
     * 0 条是编造，≥2 条说明引用粒度不够——两种都不接受。
     */
    matchEvidence(keyword: string): Fact[] {
        const k = keyword.trim();
        if (!k) return [];
        return this.facts.filter(
            (f) => f.id === k || f.finding.title.includes(k) || f.id.endsWith(`/${k}`),
        );
    }

    /** 服务端聚合的置信度：取所有规则结果里最低的那个，因数据不全时规则会主动降低。 */
    confidence(): number {
        if (this.records.length === 0) return 0.5;
        return Math.min(...this.records.map((r) => r.result.confidence ?? 1));
    }

    /** 有没有任何一条规则是 inconclusive——报告里必须显式说「没查全」。 */
    inconclusiveChecks(): string[] {
        return this.records.filter((r) => r.result.verdict === 'inconclusive').map((r) => r.check);
    }

    failedChecks(): string[] {
        return this.records.filter((r) => r.result.error).map((r) => r.check);
    }
}
