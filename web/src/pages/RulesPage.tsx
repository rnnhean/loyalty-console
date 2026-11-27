import { useCallback, useEffect, useState } from 'react';
import {
    Alert,
    Button,
    Card,
    Drawer,
    Form,
    Input,

    message,
    Modal,
    Space,
    Switch,
    Table,
    Tag,
    Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { http } from '@/lib/http';

/**
 * 可配置规则页面。
 *
 * 和「运营体检」的区别：体检规则写在代码里，这页的规则是运营自己配的。
 * 所以这里必须有**试运行**——改错一条规则会影响成千上万会员，
 * 让运营先在样例数据上看到结果，是唯一能让他们自己确认的手段。
 */

interface RuleItem {
    id: number;
    name: string;
    domain: string;
    enabled: boolean;
    action: string;
    version: number;
    updated_at: string;
    expr: unknown;
}

interface HitStat {
    rule_id: number;
    rule_name: string;
    hit: boolean;
    steps?: Array<{ field?: string; op?: string; expected?: string; actual?: unknown; hit: boolean }>;
    missing_fields?: string[];
}

const formatExpr = (e: unknown) => {
    try {
        return JSON.stringify(e, null, 2);
    } catch {
        return '{}';
    }
};

export default function RulesPage() {
    const [items, setItems] = useState<RuleItem[]>([]);
    const [loading, setLoading] = useState(false);
    const [editorOpen, setEditorOpen] = useState(false);
    const [editing, setEditing] = useState<RuleItem | null>(null);
    const [dryOpen, setDryOpen] = useState(false);
    const [dryItems, setDryItems] = useState<HitStat[]>([]);
    const [explain, setExplain] = useState<{ rule: string; summary: string; steps: HitStat['steps'] } | null>(null);
    const [form] = Form.useForm();

    const load = useCallback(async () => {
        setLoading(true);
        try {
            const res = await http.get<{ items: RuleItem[] }>('/api/rules');
            setItems(res.items ?? []);
        } catch {
            message.error('加载规则失败');
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => {
        void load();
    }, [load]);

    const openEditor = (r?: RuleItem) => {
        setEditing(r ?? null);
        form.setFieldsValue(
            r
                ? { name: r.name, domain: r.domain, enabled: r.enabled, action: r.action, expr: formatExpr(r.expr) }
                : { name: '', domain: 'operation', enabled: true, action: '', expr: '{"field":"tier.state","op":"eq","value":1}' },
        );
        setEditorOpen(true);
    };

    const save = async () => {
        const v = await form.validateFields();
        let expr: unknown;
        try {
            expr = JSON.parse(v.expr);
        } catch {
            message.error('条件表达式不是合法 JSON');
            return;
        }
        const body = { name: v.name, domain: v.domain, enabled: v.enabled, action: v.action, expr };
        try {
            if (editing) {
                await http.put(`/api/rules/${editing.id}`, body);
            } else {
                await http.post('/api/rules', body);
            }
            message.success('已保存');
            setEditorOpen(false);
            void load();
        } catch (e) {
            // 后端校验失败（非法算子、字段缺失等）会带具体原因，直接透出
            message.error((e as Error).message || '保存失败');
        }
    };

    const remove = (r: RuleItem) => {
        Modal.confirm({
            title: `删除规则「${r.name}」？`,
            content: '删除会同时清掉它的历史版本，无法恢复。',
            okButtonProps: { danger: true },
            onOk: async () => {
                await http.del(`/api/rules/${r.id}`);
                message.success('已删除');
                void load();
            },
        });
    };

    const runDry = async () => {
        try {
            const res = await http.post<{ items: HitStat[]; hit: number; total: number }>('/api/rule-dry-run', {});
            setDryItems(res.items ?? []);
            setDryOpen(true);
        } catch {
            message.error('试运行失败');
        }
    };

    const doExplain = async (r: RuleItem) => {
        try {
            const res = await http.post<{ rule: string; summary: string; steps: HitStat['steps'] }>(
                `/api/rules/${r.id}/explain`,
                {},
            );
            setExplain(res);
        } catch {
            message.error('解释失败');
        }
    };

    const columns: ColumnsType<RuleItem> = [
        { title: 'ID', dataIndex: 'id', width: 70 },
        { title: '规则名', dataIndex: 'name' },
        { title: '域', dataIndex: 'domain', width: 110 },
        {
            title: '状态',
            dataIndex: 'enabled',
            width: 90,
            render: (v: boolean) => (v ? <Tag color="green">启用</Tag> : <Tag>停用</Tag>),
        },
        { title: '版本', dataIndex: 'version', width: 70 },
        { title: '更新时间', dataIndex: 'updated_at', width: 170 },
        {
            title: '操作',
            width: 210,
            render: (_, r) => (
                <Space size={4}>
                    <Button size="small" onClick={() => void doExplain(r)}>解释</Button>
                    <Button size="small" onClick={() => openEditor(r)}>编辑</Button>
                    <Button size="small" danger onClick={() => remove(r)}>删除</Button>
                </Space>
            ),
        },
    ];

    return (
        <Card
            title="可配置规则"
            extra={
                <Space>
                    <Button onClick={() => void runDry()}>试运行全部</Button>
                    <Button type="primary" onClick={() => openEditor()}>新建规则</Button>
                </Space>
            }
        >
            <Alert
                type="info"
                showIcon
                style={{ marginBottom: 12 }}
                message="规则是运营自己配的，改之前先点「试运行全部」，用样例数据看看会命中哪些。"
            />
            <Table<RuleItem>
                rowKey="id"
                size="small"
                loading={loading}
                columns={columns}
                dataSource={items}
                pagination={false}
            />

            <Modal
                title={editing ? `编辑规则 #${editing.id}` : '新建规则'}
                open={editorOpen}
                onCancel={() => setEditorOpen(false)}
                onOk={() => void save()}
                width={640}
            >
                <Form form={form} layout="vertical">
                    <Form.Item name="name" label="规则名" rules={[{ required: true }]}>
                        <Input maxLength={64} />
                    </Form.Item>
                    <Form.Item name="domain" label="所属域" rules={[{ required: true }]}>
                        <Input placeholder="operation / benefit / channel" />
                    </Form.Item>
                    <Form.Item name="action" label="命中后的动作" rules={[{ required: true }]}>
                        <Input maxLength={200} placeholder="例如：给会员打「沉默」标签" />
                    </Form.Item>
                    <Form.Item
                        name="expr"
                        label="条件表达式（JSON）"
                        rules={[{ required: true }]}
                        extra='支持 {"all":[...]} / {"any":[...]} / {"not":{...}}，算子限 eq/ne/gt/gte/lt/lte/in/nin/exists'
                    >
                        <Input.TextArea rows={8} style={{ fontFamily: 'monospace' }} />
                    </Form.Item>
                    <Form.Item name="enabled" label="启用" valuePropName="checked">
                        <Switch />
                    </Form.Item>
                </Form>
            </Modal>

            <Drawer title="试运行结果" open={dryOpen} onClose={() => setDryOpen(false)} width={560}>
                {dryItems.length === 0 ? (
                    <Typography.Text type="secondary">没有启用的规则</Typography.Text>
                ) : (
                    dryItems.map((d) => (
                        <Card key={d.rule_id} size="small" style={{ marginBottom: 8 }}>
                            <Space>
                                <Tag color={d.hit ? 'green' : 'default'}>{d.hit ? '命中' : '未命中'}</Tag>
                                <Typography.Text strong>{d.rule_name}</Typography.Text>
                            </Space>
                            {!d.hit && d.missing_fields?.length ? (
                                <div style={{ marginTop: 6 }}>
                                    <Typography.Text type="warning">
                                        缺少字段：{d.missing_fields.join(', ')}
                                    </Typography.Text>
                                </div>
                            ) : null}
                        </Card>
                    ))
                )}
            </Drawer>

            <Drawer title="规则解释" open={!!explain} onClose={() => setExplain(null)} width={560}>
                {explain && (
                    <>
                        <Typography.Title level={5}>{explain.rule}</Typography.Title>
                        <Alert type="info" message={explain.summary} style={{ marginBottom: 12 }} />
                        <Table
                            size="small"
                            rowKey={(r) => `${r.field}-${r.op}`}
                            pagination={false}
                            dataSource={explain.steps ?? []}
                            columns={[
                                { title: '字段', dataIndex: 'field' },
                                { title: '算子', dataIndex: 'op', width: 70 },
                                { title: '期望', dataIndex: 'expected', width: 100 },
                                {
                                    title: '实际',
                                    dataIndex: 'actual',
                                    width: 100,
                                    render: (v: unknown) => String(v ?? '-'),
                                },
                                {
                                    title: '结果',
                                    dataIndex: 'hit',
                                    width: 70,
                                    render: (v: boolean) => (v ? <Tag color="green">是</Tag> : <Tag>否</Tag>),
                                },
                            ]}
                        />
                    </>
                )}
            </Drawer>
        </Card>
    );
}
