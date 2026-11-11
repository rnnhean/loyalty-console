import { Alert } from 'antd';
import type { NoticeEvent } from '@shared/contract';

/** 过程提示。info 不打断阅读，warn 必须显眼——它通常意味着结论会不完整。 */
export default function NoticeCard({ notice }: { notice: NoticeEvent }) {
    if (!notice.text) return null;

    return (
        <Alert
            className="agent-notice"
            type={notice.level === 'warn' ? 'warning' : 'info'}
            showIcon
            message={notice.text}
        />
    );
}
