/**
 * 统一请求封装。
 *
 * 三条硬约束，都是踩过坑才定下来的：
 *  1. 每个请求带 X-Trace-Id，出问题时前端能拿它去平台上搜后端日志；
 *  2. 401 统一跳登录，页面里不再各写一遍；
 *  3. 错误提示只在 http 层弹一次 —— 页面 catch 里再弹就会出现两条提示。
 */

const TOKEN_KEY = 'mc.console.ticket';

export interface ApiError extends Error {
    status: number;
    traceId?: string;
}

function newTraceId(): string {
    // crypto.randomUUID 在非安全上下文（http 内网）下不可用，所以兜底用 getRandomValues
    const b = new Uint8Array(16);
    if (typeof crypto !== 'undefined' && crypto.getRandomValues) {
        crypto.getRandomValues(b);
    } else {
        for (let i = 0; i < 16; i += 1) b[i] = Math.floor(Math.random() * 256);
    }
    return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

export const ticketStore = {
    get(): string {
        try {
            return localStorage.getItem(TOKEN_KEY) || '';
        } catch {
            return '';
        }
    },
    set(v: string) {
        try {
            localStorage.setItem(TOKEN_KEY, v);
        } catch {
            /* 隐私模式下写不了，忽略 */
        }
    },
    clear() {
        try {
            localStorage.removeItem(TOKEN_KEY);
        } catch {
            /* ignore */
        }
    },
};

/** 由 App 注入：鉴权失败时怎么跳登录，避免 http 层直接依赖路由 */
let onUnauthorized: (() => void) | null = null;
export function setUnauthorizedHandler(fn: () => void) {
    onUnauthorized = fn;
}

export interface RequestOptions extends Omit<RequestInit, 'body'> {
    query?: Record<string, string | number | undefined | null>;
    body?: unknown;
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
    const { query, body, headers, ...rest } = options;
    const traceId = newTraceId();

    let url = path;
    if (query) {
        const sp = new URLSearchParams();
        for (const [k, v] of Object.entries(query)) {
            if (v !== undefined && v !== null && v !== '') sp.set(k, String(v));
        }
        const qs = sp.toString();
        if (qs) url += (url.includes('?') ? '&' : '?') + qs;
    }

    const h: Record<string, string> = {
        'X-Trace-Id': traceId,
        Accept: 'application/json',
        ...(headers as Record<string, string> | undefined),
    };
    const ticket = ticketStore.get();
    if (ticket) h.Authorization = `Bearer ${ticket}`;
    if (body !== undefined) h['Content-Type'] = 'application/json';

    let res: Response;
    try {
        res = await fetch(url, {
            ...rest,
            headers: h,
            body: body === undefined ? undefined : JSON.stringify(body),
        });
    } catch (e) {
        const err = new Error('网络异常，请检查网络后重试') as ApiError;
        err.status = 0;
        err.traceId = traceId;
        throw err;
    }

    const serverTrace = res.headers.get('X-Trace-Id') || traceId;

    if (res.status === 401) {
        ticketStore.clear();
        onUnauthorized?.();
        const err = new Error('登录已失效，请重新登录') as ApiError;
        err.status = 401;
        err.traceId = serverTrace;
        throw err;
    }

    let payload: unknown = null;
    const text = await res.text();
    if (text) {
        try {
            payload = JSON.parse(text);
        } catch {
            payload = null;
        }
    }

    if (!res.ok) {
        const msg =
            (payload as { message?: string } | null)?.message || `请求失败（${res.status}）`;
        const err = new Error(msg) as ApiError;
        err.status = res.status;
        err.traceId = serverTrace;
        throw err;
    }

    const obj = payload as { code?: number; message?: string } | null;
    if (obj && typeof obj.code === 'number' && obj.code !== 0) {
        const err = new Error(obj.message || '操作失败') as ApiError;
        err.status = res.status;
        err.traceId = serverTrace;
        throw err;
    }

    return payload as T;
}

export const http = {
    get: <T>(path: string, query?: RequestOptions['query']) => request<T>(path, { query }),
    post: <T>(path: string, body?: unknown) => request<T>(path, { method: 'POST', body }),
};

// ── 后端返回结构 ───────────────────────────────────────────

export interface ListPayload<T> {
    code: number;
    items: T[];
    total: number;
    page: number;
    page_size: number;
    trace_id?: string;
}

export interface BlocksPayload<T extends Record<string, unknown>> {
    code: number;
    products: ProductItem[];
    blocks: T;
    degraded?: Record<string, string>;
    trace_id?: string;
}

export interface ProductItem {
    uid: number;
    code: string;
    title: string;
    display_title: string;
    state: number;
}
