import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';
import { sentryVitePlugin } from '@sentry/vite-plugin';
import { execSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';

export default defineConfig(({ mode }) => {
    const env = loadEnv(mode, process.cwd(), '');

    // ── Sentry sourcemap 上传的 auth token ──
    // 优先读环境变量 SENTRY_AUTH_TOKEN；没有再读 SENTRY_AUTH_TOKEN_FILE 指向的文件
    // （只在本进程内读，绝不打印）。两者都没有 → 不启用上传插件，且生产包不生成
    // sourcemap —— 这样源码不会随产物泄露。
    //
    // 为什么用 token 才能决定是否生成 map：`sourcemap: 'hidden'` 会照常产出 .map
    // 文件（只是 bundle 里不写 sourceMappingURL），如果不上传又留在 dist 里，
    // 部署出去照样能被下载。所以「不上传就不生成」是最安全的默认。
    const authToken = (() => {
        if (process.env.SENTRY_AUTH_TOKEN) return process.env.SENTRY_AUTH_TOKEN;
        const f = process.env.SENTRY_AUTH_TOKEN_FILE;
        if (f && fs.existsSync(f)) return fs.readFileSync(f, 'utf8').trim();
        return '';
    })();

    // release 名称必须和运行时（web/src/lib/monitor.ts 里的 RELEASE）一致，
    // 否则 Sentry 拿堆栈对不上这份 sourcemap。
    //
    // 取值优先级：命令行/CI 注入的 VITE_APP_VERSION > 自动生成。
    // 自动值 = `<package.json 的 version>+<git 短提交号>`，每次提交都不一样。
    // 为什么不用 `.env.*` 里的 VITE_APP_VERSION 当默认值：那是个写死的常量，
    // 发布多少次都叫同一个名字，Sentry 里没法回答「这个错误是哪次发布引入的」。
    // 下面把算出来的值写回 process.env，Vite 的 loadEnv 会优先采用它，
    // 运行时读到的 import.meta.env.VITE_APP_VERSION 也就是同一个值。
    const packageVersion = JSON.parse(
        fs.readFileSync(path.resolve(__dirname, 'package.json'), 'utf8'),
    ).version as string;
    const gitSha = (() => {
        try {
            return execSync('git rev-parse --short HEAD', {
                cwd: __dirname,
                stdio: ['ignore', 'pipe', 'ignore'],
            })
                .toString()
                .trim();
        } catch {
            // 没有 git（比如 Docker 构建时没带 .git）就退化成纯版本号
            return '';
        }
    })();
    const RELEASE =
        process.env.VITE_APP_VERSION || (gitSha ? `${packageVersion}+${gitSha}` : packageVersion);
    process.env.VITE_APP_VERSION = RELEASE;

    return {
        plugins: [
            react(),
            // 只有拿到 token 才挂上传插件：上传 sourcemap、建 release、传完删掉 .map。
            ...(authToken
                ? [
                      sentryVitePlugin({
                          org: env.SENTRY_ORG || 'ebo-y9',
                          project: env.SENTRY_PROJECT || 'javascript-react',
                          authToken,
                          release: { name: RELEASE },
                          telemetry: false,
                          sourcemaps: {
                              filesToDeleteAfterUpload: ['dist/**/*.map'],
                          },
                      }),
                  ]
                : []),
        ],
        resolve: {
            alias: { '@': path.resolve(__dirname, 'src') },
        },
        css: {
            preprocessorOptions: {
                less: {
                    // antd 5 用的是 CSS-in-JS，这里的 less 只处理本项目自己的样式。
                    // javascriptEnabled 开着是为了能用 less 变量参与计算。
                    javascriptEnabled: true,
                },
            },
        },
        server: {
            port: 5173,
            // 开发期走代理而不是直连 8080：前端代码里就不需要写死后端地址，
            // 生产环境同源部署时也不用改。
            proxy: {
                '/api': {
                    target: env.VITE_API_ORIGIN || 'http://127.0.0.1:8080',
                    changeOrigin: true,
                },
            },
            headers: {
                // Chrome 的 JS Self-Profiling API 受 Document-Policy 门控。
                // 没有这个响应头，浏览器直接拒绝采集样本，而 Sentry SDK 不会报错 ——
                // 表现就是「Profiles 页面永远是空的」，很容易被误判成没配集成。
                // 生产环境在 nginx 里也要发同一个头，见 deploy/nginx/conf.d/site.conf。
                'Document-Policy': 'js-profiling',
            },
        },
        build: {
            // 有 token：生成隐藏 sourcemap 供上传（bundle 不引用、上传后删除）。
            // 无 token：不生成，避免把源码随 dist 一起发出去。
            sourcemap: authToken ? 'hidden' : false,
            chunkSizeWarningLimit: 1200,
            rollupOptions: {
                output: {
                    manualChunks: {
                        react: ['react', 'react-dom', 'react-router-dom'],
                        antd: ['antd', '@ant-design/icons'],
                    },
                },
            },
        },
    };
});
