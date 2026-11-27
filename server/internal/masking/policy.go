// Package masking 负责敏感字段的展示控制。
//
// 三条底线：
//  1. **默认不返回明文**：列表、详情、导出走同一条策略，导出最容易漏。
//  2. **明文访问要留痕**：谁能看明文、什么时候看的、为什么看，都进审计表。
//  3. **识别服务可以挂，系统不能挂**：远端识别服务超时/失败时降级到本地规则，
//     宁可少脱敏一类字段，也不能让运营台打不开。
package masking

import (
	"strings"
	"unicode/utf8"
)

// Strategy 是一个字段的展示策略。
type Strategy string

const (
	// StrategyMask 打码：保留首尾若干位，中间替换。
	StrategyMask Strategy = "mask"
	// StrategyHide 完全不返回，用固定占位符。
	StrategyHide Strategy = "hide"
	// StrategyPlain 明文（仅对白名单角色开放，且要写审计）。
	StrategyPlain Strategy = "plain"
)

// Policy 单个字段的策略。
type Policy struct {
	Field      string   `json:"field"`
	Strategy   Strategy `json:"strategy"`
	KeepPrefix int      `json:"keep_prefix"`
	KeepSuffix int      `json:"keep_suffix"`
	MaskChar   string   `json:"mask_char"`
	// Roles 允许看明文的角色。空表示没人能看明文——
	// 默认收紧，需要放开时显式配置，而不是反过来。
	Roles []string `json:"roles"`
	Note  string   `json:"note"`
}

// FieldKind 是识别出来的字段类型，用于本地兜底脱敏。
type FieldKind string

const (
	KindPhone  FieldKind = "phone"
	KindIDCard FieldKind = "id_card"
	KindEmail  FieldKind = "email"
	KindBank   FieldKind = "bank_card"
	KindName   FieldKind = "person_name"
	KindNone   FieldKind = "none"
)

// DefaultMaskChar 打码字符。
const DefaultMaskChar = "*"

// Apply 按策略处理一个值。
// role 为空表示匿名/无角色，只能拿到掩码。
func Apply(p Policy, value string, role string) string {
	switch p.Strategy {
	case StrategyHide:
		return "«已隐藏»"
	case StrategyPlain:
		// 即使是 plain，也要角色对得上；否则退化成 mask
		if role != "" && contains(p.Roles, role) {
			return value
		}
		return maskValue(value, len([]rune(value))/3, len([]rune(value))/4, p.MaskChar)
	case StrategyMask, "":
		fallthrough
	default:
		prefix, suffix := p.KeepPrefix, p.KeepSuffix
		if prefix == 0 && suffix == 0 {
			// 没配就按字段类型给一个合理默认，避免「配了但看不出效果」
			prefix, suffix = defaultKeep(detectKind(p.Field))
		}
		return maskValue(value, prefix, suffix, p.MaskChar)
	}
}

// maskValue 按「保留首 prefix、尾 suffix」打码。
// 用 rune 而不是 byte——中文姓名按字节切会切出乱码。
func maskValue(v string, prefix, suffix int, maskChar string) string {
	if v == "" {
		return v
	}
	ch := maskChar
	if ch == "" {
		ch = DefaultMaskChar
	}
	r := []rune(v)
	n := len(r)
	if prefix < 0 {
		prefix = 0
	}
	if suffix < 0 {
		suffix = 0
	}
	if prefix+suffix >= n {
		// 保留位覆盖了全串，退化成全掩码——
		// 否则「保留 3 位 + 保留 4 位」配在一个 5 位值上会直接返回原文
		return strings.Repeat(ch, n)
	}
	return string(r[:prefix]) + strings.Repeat(ch, n-prefix-suffix) + string(r[n-suffix:])
}

func defaultKeep(k FieldKind) (int, int) {
	switch k {
	case KindPhone:
		return 3, 4
	case KindIDCard:
		return 4, 2
	case KindBank:
		return 4, 4
	case KindEmail:
		return 1, 0
	case KindName:
		return 1, 0
	default:
		return 0, 0
	}
}

// detectKind 从字段名猜类型，用于没配策略时的默认保留位。
func detectKind(field string) FieldKind {
	f := strings.ToLower(field)
	switch {
	case strings.Contains(f, "phone") || strings.Contains(f, "mobile") || strings.Contains(f, "tel"):
		return KindPhone
	case strings.Contains(f, "id_card") || strings.Contains(f, "idcard") || strings.Contains(f, "identity"):
		return KindIDCard
	case strings.Contains(f, "email") || strings.Contains(f, "mail"):
		return KindEmail
	case strings.Contains(f, "bank") || strings.Contains(f, "card_no"):
		return KindBank
	case strings.Contains(f, "name") || strings.Contains(f, "real_name"):
		return KindName
	}
	return KindNone
}

// Detect 用本地规则识别值属于哪类敏感数据。
// 这是远端识别服务不可用时的兜底，覆盖面比远端窄，但不会阻塞请求。
func Detect(value string) FieldKind {
	s := strings.TrimSpace(value)
	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	switch {
	case strings.Contains(s, "@") && strings.Contains(s, "."):
		return KindEmail
	case digits == 11 && (strings.HasPrefix(s, "1")):
		return KindPhone
	case digits == 18 || (digits == 17 && strings.HasSuffix(strings.ToUpper(s), "X")):
		return KindIDCard
	case digits >= 16 && digits <= 19:
		// 银行卡长度区间；和手机号/证件的判别顺序已经把它排除了
		return KindBank
	}
	if utf8.RuneCountInString(s) <= 4 && utf8.RuneCountInString(s) >= 2 && !hasDigit(s) {
		// 短中文串按姓名处理，这条只是启发式，可能误伤，
		// 所以只在没有配置策略、且远端也不可用时才会走到
		return KindName
	}
	return KindNone
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
