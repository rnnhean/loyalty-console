# 默认的超时与错误处理。被 site.conf 里各 location 显式 include。
#
# 为什么和 proxy_common.inc 分开：
# nginx 不允许同一个 block 里重复写同一个 proxy_* 指令（会报 duplicate directive），
# 所以「用默认值」和「要更长的读超时」这两种 location 无法共用一份 include。
# 拆出来之后，/api/agent/ 那类长连接路由只 include 公共头、自己写超时。

proxy_connect_timeout 5s;
proxy_send_timeout    30s;
proxy_read_timeout    30s;

# 上游挂了不要让 nginx 返回自己的错误页（会泄露版本信息），
# 交给统一错误处理
proxy_intercept_errors off;

# 关闭缓冲：SSE / 流式响应不被攒着
proxy_buffering off;
