import assert from 'node:assert/strict';
import { test } from 'node:test';

import { ConfigError, loadConfig } from './config.js';

/** 一份能通过校验的最小配置；各用例只在它上面改一个变量。 */
function baseEnv(over: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv {
    return {
        AGENT_MODEL: 'M1',
        AGENT_BASE_URL: 'https://example.invalid/v1',
        AGENT_API_KEY: 'test-key',
        AGENT_PRICE_IN: '1',
        AGENT_PRICE_OUT: '2',
        ...over,
    };
}

test('默认启用，缺报价就拒绝启动', () => {
    const env = baseEnv({ AGENT_PRICE_IN: '' });
    assert.throws(() => loadConfig(env), ConfigError);
});

test('缺 baseUrl 就拒绝启动', () => {
    const env = baseEnv({ AGENT_BASE_URL: '' });
    assert.throws(() => loadConfig(env), ConfigError);
});

test('关掉总开关后，没 key 没报价也能起容器', () => {
    const env = baseEnv({ AGENT_ENABLED: '0', AGENT_PRICE_IN: '', AGENT_BASE_URL: '', AGENT_API_KEY: '' });
    const cfg = loadConfig(env);
    assert.equal(cfg.enabled, false);
    assert.equal(cfg.apiKey, '');
});

test('关掉总开关也要做型号白名单校验：拼错的型号不能悄悄留着', () => {
    const env = baseEnv({ AGENT_ENABLED: '0', AGENT_MODEL: 'M99' });
    assert.throws(() => loadConfig(env), ConfigError);
});

test('生产环境禁止假模型', () => {
    const env = baseEnv({ ENV: 'prod', AGENT_FAKE_MODEL: '1' });
    assert.throws(() => loadConfig(env), ConfigError);
});

test('降级档只能出现在 fallback：主型号必须在白名单里', () => {
    const cfg = loadConfig(baseEnv({ AGENT_MODEL_FALLBACK: 'M11' }));
    assert.equal(cfg.fallbackModel, 'M11');
    assert.throws(() => loadConfig(baseEnv({ AGENT_MODEL: 'M11' })), ConfigError);
});

test('缓存读价留空时按输入价算，避免缓存命中反而被低估成本', () => {
    const cfg = loadConfig(baseEnv({ AGENT_PRICE_IN: '3' }));
    assert.deepEqual(cfg.price, { input: 3, output: 2, cacheRead: 3 });
});
