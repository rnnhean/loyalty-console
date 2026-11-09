package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// doHealthcheck 供容器 healthcheck 使用。
//
// 为什么要程序自带而不是用 curl/nc：
// 运行镜像是 distroless，里面没有 shell，也没有任何网络工具。
// 这是有意的取舍 —— 公网部署时，「打进来也没东西可用」比「排查方便」更重要，
// 代价就是健康检查得自己实现。
func doHealthcheck() int {
	addr := env("MC_ADDR", ":8080")
	// 监听地址可能是 :8080 或 0.0.0.0:8080，健康检查一律打本机回环
	port := ":8080"
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		port = addr[i:]
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}
