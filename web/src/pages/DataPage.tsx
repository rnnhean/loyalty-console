import { useCallback, useEffect, useRef, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import {
    Alert,
    Button,
    Card,
    Input,
    Select,
    Space,
    Table,
    Tag,
    Tooltip,
    Typography,
} from 'antd';
import { ReloadOutlined, SearchOutlined } from '@ant-design/icons';
import dayjs from 'dayjs';
import type { ColumnsType } from 'antd/es/table';
import { http, type ListPayload } from '@/lib/http';
import { reportApiError } from '@/lib/monitor';
import { STATE_TEXT, type CellKind, type ColumnDef, type PageDef } from '@/config/console';
import './DataPage.less';

type Row = Record<string, unknown>;

const CLIENT_TEXT: Record<number, string> = {
    0: '全部',
    1: 'PC',
    2: '移动端',
    3: '小程序',
    4: '鸿蒙',
};

const STATE_COLOR: Record<number, string> = {
    0: 'default',
    1: 'success',
    2: 'warning',
    3: 'error',
};

function renderCell(v: unknown, kind?: CellKind) {
    if (v === null || v === undefined || v === '') return '—';
    switch (kind) {
        case 'money':
            return `¥${Number(v).toFixed(2)}`;
        case 'time':
            return dayjs(String(v)).isValid() ? dayjs(String(v)).format('YYYY-MM-DD HH:mm:ss') : String(v);
        case 'state': {
            const n = Number(v);
            return <Tag color={STATE_COLOR[n] || 'default'}>{STATE_TEXT[n] ?? '—'}</Tag>;
        }
        case 'online': {
            const n = Number(v);
            return <Tag color={n === 1 ? 'blue' : 'default'}>{n === 1 ? '已上线' : '未上线'}</Tag>;
        }
        case 'client': {
            const n = Number(v);
            return CLIENT_TEXT[n] ?? `端${n}`;
        }
        case 'bool':
            return Number(v) === 1 ? <Tag color="green">是</Tag> : <Tag>否</Tag>;
        case 'kind':
            return <Tag>{String(v)}</Tag>;
        default:
            return String(v);
    }
}

function toColumns(defs: ColumnDef[]): ColumnsType<Row> {
    return defs.map((d) => ({
        title: d.title,
        dataIndex: d.key,
        key: d.key,
        width: d.width,
        ellipsis: d.ellipsis ? { showTitle: false } : false,
        render: (v: unknown) =>
            d.ellipsis ? (
                <Tooltip title={String(v ?? '')} placement="topLeft">
                    <span>{renderCell(v, d.kind)}</span>
                </Tooltip>
            ) : (
                renderCell(v, d.kind)
            ),
    }));
}

// ── 产品下拉框 ─────────────────────────────────────────────
//
// 每个接口的选项只拉一次并缓存。15 个页面里有 4 个用产品筛选，
// 来回切换时不重复打同一个请求 —— 这是 singleflight 思路在前端的等价实现。
const optionCache = new Map<string, Promise<ProductOption[]>>();

/** 首屏默认试这几个主力产品，都没有数据才让用户自己选 */
const PRIORITY_PRODUCTS = ['AI 能力', 'AI能力', 'CleanPro', 'DocHub'];

export interface ProductOption {
    title: string;
    label?: string;
    count?: number;
}

/** 按优先级给产品排序：命中的排前面，其余保持后端返回的顺序（后端按数据量降序） */
export function sortByPriority(list: ProductOption[]): ProductOption[] {
    const rank = (t: string) => {
        const i = PRIORITY_PRODUCTS.findIndex((p) => t.includes(p) || p.includes(t));
        return i < 0 ? 999 : i;
    };
    return [...list].sort((a, b) => rank(a.title) - rank(b.title));
}

function loadProductOptions(def: PageDef): Promise<ProductOption[]> {
    // 有专属选项接口就用它（该表真实出现过的产品值），否则用全局产品目录
    const api = def.productOptionsApi || '/api/setting/products';
    const hit = optionCache.get(api);
    if (hit) return hit;

    const p = http
        .get<ListPayload<Record<string, unknown>>>(api)
        .then((r) =>
            sortByPriority(
                (r.items || []).map((x) => ({
                    title: String(x.title ?? ''),
                    label: String(x.display_title ?? x.title ?? ''),
                    count: typeof x.count === 'number' ? x.count : undefined,
                })),
            ).filter((x) => x.title),
        )
        .catch((e: Error & { status?: number; traceId?: string }) => {
            // 失败不缓存，下次进页面还能重试
            optionCache.delete(api);
            if (e.status && e.status >= 400) reportApiError(api, e.status, e.traceId);
            return [] as ProductOption[];
        });

    optionCache.set(api, p);
    return p;
}

export default function DataPage({ def }: { def: PageDef }) {
    const [sp, setSp] = useSearchParams();
    /** 诊断结论里的证据链接会带 focus=<行 id>，用于高亮并滚动到该行 */
    const focus = sp.get('focus');

    const [rows, setRows] = useState<Row[]>([]);
    const [total, setTotal] = useState(0);
    const [page, setPage] = useState(1);
    const [size, setSize] = useState(20);
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState<string>('');

    const [products, setProducts] = useState<ProductOption[]>([]);
    // 初值从地址栏读：证据跳转会带上 product，刷新后也要保持同一视图
    const [product, setProduct] = useState<string>(() => sp.get('product') ?? '');
    const [keyword, setKeyword] = useState(() => sp.get('keyword') ?? '');
    /** 搜索框的已提交值（点搜索/回车才生效，输入过程不请求） */
    const [committedKeyword, setCommittedKeyword] = useState(() => sp.get('keyword') ?? '');

    // 每次请求自增，回来时对不上就丢弃 —— 防止慢请求覆盖新结果
    const seqRef = useRef(0);
    /** 是否已完成「自动挑一个产品」的尝试，避免无限重试 */
    const autoPickedRef = useRef(false);

    const query = useCallback(
        async (opts: { productValue?: string; pageValue?: number; sizeValue?: number; kw?: string }) => {
            const p = opts.productValue ?? product;
            const pageValue = opts.pageValue ?? page;
            const sizeValue = opts.sizeValue ?? size;
            const kw = opts.kw ?? committedKeyword;

            // 需要产品筛选但没选产品：不发请求，直接空表。
            // 产品是分区键，空产品名查出来的是全库 —— 那既慢又没意义。
            if (def.productFilter && !p) {
                setRows([]);
                setTotal(0);
                return { total: 0 };
            }

            const seq = ++seqRef.current;
            setLoading(true);
            setError('');
            try {
                const res = await http.get<ListPayload<Row>>(def.api, {
                    page: pageValue,
                    page_size: sizeValue,
                    ...(def.productFilter ? { product: p } : {}),
                    ...(def.searchKey && kw ? { [def.searchKey]: kw } : {}),
                });
                if (seq !== seqRef.current) return { total: -1 }; // 过期结果，丢弃
                setRows(res.items || []);
                setTotal(res.total || 0);
                return { total: res.total || 0 };
            } catch (e) {
                if (seq !== seqRef.current) return { total: -1 };
                const err = e as Error & { status?: number; traceId?: string };
                setRows([]);
                setTotal(0);
                setError(err.message || '查询失败');
                if (err.status && err.status >= 400) reportApiError(def.api, err.status, err.traceId);
                return { total: -1 };
            } finally {
                if (seq === seqRef.current) setLoading(false);
            }
        },
        [def, product, page, size, committedKeyword],
    );

    // 产品选项
    useEffect(() => {
        if (!def.productFilter) return undefined;
        let alive = true;
        loadProductOptions(def).then((list) => {
            if (alive) setProducts(list);
        });
        return () => {
            alive = false;
        };
    }, [def]);

    /** 用户手动改筛选时清掉 focus —— 高亮的是上一次的证据行，换了条件就没意义了 */
    const clearFocus = useCallback(() => {
        if (!focus) return;
        const next = new URLSearchParams(sp);
        next.delete('focus');
        setSp(next, { replace: true });
    }, [focus, sp, setSp]);

    /** 把筛选状态写回地址栏（replace，不新增历史），让链接可复制、刷新后仍在同一视图 */
    useEffect(() => {
        const next = new URLSearchParams();
        if (product) next.set('product', product);
        if (committedKeyword) next.set('keyword', committedKeyword);
        if (focus) next.set('focus', focus);
        if (next.toString() !== sp.toString()) setSp(next, { replace: true });
    }, [product, committedKeyword, focus, sp, setSp]);

    /**
     * 首屏策略：
     *   - 无产品筛选的页面：直接查；
     *   - 有产品筛选的页面：按优先级依次试，命中第一条有数据的就停。
     *     全部为空则停在第一个产品上，让用户自己换。
     */
    useEffect(() => {
        if (def.productFilter) {
            if (!products.length || autoPickedRef.current) return;
            autoPickedRef.current = true;

            // 地址栏已指定产品（证据跳转带来的）：直接查它，不再自动挑第一个有数据的
            if (product) {
                void query({ productValue: product, pageValue: 1, kw: committedKeyword });
                return;
            }

            void (async () => {
                for (const p of products.slice(0, 6)) {
                    setProduct(p.title);
                    const r = await query({ productValue: p.title, pageValue: 1, kw: '' });
                    if (r.total > 0) return;
                }
            })();
            return;
        }
        void query({ pageValue: 1 });
        // 只在页面切换时跑一次
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [def.path, products.length]);

    /** focus 命中后滚到中间，等表格渲染完再滚 */
    useEffect(() => {
        if (!focus || !rows.length) return undefined;
        const timer = window.setTimeout(() => {
            document.querySelector('.row-focus')?.scrollIntoView({ block: 'center', behavior: 'smooth' });
        }, 100);
        return () => window.clearTimeout(timer);
    }, [focus, rows]);

    const handleSearch = () => {
        setPage(1);
        setCommittedKeyword(keyword);
        clearFocus();
        void query({ pageValue: 1, kw: keyword });
    };

    return (
        <div className="data-page">
            <Card bordered={false} className="data-page__filters">
                <Space wrap size="middle">
                    {def.productFilter && (
                        <Space size={8}>
                            <span className="data-page__label">产品</span>
                            <Select
                                showSearch
                                allowClear
                                style={{ width: 280 }}
                                placeholder="请选择产品"
                                value={product || undefined}
                                optionFilterProp="label"
                                onChange={(v) => {
                                    const val = (v as string) || '';
                                    setProduct(val);
                                    setPage(1);
                                    clearFocus();
                                    void query({ productValue: val, pageValue: 1 });
                                }}
                                options={products.map((p) => ({
                                    value: p.title,
                                    label: p.count !== undefined ? `${p.label || p.title}（${p.count}）` : p.label || p.title,
                                }))}
                            />
                        </Space>
                    )}
                    {def.searchKey && (
                        <Input
                            allowClear
                            style={{ width: 260 }}
                            placeholder={def.searchPlaceholder || '输入关键词'}
                            value={keyword}
                            prefix={<SearchOutlined />}
                            onChange={(e) => setKeyword(e.target.value)}
                            onPressEnter={handleSearch}
                        />
                    )}
                    <Button onClick={handleSearch}>查询</Button>
                    <Button
                        icon={<ReloadOutlined />}
                        onClick={() => {
                            setPage(1);
                            void query({ pageValue: 1 });
                        }}
                    >
                        刷新
                    </Button>
                </Space>
            </Card>

            {error && (
                <Alert
                    className="data-page__alert"
                    type="error"
                    showIcon
                    message={error}
                    action={
                        <Button size="small" onClick={() => void query({})}>
                            重试
                        </Button>
                    }
                />
            )}

            <Card bordered={false} className="data-page__table">
                <Table<Row>
                    rowKey={(r, i) => String(r[def.rowKey || 'uid'] ?? i)}
                    rowClassName={(r) => (focus && String(r[def.rowKey || 'uid']) === focus ? 'row-focus' : '')}
                    columns={toColumns(def.columns)}
                    dataSource={rows}
                    loading={loading}
                    size="middle"
                    scroll={{ x: 'max-content' }}
                    pagination={{
                        current: page,
                        pageSize: size,
                        total,
                        showSizeChanger: true,
                        pageSizeOptions: [10, 20, 50, 100],
                        showTotal: (n) => `共 ${n} 条`,
                        onChange: (p, s) => {
                            setPage(p);
                            setSize(s);
                            void query({ pageValue: p, sizeValue: s });
                        },
                    }}
                    locale={{
                        emptyText: def.productFilter && !product ? '请先选择产品' : '暂无数据',
                    }}
                />
            </Card>

            {def.desc && (
                <Typography.Paragraph type="secondary" className="data-page__desc">
                    {def.desc}
                </Typography.Paragraph>
            )}
        </div>
    );
}
