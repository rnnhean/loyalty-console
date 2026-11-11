/**
 * 配置全部走环境变量，不读配置文件（与 Go 后端保持一致）。
 *
 * 启动自检放在这里，而不是散在 server.ts：配置错应该在进程起来时炸，
 * 而不是等第一个用户提问才发现。
 */

export interface Price {
    /** 元 / 百万 token */
    input: number;
    output: number;
    /** 缓存读价。0 表示没有这项（按输入价算，保守）。 */
    cacheRead: number;
}

export interface AgentConfig {
    enabled: boolean;
    model: string;
    fallbackModel: string;
    baseUrl: string;
    apiKey: string;
    price: Price;
    backendUrl: string;
    maxTurns: number;
    totalTimeoutMs: number;
    toolTimeoutMs: number;
    idleTimeoutMs: number;
    dailyBudgetCny: number;
    maxInflight: number;
    userRpm: number;
    port: number;
    toggleTtlSec: number;
    fakeModel: boolean;
    /** 假模型的回放剧本路径。空 = 不加载（第一次调用就报剧本耗尽）。 */
    fakeScript: string;
    traceFile: string;
    isProd: boolean;
}

/** 白名单是「默认位」，降级档 M11 / M12 只能出现在 AGENT_MODEL_FALLBACK。 */
export const MODEL_WHITELIST = ['M1', 'M2', 'M7', 'M8'] as const;

/**
 * 档位代号 → 上下文长度（token）。
 * 启动自检要求 >= 64k：平台上大量便宜模型的窗口只有 8k–32k，
 * 装不下本服务 60k 的上下文峰值，单价看着便宜、实际跑两次就爆窗口。
 */
const CONTEXT_WINDOW: Record<string, number> = {
    M1: 1_000_000,
    M2: 1_000_000,
    M3: 1_000_000,
    M4: 1_000_000,
    M5: 1_000_000,
    M6: 1_000_000,
    M7: 1_000_000,
    M8: 1_000_000,
    M9: 256_000,
    M10: 1_000_000,
    M11: 1_000_000,
    M12: 1_000_000,
};

const MIN_CONTEXT = 64_000;

export class ConfigError extends Error {}

function num(raw: string | undefined, fallback: number): number {
    if (raw === undefined || raw === '') return fallback;
    const n = Number(raw);
    if (!Number.isFinite(n)) throw new ConfigError(`不是合法数字: ${raw}`);
    return n;
}

function bool(raw: string | undefined): boolean {
    return raw === '1' || raw === 'true';
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): AgentConfig {
    const isProd = (env.ENV ?? env.NODE_ENV ?? 'dev') === 'prod';

    // 本地总开关。关掉时进程仍然要能起来、/healthz 要能答：
    // 运维要先把这个服务部署上去、把链路打通，之后才谈得上开量。
    const enabled = env.AGENT_ENABLED === undefined ? true : bool(env.AGENT_ENABLED);

    const fakeModel = bool(env.AGENT_FAKE_MODEL);
    if (isProd && fakeModel) {
        throw new ConfigError('生产环境禁止开启 AGENT_FAKE_MODEL');
    }

    const model = (env.AGENT_MODEL ?? 'M1').trim();
    // 假模型不校验白名单：测试脚本里不需要真型号
    if (!fakeModel) {
        if (!(MODEL_WHITELIST as readonly string[]).includes(model)) {
            throw new ConfigError(
                `AGENT_MODEL=${model} 不在白名单 ${MODEL_WHITELIST.join('/')} 内`,
            );
        }
        const window = CONTEXT_WINDOW[model] ?? 0;
        if (window < MIN_CONTEXT) {
            throw new ConfigError(`AGENT_MODEL=${model} 的上下文不足 ${MIN_CONTEXT} token`);
        }
    }

    // 转售渠道普遍没有缓存读价，留空 = 按输入价算。
    const priceIn = num(env.AGENT_PRICE_IN, 0);
    const priceOut = num(env.AGENT_PRICE_OUT, 0);
    const cacheRaw = env.AGENT_PRICE_CACHE_READ;
    const cacheRead = cacheRaw === undefined || cacheRaw === '' ? priceIn : num(cacheRaw, priceIn);

    if (!fakeModel && priceIn <= 0 && enabled) {
        throw new ConfigError('AGENT_PRICE_IN 必须大于 0，否则金额闸门永远不触发');
    }

    const backendUrl = (env.AGENT_BACKEND_URL ?? 'http://127.0.0.1:8080').replace(/\/+$/, '');
    const baseUrl = (env.AGENT_BASE_URL ?? '').replace(/\/+$/, '');
    // 关掉的服务不校验下游配置：没有 key、没有报价时也应该能起容器。
    // 一旦 enabled，下面这两条就是硬门槛——缺配置宁可起不来，
    // 也不要带着「金额闸门恒不触发」的状态对外服务。
    if (enabled && !fakeModel && !/^https?:\/\//.test(baseUrl)) {
        throw new ConfigError('AGENT_BASE_URL 必须是 http(s) 地址');
    }

    return {
        enabled,
        model,
        fallbackModel: env.AGENT_MODEL_FALLBACK ?? 'M11',
        baseUrl,
        apiKey: env.AGENT_API_KEY ?? '',
        price: { input: priceIn, output: priceOut, cacheRead },
        backendUrl,
        maxTurns: num(env.AGENT_MAX_TURNS, 10),
        totalTimeoutMs: num(env.AGENT_TIMEOUT_TOTAL_MS, 45_000),
        toolTimeoutMs: num(env.AGENT_TIMEOUT_TOOL_MS, 8_000),
        idleTimeoutMs: num(env.AGENT_TIMEOUT_IDLE_MS, 20_000),
        dailyBudgetCny: num(env.AGENT_DAILY_BUDGET_CNY, 3000),
        maxInflight: num(env.AGENT_MAX_INFLIGHT, 8),
        userRpm: num(env.AGENT_USER_RPM, 5),
        port: num(env.AGENT_PORT, 3001),
        toggleTtlSec: num(env.AGENT_TOGGLE_TTL_SEC, 60),
        fakeModel,
        fakeScript: env.AGENT_FAKE_SCRIPT ?? '',
        traceFile: env.AGENT_TRACE_FILE ?? '',
        isProd,
    };
}

/** 把一次调用的 token 用量折成金额（元）。闸门累加的是这个数，不是 token。 */
export function costOf(
    price: Price,
    usage: { inTokens: number; outTokens: number; cacheHitTokens?: number },
): number {
    const hit = usage.cacheHitTokens ?? 0;
    const normalIn = Math.max(0, usage.inTokens - hit);
    return (
        (normalIn * price.input + hit * price.cacheRead + usage.outTokens * price.output) / 1_000_000
    );
}
