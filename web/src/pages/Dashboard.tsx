import { useEffect, useState, type ReactNode } from 'react';
import { Alert, Card, Col, Row, Skeleton, Statistic, Tag, Typography } from 'antd';
import {
    ClusterOutlined,
    CrownOutlined,
    FundProjectionScreenOutlined,
    ShoppingOutlined,
} from '@ant-design/icons';
import { Link } from 'react-router-dom';
import dayjs from 'dayjs';
import { http } from '@/lib/http';
import { reportApiError, setTraceId } from '@/lib/monitor';
import './Dashboard.less';

interface Overview {
    code: number;
    products?: { uid: number; title: string }[];
    blocks?: Record<string, { total?: number; items?: Record<string, unknown>[] }>;
    degraded?: Record<string, string>;
    trace_id?: string;
}

interface CardState {
    key: string;
    title: string;
    icon: ReactNode;
    total: number;
    unit: string;
    to: string;
    recent: string[];
}

const EMPTY: CardState[] = [
    {
        key: 'catalog',
        title: '商品',
        icon: <ShoppingOutlined />,
        total: 0,
        unit: '个标准商品',
        to: '/catalog/spu',
        recent: [],
    },
    {
        key: 'benefit',
        title: '权益',
        icon: <CrownOutlined />,
        total: 0,
        unit: '个会员层级',
        to: '/benefit/tiers',
        recent: [],
    },
    {
        key: 'trade',
        title: '交易',
        icon: <FundProjectionScreenOutlined />,
        total: 0,
        unit: '套结算配置',
        to: '/trade/checkouts',
        recent: [],
    },
    {
        key: 'channel',
        title: '渠道',
        icon: <ClusterOutlined />,
        total: 0,
        unit: '个商户主体',
        to: '/channel/merchants',
        recent: [],
    },
];

/**
 * 工作台。
 *
 * 数据来源是网关的 BFF 聚合接口：一个页面 3 个域的数据，
 * 原来要发 6~8 个请求、串行等好几轮，现在一次拿全。
 * 这里的 loading 是「一次请求」的 loading，不是「拼装多个请求」的 loading。
 */
export default function Dashboard() {
    const [cards, setCards] = useState<CardState[]>(EMPTY);
    const [loading, setLoading] = useState(true);
    const [degraded, setDegraded] = useState<Record<string, string>>({});
    const [trace, setTrace] = useState('');

    useEffect(() => {
        let alive = true;

        void (async () => {
            try {
                const [catalog, benefit, trade, channel] = await Promise.all([
                    http.get<Overview>('/api/bff/catalog/overview'),
                    http.get<Overview>('/api/bff/benefit/overview'),
                    http.get<Overview>('/api/bff/trade/overview'),
                    http.get<Overview>('/api/bff/channel/overview'),
                ]);
                if (!alive) return;

                const pick = (o: Overview, block: string) => o.blocks?.[block];
                const names = (o: Overview, block: string, field = 'title') =>
                    (pick(o, block)?.items || [])
                        .slice(0, 6)
                        .map((x) => String(x[field] ?? ''))
                        .filter(Boolean);

                setCards([
                    {
                        ...EMPTY[0],
                        total: pick(catalog, 'spu')?.total || 0,
                        recent: names(catalog, 'cards'),
                    },
                    {
                        ...EMPTY[1],
                        total: pick(benefit, 'tiers')?.total || 0,
                        recent: names(benefit, 'groups'),
                    },
                    {
                        ...EMPTY[2],
                        total: pick(trade, 'checkouts')?.total || 0,
                        recent: names(trade, 'checkouts'),
                    },
                    {
                        ...EMPTY[3],
                        total: pick(channel, 'merchants')?.total || 0,
                        recent: names(channel, 'merchants', 'alias'),
                    },
                ]);

                const merged = {
                    ...(catalog.degraded || {}),
                    ...(benefit.degraded || {}),
                    ...(trade.degraded || {}),
                    ...(channel.degraded || {}),
                };
                setDegraded(merged);

                const tid = catalog.trace_id || '';
                setTrace(tid);
                if (tid) setTraceId(tid);
            } catch (e) {
                const err = e as Error & { status?: number; traceId?: string };
                if (err.status && err.status >= 400) {
                    reportApiError('/api/bff/catalog/overview', err.status, err.traceId);
                }
            } finally {
                if (alive) setLoading(false);
            }
        })();

        return () => {
            alive = false;
        };
    }, []);

    return (
        <div className="dash">
            {Object.keys(degraded).length > 0 && (
                <Alert
                    type="warning"
                    showIcon
                    className="dash__alert"
                    message={`部分数据块加载失败：${Object.keys(degraded).join('、')}`}
                    description="页面其余内容仍可用。可在日志中用下方的 trace_id 定位原因。"
                />
            )}

            <Row gutter={[16, 16]}>
                {cards.map((c) => (
                    <Col key={c.key} xs={24} sm={12} lg={6}>
                        <Card bordered={false} className="dash__stat">
                            <div className="dash__stat-head">
                                <span className="dash__stat-icon">{c.icon}</span>
                                <Link to={c.to}>{c.title}</Link>
                            </div>
                            {loading ? (
                                <Skeleton active paragraph={{ rows: 1 }} title={false} />
                            ) : (
                                <Statistic value={c.total} suffix={<span className="dash__unit">{c.unit}</span>} />
                            )}
                        </Card>
                    </Col>
                ))}
            </Row>

            <Row gutter={[16, 16]} className="dash__row">
                {cards.map((c) => (
                    <Col key={c.key} xs={24} lg={12}>
                        <Card
                            bordered={false}
                            title={`${c.title} · 最近更新`}
                            extra={<Link to={c.to}>查看全部</Link>}
                            className="dash__list"
                        >
                            {loading ? (
                                <Skeleton active paragraph={{ rows: 3 }} title={false} />
                            ) : c.recent.length ? (
                                <ul className="dash__items">
                                    {c.recent.map((t, i) => (
                                        <li key={`${t}-${i}`}>
                                            <Tag color="blue">{i + 1}</Tag>
                                            <span className="dash__item-text">{t}</span>
                                        </li>
                                    ))}
                                </ul>
                            ) : (
                                <Typography.Text type="secondary">暂无数据</Typography.Text>
                            )}
                        </Card>
                    </Col>
                ))}
            </Row>

            <footer className="dash__footer">
                <Typography.Text type="secondary">
                    {trace && (
                        <>
                            trace_id：<code>{trace}</code> ·{' '}
                        </>
                    )}
                    更新于 {dayjs().format('YYYY-MM-DD HH:mm:ss')}
                </Typography.Text>
            </footer>
        </div>
    );
}
