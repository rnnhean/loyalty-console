package masking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// RemoteClient 调远端识别服务（Python/FastAPI）。
//
// 为什么识别单独做成一个服务：
//   - 识别规则会频繁迭代（新增证件类型、新增银行卡前缀），
//     放在 Go 里每次都要重新编译发布；
//   - Python 侧的实体识别生态更成熟，真要接 NER 模型时不用换语言重写。
//
// 但它必须是**可降级**的：这是一个展示路径上的调用，不能因为识别服务抖动
// 就让整个会员列表打不开。
type RemoteClient struct {
	BaseURL string
	Timeout time.Duration
	HTTP    *http.Client
}

// NewRemoteClient 构造。timeout 建议 300ms 量级——
// 超过这个数说明服务有问题，不如直接走本地规则。
func NewRemoteClient(baseURL string, timeout time.Duration) *RemoteClient {
	if timeout <= 0 {
		timeout = 300 * time.Millisecond
	}
	return &RemoteClient{
		BaseURL: baseURL,
		Timeout: timeout,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

// RedactRequest 识别并脱敏。
type RedactRequest struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// RedactResponse 识别结果。
type RedactResponse struct {
	Masked string `json:"masked"`
	Kind   string `json:"kind"`
}

// Redact 调远端。失败返回 error，调用方据此降级。
func (c *RemoteClient) Redact(ctx context.Context, field, value string) (RedactResponse, error) {
	if c == nil || c.BaseURL == "" {
		return RedactResponse{}, errors.New("识别服务未配置")
	}
	body, _ := json.Marshal(RedactRequest{Field: field, Value: value})
	reqCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.BaseURL+"/redact", bytes.NewReader(body))
	if err != nil {
		return RedactResponse{}, err
	}
	req.Header.Set("content-type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return RedactResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return RedactResponse{}, fmt.Errorf("识别服务返回 %d", resp.StatusCode)
	}
	var out RedactResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return RedactResponse{}, err
	}
	return out, nil
}

// Healthy 探活，给 /api/infra 用。
func (c *RemoteClient) Healthy(ctx context.Context) bool {
	if c == nil || c.BaseURL == "" {
		return false
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.BaseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
