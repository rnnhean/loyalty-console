# 反代公共参数。被 site.conf 里各 location 用 include 引入。
#
# 这里只放「每个 location 都要」的头与协议设置；
# 超时和缓冲在 proxy_timeouts.inc，因为唯一需要改超时的路由
# （/api/agent/，SSE 长连接）没法靠 include 覆盖默认值。

proxy_http_version 1.1;
proxy_set_header Connection "";
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;

# trace_id 透传：前端带了就带着，没带就让 Go 网关自己生成
proxy_set_header X-Trace-Id        $http_x_trace_id;
