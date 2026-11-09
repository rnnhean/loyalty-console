# ── 构建阶段 ──────────────────────────────────────────────
FROM node:20-alpine AS build

WORKDIR /app

RUN corepack enable

# 依赖清单单独一层，命中缓存
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
RUN pnpm install --frozen-lockfile

COPY . .

# Sentry 的 Release 与 sourcemap 都靠这个版本号对齐
ARG APP_VERSION=dev
ARG SENTRY_DSN=
ARG SENTRY_ENV=production
ENV VITE_APP_VERSION=${APP_VERSION} \
    VITE_SENTRY_DSN=${SENTRY_DSN} \
    VITE_SENTRY_ENV=${SENTRY_ENV}

RUN pnpm build

# ── 运行阶段 ──────────────────────────────────────────────
# 只服务静态资源。TLS、限流、反代都在最外层网关，这里不处理任何安全逻辑。
FROM nginx:1.27-alpine

COPY --from=build /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf

# sourcemap 不随镜像发布：它含有原始源码，
# 只在上传 Sentry 时使用（见 deploy 脚本），发布产物里留空目录占位。
RUN rm -rf /usr/share/nginx/html/*.map /usr/share/nginx/html/assets/*.map

EXPOSE 80
