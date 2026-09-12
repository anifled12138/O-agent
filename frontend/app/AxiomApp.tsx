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

export default function AxiomApp() {
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
    }).catch((error) => setNotice(error instanceof Error ? error.message : 'Could not connect to the local runtime')).finally(() => setLoading(false));
  }, []);
  function observeTurn(turnId: string, conversationId: string) {
    if (observationRef.current?.turnId === turnId) return observationRef.current.promise;
    observationRef.current?.controller.abort();
    const controller = new AbortController();
    const promise = waitForTurn(turnId, (event) => {
      if (activeIdRef.current !== conversationId) return;
      setTrace((items) => items.some((item) => item.id === event.id) ? items : [...items, event]);
      if (event.kind === 'turn.failed') setNotice(typeof event.details.error === 'string' ? event.details.error : 'Agent turn failed.');
      if (event.kind === 'turn.cancelled') setNotice('Turn stopped. Completed observations remain in the journal.');
      if (event.kind === 'turn.needs_reconciliation') setNotice('Turn stopped with an external effect that must be reconciled before retrying.');
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
      void observeTurn(running.id, id).then(() => refreshConversation(id)).catch((error) => setNotice(error instanceof Error ? error.message : 'Could not resume turn events')).finally(() => {
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
    const created = await request<Conversation>('/conversations', { method: 'POST', body: JSON.stringify({ title: 'New mission', providerId: providers[0].id }) });
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
    } catch (error) { setNotice(error instanceof Error ? error.message : 'Agent turn failed'); }
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
      if (!running) { setNotice('No active turn was found.'); return; }
      await request(`/agent/turns/${running.id}/cancel`, { method: 'POST', body: JSON.stringify({ reason: 'user_requested' }) });
      setNotice('Stopping after the current cancellable operation…');
    } catch (error) { setNotice(error instanceof Error ? error.message : 'Could not stop the turn'); }
  }

  if (loading) return <Splash />;
  return <main className="app-shell">
    <Sidebar conversations={conversations} activeId={active?.id} onNew={newConversation} onOpen={openConversation} onEvolution={() => setEvolutionOpen(true)} onPlugins={() => setPluginsOpen(true)} onSettings={() => setSettingsOpen(true)} />
    <section className="workspace"><header className="workspace-header"><div><span className="eyebrow">LOCAL AGENT / MISSION</span><h1>{active?.title ?? 'Untitled workspace'}</h1></div><div className="header-actions"><span className="status-pill"><i /> runtime online</span><button className="icon-button" onClick={() => setSettingsOpen(true)} aria-label="Open settings">⌘</button></div></header>
      <div className="workspace-grid"><Chat active={active} providers={providers} sending={sending} notice={notice} onSend={send} onCancel={cancelTurn} onConfigure={() => setSettingsOpen(true)} /><RuntimePanel plugins={plugins} provider={providers.find((p) => p.id === active?.providerId) ?? providers[0]} messageCount={active?.messages.length ?? 0} trace={trace} /></div>
    </section>
    {settingsOpen && <Settings providers={providers} onClose={() => setSettingsOpen(false)} onSaved={(next) => { setProviders((items) => items.some((item) => item.id === next.id) ? items.map((item) => item.id === next.id ? next : item) : [next, ...items]); setNotice('Provider saved'); }} />}
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
      if (source.readyState === EventSource.CLOSED) reject(new Error('The turn event stream closed before a terminal event.'));
    };
  });
}

function Splash() { return <div className="splash"><div className="brand-mark">A</div><span>Booting plugin runtime…</span></div>; }

function Sidebar({ conversations, activeId, onNew, onOpen, onEvolution, onPlugins, onSettings }: { conversations: Conversation[]; activeId?: string; onNew: () => void; onOpen: (id: string) => void; onEvolution: () => void; onPlugins: () => void; onSettings: () => void }) {
  return <aside className="sidebar"><div className="brand"><div className="brand-mark">A</div><span>AXIOM</span><small>0.2</small></div><button className="new-button" onClick={onNew}><span>＋</span> New mission <kbd>⌘ N</kbd></button><nav><p>MISSIONS</p>{conversations.length === 0 ? <div className="empty-nav">No missions yet.<br/>Start with an objective.</div> : conversations.map((item) => <button key={item.id} className={item.id === activeId ? 'active' : ''} onClick={() => onOpen(item.id)}><i>◫</i><span>{item.title}</span></button>)}</nav><div className="sidebar-foot"><button onClick={onEvolution}><i>⌁</i><span>Evolution Lab</span></button><button onClick={onPlugins}><i>◇</i><span>Plugin Forge</span></button><button onClick={onSettings}><i>⚙</i><span>Provider settings</span></button></div></aside>;
}

function Chat({ active, providers, sending, notice, onSend, onCancel, onConfigure }: { active: ConversationDetail | null; providers: Provider[]; sending: boolean; notice: string; onSend: (content: string) => void; onCancel: () => void; onConfigure: () => void }) {
  const [draft, setDraft] = useState(''); const suggestions = ['Review this repository architecture', 'Design a reliable execution plan', 'Trace a bug with evidence']; const submit = () => { const value = draft; if (value.trim()) { setDraft(''); onSend(value); } };
  return <div className="chat-column"><div className="messages">{!active?.messages.length ? <div className="empty-chat"><div className="pulse-orbit"><span>A</span></div><span className="eyebrow">AGENT READY</span><h2>What are we building?</h2><p>Give Axiom a concrete outcome. The runtime will retain the mission state and route it through your configured model.</p>{!providers.length ? <button className="setup-card" onClick={onConfigure}><span>01</span><div><b>Connect a model provider</b><small>Add a supported API protocol</small></div><i>→</i></button> : <div className="suggestions">{suggestions.map((text) => <button key={text} onClick={() => setDraft(text)}>{text}<span>↗</span></button>)}</div>}</div> : active.messages.map((message) => <article key={message.id} className={`message ${message.role}`}><div className="message-role">{message.role === 'user' ? 'YOU' : 'AXIOM'}</div><div className="message-body">{message.content}</div></article>)}{sending && <article className="message assistant"><div className="message-role">AXIOM</div><div className="thinking"><i/><i/><i/> reasoning</div></article>}</div>{notice && <div className="notice">{notice}</div>}<div className="composer"><textarea value={draft} onChange={(e) => setDraft(e.target.value)} onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); } }} placeholder={providers.length ? 'Describe the outcome, constraints, or next move…' : 'Configure a model provider to begin…'} disabled={!providers.length || sending}/><div className="composer-row"><span>{sending ? 'Turn is durable and can be cancelled safely' : 'Enter to run · Shift Enter for newline'}</span>{sending ? <button className="stop-button" onClick={onCancel}>Stop <i>■</i></button> : <button onClick={submit} disabled={!draft.trim()}>Run <i>↑</i></button>}</div></div></div>;
}

function RuntimePanel({ plugins, provider, messageCount, trace }: { plugins: Plugin[]; provider?: Provider; messageCount: number; trace: TraceEvent[] }) {
  const running = plugins.filter((item) => item.state === 'running').length;
  return <aside className="runtime-panel"><div className="panel-title"><div><span className="eyebrow">LIVE INSPECTOR</span><h3>Runtime</h3></div><span className="live-dot">LIVE</span></div><div className="runtime-metric"><span>Plugin graph</span><b>{running}<small> / {plugins.length} running</small></b><div className="meter"><i style={{width: plugins.length ? `${running/plugins.length*100}%` : '0%'}}/></div></div><div className="runtime-section"><p>ACTIVE MODEL</p>{provider ? <div className="model-card"><div className="model-icon">M</div><div><b>{provider.model}</b><small>{provider.name} · API</small></div><i>●</i></div> : <div className="muted-card">No provider mounted</div>}</div><div className="runtime-section"><p>TURN STATE</p><dl><div><dt>Phase</dt><dd>{trace.at(-1)?.kind ?? (messageCount ? 'checkpointed' : 'idle')}</dd></div><div><dt>Messages</dt><dd>{messageCount}</dd></div><div><dt>Policy</dt><dd>lazy / pinned</dd></div></dl></div>{!!trace.length && <div className="runtime-section trace-list"><p>RECENT TRACE</p>{trace.slice(-6).map((event) => <div key={event.id}><i/><span>{event.kind}</span><small>#{event.sequence}</small></div>)}</div>}<div className="runtime-section plugin-list"><p>PLUGIN GRAPH</p>{plugins.slice(0,6).map((item) => <div key={item.id}><i className={item.state}/><span>{item.id.replace('core.','').replace('.v1','')}</span><small>{item.version}</small></div>)}</div><div className="runtime-foot"><span>STATE</span><b>LOCAL / ENCRYPTED</b></div></aside>;
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
    }).catch((reason) => setError(reason instanceof Error ? reason.message : 'Could not load plugins'));
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
        iframeRef.current?.contentWindow?.postMessage({ type: legacy ? 'axiom.plugin.result' : 'axiom.ui.result', id: data.id, error: reason instanceof Error ? reason.message : 'UI call failed' }, '*');
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
    } catch (reason) { setError(reason instanceof Error ? reason.message : 'Could not create plugin'); }
    finally { setBusy(''); }
  }

  async function act(project: ForgeProject, action: string) {
    setBusy(`${project.id}:${action}`); setError('');
    try {
      await request(`/plugin-forge/projects/${project.id}/${action}`, { method: 'POST' });
      await refresh();
    } catch (reason) { setError(reason instanceof Error ? reason.message : 'Plugin action failed'); await refresh().catch(() => undefined); }
    finally { setBusy(''); }
  }

  async function rollback(project: ForgeProject, releaseId: string) {
    setBusy(`${project.id}:rollback`); setError('');
    try {
      await request(`/plugin-forge/projects/${project.id}/rollback`, { method: 'POST', body: JSON.stringify({ releaseId }) });
      await refresh();
    } catch (reason) { setError(reason instanceof Error ? reason.message : 'Rollback failed'); }
    finally { setBusy(''); }
  }

  const nextAction = selected ? forgeAction(selected.state) : null;
  const permissions = selected?.latestRelease?.manifest.permissions;
  const selectedSurfaces = surfaces.filter((surface) => surface.pluginId === selected?.latestRelease?.pluginId && surface.releaseId === installation?.activeReleaseId);
  const permissionChanged = !!selected?.releases?.[1] && selected.releases[0].permissionHash !== selected.releases[1].permissionHash;
  return <div className="modal-backdrop forge-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}><section className="forge-modal">
    <header className="forge-header"><div><span className="eyebrow">SYSTEM / PLUGIN FORGE</span><h2>Build capabilities, safely.</h2><p>Each plugin is generated as its own Git project, verified, approved by you, then mounted as an isolated release.</p></div><button onClick={onClose}>×</button></header>
    <div className="forge-layout"><aside className="forge-rail"><form onSubmit={create} className="forge-create"><label>PLUGIN NAME<input name="name" placeholder="Workspace Inspector" required /></label><label>PLUGIN SHAPE<select name="shape" defaultValue="hybrid"><option value="hybrid">Full stack + Agent tool</option><option value="agent-tool">Agent tool</option><option value="ui">UI extension</option><option value="service">Backend service</option><option value="skill">Lazy Agent skill</option></select></label><label>WHAT SHOULD IT DO?<textarea name="description" placeholder="Describe one concrete capability…" required /></label><button disabled={busy === 'create'}>{busy === 'create' ? 'Creating…' : 'Create proposal'} <span>＋</span></button></form><div className="forge-projects"><p className="eyebrow">PROJECTS</p>{projects.length === 0 ? <div className="forge-empty">No plugin projects yet.</div> : projects.map((project) => <button key={project.id} className={project.id === selected?.id ? 'active' : ''} onClick={() => setSelectedId(project.id)}><i className={`state-${project.state}`}/><span><b>{project.name}</b><small>{project.state.replaceAll('_', ' ')}</small></span><em>›</em></button>)}</div></aside>
      <div className="forge-stage">{selected ? <><div className="forge-title"><div><span className="eyebrow">{selected.latestRelease?.pluginId ?? `DRAFT / ${selected.slug}`}</span><h3>{selected.name}</h3><p>{selected.description}</p></div><span className={`forge-state state-${selected.state}`}>{selected.state.replaceAll('_', ' ')}</span></div>
        <div className="forge-pipeline">{['proposed','generated','tested','approved','active'].map((state, index) => <div key={state} className={pipelineReached(selected.state, state) ? 'reached' : ''}><span>{index + 1}</span><b>{state}</b></div>)}</div>
        {!!selectedSurfaces.length && <section className="surface-strip"><span><b className="eyebrow">OBSERVED SURFACES</b><small>desired {installation?.activeReleaseId.slice(0, 12)} · epoch {Math.max(...selectedSurfaces.map((item) => item.registryEpoch))}</small></span><div>{selectedSurfaces.map((surface) => <em className={`surface-${surface.status}`} title={`${surface.surfaceId} · ${surfacePrincipal(surface.kind)} authority`} key={`${surface.kind}:${surface.surfaceId}`}>{surface.kind} · {surface.status}</em>)}</div></section>}
        {selected.lastError && <div className="forge-error"><b>Last run failed</b><span>{selected.lastError}</span></div>}
        {permissions && <section className="permission-card"><div><span><b className="eyebrow">PERMISSION CONTRACT</b><small>{selected.latestRelease?.permissionHash.slice(0, 16)} · {permissionChanged ? 'changed from previous release' : 'release-bound'}</small></span><h4>User principal approval</h4></div><div className="permission-grid"><Permission label="Workspace read" enabled={permissions.filesystem?.read?.includes('${workspace}') ?? false}/><Permission label="Plugin data write" enabled={permissions.filesystem?.write?.includes('${pluginData}') ?? false}/><Permission label="Background jobs" enabled={permissions.background ?? false}/><Permission label={`Network ${permissions.network?.length ? permissions.network.join(', ') : 'blocked'}`} enabled={(permissions.network?.length ?? 0) > 0}/><Permission label={`Secrets ${permissions.secrets?.length ? permissions.secrets.join(', ') : 'none'}`} enabled={(permissions.secrets?.length ?? 0) > 0}/></div></section>}
        {selected.state === 'active' && selected.releases?.length > 1 && <section className="release-history"><span className="eyebrow">IMMUTABLE RELEASES</span>{selected.releases.map((release) => <div key={release.id}><span><b>{release.version} · {release.sourceVersion}</b><small>{release.digest.slice(0, 12)}</small></span>{installation?.activeReleaseId === release.id ? <em>ACTIVE</em> : <button onClick={() => rollback(selected, release.id)} disabled={busy !== ''}>Roll back</button>}</div>)}</section>}
        {installation && selected.latestRelease?.manifest.ui ? <section className="plugin-preview"><div className="preview-bar"><span><i/> LIVE · {selected.latestRelease.version}</span><small>Sandboxed frontend · pinned release</small></div><iframe ref={iframeRef} title={`${selected.name} plugin`} sandbox="allow-scripts" src={`${API}/plugin-assets/${installation.activeReleaseId}/${selected.latestRelease.manifest.ui.entry.split('/').pop()}`} /></section> : <section className="forge-wait"><span>{selected.state === 'proposed' ? '◇' : '◌'}</span><h4>{forgeGuidance(selected.state).title}</h4><p>{selected.state === 'active' && !selected.latestRelease?.manifest.ui ? `Mounted without UI. Active surfaces: ${selectedSurfaces.map((item) => item.kind).join(', ') || 'none'}.` : forgeGuidance(selected.state).body}</p></section>}
        <div className="forge-actions"><div><span className="eyebrow">NEXT CONTROLLED STEP</span><small>Nothing installs or expands permissions without approval.</small></div><div className="forge-action-buttons">{selected.state === 'active' && <button className="secondary" onClick={() => act(selected, 'revise')} disabled={busy !== ''}>Create update</button>}{nextAction && <button onClick={() => act(selected, nextAction.action)} disabled={busy !== ''}>{busy.startsWith(selected.id) ? 'Working…' : nextAction.label}<span>→</span></button>}</div></div>
      </> : <div className="forge-wait"><span>◇</span><h4>Define the first capability</h4><p>Create a proposal. Generation will only begin when you explicitly start it.</p></div>}</div>
    </div>{error && <div className="forge-toast">{error}</div>}
  </section></div>;
}

function Permission({ label, enabled }: { label: string; enabled: boolean }) { return <div className={enabled ? 'enabled' : ''}><i>{enabled ? '✓' : '—'}</i><span>{label}</span></div>; }
function surfacePrincipal(kind: string) { return kind === 'ui' ? 'UI principal' : kind === 'tool' || kind === 'skill' ? 'Agent principal' : 'Host principal'; }
function forgeAction(state: string): { action: string; label: string } | null {
  if (state === 'proposed' || state === 'generation_failed') return { action: 'generate', label: 'Generate source' };
  if (state === 'generated' || state === 'build_failed') return { action: 'build', label: 'Build & test' };
  if (state === 'tested') return { action: 'request-approval', label: 'Review permissions' };
  if (state === 'awaiting_approval') return { action: 'approve', label: 'Approve release' };
  if (state === 'approved' || state === 'installed' || state === 'inactive' || state === 'activation_failed') return { action: 'install', label: state === 'inactive' ? 'Activate plugin' : 'Install & activate' };
  if (state === 'active') return { action: 'deactivate', label: 'Deactivate' };
  return null;
}
function pipelineReached(current: string, target: string) {
  const order = ['proposed','generating','generated','building','tested','awaiting_approval','approved','installed','active'];
  const normalized = current.includes('failed') ? current.replace('_failed', '') : current;
  return order.indexOf(normalized) >= order.indexOf(target);
}
function forgeGuidance(state: string) {
  const copy: Record<string, { title: string; body: string }> = {
    proposed: { title: 'Proposal ready', body: 'Generate a standalone full-stack plugin source tree with its own Git history.' },
    generated: { title: 'Source generated', body: 'The source exists outside Axiom core. Build & test creates an immutable release.' },
    tested: { title: 'Verification passed', body: 'Inspect the permission contract before opening the approval gate.' },
    awaiting_approval: { title: 'Your approval is required', body: 'Approval is bound to this exact release digest and permission hash.' },
    approved: { title: 'Release approved', body: 'Install starts a candidate sidecar, health-checks it, then atomically mounts its capabilities.' },
    inactive: { title: 'Plugin is detached', body: 'Its release remains installed and can be mounted again without rebuilding.' },
  };
  return copy[state] ?? { title: 'Lifecycle in progress', body: 'Axiom is preserving the current state and audit trail.' };
}

function Settings({ providers, onClose, onSaved }: { providers: Provider[]; onClose: () => void; onSaved: (provider: Provider) => void }) {
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState('');
  const [editing, setEditing] = useState<Provider | null>(providers[0] ?? null);
  const [kinds, setKinds] = useState<ProviderKind[]>([]);
  const defaultName = useMemo(() => providers.length ? `Provider ${providers.length + 1}` : 'Primary model', [providers.length]);

  useEffect(() => {
    request<ProviderKind[]>('/provider-kinds').then(setKinds).catch((error) => setStatus(error instanceof Error ? error.message : 'Could not load provider protocols'));
  }, []);

  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setBusy(true); setStatus('');
    const form = new FormData(event.currentTarget);
    try {
      const saved = await request<Provider>(editing ? `/providers/${editing.id}` : '/providers', { method: editing ? 'PUT' : 'POST', body: JSON.stringify(Object.fromEntries(form)) });
      onSaved(saved); setEditing(saved); setStatus('Provider saved. Run Test to verify authentication and the model ID.');
    } catch (error) { setStatus(error instanceof Error ? error.message : 'Could not save provider'); }
    finally { setBusy(false); }
  }

  async function test(id: string) {
    setStatus('Testing provider…');
    try { await request(`/providers/${id}/test`, { method: 'POST' }); setStatus('Connection healthy.'); }
    catch (error) { setStatus(error instanceof Error ? error.message : 'Connection failed'); }
  }

  function selectKind(event: ChangeEvent<HTMLSelectElement>) {
    if (editing) return;
    const selected = kinds.find((item) => item.kind === event.target.value);
    const base = event.currentTarget.form?.elements.namedItem('baseUrl');
    if (selected && base instanceof HTMLInputElement) base.value = selected.defaultBaseUrl;
  }

  return <div className="modal-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
    <section className="settings-modal">
      <header><div><span className="eyebrow">SYSTEM / PROVIDERS</span><h2>Model connections</h2></div><button onClick={onClose}>×</button></header>
      <p className="modal-lead">Choose the provider&apos;s real API protocol. Credentials are encrypted by the local Go host and are never returned to the browser.</p>
      {!!providers.length && <div className="provider-list">{providers.map((item) => <div key={item.id}><span className="model-icon">M</span><div><b>{item.name}</b><small>{item.model} · {item.kind}</small></div><div className="provider-actions"><button onClick={() => { setEditing(item); setStatus(''); }}>Edit</button><button onClick={() => test(item.id)}>Test</button></div></div>)}</div>}
      <form key={editing?.id ?? 'new'} onSubmit={save} className="provider-form">
        <p>{editing ? 'EDIT MODEL CONNECTION' : 'ADD MODEL CONNECTION'}</p>
        <div className="form-grid">
          <label>PROTOCOL<select name="kind" defaultValue={editing?.kind ?? 'openai-responses'} onChange={selectKind} required>{kinds.length ? kinds.map((item) => <option key={item.kind} value={item.kind}>{item.label}</option>) : <option value={editing?.kind ?? 'openai-responses'}>{editing?.kind ?? 'Loading protocols…'}</option>}</select></label>
          <label>CONNECTION NAME<input name="name" defaultValue={editing?.name ?? defaultName} required /></label>
          <label className="wide">MODEL ID<input name="model" defaultValue={editing?.model ?? ''} placeholder="gpt-5 / claude-sonnet / deepseek-chat" required /></label>
          <label className="wide">BASE URL<input name="baseUrl" defaultValue={editing?.baseUrl ?? 'https://api.openai.com/v1'} required /></label>
          <label className="wide">API KEY<input name="apiKey" type="password" placeholder={editing ? 'Leave blank to keep the existing key' : 'API key'} required={!editing} /></label>
        </div>
        {status && <div className="form-status">{status}</div>}
        <button className="primary-button" disabled={busy}>{busy ? 'Encrypting…' : editing ? 'Update connection' : 'Save connection'}<span>→</span></button>
        {editing && <button type="button" className="text-button" onClick={() => { setEditing(null); setStatus(''); }}>Add another provider instead</button>}
      </form>
    </section>
  </div>;
}
