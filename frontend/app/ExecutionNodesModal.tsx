'use client';

import { FormEvent, useCallback, useEffect, useState } from 'react';
import { AlertCircle, Check, Copy, Laptop, RefreshCw, ShieldAlert, X } from 'lucide-react';
import { copyToClipboard } from './MarkdownView';
import { ExecutionNode, getExecutionNodes, pairExecutionNode, revokeExecutionNode } from './api';
import { useDialogA11y } from './useDialogA11y';

type PairReceipt = { node: ExecutionNode; credential: string };

const connectivityLabel: Record<string, string> = {
  connected: '在线（心跳已读回）',
  disconnected: '离线',
  revoked: '已撤销',
};

function nodeResourceLabel(node: ExecutionNode): string {
  const resources = node.resources;
  if (!resources) return '资源状态待节点上报';
  if (resources.memoryTotalBytes <= 0) return `${resources.maxConcurrentTasks} 个准入槽位 · 详细资源状态待节点上报`;
  const availableGiB = (resources.memoryAvailableBytes / 1024 ** 3).toFixed(1);
  const totalGiB = (resources.memoryTotalBytes / 1024 ** 3).toFixed(1);
  const admission = resources.maxConcurrentTasks > 0 ? `可接新任务 ${resources.maxConcurrentTasks}` : '资源不足，暂不接新任务';
  return `${admission} · ${resources.logicalCpus} 核 · 可用内存 ${availableGiB}/${totalGiB} GiB`;
}

export default function ExecutionNodesModal({ onClose }: { onClose: () => void }) {
  const dialogRef = useDialogA11y(onClose);
  const [nodes, setNodes] = useState<ExecutionNode[]>([]);
  const [name, setName] = useState('');
  const [platform, setPlatform] = useState('linux');
  const [pairReceipt, setPairReceipt] = useState<PairReceipt | null>(null);
  const [busy, setBusy] = useState(false);
  const [copyMessage, setCopyMessage] = useState('');
  const [error, setError] = useState('');

  const refresh = useCallback(async () => {
    try {
      setNodes(await getExecutionNodes());
      setError('');
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '设备状态读取失败');
    }
  }, []);

  useEffect(() => {
    const initial = window.setTimeout(() => void refresh(), 0);
    const timer = window.setInterval(() => void refresh(), 10_000);
    return () => {
      window.clearTimeout(initial);
      window.clearInterval(timer);
    };
  }, [refresh]);

  async function pair(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError('');
    setPairReceipt(null);
    setCopyMessage('');
    try {
      const receipt = await pairExecutionNode({ name: name.trim(), platform });
      if (!receipt.node.id || receipt.node.name !== name.trim() || receipt.node.revokedAt || !receipt.credential || !receipt.credentialShownOnce) {
        throw new Error('设备配对响应没有读回为有效节点和一次性凭据。');
      }
      setPairReceipt({ node: receipt.node, credential: receipt.credential });
      setNodes((current) => [receipt.node, ...current.filter((item) => item.id !== receipt.node.id)]);
      setName('');
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '设备配对失败');
      try {
        setNodes(await getExecutionNodes());
      } catch (readErr) {
        const readMessage = readErr instanceof Error ? readErr.message : '设备状态读取失败';
        setError((current) => `${current}；配对后状态读取也失败：${readMessage}`);
      }
    } finally {
      setBusy(false);
    }
  }

  async function revoke(node: ExecutionNode) {
    if (!window.confirm(`撤销「${node.name}」的节点凭据？该设备当前活动任务会进入核查状态。`)) return;
    setBusy(true);
    setError('');
    try {
      const readBack = await revokeExecutionNode(node.id);
      if (readBack.id !== node.id || readBack.connectivity !== 'revoked' || !readBack.revokedAt) {
        throw new Error('撤销请求没有读回为已撤销状态。');
      }
      setNodes((current) => current.map((item) => item.id === node.id ? readBack : item));
      if (pairReceipt?.node.id === node.id) setPairReceipt(null);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '撤销节点失败');
      try {
        setNodes(await getExecutionNodes());
      } catch (readErr) {
        const readMessage = readErr instanceof Error ? readErr.message : '设备状态读取失败';
        setError((current) => `${current}；撤销后状态读取也失败：${readMessage}`);
      }
    } finally {
      setBusy(false);
    }
  }

  async function copyCredential() {
    if (!pairReceipt) return;
    const copied = await copyToClipboard(pairReceipt.credential);
    if (copied) setCopyMessage('一次性凭据已复制；请配置在该电脑的 O 启动环境中，避免写入仓库。');
    else setCopyMessage('复制失败，请在安全环境中手动复制一次性凭据。');
  }

  return <div className="modal-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
    <section ref={dialogRef} role="dialog" aria-modal="true" aria-labelledby="execution-nodes-title" tabIndex={-1} style={{ width: 'min(720px, calc(100vw - 28px))', maxHeight: 'min(780px, calc(100dvh - 32px))', overflow: 'auto', background: 'var(--panel, #fffefa)', color: 'var(--text, #25251f)', border: '1px solid var(--border, #deddd5)', borderRadius: 16, padding: 20, boxShadow: '0 18px 60px #0003' }}>
      <header style={{ display: 'flex', alignItems: 'center', gap: 12, borderBottom: '1px solid var(--border, #deddd5)', paddingBottom: 12 }}>
        <div style={{ flex: 1 }}><h2 id="execution-nodes-title" style={{ margin: 0, fontSize: 18 }}>执行设备</h2><p style={{ margin: '5px 0 0', color: 'var(--muted, #77776e)', fontSize: 13 }}>设备和撤销状态来自云端读回；在线仅代表最近收到心跳。</p></div>
        <button type="button" className="icon-button" style={{ minWidth: 44, minHeight: 44 }} onClick={() => void refresh()} aria-label="刷新设备状态" title="刷新"><RefreshCw size={16} /></button>
        <button type="button" className="icon-button" style={{ minWidth: 44, minHeight: 44 }} onClick={onClose} aria-label="关闭设备管理"><X size={18} /></button>
      </header>

      <div role="note" style={{ display: 'flex', gap: 9, alignItems: 'flex-start', border: '1px solid #d9a441', borderRadius: 10, padding: 11, marginTop: 13, fontSize: 13 }}>
        <ShieldAlert size={16} aria-hidden="true" style={{ flex: '0 0 auto', marginTop: 1 }} />
        <span>配对后，在那台电脑启动本地 O 节点代理即可领取任务。提交本地任务时会把当前云端会话历史作为上下文快照传给所选电脑；本地创建独立会话并归档完整消息、Agent turn 与执行 trace。项目工作区和运行中检查点仍需分别同步。</span>
      </div>

      {error && <p role="alert" style={{ display: 'flex', gap: 7, color: '#a22828', fontSize: 13 }}><AlertCircle size={15} aria-hidden="true" />{error}</p>}
      {pairReceipt && <div role="status" style={{ border: '1px solid var(--border, #deddd5)', borderRadius: 10, padding: 12, marginTop: 12 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 7, fontSize: 13 }}><Check size={15} aria-hidden="true" /><strong>{pairReceipt.node.name} 已登记</strong></div>
        <p style={{ fontSize: 12, margin: '7px 0', color: 'var(--muted, #77776e)' }}>此凭据只显示这一次。请通过安全渠道安装并保存；关闭窗口后无法再次读取。</p>
        <code style={{ display: 'block', padding: 9, borderRadius: 7, background: 'var(--surface, #f4f3ed)', overflowWrap: 'anywhere', userSelect: 'all', fontSize: 12 }}>{pairReceipt.credential}</code>
        <p style={{ fontSize: 12, margin: '9px 0 4px', color: 'var(--muted, #77776e)' }}>在本地 O 的启动环境中设置以下两项，然后重启本地 O。控制地址必须使用 HTTPS；如果本地 O 配置了多个模型服务，还需设置 O_NODE_PROVIDER_ID 指向这台电脑的本地模型 ID。</p>
        <code style={{ display: 'block', padding: 9, borderRadius: 7, background: 'var(--surface, #f4f3ed)', overflowWrap: 'anywhere', whiteSpace: 'pre-wrap', userSelect: 'all', fontSize: 12 }}>O_NODE_CONTROL_URL={typeof window === 'undefined' ? '' : window.location.origin}{'\n'}O_NODE_CREDENTIAL={pairReceipt.credential}</code>
        <button type="button" className="upc-btn-secondary" style={{ minHeight: 44, marginTop: 8 }} onClick={() => void copyCredential()}><Copy size={14} /><span>复制一次性凭据</span></button>
        {copyMessage && <p role="status" style={{ fontSize: 12, margin: '6px 0 0' }}>{copyMessage}</p>}
      </div>}

      <form onSubmit={(event) => void pair(event)} style={{ display: 'flex', alignItems: 'end', flexWrap: 'wrap', gap: 8, padding: '14px 0', borderBottom: '1px solid var(--border, #deddd5)' }}>
        <label style={{ display: 'grid', gap: 4, flex: '1 1 180px', fontSize: 12 }}>设备名称<input className="settings-input" value={name} onChange={(event) => setName(event.target.value)} required maxLength={100} placeholder="例如：书房 Linux" /></label>
        <label style={{ display: 'grid', gap: 4, flex: '0 1 150px', fontSize: 12 }}>操作系统<select className="settings-input" value={platform} onChange={(event) => setPlatform(event.target.value)}><option value="linux">Linux</option><option value="windows">Windows</option><option value="darwin">macOS</option></select></label>
        <button type="submit" className="upc-btn-primary" style={{ minHeight: 44 }} disabled={busy || !name.trim()}><Laptop size={14} /><span>{busy ? '处理中…' : '登记电脑'}</span></button>
      </form>

      <ul style={{ listStyle: 'none', padding: 0, margin: 0 }}>
        {nodes.length === 0 ? <li style={{ color: 'var(--muted, #77776e)', padding: '20px 4px', fontSize: 13 }}>云端还没有登记设备。</li> : nodes.map((node) => <li key={node.id} style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '12px 2px', borderBottom: '1px solid var(--border, #deddd5)' }}>
          <Laptop size={17} aria-hidden="true" />
          <div style={{ minWidth: 0, flex: 1 }}><strong style={{ fontSize: 13 }}>{node.name}</strong><div style={{ color: 'var(--muted, #77776e)', fontSize: 12, overflowWrap: 'anywhere' }}>{node.platform} · {connectivityLabel[node.connectivity] ?? node.connectivity}{node.lastSeen ? ` · ${new Date(node.lastSeen).toLocaleString()}` : ''}</div><div style={{ color: 'var(--muted, #77776e)', fontSize: 11 }}>{nodeResourceLabel(node)}</div><div style={{ color: 'var(--muted, #77776e)', fontSize: 11 }}>{node.id}</div></div>
          {node.connectivity !== 'revoked' && <button type="button" className="upc-btn-secondary" style={{ minHeight: 44 }} disabled={busy} onClick={() => void revoke(node)}>撤销</button>}
        </li>)}
      </ul>
    </section>
  </div>;
}
