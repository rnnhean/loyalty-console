import { useCallback, useEffect, useState } from 'react';
import {
    Alert,
    Button,
    Card,
    Col,
    Form,
    Input,
    InputNumber,
    message,
    Modal,
    Row,
    Select,
    Space,
    Table,
    Tag,
    Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { http } from '@/lib/http';

/**
 * 字段脱敏页面。
 *
 * 设计上刻意做了「预览」：策略写错了在页面上看不出来，
 * 只有拿一个样例值跑一遍才知道会变成什么样。
 * 预览接口只接受白名单字段，不会变成一个任意取值探测工具。
 */

interface Policy {
    field: string;
    strategy: 'mask' | 'hide' | 'plain';
    keep_prefix: number;
    keep_suffix: number;
    mask_char: string;
    roles: string[];
    note: string;
}

interface AuditRow {
    operator: string;
    role: string;
    field: string;
    reason: string;
    row_count: number;
    created_at: string;
}

const PREVIEW_FIELDS = ['member_phone', 'member_id_card', 'member_email'];

export default function MaskingPage() {
    const [policies, setPolicies] = useState<Policy[]>([]);
    const [audits, setAudits] = useState<AuditRow[]>([]);
    const [loading, setLoading] = useState(false);
    const [editorOpen, setEditorOpen] = useState(false);
    const [editing, setEditing] = useState<Policy | null>(null);
    const [preview, setPreview] = useState<{ masked: string; kind: string; source: string } | null>(null);
    const [pvField, setPvField] = useState(PREVIEW_FIELDS[0]);
    const [pvValue, setPvValue] = useState('13800001234');
    const [form] = Form.useForm();

    const load = useCallback(async () => {
        setLoading(true);
        try {
            const [p, a] = await Promise.all([
                http.get<{ items: Policy[] }>('/api/mask-policies'),
                http.get<{ items: AuditRow[] }>('/api/mask/audit'),
            ]);
            setPolicies(p.items ?? []);
            setAudits(a.items ?? []);
        } catch {
            message.error('加载脱敏配置失败');
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => {
        void load();
    }, [load]);

    const openEditor = (p?: Policy) => {
        setEditing(p ?? null);
        form.setFieldsValue(
            p ?? {
                field: '',
                strategy: 'mask',
                keep_prefix: 3,
                keep_suffix: 4,
                mask_char: '*',
                roles: [],
                note: '',
            },
        );
        setEditorOpen(true);
    };

    const save = async () => {
        const v = await form.validateFields();
        try {
            await http.put('/api/mask-policies', v);
            message.success('已保存');
            setEditorOpen(false);
            void load();
        } catch (e) {
            message.error((e as Error).message || '保存失败');
        }
    };

    const remove = (p: Policy) => {
        Modal.confirm({
            title: `删除「${p.field}」的脱敏策略？`,
            content: '删除后该字段会退回「按值自动识别」，可能不再被脱敏。',
            okButtonProps: { danger: true },
            onOk: async () => {
                await http.del(`/api/mask-policies/${p.field}`);
                message.success('已删除');
                void load();
            },
        });
    };

    const doPreview = async () => {
        try {
            const res = await http.post<{ result: { masked: string; kind: string; source: string } }>(
                '/api/mask/preview',
                { field: pvField, value: pvValue },
            );
            setPreview(res.result);
        } catch (e) {
            message.error((e as Error).message || '预览失败');
        }
    };

    const columns: ColumnsType<Policy> = [
        { title: '字段', dataIndex: 'field' },
        {
            title: '策略',
            dataIndex: 'strategy',
            width: 100,
            render: (v: string) =>
                v === 'hide' ? <Tag color="red">完全隐藏</Tag> : v === 'plain' ? <Tag color="orange">明文</Tag> : <Tag color="blue">打码</Tag>,
        },
        { title: '保留前缀', dataIndex: 'keep_prefix', width: 100 },
        { title: '保留后缀', dataIndex: 'keep_suffix', width: 100 },
        {
            title: '可看明文角色',
            dataIndex: 'roles',
            width: 160,
            render: (v: string[]) => (v?.length ? v.map((r) => <Tag key={r}>{r}</Tag>) : <Typography.Text type="secondary">无</Typography.Text>),
        },
        { title: '备注', dataIndex: 'note', ellipsis: true },
        {
            title: '操作',
            width: 140,
            render: (_, r) => (
                <Space size={4}>
                    <Button size="small" onClick={() => openEditor(r)}>编辑</Button>
                    <Button size="small" danger onClick={() => remove(r)}>删除</Button>
                </Space>
            ),
        },
    ];

    return (
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Card
                title="字段脱敏策略"
                extra={<Button type="primary" onClick={() => openEditor()}>新建策略</Button>}
            >
                <Alert
                    type="warning"
                    showIcon
                    style={{ marginBottom: 12 }}
                    message="默认全部字段都会按值自动识别并脱敏；这里的策略只用于覆盖默认行为。"
                />
                <Table<Policy>
                    rowKey="field"
                    size="small"
                    loading={loading}
                    columns={columns}
                    dataSource={policies}
                    pagination={false}
                />
            </Card>

            <Card title="脱敏预览">
                <Row gutter={12} align="middle">
                    <Col span={6}>
                        <Select
                            style={{ width: '100%' }}
                            value={pvField}
                            onChange={setPvField}
                            options={PREVIEW_FIELDS.map((f) => ({ value: f, label: f }))}
                        />
                    </Col>
                    <Col span={10}>
                        <Input value={pvValue} onChange={(e) => setPvValue(e.target.value)} placeholder="输入样例值" />
                    </Col>
                    <Col span={4}>
                        <Button type="primary" onClick={() => void doPreview()}>预览</Button>
                    </Col>
                    <Col span={4}>
                        {preview ? (
                            <Space direction="vertical" size={0}>
                                <Typography.Text code>{preview.masked}</Typography.Text>
                                <Typography.Text type="secondary">
                                    {preview.kind} / {preview.source}
                                </Typography.Text>
                            </Space>
                        ) : null}
                    </Col>
                </Row>
                <Typography.Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0 }}>
                    来源含义：policy = 命中上面配置的策略；remote = Python 识别服务；local = 服务不可用时的本地兜底。
                </Typography.Paragraph>
            </Card>

            <Card title="明文访问审计">
                <Table<AuditRow>
                    size="small"
                    rowKey={(r) => `${r.created_at}-${r.operator}-${r.field}`}
                    dataSource={audits}
                    pagination={false}
                    columns={[
                        { title: '时间', dataIndex: 'created_at', width: 170 },
                        { title: '操作人', dataIndex: 'operator', width: 130 },
                        { title: '角色', dataIndex: 'role', width: 90 },
                        { title: '字段', dataIndex: 'field', width: 150 },
                        { title: '理由', dataIndex: 'reason' },
                        { title: '行数', dataIndex: 'row_count', width: 80 },
                    ]}
                />
            </Card>

            <Modal
                title={editing ? `编辑策略：${editing.field}` : '新建策略'}
                open={editorOpen}
                onCancel={() => setEditorOpen(false)}
                onOk={() => void save()}
                width={560}
            >
                <Form form={form} layout="vertical">
                    <Form.Item name="field" label="字段名" rules={[{ required: true }]}>
                        <Input placeholder="member_phone" disabled={!!editing} />
                    </Form.Item>
                    <Form.Item name="strategy" label="策略" rules={[{ required: true }]}>
                        <Select
                            options={[
                                { value: 'mask', label: 'mask：保留首尾，中间打码' },
                                { value: 'hide', label: 'hide：完全不返回' },
                                { value: 'plain', label: 'plain：白名单角色可看明文' },
                            ]}
                        />
                    </Form.Item>
                    <Form.Item name="keep_prefix" label="保留前缀位数">
                        <InputNumber min={0} max={10} style={{ width: '100%' }} />
                    </Form.Item>
                    <Form.Item name="keep_suffix" label="保留后缀位数">
                        <InputNumber min={0} max={10} style={{ width: '100%' }} />
                    </Form.Item>
                    <Form.Item name="mask_char" label="掩码字符">
                        <Input maxLength={1} />
                    </Form.Item>
                    <Form.Item name="roles" label="可看明文的角色">
                        <Select mode="tags" placeholder="admin" />
                    </Form.Item>
                    <Form.Item name="note" label="备注">
                        <Input maxLength={200} />
                    </Form.Item>
                </Form>
            </Modal>
        </Space>
    );
}
