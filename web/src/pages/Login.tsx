import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { Button, Card, Form, Input, Typography, message } from 'antd';
import { LockOutlined, UserOutlined } from '@ant-design/icons';
import { http, ticketStore } from '@/lib/http';
import './Login.less';

interface LoginForm {
    username: string;
    password: string;
}

/**
 * 登录。
 *
 * 本管理台自带一套账号体系，不依赖任何外部登录服务，
 * 因此可以整套独立部署、独立演示。
 * 令牌由网关签发（HMAC），前端只负责存和带。
 */
export default function Login() {
    const [loading, setLoading] = useState(false);
    const navigate = useNavigate();

    const onFinish = async (values: LoginForm) => {
        setLoading(true);
        try {
            const res = await http.post<{ code: number; token: string }>('/api/auth/login', values);
            ticketStore.set(res.token);
            message.success('登录成功');
            navigate('/', { replace: true });
        } catch (e) {
            const err = e as Error;
            message.error(err.message || '登录失败');
        } finally {
            setLoading(false);
        }
    };

    return (
        <div className="login">
            <div className="login__panel">
                <div className="login__intro">
                    <span className="login__logo" />
                    <Typography.Title level={2} className="login__title">
                        会员运营管理台
                    </Typography.Title>
                    <Typography.Paragraph className="login__subtitle">
                        商品、权益、交易、渠道、运营、配置，六个域收敛到一个控制台。
                    </Typography.Paragraph>
                    <ul className="login__points">
                        <li>统一入口，不再跨系统跳转</li>
                        <li>产品维度筛选，所见即所查</li>
                        <li>全链路 trace_id，问题可追溯</li>
                    </ul>
                </div>

                <Card className="login__card" bordered={false}>
                    <Typography.Title level={4}>账号登录</Typography.Title>
                    <Form<LoginForm> layout="vertical" onFinish={onFinish} requiredMark={false}>
                        <Form.Item
                            name="username"
                            label="用户名"
                            rules={[{ required: true, message: '请输入用户名' }]}
                        >
                            <Input size="large" prefix={<UserOutlined />} autoComplete="username" />
                        </Form.Item>
                        <Form.Item
                            name="password"
                            label="密码"
                            rules={[{ required: true, message: '请输入密码' }]}
                        >
                            <Input.Password
                                size="large"
                                prefix={<LockOutlined />}
                                autoComplete="current-password"
                            />
                        </Form.Item>
                        <Button type="primary" htmlType="submit" size="large" block loading={loading}>
                            登 录
                        </Button>
                    </Form>
                </Card>
            </div>
        </div>
    );
}
