# agent 服务镜像。
#
# 构建上下文是**仓库根目录**，因为要带上 shared/contract.ts：
#   docker build -f agent/Dockerfile .
#
# 运行阶段不拷 node_modules：这个服务零运行时依赖，产物只 import
# node:* 和相对路径。镜像因此只有几十 MB，也不存在依赖漏洞面。

# ── 构建阶段 ──────────────────────────────────────────────
FROM node:20-alpine AS build

WORKDIR /repo

# 依赖清单单独一层：改业务代码不用重装依赖
COPY agent/package.json agent/pnpm-lock.yaml ./agent/
RUN corepack enable && corepack prepare pnpm@9.12.0 --activate \
 && cd agent && pnpm install --frozen-lockfile

COPY shared ./shared
COPY agent ./agent

RUN cd agent && node ./node_modules/typescript/bin/tsc -p tsconfig.build.json

# ── 运行阶段 ──────────────────────────────────────────────
FROM node:20-alpine

WORKDIR /app
ENV NODE_ENV=production

COPY --from=build /repo/agent/dist ./dist

# trace 目录必须**先建好并交给 node 用户**。
# 不建的话，compose 挂具名卷时 Docker 会在这个路径上放一个 root 拥有的空目录，
# 进程以 node 身份写不进去 —— 表现是「服务一切正常，但 trace 文件永远是空的」。
# 具名卷初始化时会继承镜像里这个目录的属主，所以在这里 chown 就够了。
RUN mkdir -p /var/log/agent && chown -R node:node /var/log/agent

# 镜像自带的非 root 用户
USER node
EXPOSE 3001

# 直接跑编译产物。开发时用 `node --import tsx src/server.ts`，
# 生产不走 tsx —— 少一个运行时加载器，启动也更快。
CMD ["node", "dist/agent/src/server.js"]
