'use client';

import { FormEvent, useState } from 'react';
import {
  X,
  Folder,
  FolderGit2,
  FolderOpen,
  GitBranch,
  Download,
  Trash2,
  Sparkles,
  Check,
  AlertCircle,
  RefreshCw,
  Upload,
} from 'lucide-react';
import {
  Project,
  ProjectPublication,
  GitPublicationPreview,
  createProject,
  updateProject,
  deleteProject,
  selectNativeDirectory,
  cloneProjectGit,
  previewProjectPublication,
  createProjectPublication,
  reconcileProjectPublication,
  getProjectPublications,
} from './api';
import { useDialogA11y } from './useDialogA11y';

interface ProjectModalProps {
  project?: Project | null;
  onClose: () => void;
  onSaved: (project: Project) => void;
  onDeleted?: (id: string) => void;
}

export default function ProjectModal({
  project,
  onClose,
  onSaved,
  onDeleted,
}: ProjectModalProps) {
  const dialogRef = useDialogA11y(onClose);
  const isEditing = Boolean(project);

  const [name, setName] = useState(project?.name ?? '');
  const [instructions, setInstructions] = useState(project?.instructions ?? '');
  const [instructionsEnabled, setInstructionsEnabled] = useState(
    project?.instructionsEnabled ?? false,
  );
  const [workdir, setWorkdir] = useState(project?.workdir ?? '');
  const [remoteRepoUrl, setRemoteRepoUrl] = useState(project?.remoteRepoUrl ?? '');
  const [remoteBranch, setRemoteBranch] = useState(project?.remoteBranch ?? '');
  const [deleteConfirmationOpen, setDeleteConfirmationOpen] = useState(false);
  const [deleteConsent, setDeleteConsent] = useState(false);

  const [busy, setBusy] = useState(false);
  const [pickingDir, setPickingDir] = useState(false);
  const [cloningGit, setCloningGit] = useState(false);
  const [projectNotice, setProjectNotice] = useState<{ type: 'ok' | 'err'; message: string } | null>(null);
  const [publicationPreview, setPublicationPreview] = useState<GitPublicationPreview | null>(null);
  const [publications, setPublications] = useState<ProjectPublication[]>([]);
  const [publicationBusy, setPublicationBusy] = useState(false);
  const [publicationError, setPublicationError] = useState('');
  const [error, setError] = useState('');

  async function handlePickDirectory() {
    setPickingDir(true);
    setError('');
    try {
      const selected = await selectNativeDirectory(workdir || undefined);
      if (selected) {
        setWorkdir(selected);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : '打开目录选择器失败');
    } finally {
      setPickingDir(false);
    }
  }

  async function handleCloneRepo() {
    const url = remoteRepoUrl.trim();
    if (!url) {
      setError('请先输入远程 Git 仓库 URL');
      return;
    }
    setCloningGit(true);
    setError('');
    setProjectNotice(null);
    try {
      const res = await cloneProjectGit({
        projectId: project?.id,
        repoUrl: url,
        targetDir: workdir.trim() || undefined,
        branch: remoteBranch.trim() || undefined,
      });
      setWorkdir(res.targetDir);
      const sizeNotice = res.measurementStatus === 'measured'
        ? `；工作副本约 ${(res.measuredBytes / 1_000_000).toFixed(1)} MB`
        : '；工作副本大小暂时无法准确测量';
      const transferNotice = res.recommendedTransferMode === 'incremental_or_artifact_link'
        ? '；建议后续云端同步采用增量、分块续传或成果链接，本地任务不受影响'
        : '';
      setProjectNotice({ type: 'ok', message: `仓库已检出到 ${res.targetDir}${sizeNotice}${transferNotice}` });
    } catch (err) {
      const msg = err instanceof Error ? err.message : '克隆仓库失败';
      setProjectNotice({ type: 'err', message: msg });
    } finally {
      setCloningGit(false);
    }
  }

  async function handlePreviewPublication() {
    if (!project || project.remoteRepoUrl !== remoteRepoUrl.trim() || project.workdir !== workdir.trim() || project.remoteBranch !== remoteBranch.trim()) {
      setPublicationError('请先保存项目的仓库地址、工作目录和目标分支，再检查发布状态。');
      return;
    }
    setPublicationBusy(true);
    setPublicationError('');
    const [previewResult, historyResult] = await Promise.allSettled([
      previewProjectPublication(project.id, remoteBranch.trim() || undefined),
      getProjectPublications(project.id),
    ]);
    if (previewResult.status === 'fulfilled') setPublicationPreview(previewResult.value);
    if (historyResult.status === 'fulfilled') setPublications(historyResult.value);
    const failures = [previewResult, historyResult].filter((result): result is PromiseRejectedResult => result.status === 'rejected');
    if (failures.length) setPublicationError(failures.map((failure) => failure.reason instanceof Error ? failure.reason.message : '读取 Git 发布状态失败').join('；'));
    setPublicationBusy(false);
  }

  async function handlePublishCommit() {
    if (!project || !publicationPreview || !publicationPreview.worktreeClean || !publicationPreview.remoteSha) return;
    const childSummary = publicationPreview.submodules.length
      ? `\n\n同时会在 ${publicationPreview.submodules.length} 个子仓库创建提交地址 ref：\n${publicationPreview.submodules.slice(0, 3).map((item) => `• ${item.path} · ${item.repositoryUrl} · ${item.commitSha.slice(0, 12)}`).join('\n')}${publicationPreview.submodules.length > 3 ? '\n其余目标请查看发布预览中的完整清单。' : ''}`
      : '';
    const confirmed = window.confirm(`将当前提交 ${publicationPreview.commitSha.slice(0, 12)} 推送到 ${publicationPreview.targetBranch}？\n\n目标远端当前版本：${publicationPreview.remoteSha.slice(0, 12)}${childSummary}\n\n只会执行普通快进推送；不会创建提交或覆盖已有远端历史。父仓库或子仓库的 push 可能触发仓库 CI 自动化。`);
    if (!confirmed) return;
    setPublicationBusy(true);
    setPublicationError('');
    try {
      const key = `project-publish-${crypto.randomUUID()}`;
      const result = await createProjectPublication(project.id, {
        targetBranch: publicationPreview.targetBranch,
        expectedRemoteSha: publicationPreview.remoteSha,
        commitSha: publicationPreview.commitSha,
      }, key);
      setPublications((items) => [result, ...items.filter((item) => item.id !== result.id)]);
      if (result.status === 'published') setProjectNotice({ type: 'ok', message: `远端已核实发布到 ${result.targetBranch}，SHA ${result.remoteSha}` });
      else if (result.status === 'needs_reconciliation') setPublicationError(result.error || '推送结果不确定，请核查远端状态后再继续。');
      else if (result.status === 'failed') setPublicationError(result.error || '发布失败；远端没有读回目标提交。');
      const [historyResult, previewResult] = await Promise.allSettled([
        getProjectPublications(project.id),
        previewProjectPublication(project.id, remoteBranch.trim() || undefined),
      ]);
      if (historyResult.status === 'fulfilled') setPublications(historyResult.value);
      else setPublicationError((current) => [current, '发布状态已读回，但刷新发布历史失败。'].filter(Boolean).join(' '));
      if (previewResult.status === 'fulfilled') setPublicationPreview(previewResult.value);
      else setPublicationError((current) => [current, '发布状态已读回，但刷新仓库预览失败。'].filter(Boolean).join(' '));
    } catch (err) {
      const message = err instanceof Error ? err.message : 'Git 发布请求失败';
      try {
        setPublications(await getProjectPublications(project.id));
        setPublicationError(message);
      } catch (readErr) {
        const readMessage = readErr instanceof Error ? readErr.message : '发布记录读取失败';
        setPublicationError(`${message}；同时无法读取云端发布记录：${readMessage}`);
      }
    } finally {
      setPublicationBusy(false);
    }
  }

  async function handleReconcilePublication(publicationId: string) {
    if (!project) return;
    setPublicationBusy(true);
    setPublicationError('');
    try {
      const result = await reconcileProjectPublication(project.id, publicationId);
      setPublications((items) => [result, ...items.filter((item) => item.id !== result.id)]);
      if (result.status === 'publishing') setPublicationError('该发布请求仍在执行中；已返回最新持久状态，本次没有并发核查远端。');
      else if (result.status === 'needs_reconciliation') setPublicationError(result.error || '远端状态仍无法判定，请稍后重新核查。');
      else if (result.status === 'published') setProjectNotice({ type: 'ok', message: `远端已核实发布到 ${result.targetBranch}，SHA ${result.remoteSha}` });
      else if (result.status === 'failed') setPublicationError(result.error || '核查完成：远端仍处于发布前版本。');
      try {
        setPublications(await getProjectPublications(project.id));
      } catch (readErr) {
        const readMessage = readErr instanceof Error ? readErr.message : '发布记录读取失败';
        setPublicationError((current) => [current, `核查状态已读回，但刷新发布历史失败：${readMessage}`].filter(Boolean).join(' '));
      }
    } catch (err) {
      setPublicationError(err instanceof Error ? err.message : '发布核查失败');
    } finally {
      setPublicationBusy(false);
    }
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const trimmedName = name.trim();
    if (!trimmedName) {
      setError('请输入项目名称');
      return;
    }

    setBusy(true);
    setError('');

    try {
      const payload = {
        name: trimmedName,
        instructions: instructions.trim(),
        instructionsEnabled,
        workdir: workdir.trim(),
        remoteRepoUrl: remoteRepoUrl.trim(),
        remoteBranch: remoteBranch.trim(),
      };

      if (isEditing && project) {
        const updated = await updateProject(project.id, payload);
        onSaved(updated);
      } else {
        const created = await createProject(payload);
        onSaved(created);
      }
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : '保存项目失败');
    } finally {
      setBusy(false);
    }
  }

  async function handleDelete() {
    if (!project) return;
    if (!deleteConfirmationOpen) {
      setDeleteConfirmationOpen(true);
      setDeleteConsent(false);
      return;
    }
    if (!deleteConsent) {
      return;
    }

    setBusy(true);
    setError('');
    try {
      await deleteProject(project.id);
      onDeleted?.(project.id);
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : '删除项目失败');
      setBusy(false);
    }
  }

  return (
    <div
      className="upc-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <section
        ref={dialogRef}
        className="upc-modal project-modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="project-dialog-title"
        tabIndex={-1}
        style={{ maxWidth: 580 }}
      >
        <header className="upc-header">
          <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
            <div
              className="brand-mark"
              style={{
                width: 32,
                height: 32,
                borderRadius: 8,
                display: 'inline-flex',
                alignItems: 'center',
                justifyContent: 'center',
                background: '#18181b',
                color: '#ffffff',
              }}
            >
              <FolderGit2 size={16} />
            </div>
            <div>
              <div className="eyebrow" style={{ fontSize: 10.5, letterSpacing: '0.06em' }}>
                PROJECT WORKSPACE
              </div>
              <h2 id="project-dialog-title" className="upc-title" style={{ fontSize: 16 }}>
                {isEditing ? '项目设置' : '新建项目'}
              </h2>
            </div>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="upc-btn-close"
            aria-label="关闭项目弹窗"
          >
            <X size={14} strokeWidth={2} aria-hidden="true" />
          </button>
        </header>

        <form onSubmit={handleSubmit} className="project-form">
          {error && (
            <div className="upc-notice error" role="alert">
              <span>{error}</span>
              <button
                type="button"
                onClick={() => setError('')}
                aria-label="清除错误"
                style={{ background: 'none', border: 'none', cursor: 'pointer', color: 'inherit' }}
              >
                <X size={13} />
              </button>
            </div>
          )}

          {/* Section 1: Basic Info */}
          <div className="project-card-section">
            <div className="project-field">
              <label htmlFor="proj-name">
                项目名称 <span style={{ color: '#ef4444' }}>*</span>
              </label>
              <input
                id="proj-name"
                type="text"
                className="settings-input"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="例如：电商核心重构、API 网关服务、代码审查助理"
                autoFocus
                required
              />
            </div>
          </div>

          {/* Section 2: Shared Context & Instructions */}
          <div className="project-card-section">
            <div className="project-card-header">
              <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                <Sparkles size={14} className="text-zinc-600" />
                <span className="project-card-title">共享设定与全局规范 (Project Context)</span>
              </div>
              <button
                type="button"
                role="switch"
                aria-checked={instructionsEnabled}
                onClick={() => setInstructionsEnabled(!instructionsEnabled)}
                className={`project-switch ${instructionsEnabled ? 'active' : ''}`}
                title={instructionsEnabled ? '已开启共享设定' : '已关闭共享设定'}
              >
                <span className="project-switch-handle" />
              </button>
            </div>

            <div className="project-card-desc">
              {instructionsEnabled
                ? '已开启共享设定：该项目下的对话在执行时，会自动将以下全局设定注入为系统提示词。'
                : '可选功能（已关闭）：关闭时对话保持独立纯净，不会携带任何项目全局提示词。'}
            </div>

            {instructionsEnabled ? (
              <div className="project-field" style={{ marginTop: 8 }}>
                <textarea
                  id="proj-instructions"
                  className="settings-input project-textarea"
                  rows={4}
                  value={instructions}
                  onChange={(e) => setInstructions(e.target.value)}
                  placeholder="例如：本项目使用 Go 1.24 + React 19 技术栈；错误输出遵循 RFC 7807；请在编码时编写自动化单元测试。"
                />
              </div>
            ) : (
              <div className="project-instructions-hint">
                💡 需要统一代码风格、技术选型或架构约定？开启上方开关即可为项目配置专属规范。
              </div>
            )}
          </div>

          {/* Section 3: Workspace Directory & Remote Repo */}
          <div className="project-card-section">
            <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 8 }}>
              <Folder size={14} className="text-zinc-600" />
              <span className="project-card-title">工作空间与远程仓库</span>
            </div>

            {/* Workdir */}
            <div className="project-field">
              <label htmlFor="proj-workdir">本地工作区目录 (Workspace Directory)</label>
              <div className="project-input-group">
                <input
                  id="proj-workdir"
                  type="text"
                  className="settings-input"
                  value={workdir}
                  onChange={(e) => setWorkdir(e.target.value)}
                  placeholder="例如：D:\workspace\ecommerce 或 ./work/project"
                />
                <button
                  type="button"
                  onClick={handlePickDirectory}
                  disabled={pickingDir}
                  className="upc-btn-secondary"
                  style={{ height: 32, flexShrink: 0, padding: '0 10px', fontSize: 12 }}
                  title="调用系统资源管理器选择文件夹"
                >
                  <FolderOpen size={14} />
                  <span>{pickingDir ? '选择中…' : '选择目录'}</span>
                </button>
              </div>
              <span className="project-field-hint">
                Agent 在此项目对话中调用文件与命令行工具时，默认定位在此根目录中。
              </span>
            </div>

            {/* Remote Git Repo */}
            <div className="project-field" style={{ marginTop: 10 }}>
              <label htmlFor="proj-repo">远程 Git 仓库 (Remote Repository)</label>
              <div className="project-input-group">
                <div style={{ position: 'relative', flex: 1 }}>
                  <input
                    id="proj-repo"
                    type="text"
                    className="settings-input"
                    value={remoteRepoUrl}
                    onChange={(e) => setRemoteRepoUrl(e.target.value)}
                    placeholder="https://github.com/owner/repository.git"
                    style={{ paddingLeft: 26 }}
                  />
                  <GitBranch
                    size={13}
                    style={{
                      position: 'absolute',
                      left: 8,
                      top: '50%',
                      transform: 'translateY(-50%)',
                      color: '#71717a',
                      pointerEvents: 'none',
                    }}
                  />
                </div>
                <input
                  type="text"
                  className="settings-input"
                  value={remoteBranch}
                  onChange={(e) => setRemoteBranch(e.target.value)}
                  placeholder="分支 (默认 main)"
                  style={{ width: 90, flexShrink: 0 }}
                  title="指定分支"
                />
                <button
                  type="button"
                  onClick={handleCloneRepo}
                  disabled={cloningGit || !remoteRepoUrl.trim()}
                  className="upc-btn-secondary"
                  style={{ height: 32, flexShrink: 0, padding: '0 10px', fontSize: 12 }}
                  title="克隆远程仓库到本地工作区"
                >
                  <Download size={13} />
                  <span>{cloningGit ? '克隆中…' : '克隆'}</span>
                </button>
              </div>

              {projectNotice && (
                <div
                  className={`project-notice ${projectNotice.type === 'ok' ? 'success' : 'error'}`}
                  style={{ marginTop: 6 }}
                >
                  {projectNotice.type === 'ok' ? <Check size={13} /> : <AlertCircle size={13} />}
                  <span>{projectNotice.message}</span>
                </div>
              )}
              {isEditing && (
                <div style={{ marginTop: 12, borderTop: '1px solid var(--border, #deddd5)', paddingTop: 12 }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
                    <strong style={{ fontSize: 13 }}>发布已提交的 Git commit</strong>
                    <button type="button" className="upc-btn-secondary" onClick={() => void handlePreviewPublication()} disabled={publicationBusy || !remoteRepoUrl.trim() || !workdir.trim()}>
                      <RefreshCw size={13} />
                      <span>{publicationBusy ? '检查中…' : '检查当前提交'}</span>
                    </button>
                  </div>
                  <p className="project-field-hint" style={{ margin: '6px 0 8px' }}>只发布工作区中已经创建的 commit；不会替你自动提交文件。子仓库提交 ref 会在预览中逐项列出。脏工作区和远端发生变化时会阻止推送。</p>
                  {publicationError && <div className="project-notice error" role="alert" style={{ marginBottom: 8 }}><AlertCircle size={13} /><span>{publicationError}</span></div>}
                  {publicationPreview && (
                    <div style={{ border: '1px solid var(--border, #deddd5)', borderRadius: 9, padding: 10, fontSize: 12, overflowWrap: 'anywhere' }}>
                      <div>目标分支：<strong>{publicationPreview.targetBranch}</strong> · 当前分支：{publicationPreview.currentBranch || 'detached HEAD'}</div>
                      <div style={{ marginTop: 4 }}>本地 HEAD：<code>{publicationPreview.commitSha}</code></div>
                      <div style={{ marginTop: 4 }}>远端基线：<code>{publicationPreview.remoteSha || '分支不存在'}</code></div>
                      <div style={{ marginTop: 4 }}>工作区：{publicationPreview.worktreeClean ? '干净' : '有未提交或未跟踪文件'}</div>
                      {publicationPreview.submodules.length > 0 && (
                        <div style={{ marginTop: 8, borderTop: '1px solid var(--border, #deddd5)', paddingTop: 7 }}>
                          <div style={{ fontWeight: 600 }}>本次还会发布以下子仓库提交 ref（不会自动创建子仓库 commit）</div>
                          <div role="region" aria-label="子仓库提交 ref 发布清单" tabIndex={0} style={{ maxHeight: 132, overflowY: 'auto', marginTop: 4, paddingRight: 4 }}>
                            {publicationPreview.submodules.map((item) => (
                              <div key={`${item.path}:${item.ref}`} style={{ padding: '4px 0', borderTop: '1px solid var(--border, #deddd5)' }}>
                                <div>{item.path} · <code>{item.commitSha.slice(0, 12)}</code></div>
                                <div style={{ color: 'var(--muted, #77776e)', overflowWrap: 'anywhere' }}>{item.repositoryUrl}</div>
                                <div style={{ color: 'var(--muted, #77776e)', overflowWrap: 'anywhere' }}>目标 ref：<code>{item.ref}</code></div>
                              </div>
                            ))}
                          </div>
                        </div>
                      )}
                      {publicationPreview.remoteSha ? (
                        <button type="button" className="upc-btn-primary" style={{ marginTop: 9 }} onClick={() => void handlePublishCommit()} disabled={publicationBusy || !publicationPreview.worktreeClean || publicationPreview.commitSha === publicationPreview.remoteSha}>
                          <Upload size={13} />
                          <span>{publicationBusy ? '发布中…' : publicationPreview.commitSha === publicationPreview.remoteSha ? '远端已是此版本' : '确认推送此 commit'}</span>
                        </button>
                      ) : <div style={{ marginTop: 7, color: 'var(--muted, #77776e)' }}>当前 API 只允许更新已有父仓库目标分支，不会自动创建父仓库分支。</div>}
                    </div>
                  )}
                  {publications.length > 0 && (
                    <div style={{ marginTop: 9 }}>
                      <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 4 }}>发布记录</div>
                      {publications.slice(0, 5).map((item) => (
                        <div key={item.id} style={{ padding: '5px 0', fontSize: 11, borderTop: '1px solid var(--border, #deddd5)' }}>
                          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                            <span style={{ flex: 1, overflowWrap: 'anywhere' }}>{item.targetBranch} · {item.status} · {item.commitSha.slice(0, 12)}{item.error ? ` · ${item.error}` : ''}</span>
                            {(item.status === 'publishing' || item.status === 'needs_reconciliation') && <button type="button" className="upc-btn-secondary" disabled={publicationBusy} onClick={() => void handleReconcilePublication(item.id)}>核查</button>}
                          </div>
                          {item.submodules && item.submodules.length > 0 && <div style={{ marginTop: 3, color: 'var(--muted, #77776e)', overflowWrap: 'anywhere' }}>{item.submodules.map((child) => `${child.path}: ${child.status}${child.remoteSha ? ` (${child.remoteSha.slice(0, 12)})` : ''}`).join(' · ')}</div>}
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              )}
            </div>
          </div>

          <footer className="project-footer">
            {isEditing ? (
              <div>
                <button
                  type="button"
                  className="project-btn-danger"
                  onClick={handleDelete}
                  disabled={busy}
                  title="移除项目设置"
                >
                  <Trash2 size={13} />
                  <span>移除项目</span>
                </button>
                {deleteConfirmationOpen && (
                  <div className="project-delete-confirmation">
                    <p>这会移除项目设置并将关联对话移回普通列表，不会删除工作区文件夹或对话内容。</p>
                    <label className="permission-consent">
                      <input type="checkbox" checked={deleteConsent} onChange={(event) => setDeleteConsent(event.target.checked)} />
                      我确认移除此项目设置。
                    </label>
                    <button type="button" className="project-btn-danger" onClick={handleDelete} disabled={busy || !deleteConsent}>确认移除</button>
                    <button type="button" className="upc-btn-secondary" onClick={() => setDeleteConfirmationOpen(false)} disabled={busy}>取消</button>
                  </div>
                )}
              </div>
            ) : (
              <div />
            )}
            <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
              <button
                type="button"
                className="upc-btn-secondary"
                onClick={onClose}
                disabled={busy}
              >
                取消
              </button>
              <button
                type="submit"
                className="upc-btn-primary"
                disabled={busy || !name.trim()}
              >
                {busy ? '正在保存…' : isEditing ? '保存设置' : '创建项目'}
              </button>
            </div>
          </footer>
        </form>
      </section>
    </div>
  );
}
