/**
 * HTTP 入口：一个 POST 接口 + 一个健康检查。
 *
 * 用 node:http 而不是 Fastify，就为了**零运行时依赖**：
 * 这个服务只做一件事——把一次模型对话包成 SSE。
 * 引一个框架换来的路由和插件能力，这里一个都用不上，却要多背一份升级成本。
 *
 * 错误分三种出口，别混：
 *   - 401：没有 token，普通 JSON 响应
 *   - 400：请求体不合法，普通 JSON 响应
 *   - 其余准入拒绝（开关 / 限流 / 预算）：**200 + SSE error 事件**
 *     前端只有一条信号通路，让它根据 HTTP 状态码再分一套处理不值得
 */

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { readFileSync } from 'node:fs';

import type { AgentEvent, ErrorReason } from '../../shared/contract.js';
import { createFakeProvider, createHttpProvider, type Provider } from './provider.js';
import { loadConfig, type AgentConfig } from './config.js';
import { createToolExecutor } from './tools.js';
import { Guard } from './guard.js';
import { runAgent } from './loop.js';
import type { CheckInfo } from './prompt.js';
import { hashUser, newTraceId, Tracer } from './trace.js';
import { renderMetrics } from './metrics.js';
import {
    DeniedTotal,
    RequestTotal,
    recordOutcome,
    startRuntimeSampling,
    syncRuntime,
    withToolMetrics,
} from './instrument.js';

const MAX_BODY_BYTES = 64 * 1024;
const MAX_QUESTION_CHARS = 500;
const PING_INTERVAL_MS = 15_000;
const CHECKS_TTL_MS = 5 * 60_000;

const cfg = loadConfig();

if (!cfg.enabled) {
    process.stderr.write('[agent] AGENT_ENABLED=false，接口会一律返回「未启用」\n');
}

// 假模型剧本在启动时读一次：路径写错要在进程起来时就炸，
// 而不是等第一个用户提问才发现「回放是空的」。
const fakeScript = cfg.fakeModel ? loadFakeScript(cfg.fakeScript) : [];
if (cfg.fakeModel) {
    process.stderr.write(`[agent] 假模型回放：${cfg.fakeScript || '(空剧本)'} → ${fakeScript.length} 步\n`);
}

const tracer = new Tracer({ file: cfg.traceFile });
const guard = new Guard({
    enabled: cfg.enabled,
    maxInflight: cfg.maxInflight,
    userRpm: cfg.userRpm,
    dailyBudgetCny: cfg.dailyBudgetCny,
    toggleTtlSec: cfg.toggleTtlSec,
    backendUrl: cfg.backendUrl,
});

let checksCache: CheckInfo[] | null = null;
let checksCachedAt = 0;

const server = createServer((req, res) => {
    void handle(req, res).catch((err) => {
        // 到这说明是没预料到的路径（比如写响应时连接已断），别把进程带崩
        process.stderr.write(`[agent] 未处理异常: ${(err as Error).message}\n`);
        if (!res.headersSent) {
            sendJson(res, 500, { error: 'internal' });
        } else {
            res.end();
        }
    });
});

server.listen(cfg.port, () => {
    // 事件循环延迟采样跟着进程一起起：/metrics 是拉取式，
    // 没有采样器的话 gauge 永远是 0，比没有这个指标更误导。
    startRuntimeSampling();
    process.stderr.write(
        `[agent] listening on :${cfg.port} model=${cfg.fakeModel ? 'fake' : cfg.model} ` +
            `backend=${cfg.backendUrl} budget=${cfg.dailyBudgetCny}元/日\n`,
    );
});

async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const url = new URL(req.url ?? '/', 'http://localhost');

    if (req.method === 'GET' && url.pathname === '/healthz') {
        sendJson(res, 200, {
            ok: true,
            enabled: cfg.enabled,
            model: cfg.fakeModel ? 'fake' : cfg.model,
            active: guard.active,
            spent_cny: Number(guard.spent.toFixed(4)),
        });
        return;
    }

    // 指标端点。**只在容器网段内可达**：nginx 只转发 /api/agent/ 前缀，
    // /metrics 不在其中，所以公网摸不到；Prometheus 通过 observ 网段直连。
    // 和 gateway 的 /metrics 一样不再加鉴权——多一层 token 只会多一个会过期的东西。
    if (req.method === 'GET' && url.pathname === '/metrics') {
        syncRuntime({ inflight: guard.active, spentCny: guard.spent });
        sendText(res, 200, renderMetrics());
        return;
    }

    if (req.method !== 'POST' || url.pathname !== '/api/agent/diagnose') {
        sendJson(res, 404, { error: 'not_found' });
        return;
    }

    const token = bearer(req);
    if (!token) {
        RequestTotal.inc({ result: 'unauthorized' });
        sendJson(res, 401, { error: 'unauthorized' });
        return;
    }

    let body: unknown;
    try {
        body = await readJson(req);
    } catch (err) {
        RequestTotal.inc({ result: 'bad_request' });
        sendJson(res, (err as HttpError).status ?? 400, { error: (err as Error).message });
        return;
    }

    const parsed = parseBody(body);
    if (typeof parsed === 'string') {
        RequestTotal.inc({ result: 'bad_request' });
        sendJson(res, 400, { error: parsed });
        return;
    }

    const userId = hashUser(token);
    const traceId = newTraceId();
    const startedAt = Date.now();

    // 先验一次票再花钱。
    //
    // 鉴权本身在网关（agent 不持有服务账号），但「把 token 透传给后端」
    // 只会在工具调用时才被检验 —— 那时模型已经被调过一轮，钱已经花了。
    // 随便造一个 token 就能让我们付一次模型费，这条路径必须提前关掉：
    // 只有明确的 401/403 才拒，网关抖动照旧放行（真正的权限问题
    // 会在工具调用时再次暴露，不该因为网关抖一下就整个功能不可用）。
    if (await isRejectedByBackend(token)) {
        RequestTotal.inc({ result: 'unauthorized' });
        sendJson(res, 401, { error: 'unauthorized' });
        tracer.write({
            trace_id: traceId,
            user_hash: userId,
            question_len: parsed.question.length,
            gen_ai_model: cfg.fakeModel ? 'fake' : cfg.model,
            checks: [],
            turns: 0,
            stop_reason: 'degraded',
            error_reason: 'unavailable',
            cost_cny: 0,
            tokens: { input: 0, output: 0, cache_read: 0 },
            latency_ms: Date.now() - startedAt,
            tool_failures: 0,
            rejected_refs: 0,
        });
        return;
    }

    const decision = await guard.check(userId, token);
    if (!decision.ok) {
        DeniedTotal.inc({ reason: decision.reason });
        RequestTotal.inc({ result: 'denied' });
        // 准入拒绝也用 SSE 回，前端不用为「状态码 429」单独写一套
        startSSE(res, traceId);
        writeEvent(res, {
            type: 'error',
            reason: mapDeny(decision.reason),
            message: decision.detail,
        });
        res.end();
        tracer.write({
            trace_id: traceId,
            user_hash: userId,
            question_len: parsed.question.length,
            gen_ai_model: cfg.fakeModel ? 'fake' : cfg.model,
            checks: [],
            turns: 0,
            stop_reason: 'degraded',
            error_reason: mapDeny(decision.reason),
            cost_cny: 0,
            tokens: { input: 0, output: 0, cache_read: 0 },
            latency_ms: Date.now() - startedAt,
            tool_failures: 0,
            rejected_refs: 0,
        });
        return;
    }

    const lease = decision.lease;
    let closed = false;
    const controller = new AbortController();
    const onClose = (): void => {
        if (closed) return;
        closed = true;
        controller.abort();
    };
    // 客户端关页面 / 刷新时立刻停模型调用，别继续计费
    req.on('close', onClose);

    startSSE(res, traceId);
    const ping = setInterval(() => {
        if (!res.writableEnded) res.write(': ping\n\n');
    }, PING_INTERVAL_MS);

    let toolFailures = 0;
    let errorReason: ErrorReason | undefined;

    try {
        const checks = await getChecks(token);
        const provider = buildProvider();

        const outcome = await runAgent(
            parsed.question,
            parsed.context,
            {
                provider,
                executor: withToolMetrics(createToolExecutor({ backendUrl: cfg.backendUrl, token })),
                checks,
                limits: {
                    maxTurns: cfg.maxTurns,
                    totalTimeoutMs: cfg.totalTimeoutMs,
                    toolTimeoutMs: cfg.toolTimeoutMs,
                    // 单轮最多 4 个：再多就不是「查清楚」而是「把后端当数据库扫」
                    maxParallelTools: 4,
                    price: cfg.price,
                },
                traceId,
                signal: controller.signal,
                onEvent: (e) => {
                    if (e.type === 'step' && e.status === 'failed') toolFailures++;
                    writeEvent(res, e);
                },
            },
        );

        lease.addCost(outcome.costCny);
        errorReason = outcome.errorReason;
        // 埋点放在计费之后：口径是「已发生的调用」，中途 abort 的那次
        // 钱也花了，只是 stop_reason 会落在 cancelled 上。
        recordOutcome(outcome, Date.now() - startedAt);

        tracer.write({
            trace_id: traceId,
            user_hash: userId,
            question_len: parsed.question.length,
            gen_ai_model: provider.model,
            checks: outcome.ledger.checks,
            turns: outcome.turns,
            stop_reason: outcome.stopReason,
            ...(errorReason ? { error_reason: errorReason } : {}),
            cost_cny: Number(outcome.costCny.toFixed(6)),
            tokens: {
                input: outcome.usage.inTokens,
                output: outcome.usage.outTokens,
                cache_read: outcome.usage.cacheHitTokens,
            },
            latency_ms: Date.now() - startedAt,
            tool_failures: toolFailures,
            rejected_refs: outcome.issues.length,
        });
    } catch (err) {
        errorReason = controller.signal.aborted ? 'aborted' : 'server';
        // abort 是用户自己关页面，不是故障，不记进错误率
        RequestTotal.inc({ result: controller.signal.aborted ? 'cancelled' : 'error' });
        process.stderr.write(`[agent] ${traceId} 运行失败: ${(err as Error).message}\n`);
        if (!res.writableEnded) {
            writeEvent(res, {
                type: 'error',
                reason: errorReason,
                message: '诊断服务出错了，请稍后重试',
            });
        }
    } finally {
        clearInterval(ping);
        lease.release();
        req.off('close', onClose);
        if (!res.writableEnded) res.end();
    }
}

/**
 * 读假模型剧本：一个字符串数组，每项是一轮的原始返回。
 *
 * 不在这里做「缺文件就静默空跑」——那会让演练看起来正常、实际第一步就耗尽。
 * 剧本内容本身不解析（解析在 fake provider 里做），这里只保证形状对。
 */
function loadFakeScript(path: string): string[] {
    if (!path) return [];
    const raw = readFileSync(path, 'utf8');
    let parsed: unknown;
    try {
        parsed = JSON.parse(raw);
    } catch {
        throw new Error(`AGENT_FAKE_SCRIPT 不是合法 JSON: ${path}`);
    }
    if (!Array.isArray(parsed) || parsed.some((x) => typeof x !== 'string')) {
        throw new Error(`AGENT_FAKE_SCRIPT 必须是字符串数组: ${path}`);
    }
    return parsed as string[];
}

// ── 请求解析 ────────────────────────────────────────────────

interface DiagnoseBody {
    question: string;
    context: { product?: string; route?: string };
}

function parseBody(body: unknown): DiagnoseBody | string {
    if (!body || typeof body !== 'object' || Array.isArray(body)) return '请求体必须是 JSON 对象';
    const obj = body as Record<string, unknown>;

    const question = typeof obj.question === 'string' ? obj.question.trim() : '';
    if (!question) return 'question 不能为空';
    if (question.length > MAX_QUESTION_CHARS) {
        return `question 不能超过 ${MAX_QUESTION_CHARS} 字`;
    }

    const ctx: { product?: string; route?: string } = {};
    if (obj.context && typeof obj.context === 'object' && !Array.isArray(obj.context)) {
        const c = obj.context as Record<string, unknown>;
        if (typeof c.product === 'string') ctx.product = c.product.slice(0, 100);
        if (typeof c.route === 'string') ctx.route = c.route.slice(0, 200);
    }

    return { question, context: ctx };
}

class HttpError extends Error {
    constructor(
        readonly status: number,
        message: string,
    ) {
        super(message);
        this.name = 'HttpError';
    }
}

function readJson(req: IncomingMessage): Promise<unknown> {
    return new Promise((resolve, reject) => {
        const chunks: Buffer[] = [];
        let size = 0;

        req.on('data', (chunk: Buffer) => {
            size += chunk.length;
            if (size > MAX_BODY_BYTES) {
                reject(new HttpError(413, '请求体过大'));
                req.destroy();
                return;
            }
            chunks.push(chunk);
        });
        req.on('end', () => {
            const raw = Buffer.concat(chunks).toString('utf8');
            if (!raw) {
                resolve({});
                return;
            }
            try {
                resolve(JSON.parse(raw));
            } catch {
                reject(new HttpError(400, 'JSON 解析失败'));
            }
        });
        req.on('error', reject);
    });
}

function bearer(req: IncomingMessage): string {
    const raw = req.headers.authorization ?? '';
    return raw.startsWith('Bearer ') ? raw.slice(7).trim() : '';
}

// ── 响应 ────────────────────────────────────────────────────

function sendJson(res: ServerResponse, status: number, payload: unknown): void {
    const body = JSON.stringify(payload);
    res.writeHead(status, {
        'content-type': 'application/json; charset=utf-8',
        'content-length': Buffer.byteLength(body),
    });
    res.end(body);
}

/**
 * Prometheus 文本 exposition 的 content-type 必须带 version：
 * 不带的话 Prometheus 会按最老的 0.0.4 解析（碰巧也是这个），
 * 但显式声明能让 `curl -I` 一眼看出这是指标端点而不是普通文本。
 */
function sendText(res: ServerResponse, status: number, body: string): void {
    res.writeHead(status, {
        'content-type': 'text/plain; version=0.0.4; charset=utf-8',
        'content-length': Buffer.byteLength(body),
    });
    res.end(body);
}

function startSSE(res: ServerResponse, traceId: string): void {
    res.writeHead(200, {
        'content-type': 'text/event-stream; charset=utf-8',
        'cache-control': 'no-cache, no-transform',
        connection: 'keep-alive',
        'x-accel-buffering': 'no',
        'x-trace-id': traceId,
    });
    // 立刻写一行，让前端 fetch 的 reader 马上拿到首个 chunk
    res.write(': connected\n\n');
}

function writeEvent(res: ServerResponse, e: AgentEvent): void {
    if (res.writableEnded) return;
    res.write(`data: ${JSON.stringify(e)}\n\n`);
}

// ── 依赖装配 ────────────────────────────────────────────────

function buildProvider(): Provider {
    if (cfg.fakeModel) {
        // 演练用：剧本来自 AGENT_FAKE_SCRIPT（通常是 testdata/fake_script.json），
        // 没给就空回放（第一次调用就报剧本耗尽）
        return createFakeProvider({ script: fakeScript });
    }
    return createHttpProvider({
        baseUrl: cfg.baseUrl,
        apiKey: cfg.apiKey,
        model: cfg.model,
        fallbackModel: cfg.fallbackModel,
    });
}

/**
 * 用最便宜的一个受保护接口验票。
 *
 * 选择 /api/diag/checks 的理由：它不带参数、不查库、在网关鉴权链路上，
 * 验票成本就是一个进程内请求。失败（超时、5xx、网络不通）一律当作
 * 「验不了」放行，只有后端明确说 401/403 才算这个 token 不可用。
 */
async function isRejectedByBackend(token: string): Promise<boolean> {
    try {
        const res = await fetch(`${cfg.backendUrl}/api/diag/checks`, {
            headers: { authorization: `Bearer ${token}` },
            signal: AbortSignal.timeout(2_000),
        });
        return res.status === 401 || res.status === 403;
    } catch {
        return false;
    }
}

/**
 * 规则目录。
 *
 * `/api/diag/checks` 需要用户身份，所以只能在请求里拉，并按 TTL 缓存。
 * 拉不到时返回**上一次成功的值**；从没成功过就返回空数组——
 * 空目录会让模型直接说「没有可用的检查」，这比编几个规则名安全。
 */
async function getChecks(token: string): Promise<CheckInfo[]> {
    const now = Date.now();
    if (checksCache && now - checksCachedAt < CHECKS_TTL_MS) return checksCache;

    try {
        const res = await fetch(`${cfg.backendUrl}/api/diag/checks`, {
            headers: { authorization: `Bearer ${token}` },
            signal: AbortSignal.timeout(3_000),
        });
        if (!res.ok) throw new Error(`checks 接口返回 ${res.status}`);

        const json = (await res.json()) as { data?: unknown };
        // 网关这个接口把 items 放在**顶层**（{code,total,items}），
        // 其他接口才包在 data 里。两种形状都认，免得少写一层就永远是空目录。
        const list = normalizeChecks(json?.data ?? json);
        if (list.length > 0) {
            checksCache = list;
            checksCachedAt = now;
            return list;
        }
        return checksCache ?? [];
    } catch {
        return checksCache ?? [];
    }
}

function normalizeChecks(data: unknown): CheckInfo[] {
    const nested = data as { checks?: unknown; items?: unknown } | null;
    const rows = Array.isArray(data)
        ? data
        : Array.isArray(nested?.items)
          ? (nested.items as unknown[])
          : Array.isArray(nested?.checks)
            ? (nested.checks as unknown[])
            : [];

    const out: CheckInfo[] = [];
    for (const row of rows) {
        if (!row || typeof row !== 'object') continue;
        const r = row as Record<string, unknown>;
        if (typeof r.name !== 'string' || !r.name) continue;
        out.push({
            name: r.name,
            title: typeof r.title === 'string' ? r.title : r.name,
            domain: typeof r.domain === 'string' ? r.domain : '',
            params: normalizeParams(r.params),
            ...(typeof r.cost_hint === 'string' ? { cost_hint: r.cost_hint } : {}),
            ...(typeof r.timeout_ms === 'number' ? { timeout_ms: r.timeout_ms } : {}),
        });
    }
    return out;
}

function normalizeParams(
    raw: unknown,
): Record<string, { type: string; required: boolean }> | undefined {
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return undefined;
    const out: Record<string, { type: string; required: boolean }> = {};
    for (const [k, v] of Object.entries(raw as Record<string, unknown>)) {
        if (!v || typeof v !== 'object') continue;
        const p = v as Record<string, unknown>;
        out[k] = {
            type: typeof p.type === 'string' ? p.type : 'string',
            required: p.required === true,
        };
    }
    return Object.keys(out).length > 0 ? out : undefined;
}

function mapDeny(reason: 'disabled' | 'concurrency' | 'rpm' | 'budget' | 'unavailable'): ErrorReason {
    if (reason === 'budget') return 'budget';
    if (reason === 'disabled') return 'unavailable';
    if (reason === 'rpm' || reason === 'concurrency') return 'unavailable';
    return 'unavailable';
}

// 让 typecheck 知道 cfg 被用于启动日志，避免「未使用」误判
export type { AgentConfig };
