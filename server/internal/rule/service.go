package rule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/example/loyalty-console/server/internal/observ"
)

// Service 规则服务。
type Service struct{ Store *Store }

// NewService 构造。
func NewService(s *Store) *Service { return &Service{Store: s} }

// ValidateForSave 保存前校验。
// 空名称、空表达式、非法算子都在这里拦住，而不是等到求值时才炸。
func ValidateForSave(r Rule) error {
	if r.Name == "" {
		return errors.New("规则名不能为空")
	}
	if len(r.Name) > 64 {
		return errors.New("规则名不能超过 64 字")
	}
	if r.Domain == "" {
		return errors.New("必须指定所属域")
	}
	if err := r.Expr.Validate(); err != nil {
		return err
	}
	if r.Action == "" {
		return errors.New("必须填写命中后的动作")
	}
	if len(r.Action) > 200 {
		return errors.New("动作描述不能超过 200 字")
	}
	return nil
}

// Create 新建。
func (s *Service) Create(ctx context.Context, r Rule) (int64, error) {
	if err := ValidateForSave(r); err != nil {
		return 0, err
	}
	return s.Store.Create(ctx, r)
}

// Update 更新。
func (s *Service) Update(ctx context.Context, r Rule) error {
	if err := ValidateForSave(r); err != nil {
		return err
	}
	return s.Store.Update(ctx, r)
}

// HitStat 是试运行里的一行结果。
type HitStat struct {
	RuleID   int64  `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Hit      bool   `json:"hit"`
	Steps    []Step `json:"steps"`
	Missing  []string `json:"missing_fields,omitempty"`
}

// DryRun 用样例数据试跑一批规则，不落库、不影响线上判定。
//
// 为什么必须有：规则是运营写的，改一条就可能影响成千上万会员。
// 上线前先在样例上跑一遍，是唯一能让运营自己确认「我写对了」的手段。
func (s *Service) DryRun(ctx context.Context, domain string, data map[string]any) ([]HitStat, error) {
	started := time.Now()
	defer func() {
		observ.RuleEvalDuration.WithLabelValues("batch").Observe(time.Since(started).Seconds())
	}()
	rules, err := s.Store.List(ctx, domain, 200)
	if err != nil {
		return nil, err
	}
	out := make([]HitStat, 0, len(rules))
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		res := Eval(r.Expr, data)
		if res.Hit {
			observ.RuleEvalTotal.WithLabelValues("hit").Inc()
		} else {
			observ.RuleEvalTotal.WithLabelValues("miss").Inc()
		}
		out = append(out, HitStat{
			RuleID:   r.ID,
			RuleName: r.Name,
			Hit:      res.Hit,
			Steps:    res.Steps,
			Missing:  res.MissingFields,
		})
	}
	return out, nil
}

// Explain 解释单条规则为什么命中/没命中。
//
// 和 DryRun 的区别：DryRun 是「这批规则哪些会命中」，
// Explain 是「这一条为什么是这个结果」——后者是 Agent 最常用的形态。
func (s *Service) Explain(ctx context.Context, id int64, data map[string]any) (Result, Rule, error) {
	started := time.Now()
	defer func() {
		observ.RuleEvalDuration.WithLabelValues("single").Observe(time.Since(started).Seconds())
	}()
	r, err := s.Store.Get(ctx, id)
	if err != nil {
		return Result{}, Rule{}, err
	}
	res := Eval(r.Expr, data)
	if res.Hit {
		observ.RuleEvalTotal.WithLabelValues("hit").Inc()
	} else {
		observ.RuleEvalTotal.WithLabelValues("miss").Inc()
	}
	return res, r, nil
}

// SampleData 给试运行用的默认样例。
// 只包含虚构字段，不含任何真实会员数据。
//
// 刻意取「典型问题样例」而不是全 0/全 1：
// 全默认值的样例跑出来一条都不命中，运营看不出任何东西，
// 模型也会因为「结果为空」反复重试（实测连调三次 simulate_rules）。
// 这里的值对应常见的两种问题：沉默会员、层级上线状态缺失。
func SampleData() map[string]any {
	return map[string]any{
		"tier.state":         1,
		"tier.online":        0,
		"tier.rank":          3,
		"group.state":        1,
		"rule.quota":         10,
		"rule.unit":          "次",
		"member.login_days":  45,
		"member.total_spend": 0,
		"order.amount":       0,
		"channel.state":      1,
		"slot.bound":         true,
	}
}

// Summary 把求值结果压成一句话，给 Agent 和前端用。
// 不做花哨的自然语言生成——模型会基于 trace 自己组织语言，
// 这里只负责给出「命中了没有、卡在哪一步」。
func Summary(res Result) string {
	if res.Hit {
		return "命中"
	}
	if len(res.MissingFields) > 0 {
		return fmt.Sprintf("未命中：缺少字段 %v", res.MissingFields)
	}
	for _, st := range res.Steps {
		if st.Field != "" && !st.Hit {
			return fmt.Sprintf("未命中：%s 实际为 %v，需要满足 %s %s",
				st.Field, st.Actual, st.Op, st.Expected)
		}
	}
	return "未命中"
}
