import { useMemo, useState, type ReactNode } from 'react';
import { Link, Outlet, useLocation, useNavigate } from 'react-router-dom';
import { Avatar, Breadcrumb, Button, Dropdown, Layout, Menu, Typography } from 'antd';
import {
    ApiOutlined,
    AppstoreOutlined,
    CrownOutlined,
    FundProjectionScreenOutlined,
    LogoutOutlined,
    MenuFoldOutlined,
    MenuUnfoldOutlined,
    ClusterOutlined,
    SettingOutlined,
    ShoppingOutlined,
    UserOutlined,
} from '@ant-design/icons';
import { DOMAINS, domainOf, PAGE_INDEX } from '@/config/console';
import { ticketStore } from '@/lib/http';
import './ConsoleLayout.less';

const { Header, Sider, Content } = Layout;

const ICONS: Record<string, ReactNode> = {
    catalog: <ShoppingOutlined />,
    benefit: <CrownOutlined />,
    trade: <FundProjectionScreenOutlined />,
    channel: <ClusterOutlined />,
    operation: <AppstoreOutlined />,
    setting: <SettingOutlined />,
};

export default function ConsoleLayout() {
    const [collapsed, setCollapsed] = useState(false);
    const location = useLocation();
    const navigate = useNavigate();

    const menuItems = useMemo(
        () =>
            DOMAINS.map((d) => ({
                key: d.key,
                icon: ICONS[d.icon] ?? <ApiOutlined />,
                label: d.title,
                children: d.pages.map((p) => ({
                    key: p.path,
                    label: <Link to={p.path}>{p.title}</Link>,
                })),
            })),
        [],
    );

    // 打开态：根据当前路径反推所属域，刷新后菜单仍然展开
    const openKeys = useMemo(() => {
        const d = domainOf(location.pathname);
        return d ? [d.key] : [];
    }, [location.pathname]);

    const page = PAGE_INDEX[location.pathname];
    const domain = domainOf(location.pathname);

    const crumbs = [
        { title: '首页' },
        ...(domain ? [{ title: domain.title }] : []),
        ...(page ? [{ title: page.title }] : []),
    ];

    const handleLogout = () => {
        ticketStore.clear();
        navigate('/login', { replace: true });
    };

    return (
        <Layout className="console">
            <Sider
                className="console__sider"
                width={232}
                collapsible
                collapsed={collapsed}
                onCollapse={setCollapsed}
                trigger={null}
            >
                <div className="console__brand">
                    <span className="console__logo" />
                    {!collapsed && (
                        <div className="console__brand-text">
                            <strong>会员运营台</strong>
                            <small>Loyalty Console</small>
                        </div>
                    )}
                </div>
                <Menu
                    theme="dark"
                    mode="inline"
                    selectedKeys={[location.pathname]}
                    defaultOpenKeys={openKeys}
                    items={menuItems}
                />
            </Sider>

            <Layout>
                <Header className="console__header">
                    <div className="console__header-left">
                        <Button
                            type="text"
                            className="console__collapse"
                            icon={collapsed ? <MenuUnfoldOutlined /> : <MenuFoldOutlined />}
                            onClick={() => setCollapsed((c) => !c)}
                        />
                        <Breadcrumb items={crumbs} />
                    </div>
                    <Dropdown
                        menu={{
                            items: [{ key: 'logout', icon: <LogoutOutlined />, label: '退出登录', onClick: handleLogout }],
                        }}
                    >
                        <Button type="text" className="console__user">
                            <Avatar size="small" icon={<UserOutlined />} />
                            <span>管理员</span>
                        </Button>
                    </Dropdown>
                </Header>

                <Content className="console__content">
                    {page && (
                        <div className="console__page-head">
                            <Typography.Title level={4}>{page.title}</Typography.Title>
                            {page.desc && <Typography.Text type="secondary">{page.desc}</Typography.Text>}
                        </div>
                    )}
                    <Outlet />
                </Content>
            </Layout>
        </Layout>
    );
}
