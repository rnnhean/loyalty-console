/**
 * 调用留痕。
 *
 * 一行一条 JSON，字段命名对齐 OTel 的 gen_ai.* 约定，所以以后换成真正的
 * OTel exporter 时不需要改埋点位置。
 *
 * 落盘什么、不落什么：
 *   - 落：trace_id、用户哈希、规则集合、轮数、停止原因、金额、token、耗时
 *   - 不落：用户问题原文、模型输出原文、任何 token / key
 *
 * 问题原文不落盘是有意的：运营会问「XX 产品为什么没权益」这种带业务名的
 * 问题，日志文件一旦被拿去排查别的问题就顺手把业务信息带出去了。
 */

import { createHash } from 'node:crypto';
import { appendFileSync } from 'node:fs';

import type { StopReason, ErrorReason } from '../../shared/contract.js';

export interface TraceRecord {
    ts: string;
    trace_id: string;
    /** 用户 id 的哈希前缀，用于「同一个人今天问了几次」，不可反查 */
    user_hash: string;
    /** 问题长度，不存原文 */
    question_len: number;
    gen_ai_model: string;
    checks: string[];
    turns: number;
    stop_reason: StopReason;
    error_reason?: ErrorReason;
    cost_cny: number;
    tokens: { input: number; output: number; cache_read: number };
    latency_ms: number;
    tool_failures: number;
    /** 校验中被剔除的条数——这个数上去了说明模型在编 */
    rejected_refs: number;
}

export interface TracerOptions {
    /** 空字符串 = 只打一行摘要到 stderr，不落盘 */
    file: string;
    now?: () => number;
}

export class Tracer {
    private readonly now: () => number;

    constructor(private readonly opts: TracerOptions) {
        this.now = opts.now ?? Date.now;
    }

    write(rec: Omit<TraceRecord, 'ts'>): void {
        const line = JSON.stringify({ ts: new Date(this.now()).toISOString(), ...rec });

        if (!this.opts.file) {
            // 没配文件时只留一句摘要，够定位「刚才那次大概花了多少」
            process.stderr.write(
                `[trace] ${rec.trace_id} checks=${rec.checks.length} turns=${rec.turns} ` +
                    `stop=${rec.stop_reason} cost=${rec.cost_cny.toFixed(4)}元 ` +
                    `latency=${rec.latency_ms}ms\n`,
            );
            return;
        }

        try {
            appendFileSync(this.opts.file, line + '\n', 'utf8');
        } catch (err) {
            // 留痕失败不能影响用户拿到结果
            process.stderr.write(`[trace] 写入失败: ${(err as Error).message}\n`);
        }
    }
}

export function hashUser(userId: string): string {
    if (!userId) return 'anon';
    return createHash('sha256').update(userId).digest('hex').slice(0, 12);
}

/** trace_id 只用时间戳 + 随机数，不引入 uuid 依赖（Node 18 才有 randomUUID）。 */
export function newTraceId(now: () => number = Date.now): string {
    const rand = Math.random().toString(36).slice(2, 10);
    return `${now().toString(36)}-${rand}`;
}
