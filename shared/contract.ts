/**
 * 前后端共享的契约：诊断结果、报告、SSE 事件。
 *
 * 这里**只有类型，没有运行时值**。web 侧用 `import type` 引用，
 * 构建时被完全擦除，因此不需要在 vite 里配运行时别名；
 * agent 侧用相对路径 `../shared/contract.js` 引用。
 *
 * 改这个文件要同时想清楚 web/src/lib/agent.ts 的解析器：
 * `AgentEvent` 是判别联合，前端按 `type` 分支，加一种类型就得加一个分支。
 */

// ── 后端诊断结果（与 server/internal/diag 一一对应） ──────────

/** 三值枚举。没有第四种「不知道」——查不到就是 inconclusive，且必须说明缺了什么。 */
export type Verdict = 'ok' | 'issue' | 'inconclusive';

export type Severity = 'high' | 'mid' | 'low';

export interface Finding {
    /** 规则内唯一，例如 `rule:101`。跨规则时前面要加 check 名，见 ledger。 */
    id: string;
    severity: Severity;
    title: string;
    detail: string;
    /** 判据用到的原始字段。只放能支撑结论的最小集合，不透传整行。 */
    evidence: Record<string, unknown>;
    /** 能直接落到具体行的前端路由，由规则给出，模型不许生成。 */
    link: string;
}

export interface Coverage {
    /** 实际扫过的行数，不是估算值。 */
    checked: number;
    skipped: number;
    /** 没查到的部分。`inconclusive` 时这个数组必须非空。 */
    missing: string[] | null;
}

export interface CheckResult {
    check: string;
    /** 空字符串 = 这条没跑成（见 error），不能当 ok 渲染。 */
    verdict: Verdict | '';
    confidence: number;
    findings: Finding[];
    coverage: Coverage | null;
    /** 机器可读短标签：timeout / db_error / unknown_check。不塞堆栈。 */
    error?: string;
}

// ── 报告（模型唯一被允许输出的结构） ─────────────────────────

export type ReportKind = 'diagnosis' | 'query' | 'out_of_scope';

export interface AdviceRef {
    path: string;
    query?: Record<string, string>;
}

export interface Advice {
    text: string;
    ref?: AdviceRef;
}

export interface Report {
    kind: ReportKind;
    title: string;
    phenomenon: string;
    judgement: string;
    advice: Advice[];
    /** 必须 ⊆ 本次运行真实产生的 finding 集合。 */
    finding_ids: string[];
    /** 0~1，由服务端聚合覆盖，模型没有话语权。 */
    confidence: number;
    /** 本次实际跑过的 check，用于线上质量监控与反馈归因。 */
    checks?: string[];
}

// ── 循环与事件 ───────────────────────────────────────────────

export type StopReason =
    | 'completed'
    | 'max_turns'
    | 'total_timeout'
    | 'context_limit'
    | 'no_new_evidence'
    | 'budget'
    | 'repeated_violation'
    | 'aborted_by_client'
    | 'degraded';

export type ErrorReason =
    | 'network'
    | 'timeout'
    | 'budget'
    | 'server'
    | 'unavailable'
    | 'contract'
    | 'aborted';

export interface StepEvent {
    type: 'step';
    index: number;
    label: string;
    status: 'running' | 'done' | 'failed';
}

export interface NoticeEvent {
    type: 'notice';
    level: 'info' | 'warn';
    text: string;
}

export interface FinalEvent {
    type: 'final';
    report: Report;
    stop_reason: StopReason;
    trace_id: string;
    /** 本次诊断的步骤条数与命中 check，前端做反馈归因时要用。 */
    checks: string[];
}

export interface ErrorEvent {
    type: 'error';
    reason: ErrorReason;
    message: string;
}

/**
 * SSE 事件的唯一来源。前端按 `type` 做穷举分支：
 * 出现未声明的 type 时，解析器产出 `error/contract`，不 throw——
 * 抛了会让整个流停住，用户连后面的错误提示都看不到。
 */
export type AgentEvent = StepEvent | NoticeEvent | FinalEvent | ErrorEvent;
