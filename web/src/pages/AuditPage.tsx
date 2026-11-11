import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { Alert, Button, Card, Empty, Skeleton, Space, Tag, Typography } from 'antd';
import {
    ArrowRightOutlined,
    CheckCircleFilled,
    CloseCircleFilled,
    ExclamationCircleFilled,
    ReloadOutlined,
    WarningFilled,
} from '@ant-design/icons';
import { reportApiError } from '@/lib/monitor';
import {
    AUDIT_CHECKS,
    fetchChecks,
    runAudit,
    summarize,
    type CheckInfo,
    type Finding,
    type RunResult,
    type Severity,
    type Verdict,
} from '@/lib/diag';
import './AuditPage.less';

const { Title, Text, Paragraph } = Typography;

const SEVERITY_TEXT: Record<Severity, string> = {
    high: '需要处理',
    mid: '建议检查',
    low: '了解即可',
};

const SEVERITY_COLOR: Record<Severity, string> = {
    high: 'error',
    mid: 'warning',
    low: 'default',
};

const VERDICT_TEXT: Record<Verdict, string> = {
    ok: '正常',
    issue: '发现问题',
    inconclusive: '没查全',
};

const VERDICT_COLOR: Record<Verdict, string> = {
    ok: 'success',
    issue: 'error',
    inconclusive: 'warning',
};

/** 结果行里每种结论配一个图标：纯靠颜色区分对色弱用户不可用 */
function VerdictIcon({ verdict, failed }: { verdict: Verdict | ''; failed: boolean }) {
    if (failed) return <CloseCircleFilled className="audit__icon audit__icon--bad" />;
    if (verdict === 'ok') return <CheckCircleFilled className="audit__icon audit__icon--ok" />;
    if (verdict === 'issue') return <ExclamationCircleFilled className="audit__icon audit__icon--bad" />;
    return <WarningFilled className="audit__icon audit__icon--warn" />;
}

/**
 * 体检报告页。
 *
 * 它是 Phase A 的交付物：不经过 agent、不经过模型，直接调后端的确定性规则。
 * 这一页首先要证明的是「这些判定本身值不值得看」——如果运营看完不想动手，
 * 那么再做一个会说话的对话框也只是给没人要的结论配了个解说员。
 */
export default function AuditPage() {
    const [loading, setLoading] = useState(true);
    const [results, setResults] = useState<RunResult[]>([]);
    const [checks, setChecks] = useState<CheckInfo[]>([]);
    const [error, setError] = useState<string>('');
    const [ranAt, setRanAt] = useState<string>('');

    const titleOf = useMemo(() => {
        const m = new Map<string, string>();
        for (const c of checks) m.set(c.name, c.title);
        return (name: string) => m.get(name) ?? name;
    }, [checks]);

    const load = useCallback(async () => {
        setLoading(true);
        setError('');
        try {
            // 目录与结果并行拉：目录只用来补标题，失败也不该挡住体检结果。
            const [catalog, data] = await Promise.all([
                fetchChecks().catch(() => [] as CheckInfo[]),
                runAudit([...AUDIT_CHECKS]),
            ]);
            setChecks(catalog);
            setResults(data.results ?? []);
            setRanAt(new Date().toLocaleString('zh-CN'));
        } catch (e) {
            const { status = 0, traceId } = (e ?? {}) as { status?: number; traceId?: string };
            reportApiError('/api/diag/batch', status, traceId);
            setError('体检没能执行，请检查网络后重试');
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => {
        void load();
    }, [load]);

    const summary = useMemo(() => summarize(results), [results]);

    return (
        <div className="audit">
            <header className="audit__head">
                <div>
                    <Title level={3} className="audit__title">
                        运营体检
                    </Title>
                    <Paragraph type="secondary" className="audit__desc">
                        这几项判定全部由后端代码给出，模型不参与。每条结论都能点进去看到原始配置。
                    </Paragraph>
                </div>
                <Button icon={<ReloadOutlined />} onClick={load} loading={loading}>
                    重新体检
                </Button>
            </header>

            {error && (
                <Alert
                    type="error"
                    showIcon
                    message={error}
                    className="audit__alert"
                    action={
                        <Button size="small" onClick={load}>
                            重试
                        </Button>
                    }
                />
            )}

            {loading && results.length === 0 ? (
                <Card>
                    <Skeleton active paragraph={{ rows: 6 }} />
                </Card>
            ) : (
                <>
                    <SummaryBar summary={summary} ranAt={ranAt} />

                    {summary.gaps.length > 0 && (
                        <Alert
                            type="warning"
                            showIcon
                            className="audit__alert"
                            message="这次没有查全"
                            description={
                                <ul className="audit__gaps">
                                    {summary.gaps.map((g) => (
                                        <li key={g}>{g}</li>
                                    ))}
                                </ul>
                            }
                        />
                    )}

                    <section className="audit__section">
                        <Title level={5}>逐项结果</Title>
                        <ResultsList results={results} titleOf={titleOf} />
                    </section>

                    <section className="audit__section">
                        <Title level={5}>发现的问题</Title>
                        {summary.findings.length === 0 ? (
                            <Empty
                                image={Empty.PRESENTED_IMAGE_SIMPLE}
                                description={
                                    summary.failed + summary.inconclusive > 0
                                        ? '已检查的项没有发现问题，但有几项没查全，结论不完整'
                                        : '没有发现问题'
                                }
                            />
                        ) : (
                            <FindingsList findings={summary.findings} />
                        )}
                    </section>
                </>
            )}
        </div>
    );
}

function SummaryBar({
    summary,
    ranAt,
}: {
    summary: ReturnType<typeof summarize>;
    ranAt: string;
}) {
    const tiles = [
        { key: 'total', label: '检查项', value: summary.total, tone: 'plain' },
        { key: 'issue', label: '有问题', value: summary.issue, tone: 'bad' },
        { key: 'inconclusive', label: '没查全', value: summary.inconclusive, tone: 'warn' },
        { key: 'ok', label: '正常', value: summary.ok, tone: 'ok' },
    ];

    return (
        <div className="audit__tiles">
            {tiles.map((t) => (
                <div key={t.key} className={`audit__tile audit__tile--${t.tone}`}>
                    <span className="audit__tileValue">{t.value}</span>
                    <span className="audit__tileLabel">{t.label}</span>
                </div>
            ))}
            {ranAt && <Text className="audit__ranAt">体检时间 {ranAt}</Text>}
        </div>
    );
}

function ResultsList({
    results,
    titleOf,
}: {
    results: RunResult[];
    titleOf: (name: string) => string;
}) {
    if (results.length === 0) {
        return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有可展示的结果" />;
    }
    return (
        <ul className="audit__results">
            {results.map((r) => {
                const failed = Boolean(r.error);
                return (
                    <li key={r.check} className="audit__result">
                        <VerdictIcon verdict={r.verdict} failed={failed} />
                        <div className="audit__resultBody">
                            <div className="audit__resultHead">
                                <Text strong>{titleOf(r.check)}</Text>
                                {failed ? (
                                    <Tag color="error">没能执行</Tag>
                                ) : (
                                    <Tag color={VERDICT_COLOR[r.verdict as Verdict]}>
                                        {VERDICT_TEXT[r.verdict as Verdict]}
                                    </Tag>
                                )}
                                {typeof r.coverage?.checked === 'number' && (
                                    <Text type="secondary" className="audit__checked">
                                        检查 {r.coverage.checked} 条
                                    </Text>
                                )}
                            </div>
                            {failed && (
                                <Text type="secondary" className="audit__resultMeta">
                                    {r.error === 'timeout'
                                        ? '超时，数据量可能超出这条规则的扫描上限'
                                        : '数据源不可用，这条没有结论'}
                                </Text>
                            )}
                        </div>
                    </li>
                );
            })}
        </ul>
    );
}

function FindingsList({ findings }: { findings: Finding[] }) {
    return (
        <ul className="audit__findings">
            {findings.map((f, i) => (
                <li key={`${f.id}-${i}`} className="audit__finding">
                    <div className="audit__findingHead">
                        <Tag color={SEVERITY_COLOR[f.severity]}>{SEVERITY_TEXT[f.severity]}</Tag>
                        <Text strong>{f.title}</Text>
                    </div>
                    <Paragraph className="audit__findingDetail" type="secondary">
                        {f.detail}
                    </Paragraph>
                    <Space size="large" wrap>
                        {f.link && (
                            <Link to={f.link} className="audit__jump">
                                去页面核对 <ArrowRightOutlined />
                            </Link>
                        )}
                        <EvidenceView evidence={f.evidence} />
                    </Space>
                </li>
            ))}
        </ul>
    );
}

/**
 * 证据字段原样展示。
 *
 * 不做格式化、不做摘要：这些是判定依据，用户可能拿它去数据库里对。
 * 改写成更「好看」的样子只会让人对不上。
 */
function EvidenceView({ evidence }: { evidence: Record<string, unknown> }) {
    const entries = Object.entries(evidence ?? {});
    if (entries.length === 0) return null;
    return (
        <span className="audit__evidence">
            {entries.map(([k, v]) => (
                <span key={k} className="audit__evidenceItem">
                    <span className="audit__evidenceKey">{k}</span>
                    <span className="audit__evidenceVal">{String(v)}</span>
                </span>
            ))}
        </span>
    );
}
