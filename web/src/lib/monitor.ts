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
 *   - performance 采 0.1：性能数据的价值在分位数，10% 采样足够算出可信的 p75，
 *     而全采会让配额在半天内耗尽。这是「错误」和「性能」在成本上的本质差别。
 */

import * as Sentry from '@sentry/react';
import { onCLS, onFCP, onINP, onLCP, onTTFB, type Metric } from 'web-vitals';

const DSN = import.meta.env.VITE_SENTRY_DSN as string | undefined;
const ENV = (import.meta.env.VITE_SENTRY_ENV as string) || import.meta.env.MODE;
const RELEASE = (import.meta.env.VITE_APP_VERSION as string) || 'dev';
const BEACON_URL = '/api/telemetry/beacon';

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

export function initMonitoring() {
    if (DSN) {
        Sentry.init({
            dsn: DSN,
            environment: ENV,
            release: RELEASE,

            // 错误全采，性能采 10%（见文件头注释）
            tracesSampler: (ctx) => {
                if (ctx.name?.includes('/healthz')) return 0;
                return 0.1;
            },
            sampleRate: 1.0,

            // 生产不给用户看 Sentry 的反馈按钮，避免暴露内部系统
            integrations: [Sentry.browserTracingIntegration()],

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
            ],
        });
    }

    initVitals();
    initPageView();
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
    Sentry.captureMessage(`API ${status} ${path}`, {
        level: status >= 500 ? 'error' : 'warning',
        tags: { trace_id: traceId || currentTraceId || 'none', api_path: path },
    });
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
