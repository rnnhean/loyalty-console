// 校验 README 目录里的锚点能否对应到真实标题（按 GitHub 的规则生成）。
import { readFileSync } from 'node:fs';

const p = 'D:/20260916/loyalty-console/README.md';
const L = readFileSync(p, 'utf8').split(/\r?\n/);

// GitHub 锚点规则（简化版）：转小写；去掉除字母数字汉字、空格、连字符外的字符；
// 空格转连字符。
function slug(text) {
    return text
        .toLowerCase()
        .replace(/[^\p{L}\p{N}\s-]/gu, '')
        .trim()
        .replace(/\s+/g, '-');
}

const headings = new Map();
let inFence = false;
for (const line of L) {
    if (/^\s*```/.test(line)) { inFence = !inFence; continue; }
    if (inFence) continue;
    const m = line.match(/^(#{1,6})\s+(.*)$/);
    if (m) headings.set(slug(m[2]), m[2]);
}

// 抽出目录区里的链接
const start = L.findIndex(l => /^## 目录$/.test(l));
const end = L.findIndex((l, i) => i > start && /^## 一、/.test(l));
const links = [];
for (let i = start; i < end; i++) {
    for (const m of L[i].matchAll(/\[([^\]]+)\]\(#([^)]+)\)/g)) {
        links.push([m[1], m[2]]);
    }
}

let bad = 0;
for (const [text, anchor] of links) {
    if (!headings.has(anchor)) {
        console.log(`✗ [${text}] → #${anchor}   没有对应标题`);
        bad++;
    }
}
console.log(bad === 0 ? `✓ ${links.length} 个锚点全部有效` : `\n${bad}/${links.length} 个失效`);
process.exit(bad === 0 ? 0 : 1);
