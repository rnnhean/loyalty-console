import { Alert } from 'antd';
import type { ErrorEvent, ErrorReason } from '@shared/contract';

const REASON_TEXT: Record<ErrorReason, string> = {
    network: '网络不通',
    timeout: '超时',
    budget: '额度用完',
    server: '服务出错',
    unavailable: '暂不可用',
    contract: '返回格式不对',
    aborted: '已停止',
};

/** 终止性的 error 事件。契约类的错误要带上「可以重试」的暗示，业务类的不用。 */
export default function ErrorBanner({ error }: { error: Pick<ErrorEvent, 'reason' | 'message'> }) {
    const retryable = error.reason === 'network' || error.reason === 'server' || error.reason === 'timeout';

    return (
        <Alert
            className="agent-error"
            type="error"
            showIcon
            message={REASON_TEXT[error.reason] ?? '诊断失败'}
            description={
                <>
                    <div>{error.message}</div>
                    {retryable && <div className="agent-error__hint">可以再发一次试试。</div>}
                </>
            }
        />
    );
}
