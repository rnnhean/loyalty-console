package diag

import (
	"context"
	"strings"
	"testing"
)

// 注册表自身的完整性：启动自检靠它，它自己错了就没意义。
func TestValidate(t *testing.T) {
	// 现有的注册表必须是合法的，否则网关起不来。
	if err := Validate(); err != nil {
		t.Fatalf("内置注册表校验失败: %v", err)
	}
}

func TestFindNotExist(t *testing.T) {
	// 模型编一个不存在的规则名是常态，必须能干净地返回 false 而不是 panic。
	if _, ok := Find("no_such_check_xyz"); ok {
		t.Fatal("不存在的规则名不该被找到")
	}
}

// 校验必须真的能拦住坏配置——用一个临时坏表替换，跑完恢复。
func TestValidateCatchesBadRegistry(t *testing.T) {
	cases := []struct {
		name  string
		bad   Check
		match string
	}{
		{name: "没名字", bad: Check{TimeoutMS: 1, Run: dummyRun}, match: "没有名称"},
		{name: "没 Run", bad: Check{Name: "x_missing_run", TimeoutMS: 1}, match: "没有 Run"},
		{name: "超时非正", bad: Check{Name: "x_bad_timeout", TimeoutMS: 0, Run: dummyRun}, match: "TimeoutMS"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := Registry
			defer func() { Registry = old }()
			Registry = append(append([]Check{}, old...), c.bad)

			err := Validate()
			if err == nil {
				t.Fatal("坏配置必须被拦下")
			}
			if !strings.Contains(err.Error(), c.match) {
				t.Errorf("错误信息 %q 没提到 %q", err.Error(), c.match)
			}
		})
	}
}

// 重名必须被拦下：两条同名规则会让 Find 静默只返回第一条，
// 另一条永远不会执行，而且没有任何报错。
func TestValidateRejectsDuplicate(t *testing.T) {
	old := Registry
	defer func() { Registry = old }()
	Registry = append(append([]Check{}, old...), Check{
		Name:      "orphan_rules",
		Title:     "重名的伪规则",
		TimeoutMS: 1000,
		Run:       dummyRun,
	})
	if err := Validate(); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重名必须被拦下，got err = %v", err)
	}
}

func dummyRun(context.Context, DataSource, Params) (Result, error) {
	return Result{Verdict: VerdictOK}, nil
}
