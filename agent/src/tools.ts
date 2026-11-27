/**
 * 工具层：后端接口的薄包装 + 路径白名单。
 *
 * 两条硬约束：
 *
 * 1. **白名单按具体路径逐条列举，不按方法放行。**
 *    `POST /api/benefit/expire-rule` 是真实存在且会写库的接口，
 *    只要有一次「POST 都放行」，模型就能改数据。
 *
 * 2. **工具失败和「查了但结果为空」是两种状态。**
 *    前者 `{ok:false, reason}`，后者 `{ok:true, data:[]}`。
 *    混成一种，模型会把「网络超时」写成「该产品没有配置」。
 */

import type { CheckResult } from '../../shared/contract.js';

export type ToolName =
    | 'run_check'
    | 'run_checks'
    | 'search_records'
    | 'list_products'
    | 'pick_product'
    | 'list_rules'
    | 'explain_rule'
    | 'simulate_rules';

export interface ToolSpecDef {
    name: ToolName;
    description: string;
    parameters: Record<string, unknown>;
}

export interface ToolRequest {
    name: string;
    args: unknown;
    /** 单工具超时（默认 8s），不看总预算 */
    timeoutMs: number;
}

export type ToolResult =
    | { ok: true; data: unknown }
    | { ok: false; reason: string; message: string };

export interface ToolExecutorOptions {
    backendUrl: string;
    /** 用户 JWT，原样透传；agent 本身不持有服务账号 */
    token: string;
    fetchImpl?: typeof fetch;
    /** 单次结果上限 */
    maxRows?: number;
    maxBytes?: number;
}

const MAX_ROWS = 50;
const MAX_BYTES = 20 * 1024;

/** 实体 → 后端路径 + 允许回给模型的字段。 */
export const ENTITIES: Record<string, { path: string; fields: string[] }> = {
    spu: { path: '/api/catalog/spu', fields: ['uid', 'title', 'kind', 'amount', 'state'] },
    sku: { path: '/api/catalog/sku', fields: ['uid', 'label', 'product', 'amount', 'state'] },
    card: { path: '/api/catalog/cards', fields: ['uid', 'title', 'variant', 'client', 'state'] },
    pricing: {
        path: '/api/catalog/pricing',
        fields: ['uid', 'title', 'product', 'client', 'state'],
    },
    bind: { path: '/api/catalog/binds', fields: ['uid', 'policy_uid', 'card_uid', 'ordinal'] },
    tier: {
        path: '/api/benefit/tiers',
        fields: ['uid', 'label', 'product', 'rank', 'state', 'online'],
    },
    group: { path: '/api/benefit/groups', fields: ['uid', 'title', 'product', 'kind', 'state'] },
    rule: {
        path: '/api/benefit/rules',
        fields: ['uid', 'title', 'group_uid', 'kind', 'quota', 'unit', 'state'],
    },
    task: {
        path: '/api/benefit/tasks',
        fields: ['uid', 'title', 'product', 'kind', 'threshold', 'state'],
    },
    checkout: { path: '/api/trade/checkouts', fields: ['uid', 'product', 'amount', 'state'] },
    merchant: { path: '/api/channel/merchants', fields: ['uid', 'title', 'state'] },
    node: { path: '/api/channel/nodes', fields: ['uid', 'title', 'product', 'state'] },
    campaign: {
        path: '/api/operation/campaigns',
        fields: ['uid', 'title', 'product', 'state', 'start_at', 'end_at'],
    },
    voucher: {
        path: '/api/operation/vouchers',
        fields: ['uid', 'title', 'batch_uid', 'face_amount', 'state'],
    },
    slot: { path: '/api/operation/slots', fields: ['uid', 'title', 'product', 'state'] },
    debit: { path: '/api/trade/debits', fields: ['uid', 'product', 'amount', 'state'] },
    promo: { path: '/api/trade/promos', fields: ['uid', 'title', 'product', 'state'] },
    attribution: {
        path: '/api/channel/attribution',
        fields: ['uid', 'channel', 'product', 'amount'],
    },
    toggle: { path: '/api/setting/toggles', fields: ['toggle_key', 'product', 'value', 'state'] },
    product: { path: '/api/setting/products', fields: ['title'] },
};

/** 写死的只读路径白名单。executor 只允许请求这里出现的 URL。 */
const PATH_WHITELIST = new Set<string>([
    ...Object.values(ENTITIES).map((e) => e.path),
    '/api/diag/checks',
    '/api/diag/batch',
    '/api/rules',
    '/api/rule-dry-run',
]);

/**
 * 不在实体表里、但建议里可以跳转的页面。
 *
 * 实体表只收录「有对应 list 接口」的数据源；新加的三个运维页面
 * （规则/作业/脱敏）是配置页，没有可检索的实体，所以单独列。
 * validate.ts 的 PAGE_WHITELIST 和 prompt.ts 的可跳转清单都引用这一份，
 * 避免两处各写一遍导致模型看到的路径和校验用的路径不一致。
 */
export const EXTRA_PAGES = ['/operation/rules', '/setting/jobs', '/setting/masking'] as const;

export const TOOL_SPECS: ToolSpecDef[] = [
    {
        name: 'run_check',
        description:
            '执行单条确定性诊断规则。规则名必须是 /api/diag/checks 返回的 name。' +
            'verdict 三值：ok 查过没问题 / issue 查过有问题 / inconclusive 数据不足判不了。',
        parameters: {
            type: 'object',
            additionalProperties: false,
            required: ['name'],
            properties: {
                name: { type: 'string', description: '规则名，例如 orphan_rules' },
                params: {
                    type: 'object',
                    additionalProperties: { type: 'string' },
                    description: '规则声明的参数，例如 {"product":"超级会员"}',
                },
            },
        },
    },
    {
        name: 'run_checks',
        description: '一次执行多条规则（体检）。最多 8 条。单条失败不影响其他条。',
        parameters: {
            type: 'object',
            additionalProperties: false,
            required: ['checks'],
            properties: {
                checks: { type: 'array', items: { type: 'string' }, maxItems: 8 },
                params: { type: 'object', additionalProperties: { type: 'string' } },
            },
        },
    },
    {
        name: 'search_records',
        description:
            '按实体查询管理台列表数据。只用于回答「有多少 / 有哪些」这类问题，' +
            '不做判定——判定走 run_check。',
        parameters: {
            type: 'object',
            additionalProperties: false,
            required: ['entity'],
            properties: {
                entity: { type: 'string', enum: Object.keys(ENTITIES) },
                filters: {
                    type: 'object',
                    additionalProperties: false,
                    properties: {
                        product: { type: 'string' },
                        keyword: { type: 'string' },
                        page: { type: 'integer', minimum: 1 },
                        page_size: { type: 'integer', minimum: 1, maximum: 50 },
                    },
                },
                limit: { type: 'integer', minimum: 1, maximum: 50 },
            },
        },
    },
    {
        name: 'list_products',
        description: '列出全部产品名，用于把用户嘴里的名字归一化成系统里的写法。',
        parameters: { type: 'object', additionalProperties: false, properties: {} },
    },
    {
        name: 'pick_product',
        description: '产品名对不上时，返回最接近的几个候选，让用户自己选。',
        parameters: {
            type: 'object',
            additionalProperties: false,
            required: ['query'],
            properties: { query: { type: 'string' } },
        },
    },
    {
        name: 'list_rules',
        description:
            '列出运营在页面上配置的规则（不是诊断规则）。用于找到某条规则后解释它为什么命中/没命中。',
        parameters: {
            type: 'object',
            additionalProperties: false,
            properties: {
                domain: { type: 'string', description: '业务域，留空表示全部' },
            },
        },
    },
    {
        name: 'explain_rule',
        description:
            '解释一条可配置规则为什么命中或没命中。返回每一步的「实际值 vs 期望值」。' +
            '需要 rule_id，可从 list_rules 获得。data 可选，不传用后端内置样例。',
        parameters: {
            type: 'object',
            additionalProperties: false,
            required: ['rule_id'],
            properties: {
                rule_id: { type: 'number', description: '规则 ID' },
                data: {
                    type: 'object',
                    additionalProperties: { type: ['string', 'number', 'boolean'] },
                    description: '待求值的字段表，键用点路径如 "tier.state"',
                },
            },
        },
    },
    {
        name: 'simulate_rules',
        description:
            '用一份样例数据试跑全部启用的规则，返回哪些命中、哪些没命中。' +
            '改规则之前先跑一遍，能提前发现误伤。',
        parameters: {
            type: 'object',
            additionalProperties: false,
            properties: {
                domain: { type: 'string', description: '只试跑某个业务域' },
                data: {
                    type: 'object',
                    additionalProperties: { type: ['string', 'number', 'boolean'] },
                    description: '待求值的字段表，不传用内置样例',
                },
            },
        },
    },
];

export interface ToolExecutor {
    execute(req: ToolRequest): Promise<ToolResult>;
}

export function createToolExecutor(opts: ToolExecutorOptions): ToolExecutor {
    const doFetch = opts.fetchImpl ?? fetch;
    const maxRows = opts.maxRows ?? MAX_ROWS;
    const maxBytes = opts.maxBytes ?? MAX_BYTES;

    function assertAllowed(pathname: string, extra: string[] = []): void {
        if (PATH_WHITELIST.has(pathname) || extra.includes(pathname)) return;
        throw new Error(`路径不在白名单内: ${pathname}`);
    }

    async function request(
        path: string,
        init: { method?: 'GET' | 'POST'; query?: Record<string, string>; body?: unknown },
        timeoutMs: number,
    ): Promise<unknown> {
        const url = new URL(opts.backendUrl + path);
        for (const [k, v] of Object.entries(init.query ?? {})) {
            if (v !== '') url.searchParams.set(k, v);
        }
        assertAllowed(url.pathname, [
            // run/:name 是动态段，白名单按前缀放行，但仍然逐条判
            ...(url.pathname.startsWith('/api/diag/run/') ? [url.pathname] : []),
            // 规则解释接口同样带动态 id，用严格正则放行——
            // 只允许「数字 id + /explain」这一种形状，防止拼出别的路径
            ...(/^\/api\/rules\/\d+\/explain$/.test(url.pathname) ? [url.pathname] : []),
        ]);

        const res = await doFetch(url, {
            method: init.method ?? 'GET',
            headers: {
                authorization: `Bearer ${opts.token}`,
                ...(init.body ? { 'content-type': 'application/json' } : {}),
            },
            body: init.body ? JSON.stringify(init.body) : undefined,
            signal: AbortSignal.timeout(timeoutMs),
        });

        if (!res.ok) {
            throw new ToolHttpError(res.status, `后端返回 ${res.status}`);
        }
        return await res.json();
    }

    return {
        async execute(req) {
            try {
                return await run(req);
            } catch (err) {
                // 单工具失败不阻断整轮：把失败作为一种**状态**交回模型
                if (err instanceof BadArgs) {
                    return { ok: false, reason: 'invalid_param', message: err.message };
                }
                if (err instanceof ToolHttpError) {
                    return { ok: false, reason: httpReason(err.status), message: err.message };
                }
                const name = (err as Error).name;
                if (name === 'TimeoutError' || name === 'AbortError') {
                    return { ok: false, reason: 'timeout', message: '工具调用超时' };
                }
                return { ok: false, reason: 'network', message: (err as Error).message };
            }
        },
    };

    async function run(req: ToolRequest): Promise<ToolResult> {
        const args = req.args;

        switch (req.name) {
            case 'run_check': {
                const a = asObject(args, ['name', 'params']);
                const name = requireString(a, 'name');
                const params = toStringMap(a.params);
                const json = await request(
                    `/api/diag/run/${encodeURIComponent(name)}`,
                    { query: params },
                    req.timeoutMs,
                );
                return { ok: true, data: envelope(json) };
            }

            case 'run_checks': {
                const a = asObject(args, ['checks', 'params']);
                const checks = a.checks;
                if (!Array.isArray(checks) || checks.length === 0) {
                    return { ok: false, reason: 'invalid_param', message: 'checks 不能为空' };
                }
                if (checks.length > 8) {
                    return { ok: false, reason: 'invalid_param', message: '一次最多 8 条' };
                }
                const json = await request(
                    '/api/diag/batch',
                    {
                        method: 'POST',
                        body: { checks: checks.map(String), params: toStringMap(a.params) },
                    },
                    req.timeoutMs,
                );
                return { ok: true, data: envelope(json) };
            }

            case 'search_records': {
                const a = asObject(args, ['entity', 'filters', 'limit']);
                const entity = requireString(a, 'entity');
                const def = ENTITIES[entity];
                if (!def) {
                    return {
                        ok: false,
                        reason: 'invalid_param',
                        message: `未声明的实体 ${entity}；可用：${Object.keys(ENTITIES).join(', ')}`,
                    };
                }
                const filters = toStringMap(a.filters);
                const limit = Math.min(Number(a.limit ?? maxRows) || maxRows, maxRows);
                const query: Record<string, string> = { ...filters };
                query.page_size = String(limit);
                const json = await request(def.path, { query }, req.timeoutMs);
                return { ok: true, data: trimRows(envelope(json), def.fields, maxRows, maxBytes) };
            }

            case 'list_products': {
                const json = await request('/api/setting/products', {}, req.timeoutMs);
                const rows = rowsOf(envelope(json));
                const trimmed = trimRows(envelope(json), ['title'], maxRows, maxBytes);
                // 模型要回答「一共多少个」，但它数不准一长串数组——
                // 实测问「现在一共有多少个产品」时它连调 5 次这个工具，
                // 因为每次都只给回一坨名单、没有现成的数字。把 count 直接给它。
                const data = Array.isArray(trimmed)
                    ? { count: rows.length, products: trimmed }
                    : { count: rows.length, ...(trimmed as Record<string, unknown>) };
                return { ok: true, data };
            }

            case 'pick_product': {
                const a = asObject(args, ['query']);
                const query = requireString(a, 'query');
                const json = await request('/api/setting/products', {}, req.timeoutMs);
                const names = extractNames(envelope(json));
                return {
                    ok: true,
                    data: { candidates: fuzzyPick(query, names, 5) },
                };
            }

            case 'list_rules': {
                const a = asObject(args, ['domain']);
                const query: Record<string, string> = {};
                if (typeof a.domain === 'string' && a.domain) query.domain = a.domain;
                const json = await request('/api/rules', { query }, req.timeoutMs);
                return { ok: true, data: envelope(json) };
            }

            case 'explain_rule': {
                const a = asObject(args, ['rule_id', 'data']);
                const id = Number(a.rule_id);
                if (!Number.isFinite(id) || id <= 0) {
                    throw new BadArgs('rule_id 必须是正整数');
                }
                const json = await request(
                    `/api/rules/${Math.trunc(id)}/explain`,
                    { method: 'POST', body: { data: a.data ?? {} } },
                    req.timeoutMs,
                );
                return { ok: true, data: envelope(json) };
            }

            case 'simulate_rules': {
                const a = asObject(args, ['domain', 'data']);
                const json = await request(
                    '/api/rule-dry-run',
                    {
                        method: 'POST',
                        body: {
                            domain: typeof a.domain === 'string' ? a.domain : '',
                            data: a.data ?? {},
                        },
                    },
                    req.timeoutMs,
                );
                return { ok: true, data: envelope(json) };
            }

            default:
                return {
                    ok: false,
                    reason: 'unknown_tool',
                    message: `没有这个工具：${req.name}`,
                };
        }
    }
}

class ToolHttpError extends Error {
    constructor(
        readonly status: number,
        message: string,
    ) {
        super(message);
        this.name = 'ToolHttpError';
    }
}

function httpReason(status: number): string {
    if (status === 401 || status === 403) return 'unauthorized';
    if (status === 400) return 'invalid_param';
    if (status === 429) return 'rate_limited';
    if (status >= 500) return 'server_error';
    return 'http_error';
}

/**
 * 参数校验是闭集：出现未声明的字段直接失败。
 * 静默丢弃会让模型以为调用成功了，然后基于一个没生效的参数编结论。
 */
function asObject(input: unknown, allowed: string[]): Record<string, unknown> {
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
        throw new BadArgs('参数必须是对象');
    }
    const obj = input as Record<string, unknown>;
    for (const key of Object.keys(obj)) {
        if (!allowed.includes(key)) throw new BadArgs(`不认识的参数 ${key}`);
    }
    return obj;
}

class BadArgs extends Error {}

function requireString(obj: Record<string, unknown>, key: string): string {
    const v = obj[key];
    if (typeof v !== 'string' || v === '') throw new BadArgs(`缺少 ${key}`);
    return v;
}

function toStringMap(v: unknown): Record<string, string> {
    if (v === undefined || v === null) return {};
    if (typeof v !== 'object' || Array.isArray(v)) return {};
    const out: Record<string, string> = {};
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
        if (typeof val === 'string' || typeof val === 'number') out[k] = String(val);
    }
    return out;
}

/** 后端信封 → 只取 data；没有 data 时把 items/list 原样交给调用方裁剪。 */
function envelope(json: unknown): unknown {
    if (json && typeof json === 'object' && 'data' in (json as Record<string, unknown>)) {
        return (json as Record<string, unknown>).data;
    }
    return json;
}

function rowsOf(data: unknown): unknown[] {
    if (!data || typeof data !== 'object') return [];
    const obj = data as Record<string, unknown>;
    for (const key of ['items', 'list', 'rows', 'results'] as const) {
        if (Array.isArray(obj[key])) return obj[key] as unknown[];
    }
    return Array.isArray(data) ? data : [];
}

/** 字段裁剪 + 行数 / 字节上限。截断要标注，不能让模型以为拿到了全集。 */
function trimRows(data: unknown, fields: string[], maxRows: number, maxBytes: number): unknown {
    const rows = rowsOf(data);
    const trimmed = rows.slice(0, maxRows).map((row) => {
        if (!row || typeof row !== 'object') return row;
        const src = row as Record<string, unknown>;
        const out: Record<string, unknown> = {};
        for (const f of fields) if (f in src) out[f] = src[f];
        return out;
    });

    let payload: unknown = trimmed;
    let json = JSON.stringify(payload);
    if (json.length > maxBytes) {
        // 按字节逐步砍，宁可少给也不让超长结果进上下文
        let n = trimmed.length;
        while (n > 0 && json.length > maxBytes) {
            n = Math.floor(n * 0.7);
            payload = trimmed.slice(0, n);
            json = JSON.stringify(payload);
        }
        return { rows: payload, truncated: true, shown: n, total: rows.length };
    }

    return rows.length > maxRows
        ? { rows: payload, truncated: true, shown: maxRows, total: rows.length }
        : payload;
}

function extractNames(data: unknown): string[] {
    const rows = rowsOf(data);
    const out: string[] = [];
    for (const r of rows) {
        if (typeof r === 'string') out.push(r);
        else if (r && typeof r === 'object') {
            const t = (r as Record<string, unknown>).title;
            if (typeof t === 'string') out.push(t);
        }
    }
    return out;
}

/** 轻量近似：包含匹配优先，其次按公共前缀长度排。不引第三方库。 */
export function fuzzyPick(query: string, candidates: string[], n: number): string[] {
    const q = query.trim();
    if (!q) return candidates.slice(0, n);
    const scored = candidates.map((c) => {
        let score = 0;
        if (c === q) score = 100;
        else if (c.includes(q) || q.includes(c)) score = 60;
        else {
            let i = 0;
            while (i < c.length && i < q.length && c[i] === q[i]) i++;
            score = i * 5;
        }
        return { c, score };
    });
    return scored
        .filter((s) => s.score > 0)
        .sort((a, b) => b.score - a.score)
        .slice(0, n)
        .map((s) => s.c);
}

/** 从后端 batch 结果里取出每条 check 的结论，供 ledger 记账。 */
export function asCheckResults(data: unknown): CheckResult[] {
    if (!data || typeof data !== 'object') return [];
    const results = (data as { results?: unknown }).results;
    return Array.isArray(results) ? (results as CheckResult[]) : [];
}
