# 反代公共参数。被 site.conf 里各 location 用 include 引入。

proxy_http_version 1.1;
proxy_set_header Connection "";
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;

# trace_id 透传：前端带了就带着，没带就让 Go 网关自己生成
proxy_set_header X-Trace-Id        $http_x_trace_id;

proxy_connect_timeout 5s;
proxy_send_timeout    30s;
proxy_read_timeout    30s;

# 上游挂了不要让 nginx 返回自己的错误页（会泄露版本信息），
# 交给统一错误处理
proxy_intercept_errors off;

# 关闭缓冲：SSE / 流式响应不被攒着
proxy_buffering off;
