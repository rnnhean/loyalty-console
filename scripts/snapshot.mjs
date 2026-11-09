/**
 * 数据层状态快照。
 *
 * 用途：数据库、中间件没有界面，截图截不到，但「改了一个业务操作之后数据变没变」
 * 完全可以靠「改动前存一份、改动后再存一份、diff 两份」来证明。
 *
 * 跑法：
 *   node scripts/snapshot.mjs before
 *   （做点操作，比如在前端下个单、改条数据）
 *   node scripts/snapshot.mjs after
 *   node scripts/snapshot-diff.mjs before after
 *
 * 输出：docs/shots/snapshots/<tag>.json
 *
 * 凭据安全：连接信息从 deploy/env/local.env 现读现用，
 * 全程只在内存里，不打印、不落盘、不进快照文件。
 * 快照里只有「表名 / 行数 / key 前缀 / topic offset」这类结构信息。
 */
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(__dirname, '..');
const ENV_FILE = path.join(ROOT, 'deploy', 'env', 'local.env');
const OUT_DIR = path.join(ROOT, 'docs', 'shots', 'snapshots');

const tag = process.argv[2] || 'snapshot';

/* ── 读配置文件：只解析，绝不打印 ───────────────────────── */
function readEnvFile(file) {
    const out = {};
    if (!fs.existsSync(file)) return out;
    for (const raw of fs.readFileSync(file, 'utf8').split(/\r?\n/)) {
        const line = raw.trim();
        if (!line || line.startsWith('#')) continue;
        const eq = line.indexOf('=');
        if (eq < 1) continue;
        const k = line.slice(0, eq).replace(/^export\s+/, '').trim();
        let v = line.slice(eq + 1).trim();
        if ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'"))) {
            v = v.slice(1, -1);
        }
        out[k] = v;
    }
    return out;
}

const env = readEnvFile(ENV_FILE);

/** 安全执行，失败返回 {__error}，不让单个数据源拖垮整次快照 */
function run(cmd, args, opts = {}) {
    try {
        return {
            ok: true,
            out: execFileSync(cmd, args, {
                encoding: 'utf8',
                timeout: opts.timeout || 20000,
                stdio: ['ignore', 'pipe', 'pipe'],
            }),
        };
    } catch (e) {
        const msg = String(e.stderr || e.message || '').trim().split('\n').slice(-2).join(' ');
        return { ok: false, out: '', error: msg.slice(0, 200) };
    }
}

function dockerExec(service, args, opts) {
    return run('docker', ['exec', service, ...args], opts);
}

/* ── MySQL ──────────────────────────────────────────────── */
function parseDSN(dsn) {
    // user:pass@tcp(host:port)/dbname?params
    const m = /^([^:]+):(.*)@tcp\(([^)]+)\)\/([^?]+)/.exec(dsn || '');
    if (!m) return null;
    return { user: m[1], pass: m[2], db: m[4] };
}

function snapshotMySQL() {
    const d = parseDSN(env.MC_MYSQL_DSN);
    if (!d) return { __error: '未找到 MC_MYSQL_DSN 或格式不匹配' };

    // ★ 必须带 -D <db>：information_schema 里查得到表名，但 COUNT(*) 需要当前库，
    //   否则报 ERROR 1046 No database selected。
    const base = ['mysql', `-u${d.user}`, `-p${d.pass}`, '-D', d.db, '-N', '-B', '-e'];

    const tables = dockerExec('mc-mysql', [...base,
        `SELECT table_name FROM information_schema.tables WHERE table_schema='${d.db}' ORDER BY table_name;`]);
    if (!tables.ok) return { __error: tables.error || '连接失败' };

    const names = tables.out.split('\n').map((s) => s.trim()).filter(Boolean);
    if (!names.length) return { __error: '库里没有表' };

    // 一条 UNION ALL 查完所有表，别开 N 次连接
    const sql = names
        .map((t) => `SELECT '${t}' AS t, COUNT(*) AS c FROM \`${t}\``)
        .join(' UNION ALL ');
    const r = dockerExec('mc-mysql', [...base, sql], { timeout: 30000 });
    if (!r.ok) return { __error: r.error };

    const result = {};
    for (const line of r.out.split('\n')) {
        const [t, c] = line.split('\t');
        if (t) result[t.trim()] = Number(c);
    }
    return result;
}

/* ── Redis ──────────────────────────────────────────────── */
function snapshotRedis() {
    // 本地 Redis 没设密码：直接扫
    const r = dockerExec('mc-redis', ['redis-cli', '--scan', '--count', '1000']);
    if (!r.ok) return { __error: r.error };

    const keys = r.out.split('\n').map((s) => s.trim()).filter(Boolean);
    const prefixes = {};
    for (const k of keys) {
        // 取第一段作为前缀：mc:lock:xxx → mc:lock
        const parts = k.split(':');
        const p = parts.length > 1 ? parts.slice(0, 2).join(':') : parts[0];
        prefixes[p] = (prefixes[p] || 0) + 1;
    }
    const info = dockerExec('mc-redis', ['redis-cli', 'INFO', 'keyspace']);
    return {
        总key数: keys.length,
        按前缀: Object.fromEntries(Object.entries(prefixes).sort((a, b) => b[1] - a[1])),
        keyspace: (info.ok ? info.out : '').trim().split('\n').filter((l) => l.startsWith('db')).join(' | '),
    };
}

/* ── Kafka ──────────────────────────────────────────────── */
function snapshotKafka() {
    const bin = '/opt/kafka/bin';
    const bs = '--bootstrap-server localhost:9092';

    const topics = dockerExec('mc-kafka', [`${bin}/kafka-topics.sh`, ...bs.split(' '), '--list']);
    if (!topics.ok) return { __error: topics.error };

    const list = topics.out.split('\n').map((s) => s.trim()).filter(Boolean).filter((t) => !t.startsWith('__'));
    const offsets = {};

    for (const t of list) {
        const r = dockerExec('mc-kafka', [
            `${bin}/kafka-get-offsets.sh`, ...bs.split(' '),
            '--topic', t, '--time', '-1',
        ]);
        if (!r.ok) { offsets[t] = '?'; continue; }
        // 输出形如 topic:0:123 / topic:1:456 —— 各分区求和
        let sum = 0;
        for (const line of r.out.trim().split('\n')) {
            const n = Number(line.split(':').pop());
            if (Number.isFinite(n)) sum += n;
        }
        offsets[t] = sum;
    }

    // 消费组有没有积压
    const groups = dockerExec('mc-kafka', [`${bin}/kafka-consumer-groups.sh`, ...bs.split(' '), '--all-groups', '--describe']);
    let lag = '';
    if (groups.ok) {
        const lines = groups.out.trim().split('\n').slice(1);
        lag = lines.length ? `${lines.length} 个分区分组的位点记录` : '无消费组';
    }

    return { topics: list, 最新offset: offsets, 消费组: lag };
}

/* ── RabbitMQ ───────────────────────────────────────────── */
function snapshotRabbit() {
    // 从 amqp://user:pass@host:port/vhost 里取 vhost
    const m = /^amqps?:\/\/[^@]+@[^/]+\/?(.*)$/.exec(env.MC_RABBIT_URL || '');
    if (!m) return { __error: '未解析出 MC_RABBIT_URL' };
    const vhost = decodeURIComponent(m[1] || '/');

    // ★ 用 rabbitmqctl 而不是 management API：镜像里没有 curl，
    //   而且 management 接口要单独记账号，ctl 直接在本机跑。
    const r = dockerExec('mc-rabbitmq', [
        'rabbitmqctl', 'list_queues', '-p', vhost, '--formatter', 'json',
        'name', 'messages', 'messages_unacknowledged', 'consumers',
    ]);
    if (!r.ok) return { __error: r.error };

    let arr;
    try { arr = JSON.parse(r.out); } catch { return { __error: '返回非 JSON' }; }
    if (!Array.isArray(arr)) return { __error: '队列列表解析失败' };

    const q = {};
    for (const it of arr) {
        q[it.name] = {
            ready: it.messages,
            unacked: it.messages_unacknowledged,
            consumers: it.consumers,
        };
    }
    return { vhost, 队列数: arr.length, 队列: q };
}

/* ── MinIO ──────────────────────────────────────────────── */
function snapshotMinIO() {
    // ★ 数据目录是 /data（bitnami 镜像），不是 /bitnami/minio/data
    const dir = '/data';

    const r = dockerExec('mc-minio', ['sh', '-c',
        `ls "${dir}" 2>/dev/null | while read b; do ` +
        `n=$(find "${dir}/$b" -type f -not -path '*/.minio.sys/*' 2>/dev/null | wc -l); ` +
        `echo "$b $n"; done`]);
    if (!r.ok) return { __error: r.error };

    const buckets = {};
    for (const line of r.out.trim().split('\n')) {
        const [b, n] = line.trim().split(/\s+/);
        if (b) buckets[b] = Number(n);
    }
    return { 数据目录: dir, 桶与对象数: buckets };
}

/* ── 汇总 ───────────────────────────────────────────────── */
const snap = {
    tag,
    at: new Date().toISOString(),
    汇总: {
        mysql: snapshotMySQL(),
        redis: snapshotRedis(),
        kafka: snapshotKafka(),
        rabbitmq: snapshotRabbit(),
        minio: snapshotMinIO(),
    },
};

fs.mkdirSync(OUT_DIR, { recursive: true });
const file = path.join(OUT_DIR, `${tag}.json`);
fs.writeFileSync(file, JSON.stringify(snap, null, 2), 'utf8');

/* ── 终端摘要 ───────────────────────────────────────────── */
console.log(`\n快照已写入 ${path.relative(ROOT, file)}\n`);

const s = snap.汇总;
for (const [name, data] of Object.entries(s)) {
    if (data?.__error) { console.log(`✗ ${name}: ${data.__error}`); continue; }
    if (name === 'mysql') {
        const n = Object.keys(data).length;
        const rows = Object.values(data).reduce((a, b) => a + (Number(b) || 0), 0);
        console.log(`✓ mysql: ${n} 张表 / 合计 ${rows} 行`);
    } else if (name === 'redis') {
        console.log(`✓ redis: ${data.总key数} 个 key，${Object.keys(data.按前缀 || {}).length} 个前缀`);
    } else if (name === 'kafka') {
        console.log(`✓ kafka: ${data.topics?.length || 0} 个业务 topic`);
    } else if (name === 'rabbitmq') {
        console.log(`✓ rabbitmq: ${Object.keys(data.队列 || {}).length} 个队列`);
    } else if (name === 'minio') {
        console.log(`✓ minio: ${Object.keys(data.桶与对象数 || {}).length} 个桶`);
    }
}
