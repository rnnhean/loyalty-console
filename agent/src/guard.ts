/**
 * 准入与花费闸门。
 *
 * 三层限流 + 一道开关 + 一道金额闸门，全部在**调用模型之前**判，
 * 因为模型调用是按次计费的，放进去再中断等于钱已经花了。
 *
 * 金额闸门累加的是**元**，不是 token。换模型时 token 闸门会失效（同样的
 * token 数，贵的模型花 5 倍），金额不会。这就是为什么单价要配置化。
 *
 * 开关采用「缓存 + 拉不到就沿用上次已知值 + 从没拉到过就关着」：
 * 这是省钱用的闸门，网络抖动时宁可少服务一会儿，也不能放行。
 */

export type DenyReason = 'disabled' | 'concurrency' | 'rpm' | 'budget' | 'unavailable';

export interface Lease {
    /** 必须在 finally 里调用，否则并发计数永久占住 */
    release(): void;
    /** 本次实际花费（元）。由 loop 结束后回填。 */
    addCost(cny: number): void;
}

export type GuardDecision =
    | { ok: true; lease: Lease }
    | { ok: false; reason: DenyReason; detail: string; retryAfterMs?: number };

export interface GuardOptions {
    /** 环境变量的总开关，关掉时连 toggle 接口都不查 */
    enabled: boolean;
    maxInflight: number;
    userRpm: number;
    dailyBudgetCny: number;
    toggleTtlSec: number;
    backendUrl: string;
    fetchImpl?: typeof fetch;
    now?: () => number;
}

const WINDOW_MS = 60_000;
const TOGGLE_KEY = 'agent_enabled';

export class Guard {
    private inflight = 0;
    private readonly perUser = new Map<string, number>();
    private readonly windows = new Map<string, number[]>();
    private spentCny = 0;
    private dayKey = '';
    private toggle: { value: boolean; at: number } | null = null;
    private toggleLoading: Promise<boolean> | null = null;

    private readonly now: () => number;
    private readonly doFetch: typeof fetch;

    constructor(private readonly opts: GuardOptions) {
        this.now = opts.now ?? Date.now;
        this.doFetch = opts.fetchImpl ?? fetch;
    }

    async check(userId: string, token: string): Promise<GuardDecision> {
        if (!this.opts.enabled) {
            return { ok: false, reason: 'disabled', detail: '诊断助手未启用' };
        }

        // 1) 远程开关：灰度时靠它一键关掉，不用重新发版
        const remote = await this.readToggle(token);
        if (!remote) {
            return { ok: false, reason: 'disabled', detail: '诊断助手已关闭' };
        }

        // 2) 当日金额
        this.rollDay();
        if (this.spentCny >= this.opts.dailyBudgetCny) {
            return {
                ok: false,
                reason: 'budget',
                detail: '今日诊断额度已用完，明天再试',
            };
        }

        // 3) 全局并发
        if (this.inflight >= this.opts.maxInflight) {
            return { ok: false, reason: 'concurrency', detail: '当前诊断人数较多，请稍后再试' };
        }

        // 4) 单用户并发：同一个人开两个窗口会跑两份账单
        if ((this.perUser.get(userId) ?? 0) > 0) {
            return { ok: false, reason: 'concurrency', detail: '你已经有一个诊断在跑，等它结束' };
        }

        // 5) 单用户每分钟次数
        const recent = this.recentCalls(userId);
        if (recent.length >= this.opts.userRpm) {
            const oldest = recent[0] ?? this.now();
            return {
                ok: false,
                reason: 'rpm',
                detail: `每分钟最多 ${this.opts.userRpm} 次，请稍等`,
                retryAfterMs: Math.max(0, WINDOW_MS - (this.now() - oldest)),
            };
        }

        this.inflight++;
        this.perUser.set(userId, (this.perUser.get(userId) ?? 0) + 1);
        recent.push(this.now());
        this.windows.set(userId, recent);

        let released = false;
        const self = this;
        return {
            ok: true,
            lease: {
                release(): void {
                    if (released) return;
                    released = true;
                    self.inflight = Math.max(0, self.inflight - 1);
                    const left = (self.perUser.get(userId) ?? 1) - 1;
                    if (left <= 0) self.perUser.delete(userId);
                    else self.perUser.set(userId, left);
                },
                addCost(cny: number): void {
                    if (!Number.isFinite(cny) || cny <= 0) return;
                    self.rollDay();
                    self.spentCny += cny;
                },
            },
        };
    }

    /** 当日已花（元），给 /healthz 和 trace 用 */
    get spent(): number {
        this.rollDay();
        return this.spentCny;
    }

    get active(): number {
        return this.inflight;
    }

    /**
     * 远程开关。
     *
     * 缓存 60 秒；并发请求共享同一次拉取（否则冷启动时 8 个请求会打 8 次）。
     * 拉取失败**不清空缓存**——沿用上次已知值；从没成功过则按关闭处理。
     */
    private async readToggle(token: string): Promise<boolean> {
        const cached = this.toggle;
        if (cached && this.now() - cached.at < this.opts.toggleTtlSec * 1000) {
            return cached.value;
        }

        if (!this.toggleLoading) {
            this.toggleLoading = this.fetchToggle(token).finally(() => {
                this.toggleLoading = null;
            });
        }

        try {
            return await this.toggleLoading;
        } catch {
            return cached?.value ?? false;
        }
    }

    private async fetchToggle(token: string): Promise<boolean> {
        if (!token) throw new Error('缺少 token，无法查询开关');

        const res = await this.doFetch(`${this.opts.backendUrl}/api/setting/toggles`, {
            headers: { authorization: `Bearer ${token}` },
            signal: AbortSignal.timeout(5_000),
        });
        if (!res.ok) throw new Error(`开关接口返回 ${res.status}`);

        const json = (await res.json()) as { data?: unknown };
        const data = json?.data;
        const rows = Array.isArray(data)
            ? data
            : Array.isArray((data as { list?: unknown })?.list)
              ? ((data as { list?: unknown[] }).list ?? [])
              : [];

        let value = this.opts.enabled;
        for (const row of rows) {
            if (!row || typeof row !== 'object') continue;
            const r = row as Record<string, unknown>;
            if (r.toggle_key === TOGGLE_KEY || r.key === TOGGLE_KEY) {
                value = r.value === true || r.value === 1 || r.value === '1' || r.value === 'true';
                break;
            }
        }

        this.toggle = { value, at: this.now() };
        return value;
    }

    /** 取该用户最近 60 秒内的调用时间戳，顺便把过期的清掉（防止 Map 无限增长）。 */
    private recentCalls(userId: string): number[] {
        const t = this.now();
        const kept = (this.windows.get(userId) ?? []).filter((ts) => t - ts < WINDOW_MS);
        if (kept.length === 0) this.windows.delete(userId);
        else this.windows.set(userId, kept);
        return kept;
    }

    private rollDay(): void {
        const key = new Date(this.now()).toISOString().slice(0, 10);
        if (key !== this.dayKey) {
            this.dayKey = key;
            this.spentCny = 0;
        }
    }
}
