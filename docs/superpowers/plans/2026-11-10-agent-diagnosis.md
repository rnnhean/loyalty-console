# 运营诊断助手 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 loyalty-console 管理台上落一个运营诊断助手：确定性的诊断规则跑在 Go 后端，Node/TS 的 agent 负责选规则、组织语言、流式输出结论。

**Architecture:** 三段式。Go 后端新增 `server/internal/diag/` 领域包与 `/api/diag/*` 三个接口，规则是纯函数 + 诊断专用 SQL，模型不参与判定；新增 `agent/` 独立 Node 服务跑手写 loop，只通过 HTTP 取数，不直连数据库；`web/` 新增抽屉消费 SSE。

**Tech Stack:** Go 1.26（`gin` / 现有 `store` + `domain`）/ Node 20 + TypeScript + Fastify / React 18 + antd 5 / MySQL 8

**Spec:** `docs/TECH-DESIGN-AGENT.md`（下称「设计」）。本计划中所有 `§x.y` 均指该文档章节，执行时两篇一起读。

## 执行顺序：分两个阶段交付

**Phase A（Task 1–4、4.5）先交付一个能用的东西：体检报告页。** 5 条无参规则跑一遍，有问题列出来，每条带跳转链接。没有模型、没有对话、没有 40 秒等待。

**Phase B（Task 5–13）才是对话层。**

这个顺序不是排期，是**先验证价值、再投入成本**：体检页的价值是确定的，两三天能做完，运营立刻能用；对话层的价值建立在一个没被验证的假设上——运营愿意用自然语言描述问题。真实情况很可能是他们更想点「全面体检」，然后看一份带链接的清单。

**Phase A 跑两周，看有没有人抱怨「我只能看，不能问」。** 有人抱怨了再开 Phase B——那时候连提示词该写什么都知道，因为抱怨里就写着。没人抱怨，说明对话层不必做，省下的时间比什么都值钱。

Phase B 的 Task 5–13 在 Phase A 验收通过前不启动。

## Global Constraints

- **判定归代码，说话归模型。** 任何「有没有问题」「算不算异常」的判断只能出现在 `server/internal/diag/`，agent 与提示词都不许做判定。
- **`verdict` 只有三值**：`ok` / `issue` / `inconclusive`，没有第四种。`inconclusive` 必须带非空的 `coverage.missing`。
- **所有 HTTP 响应沿用后端既有信封**：成功 `{"code":0, ...}`，失败 `{"code":500,"message":"查询失败","trace_id":"..."}`。**不是** `error_no` 结构。
- **`Run` 的 `error` 只表示「没跑成」**，不表示「有问题」。业务上的「有问题」是 `findings` 非空。
- **仓库内不出现真实模型型号、渠道前缀与原始单价。** 配置样例、注释、测试数据一律用 `M1`–`M12` 代号。
- **`.env.example` 只写假值**（`YOUR_KEY_HERE`）。任何凭据不进仓库、不打印、不落盘到项目子目录。
- **未经明确指示不 commit / push。** 本计划的 Commit 步骤是给执行者的动作清单，实际执行时按调用方授权走。
- Node 20、pnpm；Go 1.26；前端已有依赖不新增框架。

## Review Focus

按「最可能出问题、而单个任务的测试覆盖不到」排序：

1. **空结果集被当成「一切正常」。** 规则查不到数据时若返回 `ok`，运营看到的是绿色的「无问题」——而真相是没查到。每条规则必须区分「查了、没问题」与「没查到」。
2. **证据链断裂。** 模型引用了不存在的 `finding_id`，或者 finding 的 `link` 跳到不存在的行。前者是编造，后者是信任崩塌。
3. **预算是假的。** 金额闸门用 token 累加、或单价与模型不匹配，导致闸门在换模型那天失效。
4. **关闭抽屉把请求也杀了。** 用户切页面回来发现诊断没了，或者更糟：请求继续但前端状态丢了，重开显示空白。
5. **SSE 事件粘包 / 半行 JSON。** 分块边界正好切在 `data:` 中间，解析器静默丢事件。

每条的具体测试放在对应任务的 Step 里，编号见各任务「Review Focus 覆盖」行。

---

## File Structure

**新增（Go 后端）**

| 文件 | 职责 |
| --- | --- |
| `server/internal/diag/check.go` | 类型定义：`Verdict` / `Severity` / `Finding` / `Coverage` / `Result` / `ParamSpec` / `DataSource` 与行类型 |
| `server/internal/diag/registry.go` | `Registry`、`Find`、`Validate`；`site_audit` 的 `Run` 在 `init()` 里补（绕开初始化环） |
| `server/internal/diag/util.go` | `scanLimit` / `newResult` / `finish`：截断降级统一在这里做 |
| `server/internal/diag/checks.go` | 前 4 条规则：`orphan_rules` / `voucher_health` / `slot_bindings` / `pricing_visibility` |
| `server/internal/diag/checks_more.go` | 后 4 条：`benefit_chain` / `test_data_scan` / `change_impact` / `site_audit` |
| `server/internal/diag/queries.go` | `SQLSource`：实现 `DataSource`，诊断专用 SQL |
| `server/internal/diag/fakesource.go` | `FakeSource`：非 `_test.go`，供 handler 测试与跨包复用 |
| `server/internal/diag/check_test.go` / `checks_more_test.go` | 规则单测（每规则 ≥3 用例） |
| `server/cmd/gateway/diag_handlers.go` | 三个 handler（`listChecks` / `runCheck` / `runBatch`） |
| `server/cmd/gateway/diag_handlers_test.go` | 8 个 handler 测试 |
| `server/cmd/gateway/main.go` | 改：`diag.Validate()` 启动自检 + 注册 `/api/diag` 路由 |

**新增（agent）**

| 文件                                              | 职责                                         |
| ------------------------------------------------- | -------------------------------------------- |
| `agent/package.json` / `tsconfig.json`        | 工程骨架                                     |
| `agent/shared/contract.ts`                      | 报告与事件的唯一类型来源，`web/` 也 import |
| `agent/src/server.ts`                           | Fastify 入口、SSE 路由、开关与限流挂载       |
| `agent/src/guard.ts`                            | 开关、三层限流、当日金额闸门                 |
| `agent/src/ledger.ts`                           | 事实层，只追加                               |
| `agent/src/loop.ts`                             | 四态状态机 +`stop_reason`                  |
| `agent/src/validate.ts`                         | 引用校验与重试守卫                           |
| `agent/src/provider.ts`                         | 模型调用（含假模型分支）                     |
| `agent/src/prompt.ts`                           | 系统提示词组装                               |
| `agent/src/tools.ts`                            | 工具 schema 与执行                           |
| `agent/src/trace.ts`                            | OTel 字段格式的 trace 落盘                   |
| `agent/testdata/fake_script.json`               | 假模型脚本（非生产）                         |
| `agent/eval/cases.json` / `agent/eval/run.ts` | 评测集与 runner                              |

**新增（前端）**

| 文件 | 职责 |
| --- | --- |
| `web/src/lib/diag.ts` | 后端契约的 TS 类型、`fetchChecks` / `runAudit`、纯函数 `summarize` |
| `web/src/pages/AuditPage.tsx` | 体检报告页：计数条 / 逐项结果 / 问题清单 / 未查全提示 |
| `web/src/pages/AuditPage.less` | 该页样式（语义色只用在结论上） |
| `web/src/lib/agent.ts` | SSE 客户端（纯函数解析器 + 连接管理） |
| `web/src/components/AgentDrawer/AgentDrawer.tsx` | 抽屉与状态机 |
| `web/src/components/AgentDrawer/ReportCard.tsx` 等 | 三张卡与步骤列表 |
| `web/src/layout/ConsoleLayout.tsx` | 改：挂载入口按钮 + 菜单加「运营体检」 |
| `web/src/App.tsx` | 改：加 `/audit` 路由 |

**改动（前置）**

| 文件                                   | 改动                                     |
| -------------------------------------- | ---------------------------------------- |
| `web/src/pages/DataPage.tsx:46-49`   | 状态文案统一走`STATE_TEXT`             |
| `web/src/pages/DataPage.tsx:137-160` | 筛选与 URL query 双向同步，支持`focus` |

---

### Task 1: 前端前置修复（状态文案 + URL 同步）

**Files:**

- Modify: `web/src/pages/DataPage.tsx:46-49`
- Modify: `web/src/pages/DataPage.tsx:137-160`
- Test: `web/` 无单测框架，靠 `pnpm typecheck` + 手动验证

**Interfaces:**

- Consumes: `STATE_TEXT`（`web/src/config/console.ts:67`）、`STATE_COLOR`
- Produces: `DataPage` 支持 URL 参数 `product` / `keyword` / `focus`；供 Task 10 的证据跳转使用

**为什么先做**：设计 §13.1 明确——不做这两条，证据跳转无法验收，整个诊断的「结论可核」闸门过不了。

- [ ] **Step 1: 把内联三元换成 `STATE_TEXT` 查表**

`web/src/pages/DataPage.tsx:46-49` 当前是：

```tsx
case 'state': {
    const n = Number(v);
    return <Tag color={STATE_COLOR[n] || 'default'}>{n === 1 ? '启用' : n === 2 ? '停用' : '草稿'}</Tag>;
}
```

改成：

```tsx
case 'state': {
    const n = Number(v);
    return <Tag color={STATE_COLOR[n] || 'default'}>{STATE_TEXT[n] ?? '—'}</Tag>;
}
```

状态 3 现在会正确显示「已删除」，与 `console.ts:67` 一致。

- [ ] **Step 2: 引入 `useSearchParams` 并初始化**

在 `DataPage` 顶部（`useState` 之前）：

```tsx
const [sp, setSp] = useSearchParams();
const focus = sp.get('focus');
```

把 `product` / `committedKeyword` 的初始值改为从 URL 读：

```tsx
const [product, setProduct] = useState<string>(sp.get('product') ?? '');
const [committedKeyword, setCommittedKeyword] = useState(sp.get('keyword') ?? '');
```

- [ ] **Step 3: 筛选变化写回地址栏**

```tsx
useEffect(() => {
    const next = new URLSearchParams();
    if (product) next.set('product', product);
    if (committedKeyword) next.set('keyword', committedKeyword);
    if (focus) next.set('focus', focus);
    setSp(next, { replace: true });
}, [product, committedKeyword, focus, setSp]);
```

用 `replace`，不新增历史。

- [ ] **Step 4: 实现 `focus` 高亮与定位**

给 antd `Table` 加 `rowClassName`：

```tsx
rowClassName={(row) => (focus && String(row.uid) === focus ? 'row-focus' : '')}
```

在首次数据加载完成后滚动到高亮行：

```tsx
useEffect(() => {
    if (!focus || !rows.length) return;
    const el = document.querySelector('.row-focus');
    el?.scrollIntoView({ block: 'center', behavior: 'smooth' });
}, [focus, rows]);
```

`DataPage.less` 加：

```less
.row-focus td { background: #eef3ff !important; }
```

- [ ] **Step 5: 用户手动改筛选时清掉 `focus`**

在 `onChange` 的 product / keyword 处理器里，如果新值和 URL 里的不同，就删掉 `focus`：

```tsx
const next = new URLSearchParams(sp);
next.delete('focus');
setSp(next, { replace: true });
```

- [ ] **Step 6: 验证**

```bash
cd d:/20260916/loyalty-console/web && pnpm typecheck
```

Expected: 无 TS 错误。手动打开任意列表页，地址栏出现 `product=`；带 `?focus=<uid>` 打开时该行高亮并滚到中间。

- [ ] **Step 7: Commit**

```bash
git add web/src/pages/DataPage.tsx web/src/pages/DataPage.less
git commit -m "fix(web): 状态文案统一走 STATE_TEXT，列表页支持 URL 筛选与 focus 定位"
```

---

### Task 2: diag 包骨架 + 第一条规则（打通 TDD 循环）

**Files:**

- Create: `server/internal/diag/check.go`
- Create: `server/internal/diag/util.go`
- Create: `server/internal/diag/fakesource.go`
- Create: `server/internal/diag/queries.go`
- Create: `server/internal/diag/registry.go`
- Create: `server/internal/diag/checks.go`
- Create: `server/internal/diag/check_test.go`
- Create: `server/internal/diag/registry_test.go`
- Modify: `server/cmd/gateway/main.go`（启动自检调 `diag.Validate()`）

**Interfaces:**

- Produces:
  - `type Verdict string`，常量 `VerdictOK` / `VerdictIssue` / `VerdictInconclusive`
  - `type Result struct { Verdict Verdict; Confidence float64; Findings []Finding; Coverage Coverage }`
  - `type Check struct { Name, Title, Domain string; Params map[string]ParamSpec; CostHint string; TimeoutMS int; Run func(context.Context, DataSource, Params) (Result, error) }`
  - `var Registry []Check`
  - `func Find(name string) (Check, bool)`
  - `func Validate() error`
  - `type SQLSource struct { DB *sql.DB }`，实现 `DataSource`
  - `func newResult(checked int) Result` / `func finish(res *Result, truncated bool, limit int)`（`util.go`）
  - `type FakeSource struct {...}`（`fakesource.go`，**非 `_test.go`**——handler 测试要跨包用它）
- Consumes: 无（本任务自足）

**关于第一条规则选哪条**：用 `orphan_rules`（孤儿权益规则，`group_uid = 0` 且启用），**不用**设计稿早期举例的「券有效期与活动结束不一致」。原因：`ops_voucher_batch` 表里根本没有有效期字段（只有 `batch_state` / `lifecycle_state` / `quota_total` / `quota_issued`），那条规则要查的数据不存在，写出来只能靠编。`orphan_rules` 在真实 schema 上可查可验，且判定条件简单，适合当模板。

**注意 `group_uid` 是 `BIGINT NOT NULL DEFAULT 0`**：「未挂载」是等于 0，不是 `IS NULL`。写成 `IS NULL` 会永远返回空集，而空集被判成 `ok`——把「全都没挂」报成「一切正常」，这是最危险的一类错。

**Review Focus 覆盖**：第 1 条（空结果集不能判 ok）、第 4 条（数据源报错不能吞成 ok）、第 5 条（取数被截断不能报 ok）—— Step 1 的三个用例 + 截断用例覆盖。

- [ ] **Step 1: 写失败的测试**

`server/internal/diag/fakesource.go`：

```go
// 故意不是 _test.go：handler 测试在另一个包里要用它。
package diag

type FakeSource struct {
	Rules    []EntitlementRule
	Batches  []VoucherBatch
	Slots    []PromotionSlot
	Policies []PricingPolicy
	Tiers    []Tier
	Groups   []Group
	Tasks    []TaskThreshold
	Entities []NamedEntity
	Toggles  []FeatureToggle
	Now_     time.Time // 零值 → 固定为 2026-11-11 12:00 UTC，时间相关用例才不会随机失败
	Err      error     // 非空时每个 List* 都返回它，用来测「数据源报错不能吞成 ok」
}
```

每个 `List*` 方法做两件事：`Err` 非空就返回它，否则按 `product` 过滤并 `clip(in, limit)` 模拟 SQL `LIMIT`。

`server/internal/diag/check_test.go`：

```go
package diag

import (
	"context"
	"errors"
	"testing"
)

func TestOrphanRules(t *testing.T) {
	cases := []struct {
		name        string
		ds          DataSource
		wantVerdict Verdict
		wantIDs     []string
		wantChecked int
	}{
		{
			name: "命中：启用但 group_uid 为 0",
			ds: &FakeSource{Rules: []EntitlementRule{
				{UID: 101, Title: "生日礼", GroupUID: 0, LifecycleState: 1},
				{UID: 102, Title: "专属客服", GroupUID: 9, LifecycleState: 1},
			}},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"rule:101"},
			wantChecked: 2,
		},
		{
			name: "未命中：全都挂上了",
			ds: &FakeSource{Rules: []EntitlementRule{
				{UID: 201, Title: "生日礼", GroupUID: 3, LifecycleState: 1},
			}},
			wantVerdict: VerdictOK,
			wantChecked: 1,
		},
		{
			// 空表是「确实没有」，不是「没查到」——必须是 ok + checked:0。
			name:        "空表：ok 且 checked 为 0",
			ds:          &FakeSource{Rules: []EntitlementRule{}},
			wantVerdict: VerdictOK,
			wantChecked: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check, ok := Find("orphan_rules")
			if !ok {
				t.Fatal("orphan_rules 未注册")
			}
			res, err := check.Run(context.Background(), c.ds, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", res.Verdict, c.wantVerdict)
			}
			if res.Coverage.Checked != c.wantChecked {
				t.Errorf("checked = %d, want %d", res.Coverage.Checked, c.wantChecked)
			}
			var ids []string
			for _, f := range res.Findings {
				ids = append(ids, f.ID)
			}
			if !equalStrings(ids, c.wantIDs) {
				t.Errorf("ids = %v, want %v", ids, c.wantIDs)
			}
		})
	}
}

// 数据源报错必须是 error，不能变成「没发现问题」。
// 混淆的后果：库挂了，运营看到绿色「一切正常」。
func TestOrphanRules_DataSourceError(t *testing.T) {
	check, _ := Find("orphan_rules")
	_, err := check.Run(context.Background(), &FakeSource{Err: errors.New("db down")}, nil)
	if err == nil {
		t.Fatal("数据源报错必须返回 error，不能被吞成空结果")
	}
}

// 超过扫描上限时必须降级，不许报 ok：
// 6000 条只看了前 5000 条且都正常，剩下 1000 条可能全是孤儿。
func TestOrphanRules_TruncatedMustNotReportOK(t *testing.T) {
	clean := make([]EntitlementRule, 0, 6000)
	for i := 1; i <= 6000; i++ {
		clean = append(clean, EntitlementRule{UID: int64(i), GroupUID: 7, LifecycleState: 1})
	}
	check, _ := Find("orphan_rules")
	res, err := check.Run(context.Background(), &FakeSource{Rules: clean}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Verdict != VerdictInconclusive {
		t.Errorf("verdict = %q, want inconclusive", res.Verdict)
	}
	if len(res.Coverage.Missing) == 0 {
		t.Error("inconclusive 必须说明缺了什么")
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
cd d:/20260916/loyalty-console/server && go test ./internal/diag/ -run TestOrphanRules -v
```

Expected: 编译失败，`undefined: Find` / `undefined: EntitlementRule`。

- [ ] **Step 3: 写 `check.go` 类型定义**

按设计 §6.6.2 落 `Verdict` / `Severity` / `Finding` / `Coverage` / `ParamSpec` / `Params` / `EntitlementRule` / `DataSource` / `Check` / `Result`。

**`DataSource` 只声明本阶段用得到的方法**，后续任务按需加。方法按「取数」命名而不是按规则命名，且**查询里不带判定条件**：

```go
type EntitlementRule struct {
	UID            int64
	Title          string
	GroupUID       int64
	BenefitKind    int8
	LifecycleState int8
}

type DataSource interface {
	ListActiveRules(ctx context.Context, limit int) ([]EntitlementRule, error)
}
```

- [ ] **Step 4: 写 `registry.go`**

```go
var Registry = []Check{{Name: "orphan_rules", Title: "孤儿权益规则", Domain: "benefit", CostHint: "low", TimeoutMS: 6000, Run: orphanRules}}

// Find 名字不存在时返回 false，由调用方转 400（不是 404，见 Task 4）。
// 不要 panic——模型编一个规则名是常见情况，不是程序错误。
func Find(name string) (Check, bool)

// Validate 在启动时调用：检查 Name 非空且唯一、Run 非 nil、TimeoutMS > 0。
func Validate() error
```

- [ ] **Step 5: 实现 `orphanRules`**

`checks.go` 里的判定：

- `ListActiveRules(ctx, scanLimit+limitProbe)`——**多取一条用来判断是否被截断**。
- 逐条：`GroupUID != 0` 跳过；`GroupUID == 0` → `Finding{ID: "rule:"+uid, Severity: high, Link: "/benefit/rules?focus="+uid}`。
- `res := newResult(len(rules))`，最后调 `finish(&res, truncated, scanLimit)`。

`finish()` 在 `util.go`，一次把「截断 → `Coverage.Missing`、`Confidence` 0.5、零 finding 时 `inconclusive`；否则按 findings 判 `issue` / `ok`」做完。**这一步不要写进每条规则**——写十遍会漏一遍，而漏的那次正好会把「只查了一半」报成「没问题」。

- [ ] **Step 6: 写 `queries.go`（`SQLSource`）**

```go
type SQLSource struct{ DB *sql.DB }
var _ DataSource = (*SQLSource)(nil)
```

`ListActiveRules` 的 SQL：

```sql
SELECT rule_uid, rule_title, group_uid, benefit_kind, lifecycle_state
FROM bft_entitlement_rule
WHERE voided = 0 AND lifecycle_state = 1
ORDER BY rule_uid
LIMIT ?
```

**不复用 `domain.BenefitService.ListRules`**：那条路径带分页和 UI 排序，语义是给列表页用的；诊断要的是「当前生效的全集」，共用一条 SQL 会在两边各自改需求时互相牵连。

- [ ] **Step 7: 写 `registry_test.go`**

`Validate()` 对现有注册表返回 nil；重名 / 缺 Run / 缺 Name / `TimeoutMS <= 0` 各拦一次。

- [ ] **Step 8: 接启动自检**

`server/cmd/gateway/main.go`，在 `store.Open` **之前**（纯静态检查，不依赖数据库）：

```go
if err := diag.Validate(); err != nil {
	return err
}
```

- [ ] **Step 9: 运行确认通过**

```bash
cd d:/20260916/loyalty-console/server && go build ./... && go test ./internal/diag/ -v -count=1
```

Expected: 6 个用例全部 PASS，`go build ./...` 无输出。

- [ ] **Step 10: Commit**

```bash
git add server/internal/diag/ server/cmd/gateway/main.go
git commit -m "feat(diag): 规则框架与 orphan_rules 规则"
```

---

### Task 3: 其余 7 条规则

**Files:**

- Create: `server/internal/diag/checks_more.go`
- Modify: `server/internal/diag/check.go`（扩 `DataSource` 与查询结构体）
- Modify: `server/internal/diag/checks.go`
- Modify: `server/internal/diag/registry.go`
- Modify: `server/internal/diag/check_test.go`
- Test: `server/internal/diag/checks_more_test.go`

**Interfaces:**

- Consumes: Task 2 的 `Check` / `Result` / `Find` / `FakeSource` / `newResult` / `finish`
- Produces: `Registry` 含 8 条规则；每条都可用 `Find(name)` 取到

**规则清单见设计 §5.3。** 分成两批，避免一次改得太大。

- [ ] **Step 1: 先写 5 条无参数规则的测试**

`test_data_scan` / `orphan_rules` / `voucher_health` / `slot_bindings` / `site_audit`。每条至少：命中、未命中、空表（**空表必须 `ok` + `checked:0`，不能 `inconclusive`**——`inconclusive` 留给「该查的查不到」，空表是「确实没有」）。

- [ ] **Step 2: 运行确认全部失败**

```bash
cd d:/20260916/loyalty-console/server && go test ./internal/diag/ -v
```

Expected: 新用例 FAIL（`check not registered`）。

- [ ] **Step 3: 实现这 5 条**

每条都用 Task 2 的 `newResult` / `finish`，不要各自重写截断逻辑。

三个易错点按设计 §5.3 逐字落实：

- `group_uid` 判 `= 0`，**不是 `IS NULL`**（列是 `BIGINT NOT NULL DEFAULT 0`）。
- 定价发布状态是 `0` 未发布 / `4` 已发布，**不是 `0/1`**。
- `ops_voucher_batch` 本地无种子数据，测试用 `FakeSource`，不依赖本地库。

- [ ] **Step 4: 再写 3 条带参数规则的测试**

`benefit_chain(product)` / `pricing_visibility(product?)` / `change_impact(toggle_key)`。各补一个「参数缺失」用例：

- `benefit_chain` 缺 `product` → `inconclusive`，`missing` 含 `"product"`，**不能 panic，也不能默认取第一个产品**（设计 §15 风险表）。
- `change_impact` 缺 `toggle_key` → 同上。
- `pricing_visibility` **不缺参数就扫全部产品**（`product` 是可选的「缩小范围」）；补一个「指定了不存在的产品 → `inconclusive`」，而不是 `ok`。

- [ ] **Step 5: 实现这 3 条**

`benefit_chain` 按「层级 → 权益包 → 任务阈值」逐环检查，**只报最靠上的断点**——下游问题都是它的后果。`tier_uid == 0`（通用包）和 `reward_group_uid == 0`（不发奖任务）是合法配置，不报。

- [ ] **Step 6: 补启动自检**

`registry.go` 加 `func Validate() error`：检查 `Name` 非空且唯一、`Run` 非 nil、`TimeoutMS > 0`、`CostHint` ∈ {low, mid, high}。在 `main.go` 启动时（`store.Open` 之前）调用，失败即退出。

`site_audit` 的 `Run` **不能写在 `Registry` 字面量里**——它要遍历 `Registry`，会构成 Go 的初始化环（`Registry → siteAudit → Registry`），编译不过。字面量里留空，在 `init()` 里补：`init` 跑在包初始化完成之后，那时 `Registry` 已是最终值。

- [ ] **Step 7: 运行全部通过**

```bash
cd d:/20260916/loyalty-console/server && go test ./internal/diag/ -v -count=1
```

Expected: 8 条规则、≥24 个用例全 PASS。

- [ ] **Step 8: Commit**

```bash
git add server/internal/diag/
git commit -m "feat(diag): 补齐 8 条诊断规则与启动自检"
```

---

### Task 4: 诊断接口（三个 handler + 路由）

**Files:**

- Create: `server/internal/diag/queries.go`
- Create: `server/cmd/gateway/diag_handlers.go`
- Modify: `server/cmd/gateway/main.go`（在「基础设施能力接口」之前注册）
- Test: `server/cmd/gateway/diag_handlers_test.go`

**Interfaces:**

- Consumes: `diag.Registry` / `diag.Find` / `diag.Check.Run`
- Produces:
  - `GET /api/diag/checks` → `{"code":0,"total":N,"items":[...],"trace_id":"..."}`
  - `GET /api/diag/run/:name` → `{"code":0,"data":{...},"trace_id":"..."}`
  - `POST /api/diag/batch` → `{"code":0,"data":{"partial":bool,"results":[...]},"trace_id":"..."}`

- [ ] **Step 1: 写 handler 测试（用 `httptest` + `diag.FakeSource`）**

八个用例：

| 用例 | 断言 |
| --- | --- |
| `TestDiagChecks` | `items` 条数等于 `len(diag.Registry)`，每项都有 `name` / `timeout_ms` |
| `TestDiagRun` | `orphan_rules` 在有孤儿规则时 `verdict=issue`，`findings[0].link` 非空 |
| `TestDiagRunUnknownCheck` | 未知名字 → **400**（不是 404），`code=400` |
| `TestDiagRunDataSourceError` | `FakeSource.Err` 非空 → 500 |
| `TestDiagBatch` | 两条都成功 → `partial=false`，`results` 顺序与请求一致 |
| `TestDiagBatchPartial` | 一条名字不存在 → `partial=true`，该项 `verdict=""` + `error="unknown_check"` |
| `TestDiagBatchValidation` | `checks` 为空 / 超过 8 条 → 400 |
| `TestDiagRunWithParams` | `?product=` 传进规则，未声明的 query 键被忽略 |

- [ ] **Step 2: 运行确认失败**

```bash
cd d:/20260916/loyalty-console/server && go test ./cmd/gateway/ -run TestDiag -v
```

Expected: `undefined: listChecks`。

- [ ] **Step 3: 实现 `queries.go` 的 `SQLSource`**

实现 `DataSource` 全部方法。**每条查询必须带 `LIMIT` 上限**（`scanLimit = 5000`），并用「取 `limit+1` 条」判断是否被截断——截断时把说明写进 `Coverage.Missing`，不是塞进 `Skipped`（`Skipped` 是「有意跳过」）。

- [ ] **Step 4: 实现三个 handler**

用 `c.JSON(http.StatusOK, gin.H{"code":0, ...})` 直接返回，**不要套 `ok()`**——`ok()` 的信封是 `{code, items, total, trace_id}`，装不下 `data` 这个嵌套字段。`run` 的超时用 `check.TimeoutMS` 包 `context.WithTimeout`（`runWithTimeout`）。

`batch` **串行**执行，单条失败不整体失败：

```go
for _, name := range req.Checks {
    ch, found := diag.Find(name)
    if !found {
        out.Partial = true
        out.Results = append(out.Results, diagRunResponse{Check: name, Error: "unknown_check"})
        continue
    }
    // ...
}
```

失败项的 `verdict` 是空字符串、`error` 是短标签（`timeout` / `db_error` / `unknown_check`）。**`error` 只给机器看**，不塞堆栈——堆栈里可能有 SQL、表名、连接串片段。

- [ ] **Step 5: 注册路由**

`main.go` 里，`api` 组注册两条 GET；batch **单独开一组**：

```go
api.GET("/diag/checks", listChecks())
api.GET("/diag/run/:name", runCheck(diagDS))

audit := r.Group("/api/diag",
    gateway.RateLimit(apiLimiter),
    requireAuth,
    gateway.Timeout(time.Duration(envInt("MC_AUDIT_TIMEOUT_SEC", 35))*time.Second),
)
audit.POST("/batch", runBatch(diagDS))
```

**为什么 batch 不能挂在 `api` 组下**：`gateway.Timeout` 用 `context.WithTimeout` 套在 `c.Request.Context()` 上，而 `WithTimeout` 取的是**更早**的那个 deadline。挂在 6s 的组里再套 30s，实际生效的仍是 6s——串行跑 5~8 条时后面的会成批超时。必须从路由层重开一条链。

`requireAuth` 要把原来 `api` 组里的 `gateway.Auth(...)` 字面量抽成变量，两组共用。

- [ ] **Step 6: 运行通过**

```bash
cd d:/20260916/loyalty-console/server && go test ./cmd/gateway/ -run TestDiag -v
```

- [ ] **Step 7: 手动冒烟**

```bash
go run ./cmd/gateway &
curl -s localhost:8080/api/diag/checks | head -c 400
curl -s "localhost:8080/api/diag/run/orphan_rules" | head -c 400
```

Expected: 两个都返回 `"code":0`。

- [ ] **Step 8: Commit**

```bash
git add server/internal/diag/queries.go server/cmd/gateway/diag_handlers.go server/cmd/gateway/diag_handlers_test.go server/cmd/gateway/main.go
git commit -m "feat(api): 新增 /api/diag 三个诊断接口"
```

---

### Task 4.5: 体检报告页（Phase A 的交付物）

**Files:**

- Create: `web/src/lib/diag.ts`
- Create: `web/src/pages/AuditPage.tsx`
- Create: `web/src/pages/AuditPage.less`
- Modify: `web/src/App.tsx`（加 `/audit` 路由）
- Modify: `web/src/layout/ConsoleLayout.tsx`（菜单加「运营体检」，面包屑补一级）

**Interfaces:**

- Consumes:
  - `POST /api/diag/batch` → `{code:0, data:{partial:bool, results:RunResult[]}}`
  - `GET /api/diag/checks` → `{code:0, total:N, items:CheckInfo[]}`
  - `http.get` / `http.post`（`@/lib/http`）
  - `reportApiError(path, status, traceId)`（`@/lib/monitor`）
- Produces（供 Phase B 复用）：
  - `AUDIT_CHECKS: readonly string[]` —— 体检只跑无参数规则
  - `summarize(results: RunResult[]): AuditSummary`

**为什么先做这一页**：体检不需要模型。它证明的是「这些判定本身值不值得看」——如果运营看完这些结论不想动手，那么对话层只是给没人要的结论配了个解说员。Phase B 的价值依赖「结论有用」这个尚未验证的假设。

**不做的事**：这一页不调用 agent、不带参数、不能按产品过滤。带参数的规则（`benefit_chain` / `change_impact`）故意不进体检——它们在无参数时必然返回 `inconclusive`，混进来只会让报告里多两条噪音。

- [ ] **Step 1: 写 `web/src/lib/diag.ts`**

放三样东西：后端契约的 TS 类型（`Verdict` / `Severity` / `Finding` / `Coverage` / `RunResult` / `CheckInfo`）、两个请求函数（`fetchChecks` / `runAudit`）、以及纯函数 `summarize`。

`summarize` 的硬约束：`ok + issue + inconclusive + failed === total`。任何一个结果都必须落进某一类，不能有「不知道算哪类」的灰色地带——页面上的数字加不起来，用户第一时间就不信这份报告。

`findings` 按 `severity` 排序（high → mid → low）：运营第一眼该看到的是必须处理的那几条。

- [ ] **Step 2: 写 `AuditPage.tsx`**

四个区块，自上而下：

1. **顶部**：标题 + 重新体检按钮。
2. **计数条**：检查项 / 有问题 / 没查全 / 正常 四格。
3. **逐项结果**：每条规则一行——图标 + 标题 + 结论标签 + 「检查 N 条」。
4. **问题清单**：每条 finding 一张卡——严重度标签 + 标题 + 说明 + 「去页面核对」链接 + 证据字段。

三处必须做对：

- **`coverage.missing` 非空时必须显式提示。** 报告页最容易犯的错是把「没查全」渲染成「没问题」。有缺口就挂一条 `Alert type="warning"`，逐条列出缺了什么。
- **失败项（`error` 非空）不能显示成 ok。** `verdict` 是空串时走「没能执行」分支，`error` 是 `timeout` 时补一句「数据量可能超出扫描上限」。
- **图标必须和颜色同时存在。** 只靠红/绿区分对色弱用户不可用。

`reportApiError` 的签名是 `(path: string, status: number, traceId?: string)`，不是 `(err, path)`。

- [ ] **Step 3: 写 `AuditPage.less`**

沿用管理台的中性底（`#f6f7fb` 灰蓝），只在「结论」上用色：绿 / 红 / 橙三色**全部给语义用**，不拿来做装饰。计数格用左边框色条区分，不用整块填色——四个大色块会淹没真正的信息。数字加 `font-variant-numeric: tabular-nums`，否则并排时右边缘会晃。

- [ ] **Step 4: 接路由与菜单**

`App.tsx`：在 `Dashboard` 之后加 `<Route path="audit" element={<AuditPage />} />`。它**不进** `DOMAINS`——那份配置是「表格页」的声明式驱动，体检页没有表格、没有筛选、没有分页，塞进去会撑破 `PageDef` 的语义。

`ConsoleLayout.tsx`：菜单在 `DOMAINS` 之前插入两项（工作台 / 运营体检）；面包屑单独补 `/audit` 这一级，否则从菜单点进来时只显示「首页」。

- [ ] **Step 5: 类型检查与构建**

```bash
cd d:/20260916/loyalty-console/web && npx tsc --noEmit && npx vite build
```

Expected: 无 TS 错误，构建 exit 0。

- [ ] **Step 6: 人工核对三种状态**

起后端（**不启 Docker 时跳过**，用假数据或直接看代码路径）：

- 全部 ok → 计数条「正常 5」，问题清单显示「没有发现问题」
- 有问题 → 高危排在最前，每条都有可点的「去页面核对」
- 有 `coverage.missing` → 顶部出现「这次没有查全」的橙色提示

- [ ] **Step 7: Commit**

```bash
git add web/src/lib/diag.ts web/src/pages/AuditPage.tsx web/src/pages/AuditPage.less web/src/App.tsx web/src/layout/ConsoleLayout.tsx
git commit -m "feat(web): 运营体检报告页（Phase A 交付物）"
```

---

### Task 5: agent 工程骨架 + 契约 + 假模型

**Files:**

- Create: `agent/package.json` / `agent/tsconfig.json` / `agent/.env.example`
- Create: `agent/shared/contract.ts`
- Create: `agent/src/provider.ts`
- Create: `agent/testdata/fake_script.json`
- Create: `agent/src/provider.test.ts`

**Interfaces:**

- Produces:
  - `contract.ts`：`Verdict` / `Severity` / `Finding` / `Coverage` / `CheckResult` / `Report` / `AgentEvent`（判别联合）
  - `provider.ts`：`interface Provider { complete(req: CompleteRequest): Promise<CompleteResponse> }`，`createProvider(cfg): Provider`
- Consumes: 无

**Review Focus 覆盖**：第 5 条（SSE 事件类型必须唯一来源）—— `AgentEvent` 在这里定义，Task 7 与 Task 9 都 import 它。

- [ ] **Step 1: 建工程**

```bash
cd d:/20260916/loyalty-console && mkdir -p agent/src agent/shared agent/testdata agent/eval
cd agent && pnpm init && pnpm add fastify undici && pnpm add -D typescript tsx @types/node
```

`tsconfig.json`：`"target": "ES2022"`、`"module": "NodeNext"`、`"strict": true`、`"noUncheckedIndexedAccess": true`。

- [ ] **Step 2: 写 `contract.ts`**

按设计 §6.6.1 / §6.8 落类型。`AgentEvent` 必须是判别联合：

```ts
export type AgentEvent =
  | { type: 'step'; index: number; label: string; status: 'running' | 'done' | 'failed' }
  | { type: 'final'; report: Report }
  | { type: 'error'; reason: string; message: string };
```

`web/tsconfig.json` 的 `paths` 加 `"@agent/*": ["../agent/*"]`，供 Task 9 复用。

- [ ] **Step 3: 写假 provider 的测试**

```ts
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createFakeProvider } from '../src/provider.js';

test('按脚本顺序返回', async () => {
  const p = createFakeProvider({ script: ['{"a":1}', '{"b":2}'] });
  assert.deepEqual(await p.complete({ messages: [] }), { text: '{"a":1}' });
  assert.deepEqual(await p.complete({ messages: [] }), { text: '{"b":2}' });
});

test('脚本耗尽后抛错，而不是返回空', async () => {
  const p = createFakeProvider({ script: [] });
  await assert.rejects(() => p.complete({ messages: [] }), /script exhausted/);
});
```

- [ ] **Step 4: 运行确认失败**

```bash
cd d:/20260916/loyalty-console/agent && node --import tsx --test src/provider.test.ts
```

Expected: 模块不存在。

- [ ] **Step 5: 实现 `createProvider` / `createFakeProvider`**

`AGENT_FAKE_MODEL=1` 时走 `createFakeProvider`，否则走真实 HTTP。**启动自检：`ENV=prod` 且 `AGENT_FAKE_MODEL` 非空 → 直接退出**（设计 §14.6）。

- [ ] **Step 6: 运行通过**

```bash
cd d:/20260916/loyalty-console/agent && node --import tsx --test src/provider.test.ts
```

- [ ] **Step 7: Commit**

```bash
git add agent/
git commit -m "feat(agent): 工程骨架、共享契约与假模型 provider"
```

---

### Task 6: loop / ledger / validate（最小闭环，同步 JSON）

**Files:**

- Create: `agent/src/ledger.ts` / `agent/src/loop.ts` / `agent/src/validate.ts` / `agent/src/tools.ts` / `agent/src/prompt.ts`
- Create: `agent/src/loop.test.ts`

**Interfaces:**

- Consumes: `Provider`（Task 5）、`AgentEvent`（`contract.ts`）
- Produces: `runAgent(input: { question: string; context: AgentContext }, deps: LoopDeps): Promise<Report>`
  `deps = { provider, callBackend, now, budget }`

**Review Focus 覆盖**：第 2 条（引用校验）—— Step 5 的跨轮用例；第 3 条（预算）—— Step 4 的预算用例。

- [ ] **Step 1: 写正常两轮的测试**

假 provider 吐 `tool_call(orphan_rules)` → 假 backend 返回 1 条 finding → 假 provider 吐 `final`。断言：`report.verdict === 'issue'`、`report.evidence` 非空、`steps.length === 2`。

- [ ] **Step 2: 写失败路径测试**

| 用例                | 断言                                                                 |
| ------------------- | -------------------------------------------------------------------- |
| 永远吐 tool_call    | 第`MAX_TURNS` 轮停止，`stop_reason === 'max_turns'`              |
| final 缺`verdict` | 重试一次；两次都坏 → 抛`SchemaViolation`，**不返回坏 JSON** |
| 后端 500            | 该 check`inconclusive`，其余继续                                   |
| 预算 1 分钱         | 第二轮前中断，`stop_reason === 'budget'`                           |
| ctx 取消            | 立即返回，**不调用 provider 第二次**                           |
| 工具名不存在        | 返回结构化错误给模型，不 panic                                       |

- [ ] **Step 3: 运行确认全部失败**

```bash
cd d:/20260916/loyalty-console/agent && node --import tsx --test src/loop.test.ts
```

- [ ] **Step 4: 实现 `loop.ts` 四态状态机**

状态：`classify → gather → compose → done`，每轮记录 `stop_reason`。预算在**每次调用 provider 前**检查，不是调用后。

- [ ] **Step 5: 实现 `validate.ts`**

`finding_ids ⊆ 本次真实产生的集合`。跨轮引用要能追溯到产生它的那一轮；追溯不到 → 判定为编造 → 丢弃该引用并记 `dropped` 事件（设计 §10 的 `agent_citation_dropped_total`）。

- [ ] **Step 6: 实现 `ledger.ts`**

只追加。每轮把白名单事实重新注入提示词（设计 §6.4.1），**不做摘要压缩**。

- [ ] **Step 7: 运行通过**

```bash
cd d:/20260916/loyalty-console/agent && node --import tsx --test src/loop.test.ts
```

- [ ] **Step 8: Commit**

```bash
git add agent/src/
git commit -m "feat(agent): loop 状态机、ledger 与引用校验"
```

---

### Task 7: SSE 流式

**Files:**

- Modify: `agent/src/server.ts`（新建）
- Modify: `agent/src/loop.ts`（加事件回调）
- Create: `agent/src/sse.test.ts`

**Interfaces:**

- Produces: `POST /agent/diagnose` → `text/event-stream`，逐条 `AgentEvent`
- Consumes: `runAgent`（Task 6）、`AgentEvent`（`contract.ts`）

- [ ] **Step 1: 写事件序列测试**

断言输出字节流按 `data: {...}\n\n` 分帧，事件顺序为 `step*` → `final`，且**取消后不再有 `final`**。

- [ ] **Step 2: 实现 SSE 路由**

要点：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`X-Accel-Buffering: no`；`req.raw.on('close')` 时 abort 内部 `AbortController`；每 15s 发一次 `: ping\n\n` 保活。

- [ ] **Step 3: 运行通过 + 手动验证**

```bash
curl -N -X POST localhost:3001/agent/diagnose -H 'content-type: application/json' -d '{"question":"有没有配错的券"}'
```

Expected: 逐条打印 `data: {...}`。

- [ ] **Step 4: Commit**

```bash
git add agent/src/server.ts agent/src/loop.ts agent/src/sse.test.ts
git commit -m "feat(agent): SSE 流式输出"
```

---

### Task 8: guard（开关 / 限流 / 预算 / 降级）

**Files:**

- Create: `agent/src/guard.ts`
- Create: `agent/src/guard.test.ts`
- Modify: `agent/src/server.ts`

**Interfaces:**

- Produces:
  - `guard.check(req): GuardResult`（`{ ok: true } | { ok: false; reason: 'inflight' | 'rate' | 'budget' | 'off' }`）
  - `guard.recordUsage(cost: number): void`
- Consumes: `AGENT_DAILY_BUDGET_CNY` 等配置

**Review Focus 覆盖**：第 3 条（预算按金额不按 token）。

- [ ] **Step 1: 写测试**

预算测试：单价 A 下花到 2999 允许，3000 后拒绝；**换一个单价 5 倍的模型，同样 token 数在第 1/5 次就被拒绝**——这条证明闸门累加的是金额。

- [ ] **Step 2: 实现**

开关读 60s 缓存；三层限流（全局 inflight / 单用户 rate / 当日金额）；重启清零的代价在注释里写明（设计 §9）。

- [ ] **Step 3: 运行通过**

```bash
cd d:/20260916/loyalty-console/agent && node --import tsx --test src/guard.test.ts
```

- [ ] **Step 4: Commit**

```bash
git add agent/src/guard.ts agent/src/guard.test.ts agent/src/server.ts
git commit -m "feat(agent): 开关、限流与金额闸门"
```

---

### Task 9: 前端 SSE 客户端

**Files:**

- Create: `web/src/lib/agent.ts`
- Create: `web/src/lib/agent.test.ts`（用 `node --test` 跑纯函数部分）

**Interfaces:**

- Consumes: `AgentEvent`（`@agent/shared/contract`）
- Produces:
  - `parseSSEChunk(buffer: string): { events: AgentEvent[]; rest: string }` —— **纯函数，可单测**
  - `diagnose(input, { onEvent, signal }): Promise<void>`
  - `AgentErrorKind`（与设计 §7.5.3 的 `ErrorReason` 一致）

**Review Focus 覆盖**：第 5 条（粘包 / 半行 JSON）—— Step 1 的核心用例。

- [ ] **Step 1: 写解析器测试**

```ts
test('一行内多个事件', () => {
  const { events, rest } = parseSSEChunk('data: {"type":"step","index":0,"label":"查券","status":"running"}\n\ndata: {"type":"final","report":{}}\n\n');
  assert.equal(events.length, 2);
  assert.equal(rest, '');
});

test('半个 JSON 留到下一块，不能丢也不能抛', () => {
  const a = parseSSEChunk('data: {"type":"step","index":0,"la');
  assert.equal(a.events.length, 0);
  const b = parseSSEChunk(a.rest + 'bel":"查券","status":"running"}\n\n');
  assert.equal(b.events.length, 1);
});

test('未知 type 不抛错，返回 contract 错误', () => {
  const { events } = parseSSEChunk('data: {"type":"wat"}\n\n');
  assert.equal(events[0]?.type, 'error');
});
```

- [ ] **Step 2: 运行确认失败**

```bash
cd d:/20260916/loyalty-console/web && node --import tsx --test src/lib/agent.test.ts
```

Expected: 模块不存在。（`web` 需临时加 `tsx` 到 devDependencies，或把该测试放进 `agent/` 目录跑——二选一，别为此给 web 引一套测试设施。）

- [ ] **Step 3: 实现 `parseSSEChunk`**

按 `\n\n` 切帧，保留最后一段不完整的内容进 `rest`；`JSON.parse` 失败 → 产出 `{type:'error', reason:'contract'}`，**不 throw**（抛了会让整个流停住，用户看不到后面的错误提示）。

- [ ] **Step 4: 实现 `diagnose`**

用 `fetch` + `ReadableStream`（设计 §4.4：不用 `EventSource`，因为要 POST 和自定义 header）。60s 客户端兜底超时。

- [ ] **Step 5: 运行通过**

```bash
cd d:/20260916/loyalty-console/web && pnpm typecheck && node --import tsx --test src/lib/agent.test.ts
```

- [ ] **Step 6: Commit**

```bash
git add web/src/lib/agent.ts web/src/lib/agent.test.ts
git commit -m "feat(web): SSE 客户端与流解析器"
```

---

### Task 10: 抽屉与三张卡

**Files:**

- Create: `web/src/components/AgentDrawer/AgentDrawer.tsx`
- Create: `web/src/components/AgentDrawer/AgentDrawer.less`
- Create: `web/src/components/AgentDrawer/ReportCard.tsx` / `AnswerCard.tsx` / `NoticeCard.tsx` / `StepList.tsx` / `ErrorBanner.tsx`
- Modify: `web/src/layout/ConsoleLayout.tsx`
- Test: `pnpm typecheck` + Playwright 一条 happy path

**Interfaces:**

- Consumes: `diagnose` / `AgentEvent`（Task 9、7）、`Report`（`contract.ts`）
- Produces: `<AgentDrawer open={boolean} onClose={() => void} />`

- [ ] **Step 1: 实现状态机**

按设计 §7.5.2 的 `DrawerPhase` 判别联合。**不要用三个布尔量拼**。关闭抽屉**不 abort**；只有「停止」按钮 abort。

- [ ] **Step 2: 实现组件树**

按 §7.5.1。`AgentBody` 必须 `min-height: 0`——否则消息一多输入框被顶出屏幕。

- [ ] **Step 3: 实现三张卡**

`ReportCard` 四段固定；`AnswerCard` 超过 5 条显示「列前 5 条，共 N 条」；`NoticeCard` 带三条示例。配色走 antd token，不引新变量。

- [ ] **Step 4: 证据链接用真实 `<a href>`**

```tsx
<a href={finding.link} target="_blank" rel="noreferrer">{finding.title}</a>
```

不用 `navigate()`——中键、键盘、新标签都要能用。

- [ ] **Step 5: 挂载入口**

`ConsoleLayout.tsx` 右下角 `AgentEntry` 悬浮按钮（避开 antd `BackTop` 的位置），点击开抽屉。

- [ ] **Step 6: 验证**

```bash
cd d:/20260916/loyalty-console/web && pnpm typecheck && pnpm build
```

Expected: 无错误。手动走一遍：问「有没有配错的券」→ 看到步骤推进 → 结论卡 → 点证据跳到列表页且该行高亮。

- [ ] **Step 7: Commit**

```bash
git add web/src/components/AgentDrawer/ web/src/layout/ConsoleLayout.tsx
git commit -m "feat(web): 诊断助手抽屉与结果卡"
```

---

### Task 11: 评测集与 runner

**Files:**

- Create: `agent/eval/cases.json`（22 条，设计 §11）
- Create: `agent/eval/run.ts`
- Create: `agent/eval/fixtures/voucher_batch.sql`

**Interfaces:**

- Consumes: `runAgent`（Task 6）、`AGENT_FAKE_MODEL` 之外的**真实** provider
- Produces: 四个指标 —— 规则命中率 / 结论正确率 / 引用合规率 / 拒答准确率

- [ ] **Step 1: 写 cases.json 的 schema 与 22 条用例**

字段按设计 §11：`id` / `question` / `kind` / `expectCheck` / `expectVerdict` / `mustCite` / `mustRefuse`。断言**结论与证据 id，不断言具体数字**（数字会随数据变化过期）。

- [ ] **Step 2: 写 runner**

逐条跑真实模型，输出四指标 + 每条的实际 vs 期望。**不接 CI**——它花钱且需要真实后端。

- [ ] **Step 3: 补券批次 fixture**

`ops_voucher_batch` 本地无种子数据（设计 §5.3 第 3 点），S03/S06 必须自带样本行。

- [ ] **Step 4: 跑一次，记录基线**

```bash
cd d:/20260916/loyalty-console/agent && node --import tsx eval/run.ts
```

Expected: 打印四指标。**把这次的结果写进 `docs/` 下的评测基线，后续对比用。**

- [ ] **Step 5: Commit**

```bash
git add agent/eval/
git commit -m "feat(eval): 22 条评测用例与 runner"
```

---

### Task 12: 可观测与 trace 落盘

**Files:**

- Create: `agent/src/trace.ts`
- Modify: `agent/src/server.ts` / `server/internal/observ/observ.go`

**Interfaces:**

- Produces: `writeTrace(t: TraceRecord): void`，一行 JSON，OTel 字段名

- [ ] **Step 1: 实现 `writeTrace`**

按设计 §10.1 的字段：`trace_id` / `session_id` / `spans[]`（`span_id` / `parent_span_id` / `name` / `start_ms` / `end_ms` / `attributes`）。**`attributes` 不放模型原文**。

- [ ] **Step 2: 接入 loop**

每次诊断结束（无论成功失败）落一条。

- [ ] **Step 3: 后端加三个指标**

`mc_diag_check_total{check,verdict}` / `mc_diag_check_duration_seconds{check}` / `mc_diag_coverage_missing_total{check}`，沿用 `observ.go` 的 `mc_<subsystem>_<name>` 命名。

- [ ] **Step 4: 配告警**

按设计 §10 的五条。**`agent_citation_dropped_total > 0` 持续 10 分钟必须告警**——它意味着模型在编造证据。

- [ ] **Step 5: Commit**

```bash
git add agent/src/trace.ts agent/src/server.ts server/internal/observ/observ.go
git commit -m "feat(obs): 诊断 trace 落盘与后端指标"
```

---

### Task 13: 灰度、回滚演练

**Files:**

- Modify: `deploy/` 下 compose
- Modify: `agent/.env.example`

**Interfaces:**

- Consumes: Task 8 的 `guard`
- Produces: 可演练的开关与回滚路径

- [ ] **Step 1: 配 `AGENT_ENABLED` 开关**

关掉后一分钟内所有入口消失、无模型调用。

- [ ] **Step 2: 真跑一次「能关」演练**

**不是看代码觉得能关**——在灰度环境实际关一次，用 `curl` 确认 `/agent/diagnose` 返回 503 且无模型调用日志。

- [ ] **Step 3: 真跑一次「能回滚」演练**

agent 镜像回滚到上一版，确认管理台功能恢复正常。

- [ ] **Step 4: 灰度期预算设 300**

`AGENT_DAILY_BUDGET_CNY=300`，够 3 个人随便点。

- [ ] **Step 5: Commit**

```bash
git add deploy/ agent/.env.example
git commit -m "chore(deploy): 诊断助手开关、灰度预算与回滚路径"
```

---

## Self-Review

**1. Spec coverage**

| 设计章节                     | 对应任务                                          |
| ---------------------------- | ------------------------------------------------- |
| §4 架构决策                 | 在计划层面体现（Task 4/5/7 的分工），无需单独任务 |
| §5.1 路由 / §5.2 契约      | Task 4                                            |
| §5.2.1 不走列表接口         | Task 4 Step 3（`SQLSource` 自写 SQL）           |
| §5.3 规则清单               | Task 3                                            |
| §6.2 配置                   | Task 5、8、13 分散落地                            |
| §6.4 上下文管理             | Task 6 Step 6（ledger，不做压缩）                 |
| §6.6.1 接口契约             | Task 4                                            |
| §6.6.2 规则签名             | Task 2                                            |
| §6.7 Loop 与预算            | Task 6、8                                         |
| §6.8 报告 schema / 引用校验 | Task 5、6                                         |
| §7 前端                     | Task 1、4.5（体检页）、9、10                      |
| §9 安全与预算               | Task 8                                            |
| §10 可观测                  | Task 12                                           |
| §10.1 trace                 | Task 12                                           |
| §11 评测                    | Task 11                                           |
| §12 降级矩阵                | Task 8、13                                        |
| §14 测试策略                | 各任务 TDD 步骤                                   |
| §15 风险                    | 在各任务约束里逐条落                              |

**2. Step scan** — 每个 Step 都有可检查的结果；无「TBD」「处理边界情况」这类空话。

**3. Type consistency** — `Verdict` / `Finding` / `Coverage` / `CheckResult` / `AgentEvent` 在 Task 2（Go）与 Task 5（TS）各定义一次，字段名一一对应；`parseSSEChunk` / `runAgent` / `createProvider` / `guard.check` 在产出任务与消费任务中名称一致。

**4. Review Focus** — 五条都找到了归属任务并在 Step 中写了对应测试。

**5. Proportion** — 本计划约 700 行，设计文档约 1700 行；无大段实现体，只给签名、断言与判定逻辑。
