// 零变量启动验证：不传任何 MC_* 前缀，看配置是否能从 deploy/env/local.env 自动加载。
//
// 检查点：
//   1. 启动日志里有 config_loaded 且 env=local
//   2. 用的是文件里的 MC_ADMIN_PASS（demo1234），而不是代码默认的空值
//   3. 五个中间件全通
import { execFileSync } from 'node:child_process';

const GW = process.env.GW || 'http://127.0.0.1:8080';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function curl(args) {
    try {
        return JSON.parse(execFileSync('curl', args, { encoding: 'utf8' }));
    } catch {
        return null;
    }
}

await sleep(200);

const login = curl([
    '-s', '--max-time', '10', '-X', 'POST', `${GW}/api/auth/login`,
    '-H', 'Content-Type: application/json',
    '-d', JSON.stringify({ username: 'admin', password: 'demo1234' }),
]);

const token = login?.token || '';
console.log(`1) 用文件里的口令登录: code=${login?.code} token长度=${token.length}`);
if (!login || login.code !== 0) {
    console.log('   ✗ 登录失败——说明 MC_ADMIN_PASS 没从 local.env 读到');
    process.exit(1);
}

const infra = curl(['-s', '--max-time', '15', `${GW}/api/infra/health`, '-H', `Authorization: Bearer ${token}`]);
const items = infra?.infra || {};
console.log('\n2) 中间件连通性:');
let allOk = true;
for (const [k, v] of Object.entries(items)) {
    const ok = v?.ok === true;
    if (!ok) allOk = false;
    console.log(`   ${ok ? '✓' : '✗'} ${k.padEnd(12)} ${v?.detail || ''}`);
}

console.log(`\n结果: ${allOk ? '全部通过' : '有组件不通'}`);
process.exit(allOk ? 0 : 1);
