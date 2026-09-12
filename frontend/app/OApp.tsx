'use client';

import { ChangeEvent, FormEvent, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import EvolutionCenter from './EvolutionCenter';
import { API, API_V2, ASSET_ORIGIN, request } from './api';

export type Provider = { id: string; name: string; kind: string; baseUrl: string; model: string; hasApiKey: boolean };
type ProviderKind = { kind: string; label: string; description: string; defaultBaseUrl: string };
type Conversation = { id: string; title: string; providerId: string; agentGenerationId?: string; agentDefinitionDigest?: string; updatedAt: string };
type Message = { id: string; role: 'user' | 'assistant'; content: string; createdAt: string };
type ConversationDetail = Conversation & { messages: Message[] };
type Plugin = { id: string; version: string; description: string; state: string; capabilities: string[] };
type PermissionSet = { filesystem?: { read?: string[]; write?: string[] }; network?: string[]; secrets?: string[]; process?: boolean; background?: boolean };
type Release = { id: string; projectId: string; pluginId: string; version: string; digest: string; permissionHash: string; sourceVersion: 'v1' | 'v2'; manifest: { name: string; description: string; permissions: PermissionSet; ui?: { entry: string; slots?: string[] }; exports?: { tools?: Capability[]; services?: unknown[]; skills?: unknown[] } } };
type ForgeProject = { id: string; name: string; slug: string; description: string; state: string; lastError?: string; updatedAt: string; latestRelease?: Release; releases: Release[] };
type Installation = { id: string; pluginId: string; projectId: string; activeReleaseId: string; status: string };
type Capability = { id: string; summary: string; risk: string; pluginId?: string; releaseId?: string; version?: string };
type SurfaceState = { pluginId: string; releaseId: string; kind: string; surfaceId: string; status: string; registryEpoch: number };
type TraceEvent = { id: string; turnId: string; sequence: number; kind: string; details: Record<string, unknown>; createdAt: string };
type AgentTurn = { id: string; conversationId: string; status: string; stopReason?: string; recoveryClass?: string; cancelRequested: boolean; lastSequence: number; startedAt: string; completedAt?: string };
type TurnReceipt = { turnId: string; conversationId: string; inputMessageId: string; status: string };

export default function OApp() {
  const [loading, setLoading] = useState(true);
  const [providers, setProviders] = useState<Provider[]>([]);
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [plugins, setPlugins] = useState<Plugin[]>([]);
  const [active, setActive] = useState<ConversationDetail | null>(null);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [pluginsOpen, setPluginsOpen] = useState(false);
  const [evolutionOpen, setEvolutionOpen] = useState(false);
  const [sending, setSending] = useState(false);
  const [notice, setNotice] = useState('');
  const [trace, setTrace] = useState<TraceEvent[]>([]);
  const activeIdRef = useRef('');
  const observationRef = useRef<{ turnId: string; controller: AbortController; promise: Promise<void> } | null>(null);

  useEffect(() => () => observationRef.current?.controller.abort(), []);

  useEffect(() => {
    void Promise.all([
      request<Provider[]>('/providers'),
      request<Conversation[]>('/conversations'),
      request<Plugin[]>('/system/plugins'),
    ]).then(([providerList, conversationList, pluginList]) => {
      setProviders(providerList); setConversations(conversationList); setPlugins(pluginList);
    }).catch((error) => setNotice(error instanceof Error ? error.message : '无法连接本地运行时')).finally(() => setLoading(false));
  }, []);
  function observeTurn(turnId: string, conversationId: string) {
    if (observationRef.current?.turnId === turnId) return observationRef.current.promise;
    observationRef.current?.controller.abort();
    const controller = new AbortController();
    const promise = waitForTurn(turnId, (event) => {
      if (activeIdRef.current !== conversationId) return;
      setTrace((items) => items.some((item) => item.id === event.id) ? items : [...items, event]);
      if (event.kind === 'turn.failed') setNotice(typeof event.details.error === 'string' ? event.details.error : 'Agent 运行失败');
      if (event.kind === 'turn.cancelled') setNotice('任务已停止，已完成的过程仍保留在运行记录中。');
      if (event.kind === 'turn.needs_reconciliation') setNotice('任务产生了需要确认的外部影响，处理后才能重试。');
    }, controller.signal).finally(() => {
      if (observationRef.current?.turnId === turnId) observationRef.current = null;
    });
    observationRef.current = { turnId, controller, promise };
    return promise;
  }

  async function refreshConversation(id: string) {
    const [detail, events] = await Promise.all([request<ConversationDetail>(`/conversations/${id}`), request<TraceEvent[]>(`/conversations/${id}/trace`)]);
    if (activeIdRef.current === id) { setActive(detail); setTrace(events); }
    setConversations((items) => items.map((item) => item.id === detail.id ? detail : item));
  }

  async function openConversation(id: string) {
    observationRef.current?.controller.abort();
    observationRef.current = null;
    activeIdRef.current = id;
    const [detail, events, turns] = await Promise.all([request<ConversationDetail>(`/conversations/${id}`), request<TraceEvent[]>(`/conversations/${id}/trace`), request<AgentTurn[]>(`/conversations/${id}/turns`)]);
    if (activeIdRef.current !== id) return;
    setActive(detail); setTrace(events); setNotice('');
    const running = turns.find((turn) => turn.status === 'running' || turn.status === 'cancelling');
    if (running) {
      setSending(true);
      void observeTurn(running.id, id).then(() => refreshConversation(id)).catch((error) => setNotice(error instanceof Error ? error.message : '无法恢复任务事件')).finally(() => {
        if (activeIdRef.current === id) setSending(false);
      });
    } else {
      setSending(false);
    }
  }
  async function newConversation() {
    if (!providers.length) { setSettingsOpen(true); return; }
    observationRef.current?.controller.abort();
    observationRef.current = null;
    const created = await request<Conversation>('/conversations', { method: 'POST', body: JSON.stringify({ title: '新对话', providerId: providers[0].id }) });
    activeIdRef.current = created.id; setConversations((items) => [created, ...items]); setActive({ ...created, messages: [] }); setTrace([]);
  }
  async function send(content: string) {
    if (!content.trim() || sending) return;
    setSending(true); setNotice('');
    let targetId = active?.id ?? '';
    try {
      let target = active;
      if (!target) {
        if (!providers.length) { setSettingsOpen(true); return; }
        const created = await request<Conversation>('/conversations', { method: 'POST', body: JSON.stringify({ title: content.trim().slice(0, 42), providerId: providers[0].id }) });
        target = { ...created, messages: [] }; targetId = created.id; activeIdRef.current = created.id; setConversations((items) => [created, ...items]);
      }
      const pending: Message = { id: `pending-${Date.now()}`, role: 'user', content, createdAt: new Date().toISOString() };
      setActive({ ...target, messages: [...target.messages, pending] });
      const receipt = await request<TurnReceipt>(`${API_V2}/agent/conversations/${target.id}/turns`, { method: 'POST', body: JSON.stringify({ content }) });
      await observeTurn(receipt.turnId, target.id);
    } catch (error) { setNotice(error instanceof Error ? error.message : 'Agent 运行失败'); }
    finally {
      if (targetId) {
        const [refreshed, events] = await Promise.all([request<ConversationDetail>(`/conversations/${targetId}`), request<TraceEvent[]>(`/conversations/${targetId}/trace`)]).catch(() => [null, null] as const);
        if (refreshed && events) {
          if (activeIdRef.current === targetId) { setTrace(events); setActive(refreshed); }
          setConversations((items) => items.map((item) => item.id === refreshed.id ? refreshed : item));
        }
      }
      if (activeIdRef.current === targetId) setSending(false);
    }
  }

  async function cancelTurn() {
    if (!active || !sending) return;
    try {
      const turns = await request<AgentTurn[]>(`/conversations/${active.id}/turns`);
      const running = turns.find((turn) => turn.status === 'running' || turn.status === 'cancelling');
      if (!running) { setNotice('当前没有正在运行的任务。'); return; }
      await request(`/agent/turns/${running.id}/cancel`, { method: 'POST', body: JSON.stringify({ reason: 'user_requested' }) });
      setNotice('正在安全停止当前任务…');
    } catch (error) { setNotice(error instanceof Error ? error.message : '无法停止当前任务'); }
  }

  if (loading) return <Splash />;
  return <main className="app-shell">
    <Sidebar conversations={conversations} activeId={active?.id} onNew={newConversation} onOpen={openConversation} onEvolution={() => setEvolutionOpen(true)} onPlugins={() => setPluginsOpen(true)} onSettings={() => setSettingsOpen(true)} />
    <section className="workspace"><header className="workspace-header"><div><span className="eyebrow">本地 AGENT / 对话</span><h1>{active?.title ?? '新的工作区'}</h1></div><div className="header-actions"><span className="status-pill"><i /> 运行中</span><button className="icon-button" onClick={() => setSettingsOpen(true)} aria-label="打开设置">⌘</button></div></header>
      <div className="workspace-grid"><Chat active={active} providers={providers} sending={sending} notice={notice} onSend={send} onCancel={cancelTurn} onConfigure={() => setSettingsOpen(true)} /><RuntimePanel plugins={plugins} provider={providers.find((p) => p.id === active?.providerId) ?? providers[0]} messageCount={active?.messages.length ?? 0} trace={trace} /></div>
    </section>
    {settingsOpen && <Settings providers={providers} onClose={() => setSettingsOpen(false)} onSaved={(next) => { setProviders((items) => items.some((item) => item.id === next.id) ? items.map((item) => item.id === next.id ? next : item) : [next, ...items]); setNotice('模型服务已保存'); }} />}
    {evolutionOpen && <EvolutionCenter providers={providers} onClose={() => setEvolutionOpen(false)} />}
    {pluginsOpen && <PluginCenter onClose={() => setPluginsOpen(false)} />}
  </main>;
}

function waitForTurn(turnId: string, onEvent: (event: TraceEvent) => void, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const source = new EventSource(`${API_V2}/agent/turns/${encodeURIComponent(turnId)}/events`);
    signal?.addEventListener('abort', () => { source.close(); resolve(); }, { once: true });
    source.onmessage = (message) => {
      try {
        const event = JSON.parse(message.data) as TraceEvent;
        onEvent(event);
        if (event.kind === 'turn.completed' || event.kind === 'turn.failed' || event.kind === 'turn.cancelled' || event.kind === 'turn.needs_reconciliation') {
          source.close();
          resolve();
        }
      } catch (error) {
        source.close();
        reject(error);
      }
    };
    source.onerror = () => {
      if (source.readyState === EventSource.CLOSED) reject(new Error('任务完成前事件流意外关闭。'));
    };
  });
}

function Splash() { return <div className="splash"><div className="brand-mark">O</div><span>正在启动插件运行时…</span></div>; }

function Sidebar({ conversations, activeId, onNew, onOpen, onEvolution, onPlugins, onSettings }: { conversations: Conversation[]; activeId?: string; onNew: () => void; onOpen: (id: string) => void; onEvolution: () => void; onPlugins: () => void; onSettings: () => void }) {
  return <aside className="sidebar"><div className="brand"><div className="brand-mark">O</div><span>O</span><small>0.2</small></div><button className="new-button" onClick={onNew}><span>＋</span> 新对话 <kbd>⌘ N</kbd></button><nav><p>对话</p>{conversations.length === 0 ? <div className="empty-nav">还没有对话。<br/>从一个目标开始。</div> : conversations.map((item) => <button key={item.id} className={item.id === activeId ? 'active' : ''} onClick={() => onOpen(item.id)}><i>◫</i><span>{item.title}</span></button>)}</nav><div className="sidebar-foot"><button onClick={onEvolution}><i>⌁</i><span>演化实验室</span></button><button onClick={onPlugins}><i>◇</i><span>插件工坊</span></button><button onClick={onSettings}><i>⚙</i><span>模型设置</span></button></div></aside>;
}

function Chat({ active, providers, sending, notice, onSend, onCancel, onConfigure }: { active: ConversationDetail | null; providers: Provider[]; sending: boolean; notice: string; onSend: (content: string) => void; onCancel: () => void; onConfigure: () => void }) {
  const [draft, setDraft] = useState(''); const suggestions = ['检查这个项目的架构', '设计一份可靠的执行计划', '根据证据定位一个问题']; const submit = () => { const value = draft; if (value.trim()) { setDraft(''); onSend(value); } };
  return <div className="chat-column"><div className="messages">{!active?.messages.length ? <div className="empty-chat"><div className="pulse-orbit"><span>O</span></div><span className="eyebrow">AGENT 已就绪</span><h2>今天想做什么？</h2><p>告诉 O 你想达成的结果和约束。运行时会保留任务状态，并交给你配置的模型处理。</p>{!providers.length ? <button className="setup-card" onClick={onConfigure}><span>01</span><div><b>连接模型服务</b><small>配置模型供应商及其 API 协议</small></div><i>→</i></button> : <div className="suggestions">{suggestions.map((text) => <button key={text} onClick={() => setDraft(text)}>{text}<span>↗</span></button>)}</div>}</div> : active.messages.map((message) => <article key={message.id} className={`message ${message.role}`}><div className="message-role">{message.role === 'user' ? '你' : 'O'}</div><div className="message-body">{message.content}</div></article>)}{sending && <article className="message assistant"><div className="message-role">O</div><div className="thinking"><i/><i/><i/> 正在思考</div></article>}</div>{notice && <div className="notice">{notice}</div>}<div className="composer"><textarea value={draft} onChange={(e) => setDraft(e.target.value)} onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); } }} placeholder={providers.length ? '描述目标、约束，或者下一步行动…' : '请先配置模型服务…'} disabled={!providers.length || sending}/><div className="composer-row"><span>{sending ? '任务过程已持久化，可安全停止' : 'Enter 发送 · Shift Enter 换行'}</span>{sending ? <button className="stop-button" onClick={onCancel}>停止 <i>■</i></button> : <button onClick={submit} disabled={!draft.trim()}>发送 <i>↑</i></button>}</div></div></div>;
}

function RuntimePanel({ plugins, provider, messageCount, trace }: { plugins: Plugin[]; provider?: Provider; messageCount: number; trace: TraceEvent[] }) {
  const running = plugins.filter((item) => item.state === 'running').length;
  return <aside className="runtime-panel"><div className="panel-title"><div><span className="eyebrow">实时检查</span><h3>运行状态</h3></div><span className="live-dot">实时</span></div><div className="runtime-metric"><span>插件图</span><b>{running}<small> / {plugins.length} 个运行中</small></b><div className="meter"><i style={{width: plugins.length ? `${running/plugins.length*100}%` : '0%'}}/></div></div><div className="runtime-section"><p>当前模型</p>{provider ? <div className="model-card"><div className="model-icon">M</div><div><b>{provider.model}</b><small>{provider.name} · API</small></div><i>●</i></div> : <div className="muted-card">尚未配置模型服务</div>}</div><div className="runtime-section"><p>任务状态</p><dl><div><dt>阶段</dt><dd>{eventLabel(trace.at(-1)?.kind ?? (messageCount ? 'checkpointed' : 'idle'))}</dd></div><div><dt>消息</dt><dd>{messageCount}</dd></div><div><dt>策略</dt><dd>按需加载 / 版本固定</dd></div></dl></div>{!!trace.length && <div className="runtime-section trace-list"><p>最近轨迹</p>{trace.slice(-6).map((event) => <div key={event.id}><i/><span>{eventLabel(event.kind)}</span><small>#{event.sequence}</small></div>)}</div>}<div className="runtime-section plugin-list"><p>插件图</p>{plugins.slice(0,6).map((item) => <div key={item.id}><i className={item.state}/><span>{item.id.replace('core.','').replace('.v1','')}</span><small>{item.version}</small></div>)}</div><div className="runtime-foot"><span>状态</span><b>本地 / 已加密</b></div></aside>;
}

function eventLabel(value: string) {
  const labels: Record<string, string> = {
    idle: '空闲', checkpointed: '已保存', 'turn.started': '任务开始', 'turn.completed': '任务完成',
    'turn.failed': '任务失败', 'turn.cancelled': '任务已取消', 'turn.needs_reconciliation': '等待确认',
    'model.started': '模型调用', 'model.completed': '模型完成', 'tool.started': '工具调用', 'tool.completed': '工具完成',
  };
  return labels[value] ?? value;
}

function stateLabel(value: string) {
  const labels: Record<string, string> = {
    proposed: '待生成', generating: '生成中', generation_failed: '生成失败', generated: '已生成',
    building: '构建中', build_failed: '构建失败', tested: '验证通过', awaiting_approval: '等待批准',
    approved: '已批准', installed: '已安装', active: '运行中', inactive: '已停用', activation_failed: '启用失败',
    queued: '排队中', running: '运行中', completed: '已完成', failed: '失败', candidate: '候选', stable: '稳定', superseded: '已替代',
  };
  return labels[value] ?? value.replaceAll('_', ' ');
}

function PluginCenter({ onClose }: { onClose: () => void }) {
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
    setProjects(nextProjects); setInstallations(nextInstallations); setSurfaces(nextSurfaces);
    setSelectedId((current) => current || nextProjects[0]?.id || '');
  }, []);

  useEffect(() => {
    void Promise.all([
      request<ForgeProject[]>('/plugin-forge/projects'),
      request<Installation[]>('/plugin-runtime/installations'),
      request<SurfaceState[]>('/plugin-runtime/surfaces'),
    ]).then(([nextProjects, nextInstallations, nextSurfaces]) => {
      setProjects(nextProjects); setInstallations(nextInstallations); setSurfaces(nextSurfaces);
      setSelectedId(nextProjects[0]?.id || '');
    }).catch((reason) => setError(reason instanceof Error ? reason.message : '无法加载插件'));
  }, []);
  const selected = projects.find((project) => project.id === selectedId) ?? projects[0];
  const installation = installations.find((item) => item.projectId === selected?.id && item.status === 'active');

  useEffect(() => {
    async function bridge(event: MessageEvent) {
      if (event.source !== iframeRef.current?.contentWindow || (event.origin !== 'null' && event.origin !== ASSET_ORIGIN) || !selected?.latestRelease) return;
      const data = event.data as { type?: string; id?: string; operation?: string; serviceId?: string; capabilitySuffix?: string; input?: unknown };
      const legacy = data.type === 'axiom.plugin.invoke';
      if (!data.id || (legacy ? !data.capabilitySuffix : data.type !== 'axiom.ui.call' || (!data.operation && !data.serviceId))) return;
      try {
        const endpoint = legacy
          ? `/plugin-runtime/ui/${encodeURIComponent(selected.latestRelease.pluginId)}/legacy-invoke`
          : data.serviceId
            ? `/plugin-runtime/ui/${encodeURIComponent(selected.latestRelease.pluginId)}/services/${encodeURIComponent(data.serviceId)}/call`
            : `/plugin-runtime/ui/${encodeURIComponent(selected.latestRelease.pluginId)}/call`;
        const body = legacy ? { capabilitySuffix: data.capabilitySuffix, input: data.input ?? {} } : data.serviceId ? { input: data.input ?? {} } : { operation: data.operation, input: data.input ?? {} };
        const result = await request<{ output: unknown }>(endpoint, { method: 'POST', body: JSON.stringify(body) });
        iframeRef.current?.contentWindow?.postMessage({ type: legacy ? 'axiom.plugin.result' : 'axiom.ui.result', id: data.id, output: result.output }, '*');
      } catch (reason) {
        iframeRef.current?.contentWindow?.postMessage({ type: legacy ? 'axiom.plugin.result' : 'axiom.ui.result', id: data.id, error: reason instanceof Error ? reason.message : '界面调用失败' }, '*');
      }
    }
    window.addEventListener('message', bridge);
    return () => window.removeEventListener('message', bridge);
  }, [selected]);

  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setBusy('create'); setError('');
    const form = new FormData(event.currentTarget);
    try {
      const project = await request<ForgeProject>('/plugin-forge/projects', { method: 'POST', body: JSON.stringify(Object.fromEntries(form)) });
      setProjects((items) => [project, ...items]); setSelectedId(project.id); event.currentTarget.reset();
    } catch (reason) { setError(reason instanceof Error ? reason.message : '无法创建插件'); }
    finally { setBusy(''); }
  }

  async function act(project: ForgeProject, action: string) {
    setBusy(`${project.id}:${action}`); setError('');
    try {
      await request(`/plugin-forge/projects/${project.id}/${action}`, { method: 'POST' });
      await refresh();
    } catch (reason) { setError(reason instanceof Error ? reason.message : '插件操作失败'); await refresh().catch(() => undefined); }
    finally { setBusy(''); }
  }

  async function rollback(project: ForgeProject, releaseId: string) {
    setBusy(`${project.id}:rollback`); setError('');
    try {
      await request(`/plugin-forge/projects/${project.id}/rollback`, { method: 'POST', body: JSON.stringify({ releaseId }) });
      await refresh();
    } catch (reason) { setError(reason instanceof Error ? reason.message : '回退失败'); }
    finally { setBusy(''); }
  }

  const nextAction = selected ? forgeAction(selected.state) : null;
  const permissions = selected?.latestRelease?.manifest.permissions;
  const selectedSurfaces = surfaces.filter((surface) => surface.pluginId === selected?.latestRelease?.pluginId && surface.releaseId === installation?.activeReleaseId);
  const permissionChanged = !!selected?.releases?.[1] && selected.releases[0].permissionHash !== selected.releases[1].permissionHash;
  return <div className="modal-backdrop forge-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}><section className="forge-modal">
    <header className="forge-header"><div><span className="eyebrow">系统 / 插件工坊</span><h2>安全地构建新能力</h2><p>每个插件都拥有独立 Git 项目，经过验证和你的批准后，才会作为隔离版本加载。</p></div><button onClick={onClose}>×</button></header>
    <div className="forge-layout"><aside className="forge-rail"><form onSubmit={create} className="forge-create"><label>插件名称<input name="name" placeholder="工作区检查器" required /></label><label>插件形态<select name="shape" defaultValue="hybrid"><option value="hybrid">全栈界面 + Agent 工具</option><option value="agent-tool">Agent 工具</option><option value="ui">界面扩展</option><option value="service">后端服务</option><option value="skill">按需加载的 Skill</option></select></label><label>它需要做什么？<textarea name="description" placeholder="描述一个具体能力…" required /></label><button disabled={busy === 'create'}>{busy === 'create' ? '正在创建…' : '创建提案'} <span>＋</span></button></form><div className="forge-projects"><p className="eyebrow">项目</p>{projects.length === 0 ? <div className="forge-empty">还没有插件项目。</div> : projects.map((project) => <button key={project.id} className={project.id === selected?.id ? 'active' : ''} onClick={() => setSelectedId(project.id)}><i className={`state-${project.state}`}/><span><b>{project.name}</b><small>{stateLabel(project.state)}</small></span><em>›</em></button>)}</div></aside>
      <div className="forge-stage">{selected ? <><div className="forge-title"><div><span className="eyebrow">{selected.latestRelease?.pluginId ?? `草稿 / ${selected.slug}`}</span><h3>{selected.name}</h3><p>{selected.description}</p></div><span className={`forge-state state-${selected.state}`}>{stateLabel(selected.state)}</span></div>
        <div className="forge-pipeline">{['proposed','generated','tested','approved','active'].map((state, index) => <div key={state} className={pipelineReached(selected.state, state) ? 'reached' : ''}><span>{index + 1}</span><b>{stateLabel(state)}</b></div>)}</div>
        {!!selectedSurfaces.length && <section className="surface-strip"><span><b className="eyebrow">已加载能力面</b><small>版本 {installation?.activeReleaseId.slice(0, 12)} · 注册表 {Math.max(...selectedSurfaces.map((item) => item.registryEpoch))}</small></span><div>{selectedSurfaces.map((surface) => <em className={`surface-${surface.status}`} title={`${surface.surfaceId} · ${surfacePrincipal(surface.kind)}`} key={`${surface.kind}:${surface.surfaceId}`}>{surfaceKindLabel(surface.kind)} · {stateLabel(surface.status)}</em>)}</div></section>}
        {selected.lastError && <div className="forge-error"><b>上次运行失败</b><span>{selected.lastError}</span></div>}
        {permissions && <section className="permission-card"><div><span><b className="eyebrow">权限契约</b><small>{selected.latestRelease?.permissionHash.slice(0, 16)} · {permissionChanged ? '相较上一版本有变化' : '与当前版本绑定'}</small></span><h4>需要用户批准</h4></div><div className="permission-grid"><Permission label="读取工作区" enabled={permissions.filesystem?.read?.includes('${workspace}') ?? false}/><Permission label="写入插件数据" enabled={permissions.filesystem?.write?.includes('${pluginData}') ?? false}/><Permission label="后台任务" enabled={permissions.background ?? false}/><Permission label={`网络：${permissions.network?.length ? permissions.network.join(', ') : '禁止'}`} enabled={(permissions.network?.length ?? 0) > 0}/><Permission label={`密钥：${permissions.secrets?.length ? permissions.secrets.join(', ') : '无'}`} enabled={(permissions.secrets?.length ?? 0) > 0}/></div></section>}
        {selected.state === 'active' && selected.releases?.length > 1 && <section className="release-history"><span className="eyebrow">不可变版本</span>{selected.releases.map((release) => <div key={release.id}><span><b>{release.version} · {release.sourceVersion}</b><small>{release.digest.slice(0, 12)}</small></span>{installation?.activeReleaseId === release.id ? <em>当前版本</em> : <button onClick={() => rollback(selected, release.id)} disabled={busy !== ''}>回退</button>}</div>)}</section>}
        {installation && selected.latestRelease?.manifest.ui ? <section className="plugin-preview"><div className="preview-bar"><span><i/> 实时 · {selected.latestRelease.version}</span><small>沙箱界面 · 版本固定</small></div><iframe ref={iframeRef} title={`${selected.name} 插件`} sandbox="allow-scripts" src={`${API}/plugin-assets/${installation.activeReleaseId}/${selected.latestRelease.manifest.ui.entry.split('/').pop()}`} /></section> : <section className="forge-wait"><span>{selected.state === 'proposed' ? '◇' : '◌'}</span><h4>{forgeGuidance(selected.state).title}</h4><p>{selected.state === 'active' && !selected.latestRelease?.manifest.ui ? `插件已加载但没有界面。当前能力面：${selectedSurfaces.map((item) => surfaceKindLabel(item.kind)).join('、') || '无'}。` : forgeGuidance(selected.state).body}</p></section>}
        <div className="forge-actions"><div><span className="eyebrow">下一步</span><small>未经批准，不会安装插件或扩大权限。</small></div><div className="forge-action-buttons">{selected.state === 'active' && <button className="secondary" onClick={() => act(selected, 'revise')} disabled={busy !== ''}>创建更新</button>}{nextAction && <button onClick={() => act(selected, nextAction.action)} disabled={busy !== ''}>{busy.startsWith(selected.id) ? '处理中…' : nextAction.label}<span>→</span></button>}</div></div>
      </> : <div className="forge-wait"><span>◇</span><h4>定义第一个能力</h4><p>先创建提案；只有在你明确操作后，系统才会开始生成。</p></div>}</div>
    </div>{error && <div className="forge-toast">{error}</div>}
  </section></div>;
}

function Permission({ label, enabled }: { label: string; enabled: boolean }) { return <div className={enabled ? 'enabled' : ''}><i>{enabled ? '✓' : '—'}</i><span>{label}</span></div>; }
function surfacePrincipal(kind: string) { return kind === 'ui' ? '界面权限域' : kind === 'tool' || kind === 'skill' ? 'Agent 权限域' : '宿主权限域'; }
function surfaceKindLabel(kind: string) {
  return ({ ui: '界面', tool: '工具', skill: 'Skill', service: '服务', hook: '钩子', job: '任务' } as Record<string, string>)[kind] ?? kind;
}
function forgeAction(state: string): { action: string; label: string } | null {
  if (state === 'proposed' || state === 'generation_failed') return { action: 'generate', label: '生成源码' };
  if (state === 'generated' || state === 'build_failed') return { action: 'build', label: '构建并测试' };
  if (state === 'tested') return { action: 'request-approval', label: '检查权限' };
  if (state === 'awaiting_approval') return { action: 'approve', label: '批准此版本' };
  if (state === 'approved' || state === 'installed' || state === 'inactive' || state === 'activation_failed') return { action: 'install', label: state === 'inactive' ? '启用插件' : '安装并启用' };
  if (state === 'active') return { action: 'deactivate', label: '停用插件' };
  return null;
}
function pipelineReached(current: string, target: string) {
  const order = ['proposed','generating','generated','building','tested','awaiting_approval','approved','installed','active'];
  const normalized = current.includes('failed') ? current.replace('_failed', '') : current;
  return order.indexOf(normalized) >= order.indexOf(target);
}
function forgeGuidance(state: string) {
  const copy: Record<string, { title: string; body: string }> = {
    proposed: { title: '提案已准备', body: '生成拥有独立 Git 历史的完整插件源码。' },
    generated: { title: '源码已生成', body: '源码独立于 O 核心；构建和测试将产出不可变版本。' },
    tested: { title: '验证已通过', body: '开放批准操作前，请检查插件申请的权限契约。' },
    awaiting_approval: { title: '等待你的批准', body: '批准结果只对当前版本摘要和权限摘要有效。' },
    approved: { title: '版本已批准', body: '安装时会启动候选 Sidecar、完成健康检查，再原子加载全部能力。' },
    inactive: { title: '插件已停用', body: '版本仍保留在本地，无需重新构建即可再次启用。' },
  };
  return copy[state] ?? { title: '正在处理', body: 'O 正在保留当前状态和完整审计记录。' };
}

function Settings({ providers, onClose, onSaved }: { providers: Provider[]; onClose: () => void; onSaved: (provider: Provider) => void }) {
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState('');
  const [editing, setEditing] = useState<Provider | null>(providers[0] ?? null);
  const [kinds, setKinds] = useState<ProviderKind[]>([]);
  const defaultName = useMemo(() => providers.length ? `模型服务 ${providers.length + 1}` : '主模型', [providers.length]);

  useEffect(() => {
    request<ProviderKind[]>('/provider-kinds').then(setKinds).catch((error) => setStatus(error instanceof Error ? error.message : '无法加载模型协议'));
  }, []);

  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setBusy(true); setStatus('');
    const form = new FormData(event.currentTarget);
    try {
      const saved = await request<Provider>(editing ? `/providers/${editing.id}` : '/providers', { method: editing ? 'PUT' : 'POST', body: JSON.stringify(Object.fromEntries(form)) });
      onSaved(saved); setEditing(saved); setStatus('模型服务已保存，请运行连接测试确认 API Key 和模型 ID。');
    } catch (error) { setStatus(error instanceof Error ? error.message : '无法保存模型服务'); }
    finally { setBusy(false); }
  }

  async function test(id: string) {
    setStatus('正在测试连接…');
    try { await request(`/providers/${id}/test`, { method: 'POST' }); setStatus('连接正常。'); }
    catch (error) { setStatus(error instanceof Error ? error.message : '连接失败'); }
  }

  function selectKind(event: ChangeEvent<HTMLSelectElement>) {
    if (editing) return;
    const selected = kinds.find((item) => item.kind === event.target.value);
    const base = event.currentTarget.form?.elements.namedItem('baseUrl');
    if (selected && base instanceof HTMLInputElement) base.value = selected.defaultBaseUrl;
  }

  return <div className="modal-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
    <section className="settings-modal">
      <header><div><span className="eyebrow">系统 / 模型服务</span><h2>模型连接</h2></div><button onClick={onClose}>×</button></header>
      <p className="modal-lead">请选择供应商实际支持的 API 协议。凭证由本地 Go Host 加密保存，不会返回浏览器。</p>
      {!!providers.length && <div className="provider-list">{providers.map((item) => <div key={item.id}><span className="model-icon">M</span><div><b>{item.name}</b><small>{item.model} · {item.kind}</small></div><div className="provider-actions"><button onClick={() => { setEditing(item); setStatus(''); }}>编辑</button><button onClick={() => test(item.id)}>测试</button></div></div>)}</div>}
      <form key={editing?.id ?? 'new'} onSubmit={save} className="provider-form">
        <p>{editing ? '编辑模型连接' : '添加模型连接'}</p>
        <div className="form-grid">
          <label>API 协议<select name="kind" defaultValue={editing?.kind ?? 'openai-responses'} onChange={selectKind} required>{kinds.length ? kinds.map((item) => <option key={item.kind} value={item.kind}>{item.label}</option>) : <option value={editing?.kind ?? 'openai-responses'}>{editing?.kind ?? '正在加载协议…'}</option>}</select></label>
          <label>连接名称<input name="name" defaultValue={editing?.name ?? defaultName} required /></label>
          <label className="wide">模型 ID<input name="model" defaultValue={editing?.model ?? ''} placeholder="gpt-5 / claude-sonnet / deepseek-chat" required /></label>
          <label className="wide">基础 URL<input name="baseUrl" defaultValue={editing?.baseUrl ?? 'https://api.openai.com/v1'} required /></label>
          <label className="wide">API KEY<input name="apiKey" type="password" placeholder={editing ? '留空以继续使用现有密钥' : '输入 API Key'} required={!editing} /></label>
        </div>
        {status && <div className="form-status">{status}</div>}
        <button className="primary-button" disabled={busy}>{busy ? '正在加密保存…' : editing ? '更新连接' : '保存连接'}<span>→</span></button>
        {editing && <button type="button" className="text-button" onClick={() => { setEditing(null); setStatus(''); }}>添加另一个模型服务</button>}
      </form>
    </section>
  </div>;
}
