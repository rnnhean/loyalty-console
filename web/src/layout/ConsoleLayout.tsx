import { useMemo, useState, type ReactNode } from 'react';
import { Link, Outlet, useLocation, useNavigate } from 'react-router-dom';
import { Avatar, Breadcrumb, Button, Dropdown, Layout, Menu, Typography } from 'antd';
import {
    ApiOutlined,
    AppstoreOutlined,
    CrownOutlined,
    DashboardOutlined,
    FundProjectionScreenOutlined,
    LogoutOutlined,
    MedicineBoxOutlined,
    MenuFoldOutlined,
    MenuUnfoldOutlined,
    ClusterOutlined,
    SettingOutlined,
    ShoppingOutlined,
    UserOutlined,
} from '@ant-design/icons';
import { DOMAINS, domainOf, PAGE_INDEX } from '@/config/console';
import { ticketStore } from '@/lib/http';
import AgentHost from '@/components/AgentDrawer';
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
        () => [
            {
                key: 'overview',
                icon: <DashboardOutlined />,
                label: <Link to="/">工作台</Link>,
            },
            {
                // 体检页不属于任何一个业务域：它同时检查多个域。
                // 塞进某个域的二级菜单会让人以为它只管那个域。
                key: 'audit',
                icon: <MedicineBoxOutlined />,
                label: <Link to="/audit">运营体检</Link>,
            },
            {
                // 运维配置单独一组：这三页是「配置系统本身」，不属于任何业务域。
                key: 'ops',
                icon: <SettingOutlined />,
                label: '运维配置',
                children: [
                    { key: '/operation/rules', label: <Link to="/operation/rules">可配置规则</Link> },
                    { key: '/setting/jobs', label: <Link to="/setting/jobs">定时作业</Link> },
                    { key: '/setting/masking', label: <Link to="/setting/masking">字段脱敏</Link> },
                ],
            },
            ...DOMAINS.map((d) => ({
                key: d.key,
                icon: ICONS[d.icon] ?? <ApiOutlined />,
                label: d.title,
                children: d.pages.map((p) => ({
                    key: p.path,
                    label: <Link to={p.path}>{p.title}</Link>,
                })),
            })),
        ],
        [],
    );

    // 打开态：根据当前路径反推所属域，刷新后菜单仍然展开
    const openKeys = useMemo(() => {
        const d = domainOf(location.pathname);
        return d ? [d.key] : [];
    }, [location.pathname]);

    const page = PAGE_INDEX[location.pathname];
    const domain = domainOf(location.pathname);
    // 体检页不在 DOMAINS 里（它跨域），面包屑单独补一级，
    // 否则从菜单点进来时会显示成「首页」而没有当前页。
    const isAudit = location.pathname === '/audit';
    // 运维配置页不在 DOMAINS 里，面包屑同样单独补
    const OPS_TITLES: Record<string, string> = {
        '/operation/rules': '可配置规则',
        '/setting/jobs': '定时作业',
        '/setting/masking': '字段脱敏',
    };
    const opsTitle = OPS_TITLES[location.pathname];

    const crumbs = [
        { title: '首页' },
        ...(domain ? [{ title: domain.title }] : []),
        ...(page ? [{ title: page.title }] : []),
        ...(isAudit ? [{ title: '运营体检' }] : []),
        ...(opsTitle ? [{ title: '运维配置' }, { title: opsTitle }] : []),
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

            {/* 诊断助手挂在布局层：任何页面都能唤起，不随路由卸载 */}
            <AgentHost />
        </Layout>
    );
}
