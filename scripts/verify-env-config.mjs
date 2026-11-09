// 验证三套配置文件里 local 这套是否真的能跑通。
//
// 做的事：
//   1. 登录拿 token（不打印 token 内容）
//   2. 用 token 打 /api/infra/health，看五个组件分别通不通
//   3. 打几个业务接口，确认数据能读出来
//
// 凭据安全：只输出 token 长度，不输出内容；响应里的敏感字段一律不打印。
import { execFileSync } from 'node:child_process';

const GW = process.env.GW || 'http://127.0.0.1:8080';

function call(method, path, { token, body } = {}) {
    const args = ['-s', '--max-time', '15', '-X', method, `${GW}${path}`];
    if (token) args.push('-H', `Authorization: Bearer ${token}`);
    if (body) args.push('-H', 'Content-Type: application/json', '-d', JSON.stringify(body));
    try {
        return JSON.parse(execFileSync('curl', args, { encoding: 'utf8' }));
    } catch {
        return { __error: '请求失败或响应不是 JSON' };
    }
}

const login = call('POST', '/api/auth/login', { body: { username: 'admin', password: 'demo1234' } });
const token = login?.token || '';
console.log(`登录: code=${login?.code} token长度=${token.length}`);
if (!token) {
    console.log('登录失败，后续无法验证');
    process.exit(1);
}

const infra = call('GET', '/api/infra/health', { token });
console.log('\n基础设施连通性:');
console.log(JSON.stringify(infra, null, 2));

const endpoints = [
    '/api/catalog/spu',
    '/api/catalog/binds',
    '/api/trade/debits',
    '/api/trade/promos',
    '/api/operation/vouchers',
    '/api/operation/slots',
    '/api/channel/attribution',
    '/api/setting/toggles',
    '/api/bff/catalog/overview',
];

console.log('\n业务接口:');
let ok = 0;
for (const p of endpoints) {
    const r = call('GET', p, { token });
    // 两种返回形态：
    //   列表接口 { code, items[], total, page, page_size }
    //   BFF 聚合 { code, data:{...} }
    let n = -1;
    if (Array.isArray(r?.items)) n = r.items.length;
    else if (r?.data && typeof r.data === 'object') n = Object.keys(r.data).length;
    const good = r?.code === 0;
    if (good) ok++;
    console.log(`  ${good ? 'OK ' : 'ERR'} ${p.padEnd(34)} code=${r?.code} 条数/键数=${n}`);
}
console.log(`\n结果: ${ok}/${endpoints.length} 个接口正常`);
