#!/usr/bin/env node
/**
 * 生成「运营配置」Grafana dashboard 并推送。
 *
 * 为什么单独一个面板而不是塞进「会员运营台·总览」：
 * 那边看的是**业务**健康度（请求量、错误率、前端体验），
 * 这里看的是**配置能力**自身的运行情况（规则求值、作业执行、脱敏生效）。
 * 两类问题由不同的人处置，混在一起会让两边都看不清。
 *
 * 用法：GRAFANA_PASS=*** node scripts/build-ops-dashboard.mjs
 */
import fs from 'node:fs';

const GRAFANA = process.env.GRAFANA || 'http://127.0.0.1:13000';
const USER = process.env.GRAFANA_USER || 'admin';
const PASS = process.env.GRAFANA_PASS;
if (!PASS) {
    console.error('缺少 GRAFANA_PASS');
    process.exit(1);
}
const AUTH = 'Basic ' + Buffer.from(`${USER}:${PASS}`).toString('base64');
const DS = { type: 'prometheus', uid: 'prometheus' };

const stat = (title, expr, unit, x, y) => ({
    type: 'stat',
    title,
    datasource: DS,
    gridPos: { x, y, w: 6, h: 4 },
    fieldConfig: { defaults: { unit }, overrides: [] },
    targets: [{ refId: 'A', expr }],
    options: { reduceOptions: { calcs: ['lastNotNull'] }, colorMode: 'value', graphMode: 'area' },
});

const ts = (title, expr, unit, x, y, legend, w = 12, h = 8) => ({
    type: 'timeseries',
    title,
    datasource: DS,
    gridPos: { x, y, w, h },
    fieldConfig: { defaults: { unit }, overrides: [] },
    targets: [{ refId: 'A', expr, legendFormat: legend }],
    options: { legend: { displayMode: 'list', placement: 'bottom' } },
});

const row = (title, y) => ({ type: 'row', title, gridPos: { x: 0, y, w: 24, h: 1 }, panels: [] });

const dashboard = {
    uid: 'mc-ops-config',
    title: '会员运营台 · 运营配置',
    tags: ['ops', 'rule', 'job', 'mask'],
    timezone: 'browser',
    schemaVersion: 39,
    version: 0,
    refresh: '15s',
    time: { from: 'now-3h', to: 'now' },
    panels: [
        row('规则引擎', 0),
        stat('求值命中（1h）', 'sum(increase(mc_rule_eval_total{result="hit"}[1h]))', 'short', 0, 1),
        stat('求值未命中（1h）', 'sum(increase(mc_rule_eval_total{result="miss"}[1h]))', 'short', 6, 1),
        stat('求值 P95', 'histogram_quantile(0.95, sum by (le) (rate(mc_rule_eval_duration_seconds_bucket[5m])))', 's', 12, 1),
        stat('求值 QPS', 'sum(rate(mc_rule_eval_total[5m]))', 'reqps', 18, 1),
        ts('求值次数（按结果）', 'sum by (result) (rate(mc_rule_eval_total[5m]))', 'reqps', 0, 5, '{{result}}'),
        ts('求值耗时 P95（按范围）', 'histogram_quantile(0.95, sum by (le, scope) (rate(mc_rule_eval_duration_seconds_bucket[5m])))', 's', 12, 5, '{{scope}}'),

        row('定时作业', 13),
        ts('作业执行（按作业/结果）', 'sum by (job_name, result) (increase(mc_job_run_total[1h]))', 'short', 0, 14, '{{job_name}} / {{result}}'),
        ts('作业耗时 P95', 'histogram_quantile(0.95, sum by (le, job_name) (rate(mc_job_duration_seconds_bucket[5m])))', 's', 12, 14, '{{job_name}}'),
        ts('被跳过的作业（按原因）', 'sum by (job_name, reason) (increase(mc_job_skipped_total[1h]))', 'short', 0, 22, '{{job_name}} / {{reason}}'),

        row('字段脱敏', 30),
        ts('脱敏生效（按来源）', 'sum by (source) (increase(mc_mask_applied_total[1h]))', 'short', 0, 31, '{{source}}'),
        ts('脱敏生效（按类型）', 'sum by (kind) (increase(mc_mask_applied_total[1h]))', 'short', 12, 31, '{{kind}}'),
        // 明文访问应该是 0 或极低；这条曲线一旦抬起就必须能查到是谁
        ts('明文访问（审计指标，正常应接近 0）', 'sum by (field) (increase(mc_mask_plaintext_total[1h]))', 'short', 0, 39, '{{field}}'),
    ],
};

const res = await fetch(`${GRAFANA}/api/dashboards/db`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: AUTH },
    body: JSON.stringify({ dashboard, overwrite: true, folderUid: '' }),
});
const json = await res.json();
console.log(res.status, JSON.stringify(json).slice(0, 200));
if (json.status !== 'success') process.exit(1);

// 同时落一份到仓库，方便 review 与重建
fs.writeFileSync(
    'D:/20260916/loyalty-console/deploy/grafana/ops-dashboard.json',
    JSON.stringify({ dashboard, overwrite: true, folderUid: '' }, null, 2),
);
console.log(`\n打开：${GRAFANA}${json.url}`);
