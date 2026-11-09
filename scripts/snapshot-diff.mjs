/**
 * 对比两份数据层快照，只输出「变了的地方」。
 *
 * 跑法：node scripts/snapshot-diff.mjs baseline after
 *
 * 设计原则：
 *   - 不变的东西不打印。一次业务操作只碰几张表，全量输出等于没输出。
 *   - 每处变化都标方向：↑ 增 / ↓ 减 / + 新增 / - 消失
 *   - 中间件的「队列积压」和「offset 推进」单独说清楚含义，
 *     否则看到数字变了也不知道是好是坏。
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(__dirname, '..');
const DIR = path.join(ROOT, 'docs', 'shots', 'snapshots');

function load(tag) {
    const f = path.join(DIR, `${tag}.json`);
    if (!fs.existsSync(f)) {
        console.error(`找不到快照 ${f}\n先跑 node scripts/snapshot.mjs ${tag}`);
        process.exit(1);
    }
    return JSON.parse(fs.readFileSync(f, 'utf8'));
}

const [, , aTag = 'baseline', bTag = 'after'] = process.argv;
const A = load(aTag);
const B = load(bTag);

console.log(`对比 ${aTag} (${A.at}) → ${bTag} (${B.at})\n`);

let totalChanges = 0;

function section(title) {
    console.log(`\n── ${title} ──`);
}

/** 比较两个扁平对象（数字值），输出差异 */
function diffCounts(a, b, label) {
    const keys = new Set([...Object.keys(a || {}), ...Object.keys(b || {})]);
    const lines = [];
    for (const k of [...keys].sort()) {
        const x = a?.[k];
        const y = b?.[k];
        if (x === undefined) lines.push(`  + ${k} = ${y}`);
        else if (y === undefined) lines.push(`  - ${k}（原 ${x}）`);
        else if (x !== y) {
            const d = Number(y) - Number(x);
            const arrow = d > 0 ? '↑' : '↓';
            lines.push(`  ${arrow} ${k}: ${x} → ${y}  (${d > 0 ? '+' : ''}${d})`);
        }
    }
    if (lines.length) {
        console.log(`  ${label}:`);
        lines.forEach((l) => console.log(l));
        totalChanges += lines.length;
    }
}

/* ── MySQL ── */
const am = A.汇总?.mysql || {};
const bm = B.汇总?.mysql || {};
section('MySQL（表行数）');
if (am.__error || bm.__error) {
    console.log(`  ${am.__error || bm.__error}`);
} else {
    const before = totalChanges;
    diffCounts(am, bm, '行数变化');
    if (totalChanges === before) console.log('  无变化');
}

/* ── Redis ── */
const ar = A.汇总?.redis || {};
const br = B.汇总?.redis || {};
section('Redis（key 数量）');
if (ar.__error || br.__error) {
    console.log(`  ${ar.__error || br.__error}`);
} else {
    const before = totalChanges;
    diffCounts(ar.按前缀 || {}, br.按前缀 || {}, '前缀 key 数');
    if (totalChanges === before) console.log(`  无变化（共 ${br.总key数} 个 key）`);
    console.log(`  说明: 缓存 key 有 TTL，过期消失是正常现象；`);
    console.log(`        锁 key（mc:lock:*）在请求结束后应立即消失，长期存在=锁泄漏。`);
}

/* ── Kafka ── */
const ak = A.汇总?.kafka || {};
const bk = B.汇总?.kafka || {};
section('Kafka（topic 最新 offset）');
if (ak.__error || bk.__error) {
    console.log(`  ${ak.__error || bk.__error}`);
} else {
    const before = totalChanges;
    diffCounts(ak.最新offset || {}, bk.最新offset || {}, 'offset 推进');
    if (totalChanges === before) console.log('  无变化');
    console.log(`  说明: offset 上涨 = 有新消息写进去了，正常；持续不动 = 写链路断了。`);
}

/* ── RabbitMQ ── */
const aq = A.汇总?.rabbitmq || {};
const bq = B.汇总?.rabbitmq || {};
section('RabbitMQ（队列状态）');
if (aq.__error || bq.__error) {
    console.log(`  ${aq.__error || bq.__error}`);
} else {
    const aQ = aq.队列 || {};
    const bQ = bq.队列 || {};
    const names = new Set([...Object.keys(aQ), ...Object.keys(bQ)]);
    const lines = [];
    for (const n of [...names].sort()) {
        const x = aQ[n];
        const y = bQ[n];
        if (!x) { lines.push(`  + ${n}（新队列）`); continue; }
        if (!y) { lines.push(`  - ${n}（队列消失）`); continue; }
        const dReady = (y.ready || 0) - (x.ready || 0);
        const dUnack = (y.unacked || 0) - (x.unacked || 0);
        const dCon = (y.consumers || 0) - (x.consumers || 0);
        if (dReady || dUnack || dCon) {
            const parts = [];
            if (dReady) parts.push(`待消费 ${x.ready}→${y.ready}`);
            if (dUnack) parts.push(`未确认 ${x.unacked}→${y.unacked}`);
            if (dCon) parts.push(`消费者 ${x.consumers}→${y.consumers}`);
            lines.push(`  ~ ${n}: ${parts.join('，')}`);
        }
    }
    if (lines.length) { lines.forEach((l) => console.log(l)); totalChanges += lines.length; }
    else console.log(`  无变化（${names.size} 个队列都是空的）`);
    console.log(`  说明: 待消费数持续上涨 = 消费者没在跑/消费不过来，这是要报警的信号。`);
}

/* ── MinIO ── */
const ami = A.汇总?.minio || {};
const bmi = B.汇总?.minio || {};
section('MinIO（桶内对象数）');
if (ami.__error || bmi.__error) {
    console.log(`  ${ami.__error || bmi.__error}`);
} else {
    const before = totalChanges;
    diffCounts(ami.桶与对象数 || {}, bmi.桶与对象数 || {}, '对象数');
    if (totalChanges === before) console.log('  无变化');
}

/* ── 结论 ── */
console.log(`\n${'='.repeat(50)}`);
if (totalChanges === 0) {
    console.log('结论：数据层完全没变化。');
    console.log('  如果你刚做了操作，说明改动没落库（或落到了别的库/别的 vhost）。');
} else {
    console.log(`结论：共 ${totalChanges} 处变化。`);
}
