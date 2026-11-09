// Package auth —— 登录与令牌签发。
//
// 为什么自己签 token 而不引 JWT 库：
// 本项目只有一个服务、一种调用方，需要的只是「签发 + 验签 + 过期」三件事。
// 引入 JWT 会连带引入算法选择（none/HS256/RS256）这个历史踩坑点
// ——「alg 用 none 就能绕过」的漏洞几乎都出在这里。
// 自己实现 HMAC 固定算法，反而更小的攻击面。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// 错误分类。调用方据此决定返回 401 还是 403。
var (
	ErrMalformed = errors.New("token malformed")
	ErrSignature = errors.New("token signature invalid")
	ErrExpired   = errors.New("token expired")
)

// Identity 验签通过后的身份。
type Identity struct {
	ID   string
	Role string
}

// Issuer 令牌签发器。
type Issuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time // 便于测试注入固定时间
}

// NewIssuer 创建签发器。secret 建议 32 字节以上随机值，从环境变量注入，
// **不要写进代码或配置文件**（本项目用 MC_TOKEN_SECRET 环境变量）。
func NewIssuer(secret []byte, ttl time.Duration) *Issuer {
	return &Issuer{secret: secret, ttl: ttl, now: time.Now}
}

// Issue 签发 token。
//
// 格式：base64url(payload) + "." + base64url(hmac)
// payload：id|role|exp|nonce —— 用 | 分隔而不是 JSON，
// 省一次序列化，且天然不含 = & 这类需要在 URL 里转义的字符。
func (i *Issuer) Issue(id, role string) (string, error) {
	var nb [8]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return "", err
	}
	exp := i.now().Add(i.ttl).Unix()
	body := strings.Join([]string{id, role, strconv.FormatInt(exp, 10), base64.RawURLEncoding.EncodeToString(nb[:])}, "|")
	enc := base64.RawURLEncoding.EncodeToString([]byte(body))
	return enc + "." + base64.RawURLEncoding.EncodeToString(i.sign(enc)), nil
}

// Verify 验签并返回身份。
//
// 注意「先验签、再解析」的顺序：如果先解析再验签，
// 攻击者可以让解析器处理任意构造的 payload，扩大解析面的风险。
func (i *Issuer) Verify(token string) (Identity, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Identity{}, ErrMalformed
	}
	// Bearer 前缀允许调用方自己带，网关层也会剥一次，这里再兜一次
	token = strings.TrimPrefix(token, "Bearer ")

	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return Identity{}, ErrMalformed
	}
	enc, sigB64 := parts[0], parts[1]

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return Identity{}, ErrMalformed
	}
	// 常量时间比较，防止时序侧信道
	if subtle.ConstantTimeCompare(sig, i.sign(enc)) != 1 {
		return Identity{}, ErrSignature
	}

	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return Identity{}, ErrMalformed
	}
	f := strings.Split(string(raw), "|")
	if len(f) != 4 {
		return Identity{}, ErrMalformed
	}
	exp, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return Identity{}, ErrMalformed
	}
	if i.now().Unix() > exp {
		return Identity{}, ErrExpired
	}
	if f[0] == "" {
		return Identity{}, ErrMalformed
	}
	return Identity{ID: f[0], Role: f[1]}, nil
}

func (i *Issuer) sign(payload string) []byte {
	m := hmac.New(sha256.New, i.secret)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// ── 口令校验 ───────────────────────────────────────────────

// CheckPassword 常量时间比较口令。
//
// 这里没有存明文再比，而是让调用方传入「已计算好的期望值」，
// 由调用方从环境变量注入 —— 本项目是演示工程，不接用户表；
// 真实系统应当存 bcrypt/argon2 哈希，绝不可逆。
func CheckPassword(given, expected string) bool {
	if expected == "" {
		return false
	}
	// 长度不同直接返回 false 会泄露长度信息，先各自哈希到定长再比较
	g := sha256.Sum256([]byte(given))
	e := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(g[:], e[:]) == 1
}
