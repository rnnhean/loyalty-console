import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'node:path';

export default defineConfig(({ mode }) => {
    const env = loadEnv(mode, process.cwd(), '');

    return {
        plugins: [react()],
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
            // 生产要上传 sourcemap 到 Sentry，所以必须生成（但不能随包发布）。
            sourcemap: true,
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
