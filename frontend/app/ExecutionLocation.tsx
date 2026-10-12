'use client';

import { useState } from 'react';
import { Check, ChevronDown, Cloud, Laptop, Server, LoaderCircle } from 'lucide-react';
import type { ExecutionNode } from './api';
import { executionPhaseLabels, type ExecutionTarget, type RoutedExecutionTask } from './executionRouting';

export default function ExecutionLocation({ target, nodeId, nodes, barrier, error, busy, onChange, onDevices, onTasks }: {
  target: ExecutionTarget; nodeId: string; nodes: ExecutionNode[]; barrier?: RoutedExecutionTask;
  error: string; busy: boolean; onChange: (target: ExecutionTarget, nodeId?: string) => void;
  onDevices: () => void; onTasks: () => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const node = nodes.find((item) => item.id === nodeId);
  const label = target === 'local' ? node?.name ?? '选择电脑' : target === 'cloud' ? '云端任务队列' : '当前服务节点';
  const Icon = target === 'local' ? Laptop : target === 'cloud' ? Cloud : Server;
  const options = [{ target: 'current' as const, label: '当前服务节点', Icon: Server }, { target: 'cloud' as const, label: '云端任务队列', Icon: Cloud }];
  return <section className="execution-location" aria-label="执行位置" onKeyDown={(event) => { if (event.key === 'Escape') { setExpanded(false); event.stopPropagation(); } }}>
    <button type="button" className="execution-location-trigger" aria-expanded={expanded} aria-controls="execution-location-options" onClick={() => setExpanded(!expanded)}>
      <Icon size={18} aria-hidden="true" /><span><small>执行位置</small><strong>{label}</strong></span><ChevronDown size={15} aria-hidden="true" className={expanded ? 'open' : ''} />
    </button>
    <div id="execution-location-options" className="execution-location-options" role="group" aria-label="选择执行位置" hidden={!expanded}>
      {options.map((option) => <button key={option.target} type="button" disabled={busy} aria-pressed={target === option.target} onClick={() => { onChange(option.target); setExpanded(false); }}><option.Icon size={16} aria-hidden="true" /><span>{option.label}</span>{target === option.target && <Check size={14} aria-hidden="true" />}</button>)}
      <p>本地电脑</p>
      {nodes.length ? nodes.map((item) => <button type="button" key={item.id} disabled={busy || item.connectivity !== 'connected' || !!item.revokedAt || item.resources?.maxConcurrentTasks === 0} aria-pressed={target === 'local' && nodeId === item.id} onClick={() => { onChange('local', item.id); setExpanded(false); }}><Laptop size={16} aria-hidden="true" /><span>{item.name}<small>{item.connectivity !== 'connected' || item.revokedAt ? '离线' : item.resources?.maxConcurrentTasks === 0 ? '暂无执行容量' : item.platform}</small></span>{target === 'local' && nodeId === item.id && <Check size={14} aria-hidden="true" />}</button>) : <p className="execution-location-empty">暂无设备，请先添加电脑</p>}
      <div className="execution-location-links"><button type="button" onClick={onDevices}>管理设备</button><button type="button" onClick={onTasks}>任务与产物</button></div>
      <p className="execution-location-hint">发送时先同步会话、工作区和附件，再开始执行。</p>
    </div>
    {error ? <p className="execution-location-state" role="alert">{error}</p> : barrier && <div className="execution-location-state" role="status">
      <span>{barrier.status === 'reported_succeeded' ? '本地记录已归档，需同步后继续' : barrier.status === 'needs_reconciliation' ? '执行结果需核查，暂不能切换' : <><LoaderCircle size={13} aria-hidden="true" />{executionPhaseLabels[barrier.progressPhase ?? ''] ?? '任务处理中，等待同步'}</>}</span>
      <button type="button" onClick={onTasks}>{barrier.continuedConversation ? '打开续接会话' : '查看同步与交接'}</button>
    </div>}
  </section>;
}
