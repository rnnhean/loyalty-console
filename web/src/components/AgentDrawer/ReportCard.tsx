import { Alert, Tag, Typography } from 'antd';
import { Link } from 'react-router-dom';
import type { Report, StopReason } from '@shared/contract';

const { Paragraph, Text, Title } = Typography;

/**
 * 停止原因里，只有这几种需要向用户额外解释。
 * completed 不解释——解释「我正常做完了」是噪音。
 */
const STOP_HINT: Partial<Record<StopReason, string>> = {
    max_turns: '到了轮数上限，结论只覆盖已经查过的部分。',
    total_timeout: '这次超时了，结论只覆盖已经查过的部分。',
    no_new_evidence: '连续两轮没查到新线索，诊断在这里停下。',
    budget: '今天的诊断额度已用完，明天再试。',
    degraded: '模型这次没能给出完整结论，下面是能确认的部分。',
    context_limit: '内容太长超过了模型上限，结论可能不完整。',
};

export default function ReportCard({
    report,
    stopReason,
    checks,
}: {
    report: Report;
    stopReason: StopReason;
    checks: string[];
}) {
    const hint = STOP_HINT[stopReason];

    return (
        <div className={`agent-report agent-report--${report.kind}`}>
            <div className="agent-report__head">
                <Title level={5} className="agent-report__title">
                    {report.title || '（无标题）'}
                </Title>
                <Tag color={report.kind === 'diagnosis' ? 'blue' : 'default'}>
                    {report.kind === 'diagnosis' ? '诊断' : report.kind === 'query' ? '查询' : '超出范围'}
                </Tag>
                {report.kind === 'diagnosis' && (
                    <Text type="secondary" className="agent-report__confidence">
                        置信度 {Math.round(report.confidence * 100)}%
                    </Text>
                )}
            </div>

            {report.phenomenon && (
                <section className="agent-report__section">
                    <Text strong>现象</Text>
                    <Paragraph className="agent-report__text">{report.phenomenon}</Paragraph>
                </section>
            )}

            {report.judgement && (
                <section className="agent-report__section">
                    <Text strong>判断</Text>
                    <Paragraph className="agent-report__text">{report.judgement}</Paragraph>
                </section>
            )}

            {report.advice.length > 0 && (
                <section className="agent-report__section">
                    <Text strong>建议</Text>
                    <ol className="agent-report__advice">
                        {report.advice.map((a, i) => (
                            <li key={i}>
                                <span>{a.text}</span>
                                {a.ref && (
                                    <Link className="agent-report__link" to={buildHref(a.ref.path, a.ref.query)}>
                                        去处理 →
                                    </Link>
                                )}
                            </li>
                        ))}
                    </ol>
                </section>
            )}

            <div className="agent-report__foot">
                {report.finding_ids.length > 0 && (
                    <Text type="secondary">依据 {report.finding_ids.length} 条</Text>
                )}
                {checks.length > 0 && (
                    <Text type="secondary" className="agent-report__checks">
                        跑了 {checks.join('、')}
                    </Text>
                )}
            </div>

            {hint && (
                <Alert className="agent-report__stop" type="warning" showIcon message={hint} />
            )}
        </div>
    );
}

function buildHref(path: string, query?: Record<string, string>): string {
    if (!query) return path;
    const sp = new URLSearchParams();
    for (const [k, v] of Object.entries(query)) sp.set(k, v);
    const qs = sp.toString();
    return qs ? `${path}?${qs}` : path;
}
