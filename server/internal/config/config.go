// Package config —— 按环境加载配置。
//
// 三层优先级，从高到低：
//
//	1. 真实环境变量（docker compose 注入、CI 注入、命令行前缀）
//	2. 环境文件（deploy/env/<env>.env）
//	3. 代码里的默认值（env() 的 def 参数）
//
// 第 1 层永远赢，这是刻意的：容器化部署下配置即环境，
// 一份镜像跑三个环境靠的就是这个优先级。环境文件只是
// 让本地开发不必每次在命令行拼一长串变量。
//
// 生产镜像里通常没有 env 文件，此时本包静默跳过，不报错。
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Env 是当前运行环境标识。
type Env string

const (
	Local   Env = "local"
	Staging Env = "staging"
	Prod    Env = "prod"
)

// Current 读取 MC_ENV，默认 local。
//
// 默认值给 local 而不是 prod 是安全考虑：忘记设时最坏情况是
// 用本地配置起服务、连不上生产库，而不是反过来。
func Current() Env {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("MC_ENV"))); Env(v) {
	case Staging:
		return Staging
	case Prod:
		return Prod
	default:
		return Local
	}
}

// FileName 返回该环境对应的默认配置文件名。
func FileName(e Env) string {
	return string(e) + ".env"
}

// DefaultDir 返回环境文件所在目录。
//
// 用相对仓库根的 deploy/env。程序从 server/ 目录启动时，
// 向上找两级；找不到就返回空串，由调用方决定要不要报错。
func DefaultDir() string {
	candidates := []string{
		filepath.Join("deploy", "env"),
		filepath.Join("..", "deploy", "env"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

// Load 加载环境文件并写入进程环境。
//
// path 为空时按 MC_ENV 自动推导：<deploy/env>/<env>.env。
// 文件不存在不算错误——生产容器里本来就没有这个文件。
//
// 返回实际加载的文件路径（空串表示没加载任何文件），
// 便于启动日志里写清楚「这次用的是哪份配置」。
func Load(path string) (string, error) {
	if path == "" {
		dir := DefaultDir()
		if dir == "" {
			return "", nil
		}
		path = filepath.Join(dir, FileName(Current()))
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("打开环境文件 %s: %w", path, err)
	}
	defer f.Close()

	if err := parse(f, os.Setenv); err != nil {
		return "", fmt.Errorf("解析环境文件 %s: %w", path, err)
	}
	return path, nil
}

// parse 逐行解析 KEY=VALUE。
//
// 刻意不支持多行值、变量插值、命令替换这些 shell 特性：
// 支持得越多，配置文件的语义就越接近「执行一段脚本」，
// 出错时也越难排查。复杂场景直接写进 docker compose 的 environment 段。
func parse(f *os.File, set func(string, string) error) error {
	sc := bufio.NewScanner(f)
	// 单行上限放宽到 1MB：前端把 Sentry DSN 之外的证书也塞进来时不至于截断
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 兼容 `export KEY=VALUE` 的写法，方便直接 source 这份文件调试
		line = strings.TrimPrefix(line, "export ")

		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("第 %d 行缺少 '='：%s", lineNo, line)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)

		if k == "" {
			return fmt.Errorf("第 %d 行变量名为空", lineNo)
		}

		// 去掉成对的引号；引号内保留 # 和空格
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		} else if v == "\"\"" || v == "''" {
			v = ""
		}

		// ★ 已存在的环境变量不覆盖。这是本包最重要的行为：
		// docker compose 的 environment 段、CI 的 secrets 都靠这一点生效。
		if _, exists := os.LookupEnv(k); exists {
			continue
		}
		if err := set(k, v); err != nil {
			return fmt.Errorf("第 %d 行设置 %s: %w", lineNo, k, err)
		}
	}
	return sc.Err()
}
