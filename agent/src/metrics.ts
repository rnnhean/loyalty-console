/**
 * 手写的 Prometheus 文本 exposition。
 *
 * 为什么不用 prom-client：这个服务刻意做到零运行时依赖（见 Dockerfile），
 * 运行镜像里连 node_modules 都没有。而 Prometheus 的文本格式本身就是
 * 「# HELP / # TYPE / 一行一个序列」，手写不到 200 行，比多背一个依赖划算。
 *
 * 为什么不能让 trace JSONL 顶替：日志能回答「刚才那次花了多少」，
 * 回答不了「过去 10 分钟引用校验丢了几条」——跨请求聚合正是 Prometheus 的活，
 * 也是告警唯一能用的输入。
 *
 * 这个文件是纯数据结构，不认识任何业务；业务埋点全在 instrument.ts。
 */

export type Labels = Record<string, string | number | boolean>;

type MetricKind = 'counter' | 'gauge' | 'histogram';

interface Observe {
    /** 累计桶计数：counts[i] 是「<= buckets[i] 的观测数」 */
    counts: number[];
    sum: number;
    count: number;
}

interface MetricBase {
    readonly kind: MetricKind;
    readonly name: string;
    readonly help: string;
    readonly labelNames: readonly string[];
    readonly series: Map<string, number | Observe>;
    render(): string;
}

/** 注册顺序即输出顺序，读 /metrics 时和代码里的顺序对得上。 */
const REGISTRY: MetricBase[] = [];

function escapeLabel(v: string): string {
    return v.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/\n/g, '\\n');
}

/**
 * label 组合 → 序列 key。
 *
 * 用 \u0000 分隔：label 值里可能出现任意可打印字符，用逗号/等号拼 key
 * 会让 ("a,b","c") 和 ("a","b,c") 撞成同一串。
 */
function seriesKey(names: readonly string[], labels?: Labels): string {
    if (names.length === 0) return '';
    return names.map((n) => String(labels?.[n] ?? '')).join('\u0000');
}

function splitKey(names: readonly string[], key: string): string[] {
    if (names.length === 0) return [];
    return key.split('\u0000');
}

/** 浮点保留 6 位：金额类计数器可能出现 1.0000000000002 这种尾巴。 */
function fmt(v: number): string {
    if (Number.isInteger(v)) return String(v);
    return String(Number(v.toFixed(6)));
}

function renderLine(name: string, names: readonly string[], values: readonly string[], value: string): string {
    if (names.length === 0) return `${name} ${value}`;
    const parts = names.map((n, i) => `${n}="${escapeLabel(values[i] ?? '')}"`);
    return `${name}{${parts.join(',')}} ${value}`;
}

function header(m: MetricBase): string {
    return `# HELP ${m.name} ${m.help}\n# TYPE ${m.name} ${m.kind}\n`;
}

/** 只有 counter 和 gauge 的形状一样（key → 数字），共用一份渲染。 */
function renderScalar(m: MetricBase): string {
    let out = header(m);
    for (const [key, v] of m.series) {
        out += renderLine(m.name, m.labelNames, splitKey(m.labelNames, key), fmt(v as number)) + '\n';
    }
    return out;
}

export class Counter implements MetricBase {
    readonly kind = 'counter';
    readonly series = new Map<string, number>();

    constructor(
        readonly name: string,
        readonly help: string,
        readonly labelNames: readonly string[] = [],
    ) {
        REGISTRY.push(this);
    }

    inc(labels?: Labels, n = 1): void {
        const key = seriesKey(this.labelNames, labels);
        this.series.set(key, (this.series.get(key) ?? 0) + n);
    }

    render(): string {
        return renderScalar(this);
    }
}

export class Gauge implements MetricBase {
    readonly kind = 'gauge';
    readonly series = new Map<string, number>();

    constructor(
        readonly name: string,
        readonly help: string,
        readonly labelNames: readonly string[] = [],
    ) {
        REGISTRY.push(this);
    }

    set(labels: Labels | undefined, v: number): void {
        this.series.set(seriesKey(this.labelNames, labels), v);
    }

    inc(labels?: Labels, n = 1): void {
        const key = seriesKey(this.labelNames, labels);
        this.series.set(key, (this.series.get(key) ?? 0) + n);
    }

    dec(labels?: Labels, n = 1): void {
        this.inc(labels, -n);
    }

    render(): string {
        return renderScalar(this);
    }
}

export class Histogram implements MetricBase {
    readonly kind = 'histogram';
    readonly series = new Map<string, number | Observe>();

    constructor(
        readonly name: string,
        readonly help: string,
        readonly labelNames: readonly string[] = [],
        private readonly buckets: readonly number[] = [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5],
    ) {
        REGISTRY.push(this);
    }

    observe(labels: Labels | undefined, v: number): void {
        const key = seriesKey(this.labelNames, labels);
        let o = this.series.get(key) as Observe | undefined;
        if (!o) {
            o = { counts: this.buckets.map(() => 0), sum: 0, count: 0 };
            this.series.set(key, o);
        }
        // 累计计数：落进哪个桶，它及之后所有桶都 +1。
        // 渲染时就不用再算前缀和，也不会因为浮点比较顺序出岔子。
        for (let i = 0; i < this.buckets.length; i++) {
            if (v <= (this.buckets[i] ?? 0)) o.counts[i] = (o.counts[i] ?? 0) + 1;
        }
        o.sum += v;
        o.count += 1;
    }

    render(): string {
        let out = header(this);
        for (const [key, value] of this.series) {
            const o = value as Observe;
            const values = splitKey(this.labelNames, key);
            for (let i = 0; i < this.buckets.length; i++) {
                // le 必须排在最后一位，这是 Prometheus 对 histogram 的约定
                out += renderLine(`${this.name}_bucket`, [...this.labelNames, 'le'], [...values, fmt(this.buckets[i] ?? 0)], String(o.counts[i] ?? 0)) + '\n';
            }
            out += renderLine(`${this.name}_bucket`, [...this.labelNames, 'le'], [...values, '+Inf'], String(o.count)) + '\n';
            out += renderLine(`${this.name}_sum`, this.labelNames, values, fmt(o.sum)) + '\n';
            out += renderLine(`${this.name}_count`, this.labelNames, values, String(o.count)) + '\n';
        }
        return out;
    }
}

/** 渲染整个注册表。`/metrics` 每次被抓取时现算，不缓存——15 秒一次的开销可以忽略。 */
export function renderMetrics(): string {
    return REGISTRY.map((m) => m.render()).join('');
}
