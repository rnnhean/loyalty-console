/**
 * SSE 心跳。
 *
 * 线上问题：中间代理（nginx / 负载均衡 / CDN）对「长时间没有任何字节」的连接
 * 有空闲超时（nginx 默认 60s）。模型思考一次可能几十秒，期间一个字节都不发，
 * 代理会把连接掐掉——表现是「偶尔聊到一半就断，重试又好了」。
 *
 * 心跳用 SSE 的**注释行**（以 `:` 开头）：客户端解析器会忽略，不会当成消息；
 * 但对每个中间代理来说都是「连接还活着」的证据。间隔取 15s，远小于最常见的
 * 60s 空闲超时。
 *
 * 抽成独立模块而不是塞在 server.ts 里，是为了能单测：server.ts 一 import 就
 * listen，没法在测试里验证定时器行为。
 */

/** 只依赖这两个字段，测试里用假对象即可，不需要真 ServerResponse */
export interface SseSink {
    readonly writableEnded: boolean;
    write(chunk: string): unknown;
}

/**
 * 挂心跳，返回停止函数（**必须在连接结束时调用**，否则每断一个请求就漏一个定时器）。
 *
 * @param res 响应对象
 * @param intervalMs 间隔；<= 0 表示不挂（演练/单测）
 */
export function attachHeartbeat(res: SseSink, intervalMs: number): () => void {
    if (!(intervalMs > 0)) return () => {};

    const timer = setInterval(() => {
        // 连接已经结束还往里写会抛 ERR_STREAM_WRITE_AFTER_END
        if (res.writableEnded) return;
        try {
            res.write(': ping\n\n');
        } catch {
            // 断开时 close 事件会负责清理，这里不让异常冒出去打断请求处理
        }
    }, intervalMs);
    // 不让心跳定时器把进程钉住：测试/CLI 场景下没有别的活时就该退出
    (timer as unknown as { unref?: () => void }).unref?.();

    return () => clearInterval(timer);
}
