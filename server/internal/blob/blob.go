// Package blob —— S3 兼容对象存储。
//
// 为什么用对象存储而不是「后端直接把文件写到磁盘」：
//
//  1. 多实例下，A 实例写的文件 B 实例读不到（本地磁盘不是共享的）；
//  2. 大文件走应用进程会占内存和连接（导出 10 万行 Excel 时尤其明显）；
//  3. 公网部署下走对象存储 + 预签名 URL，可以让浏览器**直连存储**下载，
//     不消耗应用带宽。
//
// 用 S3 协议而不是某个厂商的 SDK：MinIO / 阿里云 OSS / 腾讯云 COS / AWS S3
// 都是这套 API，换供应商只改 endpoint 和凭据。
package blob

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/example/loyalty-console/server/internal/observ"
)

// Store 对象存储。
type Store struct {
	cli *minio.Client
	// signCli 专门用来签发预签名 URL 的客户端。
	//
	// 为什么要单独一个：S3 V4 签名把 Host 算进签署范围，
	// 用「容器内地址 minio:9000」签出来的 URL 拿到浏览器里必然验签失败。
	// 两个客户端共用同一套凭据，只是 Endpoint 不同。
	signCli  *minio.Client
	export   string // 导出文件桶
	tmp      string // 临时对象桶
	presignT time.Duration
}

// Config 连接配置。
type Config struct {
	Endpoint  string // 如 127.0.0.1:19000
	AccessKey string
	SecretKey string
	UseSSL    bool
	// PublicEndpoint 用来签发预签名 URL 的对外地址。
	//
	// 这个字段为什么必须单独存在：容器内访问 MinIO 用 minio:9000，
	// 但浏览器在容器外，拿到的 URL 里如果是 minio:9000 就打不开。
	// 预签名 URL 的 host 是由客户端地址算出来的，所以必须指定对外地址。
	PublicEndpoint string
	ExportBucket   string
	TmpBucket      string
}

// New 创建并确保桶存在。
func New(ctx context.Context, cfg Config) (*Store, error) {
	cli, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}

	// 签发客户端：Endpoint 用对外地址，这样算出来的签名与浏览器实际访问的
	// Host 头一致。没配置对外地址时退回内部地址（仅单机开发可用）。
	signEndpoint := cfg.PublicEndpoint
	if signEndpoint == "" {
		signEndpoint = cfg.Endpoint
	}
	signCli, err := minio.New(signEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		// 对外地址走 HTTPS 时这里也要跟着开；MinIO 本地没有 TLS，靠外层反代。
		Region: "us-east-1",
	})
	if err != nil {
		return nil, fmt.Errorf("minio sign client: %w", err)
	}

	s := &Store{
		cli: cli, signCli: signCli,
		export: cfg.ExportBucket, tmp: cfg.TmpBucket, presignT: 15 * time.Minute,
	}
	for _, b := range []string{cfg.ExportBucket, cfg.TmpBucket} {
		if b == "" {
			continue
		}
		if err := s.ensureBucket(ctx, b); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) ensureBucket(_ context.Context, name string) error {
	// 用独立的超时 context：启动阶段如果对象存储没起来，
	// 不应该阻塞整个服务。（调用方会重试或降级）
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	exists, err := s.cli.BucketExists(ctx, name)
	if err != nil {
		return fmt.Errorf("check bucket %s: %w", name, err)
	}
	if exists {
		return nil
	}
	if err := s.cli.MakeBucket(ctx, name, minio.MakeBucketOptions{}); err != nil {
		// 并发创建时可能已被别人建好，再查一次
		if ok, e2 := s.cli.BucketExists(ctx, name); e2 == nil && ok {
			return nil
		}
		return fmt.Errorf("make bucket %s: %w", name, err)
	}
	return nil
}

// Put 上传对象，返回对象键。
//
// size 必须准确传：传 -1 会让 SDK 退化为分片上传，小文件反而更慢。
func (s *Store) Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error {
	_, err := s.cli.PutObject(ctx, bucket, key, r, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		observ.BlobOperation.WithLabelValues("put", "error").Inc()
		return fmt.Errorf("put %s/%s: %w", bucket, key, err)
	}
	observ.BlobOperation.WithLabelValues("put", "ok").Inc()
	if size > 0 {
		observ.BlobBytes.WithLabelValues("put").Add(float64(size))
	}
	return nil
}

// Get 读取对象。
func (s *Store) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	obj, err := s.cli.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s/%s: %w", bucket, key, err)
	}
	// GetObject 是惰性的，错误要到第一次读才暴露。这里先 Stat 一次，
	// 让「对象不存在」在调用处就返回清晰的错误，而不是读到一半。
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, fmt.Errorf("stat %s/%s: %w", bucket, key, err)
	}
	return obj, nil
}

// PresignGet 生成下载用的临时 URL。
//
// 为什么用预签名而不是走后端转发：
//   - 后端不转发字节流，省带宽；
//   - URL 自带过期时间，泄漏了也只能用 15 分钟；
//   - 浏览器下载可以直接断点续传，不用我们实现。
//
// 关键：必须用 signCli（对外地址）签发，不能用访问用的 cli —— 见 Store 字段注释。
func (s *Store) PresignGet(ctx context.Context, bucket, key string) (string, error) {
	u, err := s.signCli.PresignedGetObject(ctx, bucket, key, s.presignT, url.Values{})
	if err != nil {
		observ.BlobOperation.WithLabelValues("presign", "error").Inc()
		return "", fmt.Errorf("presign %s/%s: %w", bucket, key, err)
	}
	observ.BlobOperation.WithLabelValues("presign", "ok").Inc()
	return u.String(), nil
}

// Delete 删除对象。
func (s *Store) Delete(ctx context.Context, bucket, key string) error {
	return s.cli.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
}

// ExportBucket / TmpBucket 桶名。
func (s *Store) ExportBucket() string { return s.export }
func (s *Store) TmpBucket() string    { return s.tmp }

// ExportKey 生成导出文件的对象键，按日期分目录便于生命周期清理。
func ExportKey(name string) string {
	now := time.Now()
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)
	return fmt.Sprintf("exports/%s/%s-%d.csv", now.Format("2006-01-02"), safe, now.UnixNano())
}
