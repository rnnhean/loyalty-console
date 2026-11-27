package masking

import "testing"

func TestMaskValueKeepsRunes(t *testing.T) {
	cases := []struct {
		name          string
		value         string
		prefix, suffix int
		want          string
	}{
		{"手机号", "13800001234", 3, 4, "138****1234"},
		{"身份证", "110101199001011234", 4, 2, "1101************34"},
		{"中文姓名", "张三丰", 1, 0, "张**"},
		{"邮箱前缀", "alice@example.com", 1, 0, "a****************"},
		{"空串", "", 3, 4, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := maskValue(c.value, c.prefix, c.suffix, "*"); got != c.want {
				t.Fatalf("期望 %q 实际 %q", c.want, got)
			}
		})
	}
}

// 保留位覆盖全串时必须退化成全掩码，
// 否则「保留 3 + 保留 4」配在一个 5 位值上会原样返回——这就是泄漏。
func TestMaskValueNeverLeaksShortValue(t *testing.T) {
	got := maskValue("12345", 3, 4, "*")
	if got == "12345" {
		t.Fatal("短值不能原样返回")
	}
	if got != "*****" {
		t.Fatalf("应全掩码，实际 %q", got)
	}
}

func TestApplyStrategies(t *testing.T) {
	phone := Policy{Field: "member_phone", Strategy: StrategyMask, KeepPrefix: 3, KeepSuffix: 4}
	if got := Apply(phone, "13800001234", ""); got != "138****1234" {
		t.Fatalf("mask 策略结果不对: %q", got)
	}
	hide := Policy{Field: "member_id_card", Strategy: StrategyHide}
	if got := Apply(hide, "110101199001011234", ""); got != "«已隐藏»" {
		t.Fatalf("hide 策略结果不对: %q", got)
	}
	plain := Policy{Field: "member_phone", Strategy: StrategyPlain, Roles: []string{"admin"}}
	if got := Apply(plain, "13800001234", "admin"); got != "13800001234" {
		t.Fatalf("admin 应能看明文，实际 %q", got)
	}
	if got := Apply(plain, "13800001234", "viewer"); got == "13800001234" {
		t.Fatal("非白名单角色不能看明文")
	}
}

func TestApplyDefaultKeepWhenNotConfigured(t *testing.T) {
	p := Policy{Field: "member_phone", Strategy: StrategyMask}
	if got := Apply(p, "13800001234", ""); got != "138****1234" {
		t.Fatalf("未配置保留位时应按字段类型给默认值，实际 %q", got)
	}
}

func TestDetect(t *testing.T) {
	cases := map[string]FieldKind{
		"13800001234":          KindPhone,
		"110101199001011234":   KindIDCard,
		"11010119900101123X":   KindIDCard,
		"alice@example.com":    KindEmail,
		"6222021234567890123":  KindBank,
	}
	for value, want := range cases {
		if got := Detect(value); got != want {
			t.Fatalf("%q 期望 %s 实际 %s", value, want, got)
		}
	}
	if got := Detect("这不是敏感数据"); got == KindPhone || got == KindIDCard {
		t.Fatalf("普通文本被误判成 %s", got)
	}
}

// 远端不可用时必须降级而不是报错——脱敏在展示路径上，不能阻断列表。
func TestApplyFallsBackWhenRemoteUnavailable(t *testing.T) {
	svc := NewService(nil, NewRemoteClient("http://127.0.0.1:1", 50*1e6)) // 必然连不上
	res := svc.Apply(t.Context(), nil, "member_phone", "13800001234", "")
	if res.Source != "local" {
		t.Fatalf("应降级到本地识别，实际来源 %s", res.Source)
	}
	if res.Masked != "138****1234" {
		t.Fatalf("本地降级结果不对: %q", res.Masked)
	}
}

func TestApplyLeavesNonSensitiveAlone(t *testing.T) {
	svc := NewService(nil, nil)
	res := svc.Apply(t.Context(), nil, "product_title", "文档云 Pro", "")
	if res.Masked != "文档云 Pro" {
		t.Fatalf("非敏感字段不应被改，实际 %q", res.Masked)
	}
	if res.Source != "none" {
		t.Fatalf("来源应为 none，实际 %s", res.Source)
	}
}
