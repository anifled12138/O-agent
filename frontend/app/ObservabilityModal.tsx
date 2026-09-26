'use client';

import { useEffect, useState } from 'react';
import { X } from 'lucide-react';
import { getToolUsageMetrics } from './api';
import type { ToolUsageMetric } from './api';
import { useDialogA11y } from './useDialogA11y';

export default function ObservabilityModal({ onClose }: { onClose: () => void }) {
  const dialogRef = useDialogA11y(onClose);
  const [days, setDays] = useState(30);
  const [items, setItems] = useState<ToolUsageMetric[]>([]);
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState('');

  useEffect(() => {
    let active = true;
    getToolUsageMetrics(days).then((nextItems) => {
      if (active) {
        setItems(nextItems);
        setError('');
      }
    }).catch((reason: unknown) => {
      if (active) setError(reason instanceof Error ? reason.message : '无法读取工具观测数据');
    }).finally(() => {
      if (active) setBusy(false);
    });
    return () => { active = false; };
  }, [days]);

  const totalCalls = items.reduce((sum, item) => sum + item.calls, 0);
  const totalFailures = items.reduce((sum, item) => sum + item.failures, 0);
  const totalAsks = items.reduce((sum, item) => sum + item.permissionAsks, 0);
  const totalCompactions = items.reduce((sum, item) => sum + (item.contextCompactions ?? 0), 0);
  const totalPluginBuilds = items.reduce((sum, item) => sum + (item.pluginBuilds ?? 0), 0);
  const totalPluginActivations = items.reduce((sum, item) => sum + (item.pluginActivations ?? 0), 0);

  return (
    <div className="modal-backdrop observability-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
      <section ref={dialogRef} className="observability-modal" role="dialog" aria-modal="true" aria-labelledby="observability-title" tabIndex={-1}>
        <header className="observability-header">
          <div>
            <span className="eyebrow">运行时 / 可观测性</span>
            <h2 id="observability-title">工具与上下文使用情况</h2>
            <p>指标由持久化运行 trace 和审批记录聚合；不包含原始工具参数或凭据。</p>
          </div>
          <div className="observability-controls">
            <label>
              时间范围
              <select value={days} onChange={(event) => { setBusy(true); setDays(Number(event.target.value)); }}>
                {[7, 30, 90, 365].map((value) => <option key={value} value={value}>最近 {value} 天</option>)}
              </select>
            </label>
            <button type="button" className="icon-button" aria-label="关闭可观测性" onClick={onClose}><X size={18} /></button>
          </div>
        </header>

        <div className="observability-summary">
          <Metric label="工具调用" value={totalCalls} />
          <Metric label="失败" value={totalFailures} />
          <Metric label="等待授权" value={totalAsks} />
          <Metric label="上下文裁剪" value={totalCompactions} />
          <Metric label="插件构建" value={totalPluginBuilds} />
          <Metric label="插件启用" value={totalPluginActivations} />
        </div>

        {error && <div className="forge-error" role="alert">{error}</div>}
        {busy ? <p className="observability-empty">正在读取聚合数据…</p> : items.length === 0 ? (
          <p className="observability-empty">所选时间范围内还没有工具或上下文观测数据。</p>
        ) : (
          <div className="observability-table-wrap">
            <table className="observability-table">
              <thead>
                <tr><th>工具 / 版本</th><th>调用</th><th>失败</th><th>耗时总计</th><th>授权</th><th>审批</th><th>上下文压缩</th><th>插件生命周期</th></tr>
              </thead>
              <tbody>
                {items.map((item) => (
                  <tr key={`${item.toolName}:${item.pluginId ?? ''}:${item.releaseId ?? ''}`}>
                    <td>
                      <b>{item.toolName === 'plugin.lifecycle' ? '插件生命周期' : item.toolName}</b>
                      {(item.pluginId || item.releaseId) && <small>{item.pluginId || 'plugin'}{item.releaseId ? ` · ${item.releaseId.slice(0, 12)}` : ''}</small>}
                      {item.projectId && <small>项目 {item.projectId}</small>}
                    </td>
                    <td>{item.calls}<small>{item.completed} 完成</small></td>
                    <td>{item.failures}</td>
                    <td>{(item.durationMillis / 1000).toFixed(1)} 秒</td>
                    <td>允许 {item.permissionAllows} · 询问 {item.permissionAsks} · 拒绝 {item.permissionDenials}</td>
                    <td>申请 {item.approvalRequests} · 待决 {item.approvalsPending} · 通过 {item.approvalsGranted} · 拒绝 {item.approvalsDenied} · 过期 {item.approvalsExpired} · 取消 {item.approvalsCancelled}</td>
                    <td>{item.contextCompactions ?? 0}{item.contextCompactions ? <small>{item.originalContextChars ?? 0} → {item.compactedContextChars ?? 0} 字符</small> : null}</td>
                    <td>
                      {(item.pluginBuilds ?? 0) + (item.pluginBuildFailures ?? 0) ? `构建 ${item.pluginBuilds ?? 0} · 失败 ${item.pluginBuildFailures ?? 0}` : ''}
                      {(item.pluginActivations ?? 0) + (item.pluginDeactivations ?? 0) + (item.pluginRollbacks ?? 0) ? <small>启用 {item.pluginActivations ?? 0} · 停用 {item.pluginDeactivations ?? 0} · 回退 {item.pluginRollbacks ?? 0}</small> : null}
                      {(item.pluginPermissionRequests ?? 0) + (item.pluginPermissionsGranted ?? 0) + (item.pluginRuntimeFailures ?? 0) + (item.pluginSourceChanges ?? 0) ? <small>授权申请 {item.pluginPermissionRequests ?? 0} · 授权 {item.pluginPermissionsGranted ?? 0} · 崩溃 {item.pluginRuntimeFailures ?? 0} · 源码改动 {item.pluginSourceChanges ?? 0}</small> : null}
                      {(item.pluginUnusableMarks ?? 0) + (item.pluginBundleCleanups ?? 0) ? <small>标记不可用 {item.pluginUnusableMarks ?? 0} · 清理 bundle {item.pluginBundleCleanups ?? 0}</small> : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}

function Metric({ label, value }: { label: string; value: number }) {
  return <div><small>{label}</small><b>{value.toLocaleString()}</b></div>;
}
