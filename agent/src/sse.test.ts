/**
 * 心跳测试。
 *
 * 这条逻辑线上出过事（代理 60s 空闲超时掐断连接），所以必须钉住三件事：
 *   1. 按时发注释行（不是普通 data 事件——data 会被客户端当成真消息）
 *   2. 连接结束后不再写（写会抛 ERR_STREAM_WRITE_AFTER_END）
 *   3. 停止函数真的停得掉（否则每断一个请求漏一个定时器）
 */

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { attachHeartbeat, type SseSink } from './sse.js';

/** 可观测的假响应 */
function sink() {
    const chunks: string[] = [];
    const s = {
        writableEnded: false,
        write(c: string) {
            if (s.writableEnded) throw new Error('ERR_STREAM_WRITE_AFTER_END');
            chunks.push(c);
            return true;
        },
        chunks,
    };
    return s as typeof s & SseSink;
}

test('按间隔发注释行（: ping），不是 data 事件', async () => {
    const res = sink();
    const stop = attachHeartbeat(res, 20);
    await new Promise((r) => setTimeout(r, 70));
    stop();

    assert.ok(res.chunks.length >= 2, `70ms / 20ms 至少应发 2 次，实际 ${res.chunks.length}`);
    for (const c of res.chunks) {
        assert.equal(c, ': ping\n\n', '必须是注释行，否则客户端会当成一条消息');
        assert.ok(!c.startsWith('data:'), '不能是 data 事件');
    }
});

test('连接结束后不再写（避免 WRITE_AFTER_END）', async () => {
    const res = sink();
    const stop = attachHeartbeat(res, 15);
    await new Promise((r) => setTimeout(r, 25));
    res.writableEnded = true;
    const before = res.chunks.length;
    await new Promise((r) => setTimeout(r, 50));
    stop();

    assert.equal(res.chunks.length, before, '结束后不应再产生任何写入');
});

test('停止函数能真正停掉定时器', async () => {
    const res = sink();
    const stop = attachHeartbeat(res, 10);
    await new Promise((r) => setTimeout(r, 30));
    stop();
    const after = res.chunks.length;
    await new Promise((r) => setTimeout(r, 60));
    assert.equal(res.chunks.length, after, 'stop() 之后不应再有心跳');
});

test('间隔 <= 0 时不挂定时器，返回空清理函数', async () => {
    const res = sink();
    const stop = attachHeartbeat(res, 0);
    await new Promise((r) => setTimeout(r, 30));
    stop();
    assert.deepEqual(res.chunks, [], '关闭心跳时一个字都不该写');
});

test('write 抛错不会冒出去打断请求', async () => {
    const res = {
        writableEnded: false,
        write() {
            throw new Error('socket gone');
        },
    };
    const stop = attachHeartbeat(res, 10);
    // 让定时器至少跑两次；若异常冒出去，process 会 uncaughtException 导致测试失败
    await new Promise((r) => setTimeout(r, 35));
    stop();
    assert.ok(true, '能走到这里就说明异常被吞住了');
});
