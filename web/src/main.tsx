import React from 'react';
import ReactDOM from 'react-dom/client';
import { ConfigProvider, App as AntdApp } from 'antd';
import zhCN from 'antd/locale/zh_CN';
import dayjs from 'dayjs';
import 'dayjs/locale/zh-cn';
import App from './App';
import { initMonitoring, setTraceId } from '@/lib/monitor';

dayjs.locale('zh-cn');

// 监控必须在 React 挂载之前初始化，否则首屏的报错会漏掉
initMonitoring();

// 后端把 trace_id 放在响应头里；这里在拦截器之外再兜一层：
// 首屏任何一个请求带回来的 trace_id 都可以作为当前会话的上下文。
const origFetch = window.fetch;
window.fetch = async (...args) => {
    const res = await origFetch(...args);
    const tid = res.headers.get('X-Trace-Id');
    if (tid) setTraceId(tid);
    return res;
};

ReactDOM.createRoot(document.getElementById('root')!).render(
    <React.StrictMode>
        <ConfigProvider
            locale={zhCN}
            theme={{
                token: {
                    colorPrimary: '#4f7cff',
                    borderRadius: 8,
                    fontSize: 14,
                },
                components: {
                    Layout: { headerBg: '#ffffff', bodyBg: '#f6f7fb' },
                    Menu: { darkItemBg: 'transparent', darkSubMenuItemBg: 'transparent' },
                },
            }}
        >
            <AntdApp>
                <App />
            </AntdApp>
        </ConfigProvider>
    </React.StrictMode>,
);
