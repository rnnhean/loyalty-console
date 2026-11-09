/**
 * 管理台的**唯一事实来源**：菜单、路由、表格列全部由这份声明驱动。
 *
 * 为什么用声明式而不是每个页面写一个组件：
 * 本站 22 个业务页面，绝大多数是「筛选 + 表格 + 分页」的同一形态。
 * 为每个页面复制一遍这套骨架，改一处就要改 22 次。
 * 所以：页面 = 一份列定义，交互骨架只有一份实现。
 *
 * 少数需要特殊交互的页面（工作台 / 详情抽屉）才写独立组件。
 */

export type CellKind =
    | 'text'
    | 'money'
    | 'time'
    | 'state'
    | 'online'
    | 'client'
    | 'kind'
    | 'bool';

export interface ColumnDef {
    key: string;
    title: string;
    width?: number;
    kind?: CellKind;
    /** 内容过长时省略并悬浮显示全文 */
    ellipsis?: boolean;
}

export interface PageDef {
    /** 前端路由路径 */
    path: string;
    title: string;
    desc?: string;
    /** 列表数据接口 */
    api: string;
    columns: ColumnDef[];
    /** 关键词搜索的 query 参数名；不填则没有搜索框 */
    searchKey?: string;
    searchPlaceholder?: string;
    /**
     * 是否带「产品」下拉框。
     *
     * 这类页面不选产品就查不出数据 —— 产品是数据的分区键，
     * 所以初始化时**不自动请求**，而是等产品选中后再发。
     */
    productFilter?: boolean;
    /**
     * 产品下拉框的选项来源。
     *
     * 不填则用全局产品目录；填了就用该表真实出现过的产品值 ——
     * 不同业务表的产品取值体系互不相同，共用一个列表必然出现「选了没数据」。
     */
    productOptionsApi?: string;
    rowKey?: string;
}

export interface DomainDef {
    key: string;
    title: string;
    icon: string;
    pages: PageDef[];
}

/** 状态列的统一文案，避免每个页面各写一套 */
export const STATE_TEXT: Record<number, string> = {
    0: '草稿',
    1: '启用',
    2: '停用',
    3: '已删除',
};

export const DOMAINS: DomainDef[] = [
    {
        key: 'catalog',
        title: '商品域',
        icon: 'catalog',
        pages: [
            {
                path: '/catalog/spu',
                title: '商品档案',
                desc: '标准商品单元，定价与权益的挂载主体',
                api: '/api/catalog/spu',
                searchKey: 'keyword',
                searchPlaceholder: '商品名称',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '商品名称', ellipsis: true },
                    { key: 'kind', title: '类型', width: 100, kind: 'kind' },
                    { key: 'amount', title: '零售价', width: 120, kind: 'money' },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                    { key: 'created_at', title: '创建时间', width: 190, kind: 'time' },
                ],
            },
            {
                path: '/catalog/sku',
                title: '单品清单',
                desc: '可独立售卖的规格，带计费周期与端侧范围',
                api: '/api/catalog/sku',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'label', title: '规格名称', ellipsis: true },
                    { key: 'product', title: '所属资源', ellipsis: true },
                    { key: 'amount', title: '单价', width: 110, kind: 'money' },
                    { key: 'span', title: '周期(天)', width: 100 },
                    { key: 'client', title: '端侧', width: 100, kind: 'client' },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                ],
            },
            {
                path: '/catalog/cards',
                title: '售卖卡',
                desc: '前台展示用的售卖位样式与投放规则',
                api: '/api/catalog/cards',
                searchKey: 'keyword',
                searchPlaceholder: '卡片名称',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '卡片名称', ellipsis: true },
                    { key: 'variant', title: '模板', width: 140, ellipsis: true },
                    { key: 'client', title: '端侧', width: 100, kind: 'client' },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                    { key: 'created_at', title: '更新时间', width: 190, kind: 'time' },
                ],
            },
            {
                path: '/catalog/pricing',
                title: '定价策略',
                desc: '同一产品下的售卖优先级与流量分配',
                api: '/api/catalog/pricing',
                productOptionsApi: '/api/catalog/pricing-products',
                productFilter: true,
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '策略名称', ellipsis: true },
                    { key: 'product', title: '产品', ellipsis: true },
                    { key: 'top', title: '置顶', width: 80, kind: 'bool' },
                    { key: 'weight', title: '流量权重', width: 110 },
                    { key: 'client', title: '端侧', width: 100, kind: 'client' },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                    { key: 'updated_at', title: '更新时间', width: 190, kind: 'time' },
                ],
            },
            {
                path: '/catalog/binds',
                title: '策略卡绑定',
                desc: '定价策略与售卖卡的挂载关系，决定同一策略下露出哪些售卖位',
                api: '/api/catalog/binds',
                columns: [
                    { key: 'uid', title: 'ID', width: 100 },
                    { key: 'policy_uid', title: '定价策略 ID', width: 140 },
                    { key: 'card_uid', title: '售卖卡 ID', width: 140 },
                    { key: 'ordinal', title: '展示顺序', width: 110 },
                ],
            },
        ],
    },
    {
        key: 'benefit',
        title: '权益域',
        icon: 'benefit',
        pages: [
            {
                path: '/benefit/tiers',
                title: '会员层级',
                desc: '产品下的会员等级与准入条件',
                api: '/api/benefit/tiers',
                productOptionsApi: '/api/benefit/tier-products',
                productFilter: true,
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'label', title: '层级名称', ellipsis: true },
                    { key: 'product', title: '产品', ellipsis: true },
                    { key: 'rank', title: '排序', width: 80 },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                    { key: 'online', title: '上线', width: 100, kind: 'online' },
                    { key: 'updated_at', title: '更新时间', width: 190, kind: 'time' },
                ],
            },
            {
                path: '/benefit/groups',
                title: '权益包',
                desc: '按产品聚合的一组权益，可挂到会员层级',
                api: '/api/benefit/groups',
                productOptionsApi: '/api/benefit/group-products',
                productFilter: true,
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '权益包名称', ellipsis: true },
                    { key: 'product', title: '产品', ellipsis: true },
                    { key: 'kind', title: '类型', width: 100, kind: 'kind' },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                ],
            },
            {
                path: '/benefit/rules',
                title: '权益规则',
                desc: '发放额度、单位与领取周期',
                api: '/api/benefit/rules',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '规则名称', ellipsis: true },
                    { key: 'group_uid', title: '所属权益包', width: 120 },
                    { key: 'kind', title: '权益类型', width: 110, kind: 'kind' },
                    { key: 'quota', title: '额度', width: 100 },
                    { key: 'unit', title: '单位', width: 90 },
                    { key: 'cycle', title: '周期(天)', width: 100 },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                ],
            },
            {
                path: '/benefit/tasks',
                title: '任务阈值',
                desc: '能力调用的触发条件与试用额度',
                api: '/api/benefit/tasks',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '任务名称', ellipsis: true },
                    { key: 'kind', title: '触发方式', width: 110, kind: 'kind' },
                    { key: 'value', title: '阈值', width: 100 },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                ],
            },
        ],
    },
    {
        key: 'trade',
        title: '交易域',
        icon: 'trade',
        pages: [
            {
                path: '/trade/checkouts',
                title: '结算页配置',
                desc: '收银台页面的端侧与支付方式组合',
                api: '/api/trade/checkouts',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '配置名称', ellipsis: true },
                    { key: 'product', title: '产品', ellipsis: true },
                    { key: 'client', title: '端侧', width: 100, kind: 'client' },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                ],
            },
            {
                path: '/trade/debits',
                title: '代扣模板',
                desc: '各支付渠道的签约模板，续费扣款按此发起',
                api: '/api/trade/debits',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '模板名称', ellipsis: true },
                    { key: 'platform_kind', title: '支付渠道', width: 130, kind: 'kind' },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                    { key: 'created_at', title: '创建时间', width: 190, kind: 'time' },
                ],
            },
            {
                path: '/trade/promos',
                title: '立减规则',
                desc: '按商户与支付方式的优惠条件，优先级决定命中顺序',
                api: '/api/trade/promos',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'product', title: '产品', ellipsis: true },
                    { key: 'merchant_code', title: '商户编号', width: 130 },
                    { key: 'pay_methods', title: '支付方式', width: 180, ellipsis: true },
                    { key: 'cond_priority', title: '优先级', width: 100 },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                    { key: 'updated_at', title: '更新时间', width: 190, kind: 'time' },
                ],
            },
        ],
    },
    {
        key: 'channel',
        title: '渠道域',
        icon: 'channel',
        pages: [
            {
                path: '/channel/merchants',
                title: '商户档案',
                desc: '结算主体与消费额度上限',
                api: '/api/channel/merchants',
                searchKey: 'keyword',
                searchPlaceholder: '商户编号/别名',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'code', title: '商户编号', width: 140 },
                    { key: 'alias', title: '商户别名', ellipsis: true },
                    { key: 'kind', title: '类型', width: 100, kind: 'kind' },
                    { key: 'primary', title: '主商户', width: 100, kind: 'bool' },
                    { key: 'cap', title: '消费上限', width: 120, kind: 'money' },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                ],
            },
            {
                path: '/channel/nodes',
                title: '分发节点',
                desc: '渠道层级与来源标记',
                api: '/api/channel/nodes',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '节点名称', ellipsis: true },
                    { key: 'code', title: '节点编码', width: 160, ellipsis: true },
                    { key: 'kind', title: '类型', width: 100, kind: 'kind' },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                ],
            },
            {
                path: '/channel/attribution',
                title: '归因记录',
                desc: '从渠道节点带过来的落地来源，用于结算与效果归因',
                api: '/api/channel/attribution',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'node_uid', title: '分发节点 ID', width: 140 },
                    { key: 'merchant_uid', title: '商户 ID', width: 120 },
                    { key: 'source_kind', title: '来源类型', width: 130, kind: 'kind' },
                    { key: 'landing_ref', title: '落地标识', ellipsis: true },
                    { key: 'created_at', title: '创建时间', width: 190, kind: 'time' },
                ],
            },
        ],
    },
    {
        key: 'operation',
        title: '运营域',
        icon: 'operation',
        pages: [
            {
                path: '/operation/campaigns',
                title: '投放计划',
                desc: '活动时间窗与人群配置',
                api: '/api/operation/campaigns',
                productOptionsApi: '/api/operation/campaign-products',
                productFilter: true,
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '计划名称', ellipsis: true },
                    { key: 'product', title: '产品', ellipsis: true },
                    { key: 'kind', title: '类型', width: 100, kind: 'kind' },
                    { key: 'starts_at', title: '开始时间', width: 190, kind: 'time' },
                    { key: 'ends_at', title: '结束时间', width: 190, kind: 'time' },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                ],
            },
            {
                path: '/operation/vouchers',
                title: '券批次',
                desc: '按活动发放的券池，含面额与发放进度',
                api: '/api/operation/vouchers',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '批次名称', ellipsis: true },
                    { key: 'campaign_uid', title: '所属活动', width: 120 },
                    { key: 'face_amount', title: '面额', width: 110, kind: 'money' },
                    { key: 'quota_total', title: '总配额', width: 100 },
                    { key: 'quota_issued', title: '已发放', width: 100 },
                    { key: 'batch_state', title: '状态', width: 100, kind: 'state' },
                    { key: 'created_at', title: '创建时间', width: 190, kind: 'time' },
                ],
            },
            {
                path: '/operation/slots',
                title: '推广位',
                desc: '活动素材在各端的位置配置，模板决定渲染形态',
                api: '/api/operation/slots',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'slot_title', title: '推广位名称', ellipsis: true },
                    { key: 'slot_kind', title: '位置类型', width: 110, kind: 'kind' },
                    { key: 'product_title', title: '产品', ellipsis: true },
                    { key: 'lifecycle_state', title: '状态', width: 100, kind: 'state' },
                ],
            },
        ],
    },
    {
        key: 'setting',
        title: '配置域',
        icon: 'setting',
        pages: [
            {
                path: '/setting/products',
                title: '产品目录',
                desc: '产品下拉框的数据源',
                api: '/api/setting/products',
                searchKey: 'keyword',
                searchPlaceholder: '产品名称',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'code', title: '产品编码', width: 140, ellipsis: true },
                    { key: 'title', title: '产品名称', ellipsis: true },
                    { key: 'display_title', title: '展示名称', ellipsis: true },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                ],
            },
            {
                path: '/setting/dict',
                title: '字典项',
                desc: '下拉选项与枚举值的统一维护入口',
                api: '/api/setting/dict',
                searchKey: 'kind',
                searchPlaceholder: '字典类型',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'kind', title: '字典类型', width: 160, ellipsis: true },
                    { key: 'code', title: '编码', width: 160, ellipsis: true },
                    { key: 'label', title: '显示值', ellipsis: true },
                    { key: 'ordinal', title: '排序', width: 80 },
                ],
            },
            {
                path: '/setting/modules',
                title: '集成模块',
                desc: '外部系统对接的注册信息',
                api: '/api/setting/modules',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'title', title: '模块名称', ellipsis: true },
                    { key: 'product', title: '产品', ellipsis: true },
                    { key: 'kind', title: '类型', width: 140, ellipsis: true },
                    { key: 'state', title: '状态', width: 100, kind: 'state' },
                ],
            },
            {
                path: '/setting/toggles',
                title: '特性开关',
                desc: '灰度与功能开关，值可以是布尔、比例或任意约定串',
                api: '/api/setting/toggles',
                columns: [
                    { key: 'uid', title: 'ID', width: 90 },
                    { key: 'toggle_key', title: '开关键', width: 240, ellipsis: true },
                    { key: 'product_title', title: '产品', ellipsis: true },
                    { key: 'toggle_value', title: '当前值', width: 140 },
                    { key: 'remarks', title: '说明', ellipsis: true },
                ],
            },
        ],
    },
];

/** 扁平索引：path → PageDef，路由渲染与面包屑共用 */
export const PAGE_INDEX: Record<string, PageDef> = Object.fromEntries(
    DOMAINS.flatMap((d) => d.pages.map((p) => [p.path, p])),
);

/** 页面所属域 */
export function domainOf(path: string): DomainDef | undefined {
    return DOMAINS.find((d) => d.pages.some((p) => p.path === path));
}
