// 检查 Markdown 里疑似被 shell 吞掉的片段：
// 反引号内容被 shell 展开成空后，会留下 "中文  中文" 或 "本地 ）" 这类痕迹。
import { readFileSync } from 'node:fs';

const path = process.argv[2];
const L = readFileSync(path, 'utf8').split(/\r?\n/);

const problems = [];
let inFence = false;

for (let i = 0; i < L.length; i++) {
    const line = L[i];
    if (/^\s*```/.test(line)) { inFence = !inFence; continue; }
    if (inFence) continue;

    // 1) 中日韩字符之间夹两个及以上空格
    if (/[一-鿿] {2,}[一-鿿]/.test(line)) {
        problems.push([i + 1, '中文间双空格', line]);
    }
    // 2) 中文后紧跟句读，中间有空格（"本地 ，"）
    if (/[一-鿿] +[，。、）】」]/.test(line)) {
        problems.push([i + 1, '中文+空格+标点', line]);
    }
    // 3) 左括号/引号后直接跟标点（"（），"）
    if (/[（【「]\s*[）】」]/.test(line)) {
        problems.push([i + 1, '空括号', line]);
    }
}

if (!problems.length) {
    console.log('✓ 未发现被吞片段');
} else {
    for (const [ln, why, line] of problems) {
        console.log(`${ln}  [${why}]  ${line.slice(0, 120)}`);
    }
    console.log(`\n共 ${problems.length} 处`);
}
