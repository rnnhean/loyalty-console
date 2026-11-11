import assert from 'node:assert/strict';
import { test } from 'node:test';

import { Ledger } from './ledger.js';
import { finding, result } from './testkit.js';
import { validateReport } from './validate.js';

function ledgerWith(): Ledger {
    const ledger = new Ledger();
    ledger.append(
        result('orphan_rules', {
            verdict: 'issue',
            confidence: 0.8,
            findings: [
                finding('rule:101', { title: '规则 101 没有挂权益包' }),
                finding('rule:102', { title: '规则 102 没有挂权益包' }),
            ],        }),
        1,
    );
    ledger.append(result('tier_gap', { confidence: 1 }), 1);
    return ledger;
}

test('未知 finding_ids 被剔除，已知的保留', () => {
    const ledger = ledgerWith();
    const out = validateReport(
        {
            kind: 'diagnosis',
            title: '两条规则没挂包',
            phenomenon: '扫到 2 条规则',
            judgement: 'issue',
            advice: [],
            finding_ids: ['orphan_rules/rule:101', 'orphan_rules/rule:999'],
        },
        ledger,
    );

    assert.equal(out.fatal, false);
    assert.deepEqual(out.report.finding_ids, ['orphan_rules/rule:101']);
    assert.ok(out.issues.some((i) => i.kind === 'unknown_finding'));
});

test('诊断型报告一条证据都没有 → 致命', () => {
    const ledger = ledgerWith();
    const out = validateReport(
        {
            kind: 'diagnosis',
            title: '空口结论',
            phenomenon: '看起来有问题',
            judgement: '有问题',
            advice: [],
            finding_ids: ['orphan_rules/rule:999'],
        },
        ledger,
    );

    assert.equal(out.fatal, true);
});

test('query 型报告没有 finding 不算致命', () => {
    const ledger = ledgerWith();
    const out = validateReport(
        {
            kind: 'query',
            title: '当前有 2 个产品',
            phenomenon: '产品列表共 2 条',
            judgement: '这是查询结果',
            advice: [],
            finding_ids: [],
        },
        ledger,
    );

    assert.equal(out.fatal, false);
});

test('confidence 一律用服务端聚合值，覆盖模型写的', () => {
    const ledger = ledgerWith();
    const out = validateReport(
        {
            kind: 'diagnosis',
            title: 'x',
            phenomenon: 'x',
            judgement: 'x',
            advice: [],
            finding_ids: ['orphan_rules/rule:101'],
            confidence: 1,
        },
        ledger,
    );

    // 两条记录里最低的是 0.8
    assert.equal(out.report.confidence, 0.8);
});

test('advice 的证据引用必须唯一命中', () => {
    const ledger = ledgerWith();
    const out = validateReport(
        {
            kind: 'diagnosis',
            title: 'x',
            phenomenon: 'x',
            judgement: 'x',
            advice: [
                { text: '唯一命中', evidence_ref: 'rule:101' },
                { text: '完全不存在', evidence_ref: 'rule:777' },
                { text: '命中两条', evidence_ref: '没有挂权益包' },
            ],
            finding_ids: ['orphan_rules/rule:101'],
        },
        ledger,
    );

    assert.deepEqual(
        out.report.advice.map((a) => a.text),
        ['唯一命中'],
    );
    assert.ok(out.issues.some((i) => i.kind === 'unknown_finding'));
    assert.ok(out.issues.some((i) => i.kind === 'ambiguous_evidence'));
});

test('advice 指向白名单外的页面被丢弃', () => {
    const ledger = ledgerWith();
    const out = validateReport(
        {
            kind: 'diagnosis',
            title: 'x',
            phenomenon: 'x',
            judgement: 'x',
            advice: [
                { text: '去一个不存在的页面', ref: { path: '/evil/delete-all' } },
                { text: '去权益规则页', ref: { path: '/benefit/rules' } },
            ],
            finding_ids: ['orphan_rules/rule:101'],
        },
        ledger,
    );

    assert.deepEqual(
        out.report.advice.map((a) => a.text),
        ['去权益规则页'],
    );
    assert.ok(out.issues.some((i) => i.kind === 'bad_advice_ref'));
});

test('现象里的数字必须能在证据里找到', () => {
    const ledger = ledgerWith();
    const out = validateReport(
        {
            kind: 'diagnosis',
            title: 'x',
            phenomenon: '有 57 条规则没挂包',
            judgement: 'x',
            advice: [],
            finding_ids: ['orphan_rules/rule:101'],
        },
        ledger,
    );

    assert.ok(out.issues.some((i) => i.kind === 'number_not_in_evidence'));
    assert.ok(out.issues.some((i) => /57/.test(i.detail)));
});

test('kind 不是三类之一 → 致命', () => {
    const ledger = ledgerWith();
    const out = validateReport({ kind: 'unknown', title: 'x' }, ledger);
    assert.equal(out.fatal, true);
    assert.equal(out.issues[0]?.kind, 'bad_shape');
});
