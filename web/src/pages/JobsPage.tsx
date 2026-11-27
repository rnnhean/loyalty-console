import { useCallback, useEffect, useState } from 'react';
import {
    Button,
    Card,
    Drawer,
    Form,
    Input,
    InputNumber,
    message,
    Modal,
    Select,
    Space,
    Switch,
    Table,
    Tag,
    Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { http } from '@/lib/http';

/**
 * 定时作业页面。
 *
 * 两个不得不有的能力：
 *   - **立即执行**：运维等不到 cron 点，必须能手动跑一次；
 *   - **执行历史**：作业失败时，只看到「上次失败」不够，要看完整错误。
 */

interface JobItem {
    id: number;
    name: string;
    cron: string;
    command: string;
    concurrency: string;
    timeout_sec: number;
    enabled: boolean;
    next_run_at: string;
    last_run_at: string;
    last_status: string;
    last_error: string;
}

interface RunItem {
    id: number;
    job_id: number;
    status: string;
    duration_ms: number;
    error: string;
    started_at: string;
}

export default function JobsPage() {
    const [items, setItems] = useState<JobItem[]>([]);
    const [commands, setCommands] = useState<string[]>([]);
    const [loading, setLoading] = useState(false);
    const [editorOpen, setEditorOpen] = useState(false);
    const [editing, setEditing] = useState<JobItem | null>(null);
    const [runs, setRuns] = useState<RunItem[]>([]);
    const [runsOpen, setRunsOpen] = useState(false);
    const [runningId, setRunningId] = useState<number | null>(null);
    const [form] = Form.useForm();

    const load = useCallback(async () => {
        setLoading(true);
        try {
            const res = await http.get<{ items: JobItem[]; commands: string[] }>('/api/jobs');
            setItems(res.items ?? []);
            setCommands(res.commands ?? []);
        } catch {
            message.error('加载作业失败');
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => {
        void load();
    }, [load]);

    const openEditor = (j?: JobItem) => {
        setEditing(j ?? null);
        form.setFieldsValue(
            j ?? {
                name: '',
                cron: '*/30 * * * *',
                command: commands[0] ?? 'snapshot_metrics',
                concurrency: 'skip',
                timeout_sec: 30,
                enabled: true,
            },
        );
        setEditorOpen(true);
    };

    const save = async () => {
        const v = await form.validateFields();
        try {
            if (editing) await http.put(`/api/jobs/${editing.id}`, v);
            else await http.post('/api/jobs', v);
            message.success('已保存');
            setEditorOpen(false);
            void load();
        } catch (e) {
            message.error((e as Error).message || '保存失败');
        }
    };

    const remove = (j: JobItem) => {
        Modal.confirm({
            title: `删除作业「${j.name}」？`,
            okButtonProps: { danger: true },
            onOk: async () => {
                await http.del(`/api/jobs/${j.id}`);
                message.success('已删除');
                void load();
            },
        });
    };

    const runNow = async (j: JobItem) => {
        setRunningId(j.id);
        try {
            const res = await http.post<{ output: string }>(`/api/jobs/${j.id}/run`);
            message.success(res.output || '已执行');
            void load();
        } catch (e) {
            // 作业失败时后端会把错误原因带回来，原样展示比「执行失败」有用
            message.error((e as Error).message || '执行失败');
        } finally {
            setRunningId(null);
        }
    };

    const showRuns = async (j: JobItem) => {
        try {
            const res = await http.get<{ items: RunItem[] }>(`/api/job-runs`, { job_id: j.id });
            setRuns(res.items ?? []);
            setRunsOpen(true);
        } catch {
            message.error('加载历史失败');
        }
    };

    const columns: ColumnsType<JobItem> = [
        { title: 'ID', dataIndex: 'id', width: 60 },
        { title: '作业名', dataIndex: 'name' },
        {
            title: 'cron',
            dataIndex: 'cron',
            width: 130,
            render: (v: string) => <Typography.Text code>{v}</Typography.Text>,
        },
        { title: '命令', dataIndex: 'command', width: 190 },
        { title: '并发', dataIndex: 'concurrency', width: 80 },
        {
            title: '状态',
            dataIndex: 'enabled',
            width: 80,
            render: (v: boolean) => (v ? <Tag color="green">启用</Tag> : <Tag>停用</Tag>),
        },
        { title: '下次执行', dataIndex: 'next_run_at', width: 170 },
        {
            title: '上次',
            dataIndex: 'last_status',
            width: 90,
            render: (v: string, r) =>
                v === 'ok' ? (
                    <Tag color="green">ok</Tag>
                ) : v === 'error' ? (
                    <Tag color="red" title={r.last_error}>error</Tag>
                ) : (
                    <Tag>{v || '-'}</Tag>
                ),
        },
        {
            title: '操作',
            width: 210,
            render: (_, r) => (
                <Space size={4}>
                    <Button size="small" loading={runningId === r.id} onClick={() => void runNow(r)}>
                        立即执行
                    </Button>
                    <Button size="small" onClick={() => void showRuns(r)}>历史</Button>
                    <Button size="small" onClick={() => openEditor(r)}>编辑</Button>
                    <Button size="small" danger onClick={() => remove(r)}>删除</Button>
                </Space>
            ),
        },
    ];

    return (
        <Card
            title="定时作业"
            extra={<Button type="primary" onClick={() => openEditor()}>新建作业</Button>}
        >
            <Table<JobItem>
                rowKey="id"
                size="small"
                loading={loading}
                columns={columns}
                dataSource={items}
                pagination={false}
            />

            <Modal
                title={editing ? `编辑作业 #${editing.id}` : '新建作业'}
                open={editorOpen}
                onCancel={() => setEditorOpen(false)}
                onOk={() => void save()}
                width={560}
            >
                <Form form={form} layout="vertical">
                    <Form.Item name="name" label="作业名" rules={[{ required: true }]}>
                        <Input maxLength={64} />
                    </Form.Item>
                    <Form.Item
                        name="cron"
                        label="cron（分 时 日 月 周）"
                        rules={[{ required: true }]}
                        extra="支持 * / */n / a-b / 逗号组合，例如 0 3 * * *"
                    >
                        <Input placeholder="*/30 * * * *" />
                    </Form.Item>
                    <Form.Item name="command" label="执行的命令" rules={[{ required: true }]}>
                        <Select options={commands.map((c) => ({ value: c, label: c }))} />
                    </Form.Item>
                    <Form.Item name="concurrency" label="并发策略">
                        <Select
                            options={[
                                { value: 'skip', label: 'skip：上次没跑完就跳过' },
                                { value: 'queue', label: 'queue：排队等上次结束' },
                                { value: 'allow', label: 'allow：允许并发（仅幂等作业）' },
                            ]}
                        />
                    </Form.Item>
                    <Form.Item name="timeout_sec" label="超时（秒）">
                        <InputNumber min={1} max={600} style={{ width: '100%' }} />
                    </Form.Item>
                    <Form.Item name="enabled" label="启用" valuePropName="checked">
                        <Switch />
                    </Form.Item>
                </Form>
            </Modal>

            <Drawer title="执行历史" open={runsOpen} onClose={() => setRunsOpen(false)} width={640}>
                <Table<RunItem>
                    size="small"
                    rowKey="id"
                    pagination={false}
                    dataSource={runs}
                    columns={[
                        { title: '时间', dataIndex: 'started_at', width: 170 },
                        {
                            title: '结果',
                            dataIndex: 'status',
                            width: 80,
                            render: (v: string) =>
                                v === 'ok' ? <Tag color="green">ok</Tag> : <Tag color="red">{v}</Tag>,
                        },
                        { title: '耗时', dataIndex: 'duration_ms', width: 90, render: (v: number) => `${v}ms` },
                        { title: '错误', dataIndex: 'error', ellipsis: true },
                    ]}
                />
            </Drawer>
        </Card>
    );
}
