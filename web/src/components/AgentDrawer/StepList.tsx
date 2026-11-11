import { CheckCircleFilled, CloseCircleFilled, LoadingOutlined } from '@ant-design/icons';
import type { StepEvent } from '@shared/contract';

/** 步骤条。同一 index 会被服务端推两次（running → done/failed），这里按 index 覆盖。 */
export default function StepList({ steps }: { steps: StepEvent[] }) {
    if (steps.length === 0) return null;

    return (
        <ul className="agent-steps">
            {steps.map((s) => (
                <li key={s.index} className={`agent-steps__item agent-steps__item--${s.status}`}>
                    <span className="agent-steps__icon" aria-hidden>
                        {s.status === 'running' && <LoadingOutlined />}
                        {s.status === 'done' && <CheckCircleFilled />}
                        {s.status === 'failed' && <CloseCircleFilled />}
                    </span>
                    <span className="agent-steps__label">{s.label}</span>
                </li>
            ))}
        </ul>
    );
}
