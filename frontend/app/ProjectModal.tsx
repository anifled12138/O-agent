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
} from 'lucide-react';
import {
  Project,
  createProject,
  updateProject,
  deleteProject,
  selectNativeDirectory,
  cloneProjectGit,
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
  const [remoteBranch, setRemoteBranch] = useState(project?.remoteBranch ?? 'main');

  const [busy, setBusy] = useState(false);
  const [pickingDir, setPickingDir] = useState(false);
  const [cloningGit, setCloningGit] = useState(false);
  const [cloneNotice, setCloneNotice] = useState<{ type: 'ok' | 'err'; message: string } | null>(null);
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
    setCloneNotice(null);
    try {
      const res = await cloneProjectGit({
        projectId: project?.id,
        repoUrl: url,
        targetDir: workdir.trim() || undefined,
        branch: remoteBranch.trim() || undefined,
      });
      if (res.ok) {
        setWorkdir(res.targetDir);
        setCloneNotice({ type: 'ok', message: `仓库克隆成功: ${res.targetDir}` });
      }
    } catch (err) {
      const msg = err instanceof Error ? err.message : '克隆仓库失败';
      setCloneNotice({ type: 'err', message: msg });
    } finally {
      setCloningGit(false);
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
    if (
      !window.confirm(
        `确定要删除项目文件夹“${project.name}”吗？\n删除后该项目下的对话将移至普通对话列表，不会被删除。`,
      )
    ) {
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

              {cloneNotice && (
                <div
                  className={`project-notice ${cloneNotice.type === 'ok' ? 'success' : 'error'}`}
                  style={{ marginTop: 6 }}
                >
                  {cloneNotice.type === 'ok' ? <Check size={13} /> : <AlertCircle size={13} />}
                  <span>{cloneNotice.message}</span>
                </div>
              )}
            </div>
          </div>

          <footer className="project-footer">
            {isEditing ? (
              <button
                type="button"
                className="project-btn-danger"
                onClick={handleDelete}
                disabled={busy}
                title="删除此项目文件夹"
              >
                <Trash2 size={13} />
                <span>删除项目</span>
              </button>
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
