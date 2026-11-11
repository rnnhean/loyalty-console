import { describe, expect, it } from 'vitest';

import { parseSSEChunk } from './agent';

function frame(payload: unknown): string {
    return `data: ${JSON.stringify(payload)}\n\n`;
}

describe('parseSSEChunk', () => {
    it('解析一个完整的 step 帧', () => {
        const { events, rest } = parseSSEChunk(
            frame({ type: 'step', index: 0, label: '检查「orphan_rules」', status: 'running' }),
        );

        expect(rest).toBe('');
        expect(events).toEqual([
            { type: 'step', index: 0, label: '检查「orphan_rules」', status: 'running' },
        ]);
    });

    it('半截帧留在 rest 里，补全后能解析出来', () => {
        const first = parseSSEChunk('data: {"type":"st');
        expect(first.events).toEqual([]);
        expect(first.rest).toBe('data: {"type":"st');

        const second = parseSSEChunk(`${first.rest}ep","index":1,"label":"x","status":"done"}\n\n`);
        expect(second.events).toHaveLength(1);
        expect(second.events[0]?.type).toBe('step');
    });

    it('忽略心跳注释，不产生事件', () => {
        const { events } = parseSSEChunk(': ping\n\n: connected\n\n');
        expect(events).toEqual([]);
    });

    it('多行 data 按换行拼成一个 JSON', () => {
        const { events } = parseSSEChunk('data: {"type":"notice",\ndata: "level":"warn","text":"x"}\n\n');
        expect(events).toEqual([{ type: 'notice', level: 'warn', text: 'x' }]);
    });

    it('未知事件类型降级为 contract 错误，而不是抛异常', () => {
        const { events } = parseSSEChunk(frame({ type: 'thinking', text: '...' }));
        expect(events).toHaveLength(1);
        expect(events[0]).toMatchObject({ type: 'error', reason: 'contract' });
    });

    it('非法 JSON 降级为 contract 错误', () => {
        const { events } = parseSSEChunk('data: {not json\n\n');
        expect(events[0]).toMatchObject({ type: 'error', reason: 'contract' });
    });

    it('final 帧缺 report 时降级为 contract 错误', () => {
        const { events } = parseSSEChunk(frame({ type: 'final', stop_reason: 'completed' }));
        expect(events[0]).toMatchObject({ type: 'error', reason: 'contract' });
    });

    it('正常解析 final 报告与建议', () => {
        const { events } = parseSSEChunk(
            frame({
                type: 'final',
                report: {
                    kind: 'diagnosis',
                    title: '有规则没挂包',
                    phenomenon: '扫到一条',
                    judgement: 'issue',
                    advice: [{ text: '去挂上', ref: { path: '/benefit/rules', query: { focus: '101' } } }],
                    finding_ids: ['orphan_rules/rule:101'],
                    confidence: 0.9,
                },
                stop_reason: 'completed',
                trace_id: 't-1',
                checks: ['orphan_rules'],
            }),
        );

        expect(events).toHaveLength(1);
        const e = events[0];
        expect(e?.type).toBe('final');
        if (e?.type !== 'final') return;

        expect(e.report.title).toBe('有规则没挂包');
        expect(e.report.finding_ids).toEqual(['orphan_rules/rule:101']);
        expect(e.report.advice[0]?.ref?.path).toBe('/benefit/rules');
        expect(e.trace_id).toBe('t-1');
    });

    it('报告里多余的字段被丢弃，不会顺着往下游传', () => {
        const { events } = parseSSEChunk(
            frame({
                type: 'final',
                report: {
                    kind: 'query',
                    title: 't',
                    phenomenon: 'p',
                    judgement: 'j',
                    advice: [],
                    finding_ids: [],
                    confidence: 1,
                    internal_prompt: '不该出现',
                },
                stop_reason: 'completed',
                trace_id: 't',
                checks: [],
            }),
        );

        const e = events[0];
        if (e?.type !== 'final') throw new Error('应当是 final');
        expect(e.report).not.toHaveProperty('internal_prompt');
    });
});
