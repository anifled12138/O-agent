'use client';

import { FormEvent, useCallback, useEffect, useRef, useState } from 'react';
import { X, Plus, ChevronRight, ChevronLeft, ArrowRight } from 'lucide-react';
import { API, ASSET_ORIGIN, request } from './api';
import { useDialogA11y } from './useDialogA11y';

type Capability = { id: string; summary: string; risk: string; pluginId?: string; releaseId?: string; version?: string };
type PermissionSet = { filesystem?: { read?: string[]; write?: string[] }; network?: string[]; secrets?: string[]; process?: boolean; background?: boolean };
type Release = { id: string; projectId: string; pluginId: string; version: string; digest: string; permissionHash: string; sourceVersion: 'v1' | 'v2'; manifest: { name: string; description: string; permissions: PermissionSet; ui?: { entry: string; slots?: string[] }; exports?: { tools?: Capability[]; services?: unknown[]; skills?: unknown[] } } };
export type ForgeProject = { id: string; name: string; slug: string; description: string; state: string; lastError?: string; updatedAt: string; latestRelease?: Release; releases: Release[] };
export type Installation = { id: string; pluginId: string; projectId: string; activeReleaseId: string; status: string };
export type SurfaceState = { pluginId: string; releaseId: string; kind: string; surfaceId: string; status: string; registryEpoch: number };

export default function ForgeModal({ onClose, onOpenUnified }: { onClose: () => void; onOpenUnified?: () => void }) {
  const dialogRef = useDialogA11y(onClose);
  const [projects, setProjects] = useState<ForgeProject[]>([]);
  const [installations, setInstallations] = useState<Installation[]>([]);
  const [surfaces, setSurfaces] = useState<SurfaceState[]>([]);
  const [selectedId, setSelectedId] = useState<string>('');
  const [busy, setBusy] = useState<string>('');
  const [error, setError] = useState('');
  const iframeRef = useRef<HTMLIFrameElement>(null);

  const refresh = useCallback(async () => {
    const [nextProjects, nextInstallations, nextSurfaces] = await Promise.all([
      request<ForgeProject[]>('/plugin-forge/projects'),
      request<Installation[]>('/plugin-runtime/installations'),
      request<SurfaceState[]>('/plugin-runtime/surfaces'),
    ]);
    setProjects(nextProjects);
    setInstallations(nextInstallations);
    setSurfaces(nextSurfaces);
    setSelectedId((current) => current || nextProjects[0]?.id || '');
  }, []);

  useEffect(() => {
    void Promise.all([
      request<ForgeProject[]>('/plugin-forge/projects'),
      request<Installation[]>('/plugin-runtime/installations'),
      request<SurfaceState[]>('/plugin-runtime/surfaces'),
    ])
      .then(([nextProjects, nextInstallations, nextSurfaces]) => {
        setProjects(nextProjects);
        setInstallations(nextInstallations);
        setSurfaces(nextSurfaces);
        setSelectedId(nextProjects[0]?.id || '');
      })
      .catch((reason) => setError(reason instanceof Error ? reason.message : '无法加载插件'));
  }, []);

  const selected = projects.find((project) => project.id === selectedId) ?? projects[0];
  const installation = installations.find((item) => item.projectId === selected?.id && item.status === 'active');

  useEffect(() => {
    async function bridge(event: MessageEvent) {
      if (
        event.source !== iframeRef.current?.contentWindow ||
        (event.origin !== 'null' && event.origin !== ASSET_ORIGIN) ||
        !selected?.latestRelease
      )
        return;
      const data = event.data as {
        type?: string;
        id?: string;
        operation?: string;
        serviceId?: string;
        capabilitySuffix?: string;
        input?: unknown;
      };
      const legacy = data.type === 'axiom.plugin.invoke';
      if (!data.id || (legacy ? !data.capabilitySuffix : data.type !== 'axiom.ui.call' || (!data.operation && !data.serviceId)))
        return;
      try {
        const endpoint = legacy
          ? `/plugin-runtime/ui/${encodeURIComponent(selected.latestRelease.pluginId)}/legacy-invoke`
          : data.serviceId
          ? `/plugin-runtime/ui/${encodeURIComponent(selected.latestRelease.pluginId)}/services/${encodeURIComponent(data.serviceId)}/call`
          : `/plugin-runtime/ui/${encodeURIComponent(selected.latestRelease.pluginId)}/call`;
        const body = legacy
          ? { capabilitySuffix: data.capabilitySuffix, input: data.input ?? {} }
          : data.serviceId
          ? { input: data.input ?? {} }
          : { operation: data.operation, input: data.input ?? {} };
        const result = await request<{ output: unknown }>(endpoint, { method: 'POST', body: JSON.stringify(body) });
        iframeRef.current?.contentWindow?.postMessage(
          { type: legacy ? 'axiom.plugin.result' : 'axiom.ui.result', id: data.id, output: result.output },
          '*'
        );
      } catch (reason) {
        iframeRef.current?.contentWindow?.postMessage(
          { type: legacy ? 'axiom.plugin.result' : 'axiom.ui.result', id: data.id, error: reason instanceof Error ? reason.message : '界面调用失败' },
          '*'
        );
      }
    }
    window.addEventListener('message', bridge);
    return () => window.removeEventListener('message', bridge);
  }, [selected]);

  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy('create');
    setError('');
    const form = new FormData(event.currentTarget);
    try {
      const project = await request<ForgeProject>('/plugin-forge/projects', {
        method: 'POST',
        body: JSON.stringify(Object.fromEntries(form)),
      });
      setProjects((items) => [project, ...items]);
      setSelectedId(project.id);
      event.currentTarget.reset();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '无法创建插件');
    } finally {
      setBusy('');
    }
  }

  async function act(project: ForgeProject, action: string) {
    setBusy(`${project.id}:${action}`);
    setError('');
    try {
      await request(`/plugin-forge/projects/${project.id}/${action}`, { method: 'POST' });
      await refresh();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '插件操作失败');
      await refresh().catch(() => undefined);
    } finally {
      setBusy('');
    }
  }

  async function rollback(project: ForgeProject, releaseId: string) {
    setBusy(`${project.id}:rollback`);
    setError('');
    try {
      await request(`/plugin-forge/projects/${project.id}/rollback`, {
        method: 'POST',
        body: JSON.stringify({ releaseId }),
      });
      await refresh();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '回退失败');
    } finally {
      setBusy('');
    }
  }

  const nextAction = selected ? forgeAction(selected.state) : null;
  const permissions = selected?.latestRelease?.manifest.permissions;
  const selectedSurfaces = surfaces.filter(
    (surface) => surface.pluginId === selected?.latestRelease?.pluginId && surface.releaseId === installation?.activeReleaseId
  );
  const permissionChanged =
    !!selected?.releases?.[1] && selected.releases[0].permissionHash !== selected.releases[1].permissionHash;

  return (
    <div
      className="modal-backdrop forge-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <section
        ref={dialogRef}
        className="forge-modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="forge-dialog-title"
        tabIndex={-1}
      >
        <header className="forge-header">
          <div>
            <span className="eyebrow">系统 / 插件工坊</span>
            <h2 id="forge-dialog-title">安全地构建新能力</h2>
            <p>每个插件都拥有独立 Git 项目；构建验证通过后，由“安装并启用”一次性确认权限并加载隔离版本。</p>
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            {onOpenUnified && (
              <button
                type="button"
                onClick={onOpenUnified}
                className="secondary"
                style={{ display: 'inline-flex', alignItems: 'center', gap: 4, padding: '6px 12px', fontSize: 12, minHeight: 36 }}
              >
                <ChevronLeft size={15} aria-hidden="true" />
                返回插件与能力
              </button>
            )}
            <button
              type="button"
              onClick={onClose}
              aria-label="关闭插件工坊"
              className="icon-button"
              style={{ minHeight: 36, minWidth: 36, display: 'inline-flex', alignItems: 'center', justifyContent: 'center' }}
            >
              <X size={18} strokeWidth={2} aria-hidden="true" />
            </button>
          </div>
        </header>

        <div className="forge-layout">
          <aside className="forge-rail">
            <form onSubmit={create} className="forge-create">
              <label>
                插件名称
                <input name="name" placeholder="工作区检查器" required />
              </label>
              <label>
                插件形态
                <select name="shape" defaultValue="hybrid">
                  <option value="hybrid">全栈界面 + Agent 工具</option>
                  <option value="agent-tool">Agent 工具</option>
                  <option value="ui">界面扩展</option>
                  <option value="service">后端服务</option>
                  <option value="skill">按需加载的 Skill</option>
                </select>
              </label>
              <label>
                它需要做什么？
                <textarea name="description" placeholder="描述一个具体能力…" required />
              </label>
              <button type="submit" disabled={busy === 'create'}>
                {busy === 'create' ? '正在创建…' : '创建提案'}
                <Plus size={14} aria-hidden="true" />
              </button>
            </form>
            <div className="forge-projects">
              <p className="eyebrow">项目 ({projects.length})</p>
              {projects.length === 0 ? (
                <div className="forge-empty">还没有插件项目。</div>
              ) : (
                projects.map((project) => (
                  <button
                    type="button"
                    key={project.id}
                    className={project.id === selected?.id ? 'active' : ''}
                    onClick={() => setSelectedId(project.id)}
                  >
                    <i className={`state-${project.state}`} />
                    <span>
                      <b>{project.name}</b>
                      <small>{stateLabel(project.state)}</small>
                    </span>
                    <ChevronRight size={14} aria-hidden="true" />
                  </button>
                ))
              )}
            </div>
          </aside>

          <div className="forge-stage">
            {selected ? (
              <>
                <div className="forge-title">
                  <div>
                    <span className="eyebrow">{selected.latestRelease?.pluginId ?? `草稿 / ${selected.slug}`}</span>
                    <h3>{selected.name}</h3>
                    <p>{selected.description}</p>
                  </div>
                  <span className={`forge-state state-${selected.state}`}>{stateLabel(selected.state)}</span>
                </div>
                <div className="forge-pipeline">
				  {['proposed', 'generated', 'tested', 'active'].map((state, index) => (
                    <div key={state} className={pipelineReached(selected.state, state) ? 'reached' : ''}>
                      <span>{index + 1}</span>
                      <b>{stateLabel(state)}</b>
                    </div>
                  ))}
                </div>
                {!!selectedSurfaces.length && (
                  <section className="surface-strip">
                    <span>
                      <b className="eyebrow">已加载能力面</b>
                      <small>
                        版本 {installation?.activeReleaseId.slice(0, 12)} · 注册表{' '}
                        {Math.max(...selectedSurfaces.map((item) => item.registryEpoch))}
                      </small>
                    </span>
                    <div>
                      {selectedSurfaces.map((surface) => (
                        <em
                          className={`surface-${surface.status}`}
                          title={`${surface.surfaceId} · ${surfacePrincipal(surface.kind)}`}
                          key={`${surface.kind}:${surface.surfaceId}`}
                        >
                          {surfaceKindLabel(surface.kind)} · {stateLabel(surface.status)}
                        </em>
                      ))}
                    </div>
                  </section>
                )}
                {selected.lastError && (
                  <div className="forge-error" role="alert">
                    <b>上次运行失败</b>
                    <span>{selected.lastError}</span>
                  </div>
                )}
                {permissions && (
                  <section className="permission-card">
                    <div>
                      <span>
                        <b className="eyebrow">权限契约</b>
                        <small>
                          {selected.latestRelease?.permissionHash.slice(0, 16)} ·{' '}
                          {permissionChanged ? '相较上一版本有变化' : '与当前版本绑定'}
                        </small>
                      </span>
                      <h4>安装将授予以下权限</h4>
                    </div>
                    <div className="permission-grid">
                      <Permission label="读取工作区" enabled={permissions.filesystem?.read?.includes('${workspace}') ?? false} />
                      <Permission label="写入插件数据" enabled={permissions.filesystem?.write?.includes('${pluginData}') ?? false} />
                      <Permission label="后台任务" enabled={permissions.background ?? false} />
                      <Permission
                        label={`网络：${permissions.network?.length ? permissions.network.join(', ') : '禁止'}`}
                        enabled={(permissions.network?.length ?? 0) > 0}
                      />
                      <Permission
                        label={`密钥：${permissions.secrets?.length ? permissions.secrets.join(', ') : '无'}`}
                        enabled={(permissions.secrets?.length ?? 0) > 0}
                      />
                    </div>
                  </section>
                )}
                {selected.state === 'active' && selected.releases?.length > 1 && (
                  <section className="release-history">
                    <span className="eyebrow">不可变版本</span>
                    {selected.releases.map((release) => (
                      <div key={release.id}>
                        <span>
                          <b>
                            {release.version} · {release.sourceVersion}
                          </b>
                          <small>{release.digest.slice(0, 12)}</small>
                        </span>
                        {installation?.activeReleaseId === release.id ? (
                          <em>当前版本</em>
                        ) : (
                          <button type="button" onClick={() => rollback(selected, release.id)} disabled={busy !== ''}>
                            回退
                          </button>
                        )}
                      </div>
                    ))}
                  </section>
                )}
                {installation && selected.latestRelease?.manifest.ui ? (
                  <section className="plugin-preview">
                    <div className="preview-bar">
                      <span>
                        <i /> 实时 · {selected.latestRelease.version}
                      </span>
                      <small>沙箱界面 · 版本固定</small>
                    </div>
                    <iframe
                      ref={iframeRef}
                      title={`${selected.name} 插件`}
                      sandbox="allow-scripts"
                      src={`${API}/plugin-assets/${installation.activeReleaseId}/${selected.latestRelease.manifest.ui.entry.split('/').pop()}`}
                    />
                  </section>
                ) : (
                  <section className="forge-wait">
                    <span>{selected.state === 'proposed' ? '◇' : '◌'}</span>
                    <h4>{forgeGuidance(selected.state).title}</h4>
                    <p>
                      {selected.state === 'active' && !selected.latestRelease?.manifest.ui
                        ? `插件已加载但没有界面。当前能力面：${selectedSurfaces.map((item) => surfaceKindLabel(item.kind)).join('、') || '无'}。`
                        : forgeGuidance(selected.state).body}
                    </p>
                  </section>
                )}
                <div className="forge-actions">
                  <div>
                    <span className="eyebrow">下一步</span>
                    <small>点击安装即确认当前版本声明的权限并启用插件。</small>
                  </div>
                  <div className="forge-action-buttons">
                    {selected.state === 'active' && (
                      <button type="button" className="secondary" onClick={() => act(selected, 'revise')} disabled={busy !== ''}>
                        创建更新
                      </button>
                    )}
                    {nextAction && (
                      <button type="button" onClick={() => act(selected, nextAction.action)} disabled={busy !== ''}>
                        {busy.startsWith(selected.id) ? '处理中…' : nextAction.label}
                        <ArrowRight size={14} aria-hidden="true" />
                      </button>
                    )}
                  </div>
                </div>
              </>
            ) : (
              <div className="forge-wait">
                <span>◇</span>
                <h4>定义第一个能力</h4>
                <p>先创建提案；只有在你明确操作后，系统才会开始生成。</p>
              </div>
            )}
          </div>
        </div>
        {error && <div className="forge-toast" role="alert">{error}</div>}
      </section>
    </div>
  );
}

function Permission({ label, enabled }: { label: string; enabled: boolean }) {
  return (
    <div className={enabled ? 'enabled' : ''}>
      <i>{enabled ? '✓' : '—'}</i>
      <span>{label}</span>
    </div>
  );
}

function surfacePrincipal(kind: string) {
  return kind === 'ui' ? '界面权限域' : kind === 'tool' || kind === 'skill' ? 'Agent 权限域' : '宿主权限域';
}

function surfaceKindLabel(kind: string) {
  return (
    ({
      ui: '界面',
      tool: '工具',
      skill: 'Skill',
      service: '服务',
      hook: '钩子',
      job: '任务',
    } as Record<string, string>)[kind] ?? kind
  );
}

function forgeAction(state: string): { action: string; label: string } | null {
  if (state === 'proposed' || state === 'generation_failed') return { action: 'generate', label: '生成源码' };
  if (state === 'generated' || state === 'build_failed') return { action: 'build', label: '构建并测试' };
  if (state === 'tested' || state === 'awaiting_approval' || state === 'approved' || state === 'installed' || state === 'inactive' || state === 'activation_failed')
    return { action: 'install', label: state === 'inactive' ? '启用插件' : '安装并启用' };
  if (state === 'active') return { action: 'deactivate', label: '停用插件' };
  return null;
}

function pipelineReached(current: string, target: string) {
  const order = ['proposed', 'generating', 'generated', 'building', 'tested', 'installed', 'active'];
  let normalized = current.includes('failed') ? current.replace('_failed', '') : current;
  // Old projects may still contain the removed approval states. Treat them as
  // install-ready rather than exposing the obsolete state machine in the UI.
  if (normalized === 'awaiting_approval' || normalized === 'approved') normalized = 'tested';
  return order.indexOf(normalized) >= order.indexOf(target);
}

function stateLabel(value: string) {
  const labels: Record<string, string> = {
    proposed: '已提案',
    generating: '生成中',
    generated: '源码就绪',
    building: '构建中',
    tested: '已通过测试',
    awaiting_approval: '待安装',
    approved: '待安装',
    installed: '已安装',
    active: '运行中',
    inactive: '已停用',
    activation_failed: '启用失败',
    queued: '排队中',
    running: '运行中',
    completed: '已完成',
    failed: '失败',
    candidate: '候选',
    stable: '稳定',
    superseded: '已替代',
  };
  return labels[value] ?? value.replaceAll('_', ' ');
}

function forgeGuidance(state: string) {
  const copy: Record<string, { title: string; body: string }> = {
    proposed: { title: '提案已准备', body: '生成拥有独立 Git 历史的完整插件源码。' },
    generated: { title: '源码已生成', body: '源码独立于 O 核心；构建和测试将产出不可变版本。' },
    tested: { title: '验证已通过', body: '检查权限契约后，点击“安装并启用”完成授权和加载。' },
    awaiting_approval: { title: '等待安装', body: '检查权限契约后，点击“安装并启用”完成授权和加载。' },
    approved: { title: '等待安装', body: '安装时会启动候选 Sidecar、完成健康检查，再原子加载全部能力。' },
    inactive: { title: '插件已停用', body: '版本仍保留在本地，无需重新构建即可再次启用。' },
  };
  return copy[state] ?? { title: '正在处理', body: 'O 正在保留当前状态和完整审计记录。' };
}
