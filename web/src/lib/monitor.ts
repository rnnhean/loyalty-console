/**
 * 前端监控装配。
 *
 * 选型：Sentry（错误 + 性能）+ 自建 beacon 兜底。
 *
 * 为什么不是「只装 Sentry」：
 *   - Sentry SaaS 免费额度有限（5k errors/月），公网部署下很容易被扫描器打满；
 *   - 但完全自建又做不出聚合、堆栈去重、Release 对比这些能力。
 * 所以走两层：
 *   1. Sentry 负责「能定位到代码行」的错误与性能 —— 它不可替代；
 *   2. 自建 beacon 只上报「数量级」指标（Web Vitals 分档、错误计数），
 *      成本几乎为零，且 Sentry 挂掉/超额时仍有数据。
 *
 * 采样策略是这里最需要想清楚的地方：
 *   - error 全采（1.0）：错误本身就是异常事件，漏一条可能就漏掉一个真实 bug；
 *   - performance 默认也是 1.0：本项目是低流量的内部运营后台，导航量很小，
 *     10% 采样会让 Traces / Profiles 时有时无，演示时看起来就像坏了。这个量级下
 *     全采的配额开销可以忽略；真上高流量时用 VITE_TRACES_SAMPLE_RATE 下调即可。
 * 这是「错误」和「性能」在成本上的本质差别：前者必须全采，后者按流量调。
 */

import * as Sentry from '@sentry/react';
import { onCLS, onFCP, onINP, onLCP, onTTFB, type Metric } from 'web-vitals';

const DSN = import.meta.env.VITE_SENTRY_DSN as string | undefined;
const ENV = (import.meta.env.VITE_SENTRY_ENV as string) || import.meta.env.MODE;
const RELEASE = (import.meta.env.VITE_APP_VERSION as string) || 'dev';
const BEACON_URL = '/api/telemetry/beacon';

/**
 * 性能追踪采样率（同时决定 Traces 和 Profiles 有没有数据）。
 *
 * 默认 **1.0（全采）** 而不是 0.1：本项目是低流量的内部运营后台，页面导航量很小，
 * 10% 采样会让 Traces / Profiles 时有时无 —— 演示时看起来就像坏了。
 * 这个量级下全采的配额开销可以忽略，换来的是三页稳定有数据。
 * 真上到高流量时，用 `VITE_TRACES_SAMPLE_RATE=0.1` 之类的值把成本压下去即可。
 */
const TRACES_RATE = Number(import.meta.env.VITE_TRACES_SAMPLE_RATE ?? 1);

/**
 * 浏览器端 Profile 的会话采样率。
 *
 * 为什么默认是 1.0 而不是 0.1 —— 这里有个很容易踩的乘法：
 * `profileLifecycle: 'trace'` 下，一个会话要被剖析，必须**同时**满足
 *   (a) 该 profile 会话被采样，概率 = 本值；
 *   (b) 该次根 span 被追踪采样，概率 = TRACES_RATE。
 * 实际概率是两者相乘。如果两个都填 0.1，真实剖析率只有 **1%** ——
 * 表现就是「Profiles 页几乎永远是空的，偶尔冒一条」，而排查时很难想到是乘法。
 *
 * 把本值设成 1.0，等于「凡是被采样的 trace，都附一份 profile」，
 * 总量由 TRACES_RATE 控制（默认也是 1.0，低流量内部后台全采；高流量时再一起降）。
 */
const PROFILE_RATE = Number(import.meta.env.VITE_PROFILE_SAMPLE_RATE ?? 1);

/** 当前页面的 trace_id，用于把前端事件和后端日志串起来 */
let currentTraceId = '';

export function setTraceId(id: string) {
    currentTraceId = id;
}

/**
 * URL 收敛：进 Sentry 之前必须过这一道。
 *
 * 后台系统的 URL 经常带 query（订单号、时间戳、有时还有临时票据），
 * 原样上报等于把业务数据送到第三方服务器。这里只保留 pathname。
 */
function scrubUrl(raw: string): string {
    try {
        const u = new URL(raw, window.location.origin);
        return `${u.origin}${u.pathname}`;
    } catch {
        return raw.split('?')[0].split('#')[0];
    }
}

const DROP_HEADERS = ['authorization', 'cookie', 'set-cookie', 'x-trace-id'];

function scrubHeaders(headers?: Record<string, string>) {
    if (!headers) return headers;
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(headers)) {
        out[k] = DROP_HEADERS.includes(k.toLowerCase()) ? '«redacted»' : v;
    }
    return out;
}

/**
 * Sentry Logs（v10 起 SDK 自带的结构化日志）。
 *
 * 为什么**不用** `consoleLoggingIntegration`：
 *   那个集成会把 `console.*` 全量转发到 Sentry 的服务器。后台系统的 console 里
 *   混着接口返回体、订单号、用户标识，转发等于把这些原样送进第三方 SaaS ——
 *   和本文件 `scrubUrl` / `scrubHeaders` 守住的那条底线直接冲突。
 *
 * 所以改成**显式打点 + 按构造脱敏**：只有调用 `logEvent` 的地方才会发日志，
 * 属性只接受标量（字符串/数字/布尔），调用方没有机会塞进去一个对象或数组。
 * 想加敏感字段，必须自己显式写上来 —— 而不是「不小心带出去了才发现」。
 */
type LogLevel = 'trace' | 'debug' | 'info' | 'warn' | 'error' | 'fatal';
type Attrs = Record<string, string | number | boolean | undefined>;

/**
 * 属性清洗：只留标量。
 *
 * 这是「按构造脱敏」而不是「事后检查」——调用方根本没机会塞进来一个对象、
 * 一个数组、一个 DOM 节点或一整个接口返回体，也就不会出现「不小心把业务数据
 * 带上去了」这种事故。想发复杂结构，得先自己压成标量并想清楚为什么要发。
 */
function cleanAttrs(attributes?: Attrs): Record<string, string | number | boolean> {
    const clean: Record<string, string | number | boolean> = {};
    if (attributes) {
        for (const [k, v] of Object.entries(attributes)) {
            if (v === undefined || v === null) continue;
            clean[k] = v;
        }
    }
    return clean;
}

export function logEvent(level: LogLevel, message: string, attributes?: Attrs) {
    const clean = cleanAttrs(attributes);
    // 每条日志都带上 trace_id：用户截图里的这个 id 能直接串到后端日志
    clean.trace_id = currentTraceId || 'none';

    switch (level) {
        case 'trace':
            Sentry.logger.trace(message, clean);
            break;
        case 'debug':
            Sentry.logger.debug(message, clean);
            break;
        case 'info':
            Sentry.logger.info(message, clean);
            break;
        case 'warn':
            Sentry.logger.warn(message, clean);
            break;
        case 'error':
            Sentry.logger.error(message, clean);
            break;
        case 'fatal':
            Sentry.logger.fatal(message, clean);
            break;
    }
}

/**
 * Sentry Application Metrics（v10 的 metrics API：count / gauge / distribution）。
 *
 * 和 Logs 的分工，不是重复：
 *   Logs    回答「什么时候发生了什么」——一条事件一行，看时间线；
 *   Metrics 回答「有多少、多快」——数字，用来做面板和告警阈值。
 * 同一个 api_error 两边都发，但看它的目的完全不同。
 *
 * 这里只用两种：
 *   count        —— 可累加的整数（启动次数、接口失败次数）
 *   distribution —— 需要分位数的数（Web Vitals 的毫秒值）
 * gauge 用来表示「当前值」（如内存占用），本项目前端没有这种量，不用。
 */
export function countMetric(name: string, value = 1, attributes?: Attrs) {
    Sentry.metrics.count(name, value, { attributes: cleanAttrs(attributes) });
}

export function distributionMetric(name: string, value: number, unit: string, attributes?: Attrs) {
    Sentry.metrics.distribution(name, value, { unit, attributes: cleanAttrs(attributes) });
}

export function initMonitoring() {
    if (DSN) {
        Sentry.init({
            dsn: DSN,
            environment: ENV,
            release: RELEASE,

            // 错误全采，性能按 TRACES_RATE（默认全采 1.0，见文件头注释）
            tracesSampler: (ctx) => {
                if (ctx.name?.includes('/healthz')) return 0;
                return TRACES_RATE;
            },
            sampleRate: 1.0,

            // ── 浏览器端性能剖析（Profiles）──
            // 需要两样东西同时成立，缺一个 Profiles 页就是空的：
            //   1. 这个集成 + 采样率；
            //   2. 响应头 `Document-Policy: js-profiling`
            //      （Chrome 的 JS Self-Profiling API 受这个策略门控，
            //       没有它浏览器直接拒绝采集，SDK 侧不会报错，只是永远没数据）。
            //       dev 在 vite.config.ts，生产在 nginx 配置里，两处都要有。
            profileSessionSampleRate: PROFILE_RATE,
            profileLifecycle: 'trace',

            // 结构化日志。10.71 起 SDK 默认就开着，这里显式写出来是因为
            // 「Sentry Logs 为什么有/没有数据」是排查时会问的第一个问题 ——
            // 写在这比让人去翻 SDK 版本行为更省事。
            // 注意：开了也只是开通道，实际发不发取决于有没有人调 `Sentry.logger.*`，
            // 本项目只有 `logEvent()` 会调（见上面的说明）。
            enableLogs: true,

            // 生产不给用户看 Sentry 的反馈按钮，避免暴露内部系统
            integrations: [Sentry.browserTracingIntegration(), Sentry.browserProfilingIntegration()],

            beforeSend(event) {
                // URL 只保留 origin + pathname
                if (event.request?.url) event.request.url = scrubUrl(event.request.url);
                if (event.request?.headers) event.request.headers = scrubHeaders(event.request.headers);

                // 面包屑里的 URL 同样要清
                if (event.breadcrumbs) {
                    event.breadcrumbs = event.breadcrumbs.map((b) => {
                        if (b.data?.url) b.data.url = scrubUrl(String(b.data.url));
                        if (b.data?.from) b.data.from = scrubUrl(String(b.data.from));
                        if (b.data?.to) b.data.to = scrubUrl(String(b.data.to));
                        return b;
                    });
                }

                // 把 trace_id 挂上去：用户报错时截图里就有这个 id
                event.tags = { ...event.tags, trace_id: currentTraceId || 'none' };
                return event;
            },

            ignoreErrors: [
                // 浏览器插件注入的报错，不是我们代码的问题，占了错误量大头
                'ResizeObserver loop limit exceeded',
                'Non-Error promise rejection captured',
                /^AbortError/,
                // 联调/演练脚本故意抛的错误。Sentry 默认给每条新错误发邮件，
                // 验证链路时会扰民（2026-11-27 真实发生：一条测试错误触发了告警邮件）。
                // 用固定前缀识别，业务错误不会被误伤。
                /e2e-sentry-verify/,
            ],
        });
    }

    initVitals();
    initPageView();

    // 启动即写一条 info 日志。
    // 作用不只是「记录启动」：Sentry 的 Logs 页在收到第一条日志之前会一直显示
    // 「Set up the SDK」向导，看起来像没配好。这条日志让页面立刻变成真实数据视图，
    // 之后排查时也能一眼看到「这次会话是从哪个版本起来的」。
    logEvent('info', 'app_started', {
        release: RELEASE,
        environment: ENV,
        path: window.location.pathname,
        lang: navigator.language,
    });
    countMetric('app_start', 1, { release: RELEASE, environment: ENV });
}

/**
 * PV 上报：每次路由变化记一条 page_view。
 *
 * 为什么 PV 要单独做而 Web Vitals 不用：
 *   Web Vitals 由浏览器在页面加载时自动触发一次，而 PV 是「用户切到了哪个页面」，
 *   单页应用里路由切换不会重新加载页面，必须自己监听。
 *
 * 关于 UV：**前端算不了可靠的 UV**。
 *   真正准确的 UV 要服务端按 IP + 会话去重，而本项目刻意不存 IP（隐私 + 日志卫生）。
 *   这里只上报 PV 原始事件；UV 的近似值请用 beacon 的 `rating` 维度或
 *   在 Grafana 里按时间窗口估算，详见 README「PV/UV 在哪看」。
 *
 * 只在 pathname 变化时发：search 变化（翻页、筛选）不算新页面，
 * 否则一次翻页就多一个 PV，数据会虚高得离谱。
 */
function initPageView() {
    if (typeof window === 'undefined') return;

    let lastPath = '';
    const fire = (path: string) => {
        if (path === lastPath) return;
        lastPath = path;
        // 路由切换记一条 debug 级日志：排查「用户在哪个页面出的错」时，
        // Logs 里能直接看到时间线上的页面切换，不用去猜。
        // 用 debug 而不是 info，是为了让 Logs 默认视图不被 PV 刷屏 ——
        // 需要时按 level 过滤即可。
        logEvent('debug', 'page_view', { path });
        beacon([
            {
                kind: 'page_view',
                name: 'page',
                value: 1,
                rating: 'good',
                url: path,
                trace_id: currentTraceId,
            },
        ]);
    };

    fire(window.location.pathname);

    // 拦截 history 的 pushState/replaceState：react-router 内部就是调这两个，
    // 但不会触发任何事件，所以必须自己包一层。
    const wrap = (fn: History['pushState']) =>
        function (this: History, ...args: Parameters<History['pushState']>) {
            const ret = fn.apply(this, args);
            // 放到微任务后，等 react-router 把 location 更新完
            queueMicrotask(() => fire(window.location.pathname));
            return ret;
        };
    history.pushState = wrap(history.pushState);
    history.replaceState = wrap(history.replaceState);

    // 浏览器前进/后退不经过 pushState，单独监听
    window.addEventListener('popstate', () => fire(window.location.pathname));
}

/** Web Vitals → Sentry 指标 + 自建 beacon */
function initVitals() {
    const report = (m: Metric) => {
        // 挂在面包屑上：当次会话后续报错时，这些值会一起带进 Sentry 事件，
        // 方便判断「用户是在一个卡顿严重的页面上报的错」。
        Sentry.addBreadcrumb({
            category: 'web-vital',
            level: 'info',
            message: `${m.name}=${m.value.toFixed(2)} (${m.rating})`,
            data: { metric: m.name, value: m.value, rating: m.rating },
        });

        // 进 Sentry Metrics：这样能在 Metrics 页按分位数看 LCP/INP 的分布，
        // 而且和同一时间窗口的 error 放在一起对照 —— Prometheus 那边只有
        // 分档计数，看不到真实分布的 p75/p95。
        // CLS 是无量纲的分数，其余都是毫秒；单位错了面板上的聚合会失真。
        distributionMetric('web_vital', m.value, m.name === 'CLS' ? 'none' : 'millisecond', {
            metric: m.name,
            rating: m.rating,
            path: window.location.pathname,
        });

        // 兜底 beacon：Sentry 超额或未配置时，指标仍能进 Prometheus。
        beacon([
            {
                kind: 'web_vital',
                name: m.name,
                value: Math.round(m.value * 1000) / 1000,
                rating: m.rating === 'needs-improvement' ? 'needs-improvement' : m.rating,
                url: window.location.pathname,
                trace_id: currentTraceId,
            },
        ]);
    };

    onLCP(report);
    onCLS(report);
    onINP(report);
    onFCP(report);
    onTTFB(report);
}

interface BeaconEvent {
    kind: string;
    name?: string;
    value?: number;
    rating?: string;
    url?: string;
    trace_id?: string;
}

/**
 * 上报队列：合并短时间内的多个事件成一批。
 *
 * 为什么不在每个错误里直接 sendBeacon：
 * 页面崩溃时会有十几条错误同时涌出，逐个发请求本身就成了新的性能问题，
 * 而且浏览器对同域并发连接有限制，会把真正重要的请求挤掉。
 */
let queue: BeaconEvent[] = [];
let flushTimer: ReturnType<typeof setTimeout> | null = null;

export function beacon(events: BeaconEvent[]) {
    queue.push(...events);
    if (flushTimer) return;
    flushTimer = setTimeout(() => {
        const batch = queue.slice(0, 50);
        queue = [];
        flushTimer = null;
        if (!batch.length) return;
        send(batch);
    }, 1500);
}

function send(batch: BeaconEvent[]) {
    const payload = JSON.stringify({ events: batch });
    try {
        // sendBeacon 的语义就是「页面走了也要发出去」，正好对应卸载时的上报需求
        if (navigator.sendBeacon) {
            const blob = new Blob([payload], { type: 'application/json' });
            const ok = navigator.sendBeacon(BEACON_URL, blob);
            if (ok) return;
        }
    } catch {
        /* 落到 fetch */
    }
    // keepalive 让请求在页面卸载后仍能完成
    void fetch(BEACON_URL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: payload,
        keepalive: true,
    }).catch(() => {
        /* 上报失败不影响业务 */
    });
}

/** 业务错误也上报 —— 后端 5xx 在前端是最直接的感知点 */
export function reportApiError(path: string, status: number, traceId?: string) {
    // 注意两套枚举不一样：captureMessage 用的是 SeverityLevel（'warning'），
    // logger 用的是日志级别（'warn'）。写成同一个值会有一个编译不过。
    const severity = status >= 500 ? 'error' : 'warning';
    const logLevel = status >= 500 ? 'error' : 'warn';
    Sentry.captureMessage(`API ${status} ${path}`, {
        level: severity,
        tags: { trace_id: traceId || currentTraceId || 'none', api_path: path },
    });
    // 同时写一条结构化日志。Issues 里是「这个错误发生了多少次」，
    // Logs 里是「按时间排开发生了什么」—— 两者用的是同一份数据，视角不同。
    logEvent(logLevel, 'api_error', { api_path: path, status, trace_id: traceId || undefined });
    countMetric('api_error', 1, { api_path: path, status });
    beacon([
        {
            kind: 'api_error',
            name: status >= 500 ? 'fetch' : 'xhr',
            url: window.location.pathname,
            trace_id: traceId,
        },
    ]);
}

export { Sentry };
