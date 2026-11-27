import type { ReactNode } from 'react';
import { BrowserRouter, Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { Button, Result } from 'antd';
import { DOMAINS } from '@/config/console';
import { ticketStore } from '@/lib/http';
import { Sentry } from '@/lib/monitor';
import ConsoleLayout from '@/layout/ConsoleLayout';
import AuditPage from '@/pages/AuditPage';
import DataPage from '@/pages/DataPage';
import Dashboard from '@/pages/Dashboard';
import Login from '@/pages/Login';
import RulesPage from '@/pages/RulesPage';
import JobsPage from '@/pages/JobsPage';
import MaskingPage from '@/pages/MaskingPage';
import './styles/global.less';

/** 未登录一律回登录页，并记住来源，登录后跳回来源页 */
function RequireAuth({ children }: { children: ReactNode }) {
    const location = useLocation();
    if (!ticketStore.get()) {
        return <Navigate to="/login" replace state={{ from: location.pathname }} />;
    }
    return <>{children}</>;
}

function NotFound() {
    return (
        <Result
            status="404"
            title="页面不存在"
            subTitle="链接可能已失效，请从左侧菜单进入。"
            extra={<Button href="/">回到工作台</Button>}
        />
    );
}

export default function App() {
    return (
        <BrowserRouter>
            {/* ErrorBoundary 兜住渲染期异常：页面白屏时至少还有一句可读的提示，且能上报到 Sentry */}
            <Sentry.ErrorBoundary
                fallback={({ error }) => (
                    <Result
                        status="error"
                        title="页面出错了"
                        subTitle={(error as Error)?.message || '未知错误'}
                        extra={<Button onClick={() => window.location.reload()}>刷新页面</Button>}
                    />
                )}
            >
                <Routes>
                    <Route path="/login" element={<Login />} />

                    <Route
                        path="/"
                        element={
                            <RequireAuth>
                                <ConsoleLayout />
                            </RequireAuth>
                        }
                    >
                        <Route index element={<Dashboard />} />
                        <Route path="audit" element={<AuditPage />} />
                        <Route path="operation/rules" element={<RulesPage />} />
                        <Route path="setting/jobs" element={<JobsPage />} />
                        <Route path="setting/masking" element={<MaskingPage />} />
                        {DOMAINS.flatMap((d) =>
                            d.pages.map((p) => (
                                // key 必须带 path：同一个 DataPage 组件在不同路由间复用时，
                                // useState 的初值只算一次，从证据链接跳进来会读到上一个页面的筛选值
                                <Route
                                    key={p.path}
                                    path={p.path.replace(/^\//, '')}
                                    element={<DataPage key={p.path} def={p} />}
                                />
                            )),
                        )}
                        <Route path="*" element={<NotFound />} />
                    </Route>
                </Routes>
            </Sentry.ErrorBoundary>
        </BrowserRouter>
    );
}
