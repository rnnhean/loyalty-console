package diag

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// ── benefit_chain ────────────────────────────────────────────

// benefitChain：一条权益链路是否走通。
//
// 链路是「层级 → 权益包 → 规则」，逐环检查，**只报最靠上的那个断点**。
// 理由：下游的缺失几乎总是上游断掉的后果，全报一遍会让运营
// 去修三个地方，而实际上只需要修一个。
//
// 产品为空时无法确定链路起点——不能默认取第一个产品，
// 那是替使用者做了一个它没有授权的选择，而且会给出看起来很确定的错误答案。
func benefitChain(ctx context.Context, ds DataSource, p Params) (Result, error) {
	product := paramString(p, "product")
	if product == "" {
		return inconclusive("product", "没有指定产品，无法确定要检查哪条权益链路"), nil
	}

	tiers, err := ds.ListTiers(ctx, product, scanLimit+limitProbe)
	if err != nil {
		return Result{}, err
	}
	groups, err := ds.ListGroups(ctx, product, scanLimit+limitProbe)
	if err != nil {
		return Result{}, err
	}
	tasks, err := ds.ListTasks(ctx, product, scanLimit+limitProbe)
	if err != nil {
		return Result{}, err
	}

	truncated := len(tiers) > scanLimit || len(groups) > scanLimit || len(tasks) > scanLimit
	if len(tiers) > scanLimit {
		tiers = tiers[:scanLimit]
	}
	if len(groups) > scanLimit {
		groups = groups[:scanLimit]
	}
	if len(tasks) > scanLimit {
		tasks = tasks[:scanLimit]
	}

	res := newResult(len(tiers) + len(groups) + len(tasks))

	// 第一环：层级。
	if len(tiers) == 0 {
		res.Verdict = VerdictIssue
		res.Findings = append(res.Findings, Finding{
			ID:       "tier:missing",
			Severity: SeverityHigh,
			Title:    fmt.Sprintf("产品「%s」没有任何启用中的会员层级", product),
			Detail:   "链路的第一环就断了：没有层级，权益包无处可挂，后面所有配置都不会对会员生效。",
			Evidence: map[string]any{"product": product, "tiers": 0},
			Link:     "/benefit/tiers",
		})
		finish(&res, truncated, scanLimit)
		return res, nil
	}

	// 第二环：权益包。层级存在但没有包，等于有壳没内容。
	if len(groups) == 0 {
		res.Verdict = VerdictIssue
		res.Findings = append(res.Findings, Finding{
			ID:       "group:missing",
			Severity: SeverityHigh,
			Title:    fmt.Sprintf("产品「%s」有 %d 个层级，但没有挂在任何层级下的权益包", product, len(tiers)),
			Detail:   "层级建好了，但没有权益包挂上去，会员看不到任何权益内容。",
			Evidence: map[string]any{"product": product, "tiers": len(tiers), "groups": 0},
			Link:     "/benefit/groups",
		})
		finish(&res, truncated, scanLimit)
		return res, nil
	}

	// 层级与包的对接：包上的 tier_uid 要指向真实存在的层级。
	tierByUID := make(map[int64]Tier, len(tiers))
	for _, t := range tiers {
		tierByUID[t.UID] = t
	}
	brokenGroup := false
	for _, g := range groups {
		if g.TierUID == 0 {
			continue // 不挂层级是合法配置（通用包），不是断链
		}
		if _, ok := tierByUID[g.TierUID]; ok {
			continue
		}
		res.Findings = append(res.Findings, Finding{
			ID:       fmt.Sprintf("group:%d", g.UID),
			Severity: SeverityHigh,
			Title:    fmt.Sprintf("「%s」挂在不存在的层级 #%d 上", g.Title, g.TierUID),
			Detail:   "权益包的 tier_uid 指向一个不在当前产品启用层级里的记录。这个包不会出现在任何会员的层级页上。",
			Evidence: map[string]any{
				"group_uid": g.UID, "group_title": g.Title,
				"tier_uid": g.TierUID, "product": product,
			},
			Link: fmt.Sprintf("/benefit/groups?focus=%d", g.UID),
		})
		brokenGroup = true
	}
	if brokenGroup {
		finish(&res, truncated, scanLimit)
		return res, nil
	}

	// 第三环：任务阈值的发奖包要真实存在。
	groupByUID := make(map[int64]Group, len(groups))
	for _, g := range groups {
		groupByUID[g.UID] = g
	}
	for _, t := range tasks {
		if t.RewardGroupUID == 0 {
			continue // 不发奖的任务是合法的（纯进度类）
		}
		if _, ok := groupByUID[t.RewardGroupUID]; ok {
			continue
		}
		res.Findings = append(res.Findings, Finding{
			ID:       fmt.Sprintf("task:%d", t.UID),
			Severity: SeverityMid,
			Title:    fmt.Sprintf("任务「%s」的发奖包 #%d 不存在", t.Title, t.RewardGroupUID),
			Detail:   "任务完成时会按 reward_group_uid 发奖，但该权益包不在当前产品的启用包里。用户做完任务拿不到东西。",
			Evidence: map[string]any{
				"task_uid": t.UID, "task_title": t.Title,
				"reward_group_uid": t.RewardGroupUID, "product": product,
			},
			Link: "/benefit/tasks",
		})
	}

	finish(&res, truncated, scanLimit)
	return res, nil
}

// ── test_data_scan ───────────────────────────────────────────

// testPatterns 是「一眼看出是测试数据」的名称特征。
//
// 只收明确的自述词，不收模糊的（比如「临时」「占位」）——
// 后者在真实运营数据里也可能出现，误报会让这个规则失去可信度，
// 而运营一旦不信第一条，后面的结论就都不看了。
var testPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\btest\b`),
	regexp.MustCompile(`测试`),
	regexp.MustCompile(`(?i)\bdemo\b`),
	regexp.MustCompile(`验收`),
	regexp.MustCompile(`勿投`),
	regexp.MustCompile(`(?i)\bsample\b`),
}

// pureNumeric 纯数字名称：多半是自动化脚本造的数据。
var pureNumeric = regexp.MustCompile(`^\d+$`)

// testDataScan：扫出「像测试数据」且还处于启用状态的对象。
//
// 关键限定是 Enabled：草稿或已停用的测试数据是正常的，
// 只有「启用的测试数据」才会被真实用户看到——那才是问题。
func testDataScan(ctx context.Context, ds DataSource, _ Params) (Result, error) {
	ents, err := ds.ListNamedEntities(ctx, scanLimit+limitProbe)
	if err != nil {
		return Result{}, err
	}

	truncated := len(ents) > scanLimit
	if truncated {
		ents = ents[:scanLimit]
	}

	res := newResult(len(ents))
	for _, e := range ents {
		if !e.Enabled {
			continue
		}
		if !looksLikeTestData(e.Name) {
			continue
		}
		res.Findings = append(res.Findings, Finding{
			ID:       fmt.Sprintf("%s:%d", e.Table, e.UID),
			Severity: SeverityMid,
			Title:    fmt.Sprintf("启用中的「%s」看起来是测试数据", e.Name),
			Detail:   "名称带有测试标记，但记录处于启用状态，会出现在真实用户的页面上。确认无用的话应该停用或删除。",
			Evidence: map[string]any{
				"table": e.Table, "uid": e.UID, "name": e.Name,
			},
			Link: linkForTable(e.Table, e.UID),
		})
	}

	finish(&res, truncated, scanLimit)
	return res, nil
}

func looksLikeTestData(name string) bool {
	if name == "" {
		return false
	}
	if pureNumeric.MatchString(strings.TrimSpace(name)) {
		return true
	}
	for _, re := range testPatterns {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// linkForTable 把逻辑表名映射到前端路由。
// 映射不到就不给链接——给一个点进去 404 的链接比不给更糟。
func linkForTable(table string, uid int64) string {
	routes := map[string]string{
		"rule":     "/benefit/rules",
		"group":    "/benefit/groups",
		"tier":     "/benefit/tiers",
		"task":     "/benefit/tasks",
		"batch":    "/operation/vouchers",
		"slot":     "/operation/slots",
		"campaign": "/operation/campaigns",
		"policy":   "/catalog/pricing",
		"spu":      "/catalog/spu",
	}
	route, ok := routes[table]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s?focus=%d", route, uid)
}

// ── change_impact ────────────────────────────────────────────

// changeImpact：一个开关改动会影响什么。
//
// 这不是「找错误」，是「改之前先看清楚」——所以它几乎总是返回 ok 或 issue，
// 很少 inconclusive。找不到开关才是 inconclusive（key 写错了）。
func changeImpact(ctx context.Context, ds DataSource, p Params) (Result, error) {
	key := paramString(p, "toggle_key")
	if key == "" {
		return inconclusive("toggle_key", "没有指定 toggle_key，无法判断改动影响面"), nil
	}

	toggles, err := ds.ListToggles(ctx, scanLimit+limitProbe)
	if err != nil {
		return Result{}, err
	}

	truncated := len(toggles) > scanLimit
	if truncated {
		toggles = toggles[:scanLimit]
	}

	var target *FeatureToggle
	for i := range toggles {
		if toggles[i].Key == key {
			target = &toggles[i]
			break
		}
	}

	if target == nil {
		return inconclusive("toggle_key",
			fmt.Sprintf("开关「%s」不存在，无法判断影响面（可能是 key 写错，或它已被删除）", key)), nil
	}

	res := newResult(len(toggles))

	// 同产品的其他开关：改这个开关时，同一个产品下还有哪些开关在生效。
	// 这不是问题，是「影响面」——所以给 low 严重度，让人知道该看哪里。
	sameProduct := make([]FeatureToggle, 0, 4)
	for _, t := range toggles {
		if t.UID == target.UID || t.Product != target.Product || t.Product == "" {
			continue
		}
		sameProduct = append(sameProduct, t)
	}

	if len(sameProduct) > 0 {
		others := make([]string, 0, len(sameProduct))
		for _, t := range sameProduct {
			others = append(others, t.Key)
		}
		res.Findings = append(res.Findings, Finding{
			ID:       fmt.Sprintf("toggle:%d", target.UID),
			Severity: SeverityLow,
			Title:    fmt.Sprintf("「%s」改动时会同时影响产品「%s」的 %d 个开关", key, target.Product, len(sameProduct)),
			Detail:   "同一产品下的开关经常组合生效。改这一个之前，确认其他开关的当前取值是不是你预期的组合。",
			Evidence: map[string]any{
				"toggle_key":    target.Key,
				"product":       target.Product,
				"current_value": target.Value,
				"related_keys":  others,
			},
			Link: "/setting/toggles",
		})
	}

	finish(&res, truncated, scanLimit)
	return res, nil
}

// ── site_audit ───────────────────────────────────────────────

// siteAudit：把其他规则聚合成一次整体体检。
//
// 它不自己查库，只调用 Registry 里已注册的规则——
// 这样新增一条规则会自动进体检，不需要在这里再改一遍。
//
// 名字本身在 Registry 里（agent 需要看到它），但 Run 是在 init() 里
// 事后填上的：直接写成字面量会造成 Registry → siteAudit → Registry 的
// 初始化环，Go 编译不过。
func siteAudit(ctx context.Context, ds DataSource) (Result, error) {
	var names []string
	for _, c := range Registry {
		if c.Name == siteAuditCheckName {
			continue
		}
		names = append(names, c.Name)
	}

	res := newResult(0)
	for _, name := range names {
		c, ok := Find(name)
		if !ok {
			continue
		}
		// 体检不带参数：带参数的规则（benefit_chain / change_impact）
		// 在这里会返回 inconclusive，被如实计入「没查成」。
		r, err := c.Run(ctx, ds, nil)
		if err != nil {
			return Result{}, err
		}
		res.Coverage.Checked += r.Coverage.Checked
		res.Coverage.Skipped += r.Coverage.Skipped
		res.Coverage.Missing = append(res.Coverage.Missing, r.Coverage.Missing...)
		for _, f := range r.Findings {
			// 加规则前缀：不同规则可能产出同 id，裸 id 在聚合视图里会撞。
			f.ID = name + "/" + f.ID
			res.Findings = append(res.Findings, f)
		}
	}

	// 聚合层不判断截断：子规则各自处理自己的截断，并把 missing 汇总上来。
	// 这里传 false，只让 finish 决定「有 finding 就是 issue」。
	finish(&res, false, scanLimit)

	// finish 只看「有没有 finding」，不看 missing。但聚合规则的特殊之处在于：
	// 一个 finding 都没有、却有几条缺参数没查成，这时候报 ok 就是骗人——
	// 「四条规则里两条根本没跑」，不能说成「一切正常」。
	if len(res.Findings) == 0 && len(res.Coverage.Missing) > 0 {
		res.Verdict = VerdictInconclusive
		if res.Confidence > 0.5 {
			res.Confidence = 0.5
		}
	}
	return res, nil
}

// inconclusive 造一个「查不了」的结论。
// missing 必须写清楚缺什么，否则模型会把这句当成「没问题」来讲。
func inconclusive(param, why string) Result {
	return Result{
		Verdict:    VerdictInconclusive,
		Confidence: 0,
		Findings:   []Finding{},
		Coverage: Coverage{
			Checked: 0,
			Missing: []string{why},
		},
	}
}
