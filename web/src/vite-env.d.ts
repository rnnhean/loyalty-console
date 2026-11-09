/// <reference types="vite/client" />

interface ImportMetaEnv {
    /** Sentry DSN；为空时前端监控退化为只走自建 beacon */
    readonly VITE_SENTRY_DSN?: string;
    /** 环境标识：production / staging / development */
    readonly VITE_SENTRY_ENV?: string;
    /** 发布版本号，用于 Sentry 的 Release 对比（sourcemap 匹配靠它） */
    readonly VITE_APP_VERSION?: string;
    /** 开发期后端地址，仅 vite server proxy 使用 */
    readonly VITE_API_ORIGIN?: string;
}

interface ImportMeta {
    readonly env: ImportMetaEnv;
}
