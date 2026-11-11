/**
 * 手写指标库的单测。
 *
 * 重点不在「能不能加一」，而在**输出是不是 Prometheus 认的文本**：
 * 这个文件的价值全在 render()，写错了 Prometheus 会整段丢弃，
 * 而 /metrics 本身照样返回 200 —— 那种故障只有单测能提前拦住。
 */

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { Counter, Gauge, Histogram, renderMetrics } from './metrics.js';

test('counter 无 label 时不带花括号', () => {
    const c = new Counter('unit_plain_total', '无 label 计数器');
    c.inc();
    c.inc(undefined, 2);

    const out = c.render();
    assert.match(out, /^# HELP unit_plain_total 无 label 计数器$/m);
    assert.match(out, /^# TYPE unit_plain_total counter$/m);
    assert.match(out, /^unit_plain_total 3$/m);
    assert.doesNotMatch(out, /unit_plain_total\{/);
});

test('counter 按 label 组合分别累计，互不串味', () => {
    const c = new Counter('unit_labeled_total', '带 label 计数器', ['result']);
    c.inc({ result: 'ok' });
    c.inc({ result: 'ok' });
    c.inc({ result: 'error' });

    const out = c.render();
    assert.match(out, /^unit_labeled_total\{result="ok"\} 2$/m);
    assert.match(out, /^unit_labeled_total\{result="error"\} 1$/m);
});

test('label 值里的反斜杠、引号、换行都要转义', () => {
    const c = new Counter('unit_escape_total', '转义', ['v']);
    c.inc({ v: 'a"b\\c\nd' });

    // Prometheus 文本格式：\\ 表示反斜杠、\" 表示引号、\n 表示换行
    assert.match(c.render(), /^unit_escape_total\{v="a\\"b\\\\c\\nd"\} 1$/m);
});

test('label 组合不会因为拼接符撞在一起', () => {
    // ("a,b", "c") 和 ("a", "b,c") 用逗号拼 key 会撞；用 \0 分隔就不会
    const c = new Counter('unit_collide_total', '撞 key', ['x', 'y']);
    c.inc({ x: 'a,b', y: 'c' });
    c.inc({ x: 'a', y: 'b,c' });

    const out = c.render();
    assert.match(out, /^unit_collide_total\{x="a,b",y="c"\} 1$/m);
    assert.match(out, /^unit_collide_total\{x="a",y="b,c"\} 1$/m);
});

test('histogram 桶是累计的，且 +Inf 等于 count', () => {
    const h = new Histogram('unit_lat_seconds', '耗时', [], [0.1, 1]);
    h.observe(undefined, 0.05); // 进 0.1、1 两个桶
    h.observe(undefined, 0.5); // 只进 1
    h.observe(undefined, 5); // 都不进

    const out = h.render();
    assert.match(out, /^unit_lat_seconds_bucket\{le="0\.1"\} 1$/m);
    assert.match(out, /^unit_lat_seconds_bucket\{le="1"\} 2$/m);
    assert.match(out, /^unit_lat_seconds_bucket\{le="\+Inf"\} 3$/m);

    const count = /^unit_lat_seconds_count 3$/m;
    assert.match(out, count);
    assert.match(out, /^unit_lat_seconds_sum 5\.55$/m);
});

test('histogram 带 label 时 le 必须排在最后一位', () => {
    const h = new Histogram('unit_tool_seconds', '工具耗时', ['tool'], [0.5]);
    h.observe({ tool: 'get_kpi' }, 0.2);

    // 顺序写错（le 在前）Prometheus 直接不认这个序列
    assert.match(h.render(), /^unit_tool_seconds_bucket\{tool="get_kpi",le="0\.5"\} 1$/m);
});

test('空 label 值的序列也要输出，不能当没观测过', () => {
    const h = new Histogram('unit_empty_label_seconds', '空 label', ['check'], [1]);
    h.observe({ check: '' }, 0.1);

    assert.match(h.render(), /^unit_empty_label_seconds_bucket\{check="",le="1"\} 1$/m);
    assert.match(h.render(), /^unit_empty_label_seconds_count\{check=""\} 1$/m);
});

test('gauge 支持 set / inc / dec', () => {
    const g = new Gauge('unit_inflight', '在途');
    g.set(undefined, 5);
    assert.match(g.render(), /^unit_inflight 5$/m);

    g.inc();
    g.inc();
    g.dec();
    assert.match(g.render(), /^unit_inflight 6$/m);
});

test('一个指标还没被写过时不输出空行，但 HELP/TYPE 仍在', () => {
    const c = new Counter('unit_untouched_total', '没写过');
    const out = c.render();
    assert.match(out, /^# HELP unit_untouched_total 没写过$/m);
    assert.match(out, /^# TYPE unit_untouched_total counter$/m);
    assert.doesNotMatch(out, /^unit_untouched_total /m);
});

test('renderMetrics 把注册表里所有指标拼在一起', () => {
    const out = renderMetrics();
    assert.match(out, /# TYPE unit_plain_total counter/);
    assert.match(out, /# TYPE unit_lat_seconds histogram/);
    // 每段之间不能有重复的 TYPE 行（重复声明同名指标 Prometheus 会报错）
    const types = out.match(/^# TYPE (\S+)/gm) ?? [];
    assert.equal(new Set(types).size, types.length);
});
