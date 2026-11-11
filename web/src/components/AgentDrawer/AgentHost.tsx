import { useEffect, useRef, useState } from 'react';
import { Button, Drawer, Input, Space, Spin, Typography } from 'antd';
import { CloseOutlined, RobotOutlined, SendOutlined } from '@ant-design/icons';
import { useLocation } from 'react-router-dom';
import type {
    AgentEvent,
    ErrorReason,
    NoticeEvent,
    Report,
    StepEvent,
    StopReason,
} from '@shared/contract';

import { diagnose } from '@/lib/agent';
import StepList from './StepList';
import ReportCard from './ReportCard';
import NoticeCard from './NoticeCard';
import ErrorBanner from './ErrorBanner';
import './AgentDrawer.less';

const { Text } = Typography;

const SAMPLES = [
    '为什么有些权益配好了，用户却领不到？',
    '最近券的核销对不上账，帮我查一下券批次',
    '线上是不是混进了测试数据',
];

interface Turn {
    id: string;
    question: string;
    steps: StepEvent[];
    notices: NoticeEvent[];
    report?: Report;
    stopReason?: StopReason;
    checks: string[];
    error?: { reason: ErrorReason; message: string };
    running: boolean;
}

export default function AgentHost() {
    const [open, setOpen] = useState(false);
    const [turns, setTurns] = useState<Turn[]>([]);
    const [input, setInput] = useState('');
    const [running, setRunning] = useState(false);

    const abortRef = useRef<AbortController | null>(null);
    const listRef = useRef<HTMLDivElement>(null);
    const location = useLocation();

    // 新事件进来就滚到底：用户不需要为了看见「正在检查」而手动往下拉
    useEffect(() => {
        const el = listRef.current;
        if (el) el.scrollTop = el.scrollHeight;
    }, [turns]);

    // 关页面 / 卸载时停掉正在跑的诊断，别让后端继续计费
    useEffect(() => () => abortRef.current?.abort(), []);

    const send = async (raw?: string) => {
        const question = (raw ?? input).trim();
        if (!question || running) return;

        const id = `t${Date.now()}`;
        setInput('');
        setRunning(true);
        setTurns((prev) => [
            ...prev,
            {
                id,
                question,
                steps: [],
                notices: [],
                checks: [],
                running: true,
            },
        ]);

        const controller = new AbortController();
        abortRef.current = controller;

        try {
            await diagnose({
                question,
                context: { route: location.pathname },
                signal: controller.signal,
                onEvent: (e) => setTurns((prev) => applyEvent(prev, id, e)),
            });
        } finally {
            abortRef.current = null;
            setRunning(false);
            setTurns((prev) => prev.map((t) => (t.id === id ? { ...t, running: false } : t)));
        }
    };

    const stop = () => {
        abortRef.current?.abort();
    };

    return (
        <>
            <button
                type="button"
                className={`agent-entry${open ? ' agent-entry--hidden' : ''}`}
                onClick={() => setOpen(true)}
                aria-label="打开运营诊断助手"
            >
                <RobotOutlined />
                <span>诊断助手</span>
            </button>

            <Drawer
                className="agent-drawer"
                title={
                    <Space>
                        <RobotOutlined />
                        <span>运营诊断助手</span>
                    </Space>
                }
                width={520}
                open={open}
                onClose={() => setOpen(false)}
                maskClosable={false}
                closeIcon={<CloseOutlined />}
                footer={
                    <div className="agent-composer">
                        <Input.TextArea
                            value={input}
                            onChange={(e) => setInput(e.target.value)}
                            onPressEnter={(e) => {
                                if (!e.shiftKey) {
                                    e.preventDefault();
                                    void send();
                                }
                            }}
                            placeholder="描述你看到的现象，例如「超级会员的权益包领不到」"
                            autoSize={{ minRows: 2, maxRows: 4 }}
                            maxLength={500}
                            disabled={running}
                        />
                        <div className="agent-composer__actions">
                            <Text type="secondary" className="agent-composer__hint">
                                它只读数据，不会修改任何配置
                            </Text>
                            {running ? (
                                <Button danger onClick={stop}>
                                    停止
                                </Button>
                            ) : (
                                <Button
                                    type="primary"
                                    icon={<SendOutlined />}
                                    onClick={() => void send()}
                                    disabled={!input.trim()}
                                >
                                    发送
                                </Button>
                            )}
                        </div>
                    </div>
                }
            >
                <div className="agent-thread" ref={listRef}>
                    {turns.length === 0 && (
                        <div className="agent-empty">
                            <Text type="secondary">
                                描述你遇到的现象，它会先跑确定性的检查规则，再解释结果。
                            </Text>
                            <div className="agent-empty__samples">
                                {SAMPLES.map((s) => (
                                    <button
                                        key={s}
                                        type="button"
                                        className="agent-sample"
                                        onClick={() => void send(s)}
                                    >
                                        {s}
                                    </button>
                                ))}
                            </div>
                        </div>
                    )}

                    {turns.map((t) => (
                        <div className="agent-turn" key={t.id}>
                            <div className="agent-turn__question">{t.question}</div>

                            <StepList steps={t.steps} />

                            {t.notices.map((n, i) => (
                                <NoticeCard key={i} notice={n} />
                            ))}

                            {t.report && (
                                <ReportCard
                                    report={t.report}
                                    stopReason={t.stopReason ?? 'completed'}
                                    checks={t.checks}
                                />
                            )}

                            {t.error && <ErrorBanner error={t.error} />}

                            {t.running && !t.report && !t.error && (
                                <div className="agent-thinking">
                                    <Spin size="small" />
                                    <Text type="secondary">正在排查…</Text>
                                </div>
                            )}
                        </div>
                    ))}
                </div>
            </Drawer>
        </>
    );
}

/** 事件折叠进对应的 turn。纯函数，方便以后单独测。 */
function applyEvent(turns: Turn[], id: string, e: AgentEvent): Turn[] {
    return turns.map((t) => {
        if (t.id !== id) return t;

        switch (e.type) {
            case 'step':
                return { ...t, steps: mergeStep(t.steps, e) };
            case 'notice':
                return { ...t, notices: [...t.notices, e] };
            case 'final':
                return {
                    ...t,
                    report: e.report,
                    stopReason: e.stop_reason,
                    checks: e.checks,
                };
            case 'error':
                return { ...t, error: { reason: e.reason, message: e.message } };
        }
    });
}

function mergeStep(steps: StepEvent[], next: StepEvent): StepEvent[] {
    const i = steps.findIndex((s) => s.index === next.index);
    if (i === -1) return [...steps, next];
    const copy = steps.slice();
    copy[i] = next;
    return copy;
}
