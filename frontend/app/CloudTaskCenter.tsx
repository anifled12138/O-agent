'use client';

import { useCallback, useEffect, useState } from 'react';
import { Copy, RefreshCw, Square, X } from 'lucide-react';
import { API, ArtifactShareLink, continueLocalTaskInCloud, createArtifactShareLink, ExecutionTask, HandoffEffectResolution, HandoffExternalEffect, importLocalTaskProjectDelta, listArtifactShareLinks, previewLocalTaskContinuation, Project, request, requestSafeTaskHandoff, revokeArtifactShareLink } from './api';
import { useDialogA11y } from './useDialogA11y';

type CloudTask = ExecutionTask;
type RecoveryEvidenceArtifact = { id: string; role: string; fileName: string; byteSize: number };

const terminal = new Set(['completed', 'incomplete', 'reported_succeeded', 'reported_failed', 'cancelled', 'needs_reconciliation']);
const labels: Record<string, string> = {
  queued: '排队中', leased: '已领取', accepted: '已接收', running: '运行中',
  reported_succeeded: '节点已报告成功，待核验', reported_failed: '执行失败', incomplete: '安全暂停，可继续',
  cancelled: '已取消', needs_reconciliation: '需要核查', completed: '已验证完成',
};
const progressLabels: Record<string, string> = {
  preparing: '正在准备任务输入',
  waiting_for_safe_boundary: '已请求安全暂停，等待当前操作到达安全边界',
  agent_and_snapshot: 'Agent 正在执行并整理本地产物',
  safe_boundary_reached: '已到达安全暂停边界',
  local_result_durable: '本地任务结果已持久保存',
  uploading_artifacts: '正在上传任务记录与项目成果',
  artifacts_verified: '任务成果已在云端校验，正在确认最终状态',
};

export default function CloudTaskCenter({ onClose, onOpenConversation }: { onClose: () => void; onOpenConversation: (id: string) => void }) {
  const [tasks, setTasks] = useState<CloudTask[]>([]);
  const [error, setError] = useState('');
  const [busyTask, setBusyTask] = useState('');
  const [sharingArtifact, setSharingArtifact] = useState('');
  const [shareMessage, setShareMessage] = useState('');
  const [managedShareArtifact, setManagedShareArtifact] = useState('');
  const [shareLinks, setShareLinks] = useState<Record<string, ArtifactShareLink[]>>({});
  const [importedProjects, setImportedProjects] = useState<Record<string, Project>>({});
  const [continuedConversations, setContinuedConversations] = useState<Record<string, NonNullable<ExecutionTask['continuedConversation']>>>({});
  const [continuationTaskStatuses, setContinuationTaskStatuses] = useState<Record<string, { id: string; status: string }>>({});
  const [handoffReview, setHandoffReview] = useState<{ taskId: string; effects: HandoffExternalEffect[]; recovery: boolean } | null>(null);
  const ref = useDialogA11y(onClose);
  const refresh = useCallback(async () => {
    try {
      const latest = await request<CloudTask[]>('/tasks');
      if (!Array.isArray(latest)) throw new Error('任务列表格式无效。');
      setTasks(latest);
      setImportedProjects(Object.fromEntries(latest.filter((task) => task.importedProject).map((task) => [task.id, task.importedProject as Project])));
      setContinuedConversations(Object.fromEntries(latest.filter((task) => task.continuedConversation).map((task) => [task.id, task.continuedConversation as NonNullable<ExecutionTask['continuedConversation']>])));
      setContinuationTaskStatuses(Object.fromEntries(latest.filter((task) => task.continuationTaskId && task.continuationTaskStatus).map((task) => [task.id, { id: task.continuationTaskId as string, status: task.continuationTaskStatus as string }])));
      setError('');
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '任务列表读取失败');
    }
  }, []);
  useEffect(() => { const initial = window.setTimeout(() => void refresh(), 0); const timer = window.setInterval(() => void refresh(), 5000); return () => { window.clearTimeout(initial); window.clearInterval(timer); }; }, [refresh]);

  async function cancel(id: string) {
    setBusyTask(id);
    try {
      const readBack = await request<CloudTask>(`/tasks/${encodeURIComponent(id)}/cancel`, { method: 'POST', body: '{}' });
      if (readBack.id !== id || (!readBack.cancelRequested && !terminal.has(readBack.status))) throw new Error('取消请求没有读回为已接受或终态。');
      await refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '任务取消失败');
    } finally {
      setBusyTask('');
    }
  }

  async function handoff(id: string) {
    setBusyTask(id);
    try {
      const readBack = await requestSafeTaskHandoff(id);
      if (readBack.id !== id || !readBack.handoffRequested || terminal.has(readBack.status)) throw new Error('安全暂停请求没有从活动任务状态读回。');
      setError('');
      await refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '安全暂停请求失败');
    } finally {
      setBusyTask('');
    }
  }

  async function shareArtifact(artifactId: string) {
    setSharingArtifact(artifactId);
    setShareMessage('');
    try {
      const result = await createArtifactShareLink(artifactId);
      const url = new URL(result.path, `${API}/`).href;
      const desktopCopy = window.oDesktop?.copyText;
      const copied = desktopCopy
        ? await desktopCopy(url)
        : navigator.clipboard?.writeText ? await navigator.clipboard.writeText(url).then(() => true) : false;
      const prompted = copied ? null : window.prompt('请复制这个 1 小时后失效的成果链接：', url);
      setShareMessage(copied ? '限时链接已复制（1 小时后失效）' : prompted ? '限时链接已生成（1 小时后失效）；可在云端任务中撤销。' : '限时链接已创建（1 小时后失效），但尚未复制；可在云端任务中管理或撤销。');
      setError('');
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '成果分享链接创建失败');
    } finally {
      setSharingArtifact('');
    }
  }

  async function manageArtifactLinks(artifactId: string) {
    if (managedShareArtifact === artifactId) {
      setManagedShareArtifact('');
      return;
    }
    setSharingArtifact(artifactId);
    try {
      const links = await listArtifactShareLinks(artifactId);
      setShareLinks((current) => ({ ...current, [artifactId]: links }));
      setManagedShareArtifact(artifactId);
      setError('');
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '分享链接列表读取失败');
    } finally {
      setSharingArtifact('');
    }
  }

  async function revokeShareLink(artifactId: string, linkId: string) {
    setSharingArtifact(linkId);
    try {
      const revoked = await revokeArtifactShareLink(linkId);
      if (revoked.artifactId !== artifactId) throw new Error('撤销结果指向了另一份成果。');
      const links = await listArtifactShareLinks(artifactId);
      if (!links.some((link) => link.id === linkId && link.revokedAt)) throw new Error('撤销后的分享链接状态未能读回。');
      setShareLinks((current) => ({ ...current, [artifactId]: links }));
      setError('');
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '分享链接撤销失败');
    } finally {
      setSharingArtifact('');
    }
  }

  function artifactShareControls(artifactId: string) {
    if (!artifactId) return null;
    const links = shareLinks[artifactId] ?? [];
    return <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, flexWrap: 'wrap', marginLeft: 8 }}>
      <button type="button" className="secondary-button" style={{ fontSize: 11, padding: '3px 7px' }} disabled={sharingArtifact === artifactId} onClick={() => void shareArtifact(artifactId)}><Copy size={12} />{sharingArtifact === artifactId ? '生成中…' : '复制限时链接'}</button>
      <button type="button" className="secondary-button" style={{ fontSize: 11, padding: '3px 7px' }} disabled={sharingArtifact === artifactId} onClick={() => void manageArtifactLinks(artifactId)}>{managedShareArtifact === artifactId ? '收起链接' : '管理链接'}</button>
      {managedShareArtifact === artifactId && <span style={{ display: 'inline-flex', gap: 6, flexWrap: 'wrap' }}>{links.length ? links.map((link) => <span key={link.id} style={{ color: link.revokedAt || Date.parse(link.expiresAt) <= Date.now() ? 'var(--muted, #77776e)' : '#8a5c11' }}>{new Date(link.expiresAt).toLocaleString()} · {link.revokedAt ? '已撤销' : Date.parse(link.expiresAt) <= Date.now() ? '已过期' : <button type="button" className="secondary-button" style={{ fontSize: 11, padding: '2px 6px' }} disabled={sharingArtifact === link.id} onClick={() => void revokeShareLink(artifactId, link.id)}>{sharingArtifact === link.id ? '撤销中…' : '撤销链接'}</button>}</span>) : <span style={{ color: 'var(--muted, #77776e)' }}>还没有分享链接</span>}</span>}
    </span>;
  }

  async function importProjectDelta(id: string, recovery = false) {
    setBusyTask(id);
    try {
      if (recovery && !window.confirm('这份项目快照来自租约已过期的节点。请确认原电脑上的 Agent 已停止；导入只会建立任务专属云端分支，不会重放任务。是否继续？')) return;
      const result = await importLocalTaskProjectDelta(id, recovery);
      if (!result.project?.id || !result.project.resolvedCommit || !result.project.remoteBranch) throw new Error('云端项目导入没有读回有效分支和提交。');
      setImportedProjects((current) => ({ ...current, [id]: result.project }));
      setError('');
      await refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '项目增量导入失败');
    } finally {
      setBusyTask('');
    }
  }

  async function continueInCloud(id: string, effectResolutions?: HandoffEffectResolution[], recovery = false) {
    setBusyTask(id);
    try {
      if (!effectResolutions) {
        const preview = await previewLocalTaskContinuation(id);
        if (preview.taskId !== id || !Array.isArray(preview.effects) || preview.requiresAcknowledgement !== (preview.effects.length > 0)) throw new Error('云端交接预览没有读回有效的副作用记录。');
        if (preview.recoveryContinuation && !window.confirm('恢复证据不代表原电脑已停止。请先确认原电脑上的 Agent 进程已停止；云端会创建独立续接段，不会恢复或重放原任务。是否确认？')) return;
        recovery = preview.recoveryContinuation === true;
        if (preview.requiresAcknowledgement) {
          setHandoffReview({ taskId: id, effects: preview.effects, recovery });
          return;
        }
      }
      const result = await continueLocalTaskInCloud(id, undefined, effectResolutions ?? [], recovery);
      if (!result.conversation?.id || result.project && result.conversation.projectId !== result.project.id || !result.task?.id || !result.task.status) throw new Error('云端续接会话或持久任务没有读回对应的项目分支与状态。');
      setHandoffReview(null);
      setContinuedConversations((current) => ({ ...current, [id]: result.conversation }));
      setContinuationTaskStatuses((current) => ({ ...current, [id]: { id: result.task.id, status: result.task.status } }));
      setError('');
      await refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '云端续接会话创建失败');
    } finally {
      setBusyTask('');
    }
  }

  return <div className="modal-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
    <section ref={ref} role="dialog" aria-modal="true" aria-labelledby="cloud-task-center-title" tabIndex={-1} style={{ width: 'min(720px, calc(100vw - 28px))', maxHeight: 'min(780px, calc(100dvh - 32px))', overflow: 'auto', background: 'var(--panel, #fffefa)', color: 'var(--text, #25251f)', border: '1px solid var(--border, #deddd5)', borderRadius: 16, padding: 20, boxShadow: '0 18px 60px #0003' }}>
      <header style={{ display: 'flex', alignItems: 'center', gap: 12, borderBottom: '1px solid var(--border, #deddd5)', paddingBottom: 12 }}>
        <div style={{ flex: 1 }}><h2 id="cloud-task-center-title" style={{ margin: 0, fontSize: 18 }}>云端任务</h2><p style={{ margin: '5px 0 0', color: 'var(--muted, #77776e)', fontSize: 13 }}>状态来自云端持久记录，关闭此窗口不会停止任务。</p></div>
        <button type="button" className="icon-button" onClick={() => void refresh()} aria-label="刷新任务列表" title="刷新"><RefreshCw size={16} /></button>
        <button type="button" className="icon-button" onClick={onClose} aria-label="关闭任务列表"><X size={18} /></button>
      </header>
      {error && <p role="alert" style={{ color: '#a22828', fontSize: 13 }}>{error}</p>}
      {shareMessage && <p role="status" style={{ color: '#16835d', fontSize: 12 }}>{shareMessage}</p>}
      {tasks.length === 0 ? <p style={{ color: 'var(--muted, #77776e)', padding: '24px 4px' }}>还没有已提交的任务。</p> : <ul style={{ listStyle: 'none', padding: 0, margin: 0 }}>
        {tasks.map((task) => {
          const taskResult = task.result ?? task.recoveryResult;
          const assistantText = typeof taskResult?.assistantText === 'string' ? taskResult.assistantText : '';
          const recoveryDeltaManifest = task.recoveryResult?.projectDeltaArtifact && typeof task.recoveryResult.projectDeltaArtifact === 'object' ? task.recoveryResult.projectDeltaArtifact as { id?: unknown } : undefined;
          const recoveryHasProjectDelta = !!recoveryDeltaManifest?.id || !!task.recoveryResult?.projectDeltaCommit;
          const assistantTextTruncated = taskResult?.assistantTextTruncated === true;
          const artifact = taskResult?.transcriptArtifact && typeof taskResult.transcriptArtifact === 'object'
            ? taskResult.transcriptArtifact as { id?: unknown; fileName?: unknown; byteSize?: unknown }
            : undefined;
          const artifactId = typeof artifact?.id === 'string' ? artifact.id : '';
          const artifactName = typeof artifact?.fileName === 'string' ? artifact.fileName : '会话完整记录';
          const projectDelta = task.result?.projectDeltaArtifact && typeof task.result.projectDeltaArtifact === 'object'
            ? task.result.projectDeltaArtifact as { id?: unknown; fileName?: unknown; byteSize?: unknown }
            : undefined;
          const projectDeltaId = typeof projectDelta?.id === 'string' ? projectDelta.id : '';
          const projectDeltaName = typeof projectDelta?.fileName === 'string' ? projectDelta.fileName : '项目增量包';
          const projectDeltaBase = typeof task.result?.projectDeltaBaseCommit === 'string' ? task.result.projectDeltaBaseCommit : '';
          const projectDeltaCommit = typeof task.result?.projectDeltaCommit === 'string' ? task.result.projectDeltaCommit : '';
          const workspaceOutput = task.result?.workspaceOutputArtifact && typeof task.result.workspaceOutputArtifact === 'object'
            ? task.result.workspaceOutputArtifact as { id?: unknown; fileName?: unknown; byteSize?: unknown }
            : undefined;
          const workspaceOutputId = typeof workspaceOutput?.id === 'string' ? workspaceOutput.id : '';
          const workspaceOutputName = typeof workspaceOutput?.fileName === 'string' ? workspaceOutput.fileName : '云端任务工作区成果';
          const workspaceOutputBase = typeof task.result?.workspaceOutputBaseCommit === 'string' ? task.result.workspaceOutputBaseCommit : '';
          const workspaceOutputCommit = typeof task.result?.workspaceOutputCommit === 'string' ? task.result.workspaceOutputCommit : '';
          const projectId = typeof task.result?.projectId === 'string' ? task.result.projectId : '';
          const projectCommit = typeof task.result?.projectCommit === 'string' ? task.result.projectCommit : '';
          const handoffArtifact = taskResult?.handoffCheckpointArtifact && typeof taskResult.handoffCheckpointArtifact === 'object'
            ? taskResult.handoffCheckpointArtifact as { id?: unknown }
            : undefined;
          const hasHandoffCheckpoint = typeof handoffArtifact?.id === 'string' && !!handoffArtifact.id;
          const transcriptAvailable = typeof artifactId === 'string' && !!artifactId;
          const safeStopReason = ['step_limit', 'model_call_limit', 'stalled', 'handoff_requested'].includes(String(taskResult?.agentStopReason ?? ''));
          const continuableAgentStatus = taskResult?.agentStatus === 'completed' || (taskResult?.agentStatus === 'incomplete' && safeStopReason);
          const importedProject = importedProjects[task.id] ?? task.importedProject;
          const projectDeltaReady = task.status === 'reported_succeeded' && continuableAgentStatus && !!projectId && !!projectCommit && projectCommit === projectDeltaBase && !!projectDeltaId && !!projectDeltaCommit;
          const transcriptContinuationReady = task.status === 'reported_succeeded' && continuableAgentStatus && transcriptAvailable && !projectDeltaId && !projectDeltaCommit && !projectDeltaBase;
          const recoveryCanContinue = task.status === 'needs_reconciliation' && !!task.recoveryResult && continuableAgentStatus && transcriptAvailable && (!recoveryHasProjectDelta || !!importedProject);
          const continuedConversation = continuedConversations[task.id];
          const continuationTask = continuationTaskStatuses[task.id] ?? (task.continuationTaskId && task.continuationTaskStatus ? { id: task.continuationTaskId, status: task.continuationTaskStatus } : undefined);
          const continuationTaskPending = !!continuationTask && !terminal.has(continuationTask.status);
          const segmentCount = tasks.filter((candidate) => candidate.logicalTaskId === task.logicalTaskId).length;
          return <li key={task.id} style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 12, padding: '13px 2px', borderBottom: '1px solid var(--border, #deddd5)' }}>
          <span style={{ width: 8, height: 8, borderRadius: 8, background: task.status === 'completed' ? '#16835d' : task.status === 'needs_reconciliation' || task.status === 'reported_failed' ? '#b43d34' : '#d39528', flex: '0 0 auto' }} aria-hidden="true" />
          <div style={{ minWidth: 0, flex: 1 }}><strong style={{ fontSize: 14 }}>{task.status === 'reported_succeeded' && task.result?.agentStatus === 'incomplete' ? '本地安全暂停，成果待核验' : labels[task.status] ?? task.status}</strong><div style={{ color: 'var(--muted, #77776e)', fontSize: 12, overflowWrap: 'anywhere' }}>逻辑任务 {task.logicalTaskId} · 执行段 {task.segmentIndex + 1}/{segmentCount} · {task.id} · {task.nodeId} · 第 {task.attempt} 次执行 · {new Date(task.updatedAt).toLocaleString()}</div>{task.progressPhase && <div role="status" style={{ color: task.status === 'needs_reconciliation' ? '#8a5c11' : 'var(--muted, #77776e)', fontSize: 12, marginTop: 4 }}>{progressLabels[task.progressPhase] ?? task.progressPhase}{task.progressUpdatedAt ? ` · ${new Date(task.progressUpdatedAt).toLocaleTimeString()}` : ''}</div>}{task.result?.agentStatus === 'incomplete' && <div style={{ color: '#8a5c11', fontSize: 12, marginTop: 4 }}>{hasHandoffCheckpoint ? '云端会先验证源检查点及运行环境；若有项目改动，会在增量导入到该任务专属分支后重绑定检查点再继续。不匹配时保留成果并说明原因。' : '任务有完整对话记录，但没有可迁移检查点；云端会创建带历史记录的新会话供你继续。'}</div>}{task.error && <div style={{ color: '#a22828', fontSize: 12, marginTop: 4 }}>{task.error}</div>}{assistantText && <p style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', margin: '8px 0 0', fontSize: 13 }}>{assistantText}{assistantTextTruncated ? '…（完整回复见下载记录）' : ''}</p>}{artifactId && <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginTop: 8, fontSize: 12 }}><a href={`${API}/artifacts/${encodeURIComponent(artifactId)}`} download={artifactName}>下载完整会话记录</a>{artifactShareControls(artifactId)}</div>}{workspaceOutputId && <div style={{ marginTop: 8, fontSize: 12 }}><a href={`${API}/artifacts/${encodeURIComponent(workspaceOutputId)}`} download={workspaceOutputName}>下载云端任务工作区成果</a>{artifactShareControls(workspaceOutputId)}{workspaceOutputBase && workspaceOutputCommit && <span style={{ marginLeft: 8, color: 'var(--muted, #77776e)', overflowWrap: 'anywhere' }}>{workspaceOutputBase.slice(0, 10)} → {workspaceOutputCommit.slice(0, 10)}</span>}</div>}{projectDeltaId && <div style={{ marginTop: 8, fontSize: 12 }}><a href={`${API}/artifacts/${encodeURIComponent(projectDeltaId)}`} download={projectDeltaName}>下载项目增量包</a>{artifactShareControls(projectDeltaId)}{projectDeltaBase && projectDeltaCommit && <span style={{ marginLeft: 8, color: 'var(--muted, #77776e)', overflowWrap: 'anywhere' }}>{projectDeltaBase.slice(0, 10)} → {projectDeltaCommit.slice(0, 10)}</span>}{projectDeltaReady && (importedProject ? <div style={{ marginTop: 6, color: '#16835d' }}>已导入云端项目：{importedProject.name} · 分支 {importedProject.remoteBranch} · {importedProject.resolvedCommit?.slice(0, 10)}{continuedConversation ? <div style={{ marginTop: 4 }}>云端续接已提交：{continuedConversation.title}（{continuedConversation.id}）· 任务 {continuationTask ? `${labels[continuationTask.status] ?? continuationTask.status}（${continuationTask.id}）` : '已加入云端队列'} {continuationTaskPending ? <span>等待云端完成后即可打开</span> : <button type="button" className="secondary-button" style={{ marginLeft: 8, fontSize: 12 }} onClick={() => onOpenConversation(continuedConversation.id)}>打开续接会话</button>}</div> : <button type="button" className="secondary-button" style={{ marginTop: 7, fontSize: 12 }} disabled={busyTask === task.id} onClick={() => void continueInCloud(task.id)}>{busyTask === task.id ? '正在提交云端任务…' : hasHandoffCheckpoint ? '从检查点在云端继续' : '在云端继续此任务'}</button>}</div> : <button type="button" className="secondary-button" style={{ marginTop: 7, fontSize: 12 }} disabled={busyTask === task.id} onClick={() => void importProjectDelta(task.id)}>{busyTask === task.id ? '正在校验并导入…' : '导入为云端项目分支'}</button>)}</div>}{!projectDeltaId && transcriptContinuationReady && <div style={{ marginTop: 8, fontSize: 12 }}>{continuedConversation ? <div style={{ color: '#16835d' }}>云端会话已读回：{continuedConversation.title}（{continuedConversation.id}）{continuationTask && ` · ${labels[continuationTask.status] ?? continuationTask.status}（${continuationTask.id}）`} {continuationTaskPending ? <span>等待云端完成后即可打开</span> : <button type="button" className="secondary-button" style={{ marginLeft: 8, fontSize: 12 }} onClick={() => onOpenConversation(continuedConversation.id)}>打开续接会话</button>}</div> : <button type="button" className="secondary-button" disabled={busyTask === task.id} onClick={() => void continueInCloud(task.id)}>{busyTask === task.id ? '正在提交云端任务…' : hasHandoffCheckpoint ? '从检查点在云端继续' : '创建云端续接会话'}</button>}</div>}</div>
          {task.status === 'needs_reconciliation' && <TaskRecoveryEvidence taskId={task.id} />}
          {task.status === 'needs_reconciliation' && recoveryHasProjectDelta && !importedProject && <button type="button" className="secondary-button" style={{ marginLeft: 20, fontSize: 12 }} disabled={busyTask === task.id} onClick={() => void importProjectDelta(task.id, true)}>{busyTask === task.id ? '正在校验并导入…' : '确认原节点停止并导入项目分支'}</button>}
          {task.status === 'needs_reconciliation' && recoveryCanContinue && <button type="button" className="secondary-button" style={{ marginLeft: 20, fontSize: 12 }} disabled={busyTask === task.id} onClick={() => void continueInCloud(task.id, undefined, true)}>{busyTask === task.id ? '正在提交云端续接…' : '核查后在云端建立独立续接段'}</button>}
          {task.status === 'needs_reconciliation' && task.recoveryResult && !recoveryCanContinue && (!transcriptAvailable || !continuableAgentStatus || recoveryHasProjectDelta && !importedProject) && <div style={{ width: '100%', marginLeft: 20, color: '#8a5c11', fontSize: 12 }}>恢复记录尚无完整安全 transcript，或项目增量还未导入；核对完成前原任务保持在核查状态。</div>}
          {!projectDeltaId && importedProject && <div style={{ width: '100%', paddingLeft: 20, color: '#16835d', fontSize: 12 }}>云端任务工作区已读回：{importedProject.name} · 分支 {importedProject.remoteBranch} · 基线 {importedProject.resolvedCommit?.slice(0, 10)}</div>}
          {task.handoffRequested && !terminal.has(task.status) && <span role="status" style={{ fontSize: 12, color: '#8a5c11' }}>等待安全边界暂停{task.status === 'running' ? '，当前工具操作结束后生效' : '，节点收到后生效'}</span>}
          {['leased', 'accepted', 'running'].includes(task.status) && !task.handoffRequested && !task.nodeId.startsWith('node_cloud_') && <button type="button" className="secondary-button" style={{ fontSize: 12 }} disabled={busyTask === task.id} onClick={() => void handoff(task.id)}>{busyTask === task.id ? '提交中…' : '安全暂停并准备云端续接'}</button>}
          {!terminal.has(task.status) && <button type="button" className="icon-button" aria-label={`取消任务 ${task.id}`} title="请求取消" disabled={busyTask === task.id} onClick={() => void cancel(task.id)}><Square size={13} /></button>}
        </li>;
        })}
      </ul>}
    </section>
    {handoffReview && <HandoffReviewDialog effects={handoffReview.effects} busy={busyTask === handoffReview.taskId} onClose={() => setHandoffReview(null)} onConfirm={(resolutions) => void continueInCloud(handoffReview.taskId, resolutions, handoffReview.recovery)} />}
  </div>;
}

function TaskRecoveryEvidence({ taskId }: { taskId: string }) {
  const [artifacts, setArtifacts] = useState<RecoveryEvidenceArtifact[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  async function load() {
    setBusy(true);
    try {
      const latest = await request<RecoveryEvidenceArtifact[]>(`/tasks/${encodeURIComponent(taskId)}/artifacts`);
      if (!Array.isArray(latest)) throw new Error('恢复证据清单格式无效。');
      const evidenceRoles = new Set(['recovery_transcript', 'recovery_project_delta', 'recovery_checkpoint', 'conversation_transcript', 'project_delta', 'continuation_checkpoint']);
      setArtifacts(latest.filter((item) => evidenceRoles.has(item.role)));
      setError('');
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '恢复证据读取失败');
    } finally {
      setBusy(false);
    }
  }
  return <div style={{ width: '100%', marginLeft: 20 }}>
    <div style={{ marginBottom: 7, padding: 10, borderRadius: 8, background: '#fff5df', color: '#6e4d15', fontSize: 12 }}>云端已收回旧租约。恢复证据只用于核对，不代表原电脑已停止，也不代表外部操作已确认；核对完成前不会自动重放任务。</div>
    <button type="button" className="secondary-button" disabled={busy} onClick={() => void load()}>{busy ? '正在读取恢复证据…' : '读取恢复证据'}</button>
    {error && <div role="alert" style={{ marginTop: 5, color: '#a22828', fontSize: 12 }}>{error}</div>}
    {artifacts.map((item) => <div key={`${item.id}:${item.role}`} style={{ marginTop: 5, fontSize: 12 }}><a href={`${API}/artifacts/${encodeURIComponent(item.id)}`} download={item.fileName}>下载 {item.role.replace('recovery_', '恢复：')}（{item.byteSize.toLocaleString()} 字节）</a></div>)}
  </div>;
}

function HandoffReviewDialog({ effects, busy, onClose, onConfirm }: { effects: HandoffExternalEffect[]; busy: boolean; onClose: () => void; onConfirm: (resolutions: HandoffEffectResolution[]) => void }) {
  const ref = useDialogA11y(onClose);
  const [outcomes, setOutcomes] = useState<Record<string, HandoffEffectResolution['outcome']>>({});
  const complete = effects.every((effect) => outcomes[effect.toolCallId]);
  function submit() {
    if (!complete) return;
    onConfirm(effects.map((effect) => ({ toolCallId: effect.toolCallId, outcome: outcomes[effect.toolCallId] })));
  }
  return <div className="modal-backdrop" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
    <section ref={ref} role="dialog" aria-modal="true" aria-labelledby="handoff-review-title" tabIndex={-1} style={{ width: 'min(620px, calc(100vw - 28px))', maxHeight: 'min(680px, calc(100dvh - 32px))', overflow: 'auto', background: 'var(--panel, #fffefa)', color: 'var(--text, #25251f)', border: '1px solid var(--border, #deddd5)', borderRadius: 16, padding: 20, boxShadow: '0 18px 60px #0003' }}>
      <h2 id="handoff-review-title" style={{ marginTop: 0 }}>核对本地任务的操作记录</h2>
      <p>这项任务包含非只读工具调用。云端会继承完整会话，但不会迁移本地进程或登录状态。请检查已发生的操作，避免在云端重复提交或写入。</p>
      <ul style={{ maxHeight: 300, overflow: 'auto', paddingLeft: 22 }}>
        {effects.map((effect) => <li key={effect.toolCallId} style={{ marginBottom: 10 }}>
          <strong>{effect.tool}</strong> · {effect.effect} · {effect.state === 'completed' ? '工具已完成' : effect.state === 'reported_error' ? '工具报告错误，可能有部分影响' : '结果未知，需要先核查'}
          <div style={{ marginTop: 5 }}>
            <label htmlFor={`handoff-effect-${effect.toolCallId}`} style={{ marginRight: 7 }}>实际结果</label>
            <select id={`handoff-effect-${effect.toolCallId}`} value={outcomes[effect.toolCallId] ?? ''} disabled={busy} onChange={(event) => setOutcomes((current) => ({ ...current, [effect.toolCallId]: event.target.value as HandoffEffectResolution['outcome'] }))}>
              <option value="" disabled>选择核查结果</option>
              <option value="confirmed_applied">已确认生效</option>
              <option value="confirmed_not_applied">已确认未生效</option>
              <option value="unknown">仍未知，云端先检查</option>
            </select>
          </div>
        </li>)}
      </ul>
      <p style={{ fontSize: 12, color: 'var(--muted, #77776e)' }}>每项结果会随云端任务持久保存；未知项会要求云端先检查当前状态。操作名和初始状态来自校验过的本地 trace，参数及工具输出不会放进摘要。</p>
      <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
        <button type="button" className="secondary-button" onClick={onClose}>返回</button>
        <button type="button" className="secondary-button" disabled={busy || !complete} onClick={submit}>{busy ? '正在创建云端续接…' : '提交核查并继续到云端'}</button>
      </div>
    </section>
  </div>;
}
