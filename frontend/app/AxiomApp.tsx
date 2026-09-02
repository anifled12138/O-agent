'use client';

import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from 'react';

type User = { id: string; email: string; displayName: string };
type Provider = { id: string; name: string; kind: string; baseUrl: string; model: string; hasApiKey: boolean };
type Conversation = { id: string; title: string; providerId: string; updatedAt: string };
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

const API = process.env.NEXT_PUBLIC_API_URL ?? 'http://127.0.0.1:8080/api/v1';
const ASSET_ORIGIN = new URL(API).origin;

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`${API}${path}`, { ...init, credentials: 'include', headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) } });
  if (!response.ok) {
    const body = await response.json().catch(() => ({ error: response.statusText }));
    throw new Error(body.error || 'Request failed');
  }
  if (response.status === 204) return undefined as T;
  return response.json();
}

export default function AxiomApp() {
  const [loading, setLoading] = useState(true);
  const [user, setUser] = useState<User | null>(null);
  const [providers, setProviders] = useState<Provider[]>([]);
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [plugins, setPlugins] = useState<Plugin[]>([]);
  const [active, setActive] = useState<ConversationDetail | null>(null);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [pluginsOpen, setPluginsOpen] = useState(false);
  const [sending, setSending] = useState(false);
  const [notice, setNotice] = useState('');
  const [trace, setTrace] = useState<TraceEvent[]>([]);

  const hydrate = useCallback(async () => {
    const me = await request<User>('/auth/me');
    setUser(me);
    const [providerList, conversationList, pluginList] = await Promise.all([
      request<Provider[]>('/providers'), request<Conversation[]>('/conversations'), request<Plugin[]>('/system/plugins'),
    ]);
    setProviders(providerList); setConversations(conversationList); setPlugins(pluginList);
  }, []);

  useEffect(() => {
    void Promise.all([
      request<User>('/auth/me'),
      request<Provider[]>('/providers'),
      request<Conversation[]>('/conversations'),
      request<Plugin[]>('/system/plugins'),
    ]).then(([me, providerList, conversationList, pluginList]) => {
      setUser(me); setProviders(providerList); setConversations(conversationList); setPlugins(pluginList);
    }).catch(() => setUser(null)).finally(() => setLoading(false));
  }, []);
  async function openConversation(id: string) { const [detail, events] = await Promise.all([request<ConversationDetail>(`/conversations/${id}`), request<TraceEvent[]>(`/conversations/${id}/trace`)]); setActive(detail); setTrace(events); }
  async function newConversation() {
    if (!providers.length) { setSettingsOpen(true); return; }
    const created = await request<Conversation>('/conversations', { method: 'POST', body: JSON.stringify({ title: 'New mission', providerId: providers[0].id }) });
    setConversations((items) => [created, ...items]); setActive({ ...created, messages: [] }); setTrace([]);
  }
  async function send(content: string) {
    if (!content.trim() || sending) return;
    setSending(true); setNotice('');
    try {
      let target = active;
      if (!target) {
        if (!providers.length) { setSettingsOpen(true); return; }
        const created = await request<Conversation>('/conversations', { method: 'POST', body: JSON.stringify({ title: content.trim().slice(0, 42), providerId: providers[0].id }) });
        target = { ...created, messages: [] }; setConversations((items) => [created, ...items]);
      }
      const pending: Message = { id: `pending-${Date.now()}`, role: 'user', content, createdAt: new Date().toISOString() };
      setActive({ ...target, messages: [...target.messages, pending] });
      await request<Message>(`/conversations/${target.id}/messages`, { method: 'POST', body: JSON.stringify({ content }) });
      const refreshed = await request<ConversationDetail>(`/conversations/${target.id}`);
      const events = await request<TraceEvent[]>(`/conversations/${target.id}/trace`);
      setTrace(events);
      setActive(refreshed); setConversations((items) => items.map((item) => item.id === refreshed.id ? refreshed : item));
    } catch (error) { setNotice(error instanceof Error ? error.message : 'Agent turn failed'); }
    finally { setSending(false); }
  }

  if (loading) return <Splash />;
  if (!user) return <AuthScreen onAuthenticated={async (next) => { setUser(next); await hydrate(); }} />;
  return <main className="app-shell">
    <Sidebar user={user} conversations={conversations} activeId={active?.id} onNew={newConversation} onOpen={openConversation} onPlugins={() => setPluginsOpen(true)} onSettings={() => setSettingsOpen(true)} onLogout={async () => { await request('/auth/logout', { method: 'POST' }); setUser(null); }} />
    <section className="workspace"><header className="workspace-header"><div><span className="eyebrow">LOCAL AGENT / MISSION</span><h1>{active?.title ?? 'Untitled workspace'}</h1></div><div className="header-actions"><span className="status-pill"><i /> runtime online</span><button className="icon-button" onClick={() => setSettingsOpen(true)} aria-label="Open settings">⌘</button></div></header>
      <div className="workspace-grid"><Chat active={active} providers={providers} sending={sending} notice={notice} onSend={send} onConfigure={() => setSettingsOpen(true)} /><RuntimePanel plugins={plugins} provider={providers.find((p) => p.id === active?.providerId) ?? providers[0]} messageCount={active?.messages.length ?? 0} trace={trace} /></div>
    </section>
    {settingsOpen && <Settings providers={providers} onClose={() => setSettingsOpen(false)} onSaved={(next) => { setProviders((items) => items.some((item) => item.id === next.id) ? items.map((item) => item.id === next.id ? next : item) : [next, ...items]); setNotice('Provider saved'); }} />}
    {pluginsOpen && <PluginCenter onClose={() => setPluginsOpen(false)} />}
  </main>;
}

function Splash() { return <div className="splash"><div className="brand-mark">A</div><span>Booting plugin runtime…</span></div>; }

function AuthScreen({ onAuthenticated }: { onAuthenticated: (user: User) => Promise<void> }) {
  const [mode, setMode] = useState<'login' | 'register'>('register'); const [busy, setBusy] = useState(false); const [error, setError] = useState('');
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setBusy(true); setError(''); const form = new FormData(event.currentTarget);
    try { const result = await request<User>(`/auth/${mode}`, { method: 'POST', body: JSON.stringify({ email: form.get('email'), password: form.get('password'), displayName: form.get('displayName') }) }); await onAuthenticated(result); }
    catch (reason) { setError(reason instanceof Error ? reason.message : 'Authentication failed'); } finally { setBusy(false); }
  }
  return <main className="auth-page"><section className="auth-story"><div className="brand"><div className="brand-mark">A</div><span>AXIOM</span></div><div className="story-copy"><span className="eyebrow">LOCAL-FIRST AGENT HARNESS</span><h1>Make reasoning<br/><em>observable.</em></h1><p>A plugin-native workspace for agents that plan, act, verify, and remember — under your control.</p></div><div className="system-strip"><span><i/> GO RUNTIME</span><span><i/> LOCAL STATE</span><span><i/> API MODELS</span></div></section>
    <section className="auth-panel"><div className="auth-box"><span className="step-label">01 / IDENTITY</span><h2>{mode === 'register' ? 'Create your local operator' : 'Welcome back'}</h2><p>{mode === 'register' ? 'This account exists only in your Axiom instance.' : 'Continue your local missions.'}</p><form onSubmit={submit}>{mode === 'register' && <label>DISPLAY NAME<input name="displayName" placeholder="Operator" required /></label>}<label>EMAIL<input name="email" type="email" placeholder="you@example.com" required /></label><label>PASSWORD<input name="password" type="password" minLength={10} placeholder="10+ characters" required /></label>{error && <div className="form-error">{error}</div>}<button className="primary-button" disabled={busy}>{busy ? 'Working…' : mode === 'register' ? 'Initialize workspace' : 'Enter workspace'}<span>→</span></button></form><button className="text-button" onClick={() => { setMode(mode === 'register' ? 'login' : 'register'); setError(''); }}>{mode === 'register' ? 'Already initialized? Sign in' : 'Need a local account? Create one'}</button></div></section></main>;
}

function Sidebar({ user, conversations, activeId, onNew, onOpen, onPlugins, onSettings, onLogout }: { user: User; conversations: Conversation[]; activeId?: string; onNew: () => void; onOpen: (id: string) => void; onPlugins: () => void; onSettings: () => void; onLogout: () => void }) {
  return <aside className="sidebar"><div className="brand"><div className="brand-mark">A</div><span>AXIOM</span><small>0.1</small></div><button className="new-button" onClick={onNew}><span>＋</span> New mission <kbd>⌘ N</kbd></button><nav><p>MISSIONS</p>{conversations.length === 0 ? <div className="empty-nav">No missions yet.<br/>Start with an objective.</div> : conversations.map((item) => <button key={item.id} className={item.id === activeId ? 'active' : ''} onClick={() => onOpen(item.id)}><i>◫</i><span>{item.title}</span></button>)}</nav><div className="sidebar-foot"><button onClick={onPlugins}><i>◇</i><span>Plugin Forge</span></button><button onClick={onSettings}><i>⚙</i><span>Provider settings</span></button><div className="operator"><span>{user.displayName.slice(0,2).toUpperCase()}</span><div><b>{user.displayName}</b><small>{user.email}</small></div><button onClick={onLogout} title="Sign out">↗</button></div></div></aside>;
}

function Chat({ active, providers, sending, notice, onSend, onConfigure }: { active: ConversationDetail | null; providers: Provider[]; sending: boolean; notice: string; onSend: (content: string) => void; onConfigure: () => void }) {
  const [draft, setDraft] = useState(''); const suggestions = ['Review this repository architecture', 'Design a reliable execution plan', 'Trace a bug with evidence']; const submit = () => { const value = draft; if (value.trim()) { setDraft(''); onSend(value); } };
  return <div className="chat-column"><div className="messages">{!active?.messages.length ? <div className="empty-chat"><div className="pulse-orbit"><span>A</span></div><span className="eyebrow">AGENT READY</span><h2>What are we building?</h2><p>Give Axiom a concrete outcome. The runtime will retain the mission state and route it through your configured model.</p>{!providers.length ? <button className="setup-card" onClick={onConfigure}><span>01</span><div><b>Connect a model provider</b><small>Add any OpenAI-compatible API endpoint</small></div><i>→</i></button> : <div className="suggestions">{suggestions.map((text) => <button key={text} onClick={() => setDraft(text)}>{text}<span>↗</span></button>)}</div>}</div> : active.messages.map((message) => <article key={message.id} className={`message ${message.role}`}><div className="message-role">{message.role === 'user' ? 'YOU' : 'AXIOM'}</div><div className="message-body">{message.content}</div></article>)}{sending && <article className="message assistant"><div className="message-role">AXIOM</div><div className="thinking"><i/><i/><i/> reasoning</div></article>}</div>{notice && <div className="notice">{notice}</div>}<div className="composer"><textarea value={draft} onChange={(e) => setDraft(e.target.value)} onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); } }} placeholder={providers.length ? 'Describe the outcome, constraints, or next move…' : 'Configure a model provider to begin…'} disabled={!providers.length || sending}/><div className="composer-row"><span>Enter to run · Shift Enter for newline</span><button onClick={submit} disabled={!draft.trim() || sending}>Run <i>↑</i></button></div></div></div>;
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
  const [busy, setBusy] = useState(false); const [status, setStatus] = useState(''); const [editing, setEditing] = useState<Provider | null>(providers[0] ?? null); const defaultName = useMemo(() => providers.length ? `Provider ${providers.length + 1}` : 'Primary model', [providers.length]);
  async function save(event: FormEvent<HTMLFormElement>) { event.preventDefault(); setBusy(true); setStatus(''); const form = new FormData(event.currentTarget); try { const saved = await request<Provider>(editing ? `/providers/${editing.id}` : '/providers', { method: editing ? 'PUT' : 'POST', body: JSON.stringify(Object.fromEntries(form)) }); onSaved(saved); setEditing(saved); setStatus('Provider saved. Run Test to verify the model ID.'); } catch (error) { setStatus(error instanceof Error ? error.message : 'Could not save provider'); } finally { setBusy(false); } }
  async function test(id: string) { setStatus('Testing provider…'); try { await request(`/providers/${id}/test`, { method: 'POST' }); setStatus('Connection healthy.'); } catch (error) { setStatus(error instanceof Error ? error.message : 'Connection failed'); } }
  return <div className="modal-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}><section className="settings-modal"><header><div><span className="eyebrow">SYSTEM / PROVIDERS</span><h2>Model connections</h2></div><button onClick={onClose}>×</button></header><p className="modal-lead">Connect an API model. Credentials are encrypted at rest by the local Go service and are never returned to the browser.</p>{!!providers.length && <div className="provider-list">{providers.map((item) => <div key={item.id}><span className="model-icon">M</span><div><b>{item.name}</b><small>{item.model} · {item.baseUrl}</small></div><div className="provider-actions"><button onClick={() => { setEditing(item); setStatus(''); }}>Edit</button><button onClick={() => test(item.id)}>Test</button></div></div>)}</div>}<form key={editing?.id ?? 'new'} onSubmit={save} className="provider-form"><p>{editing ? 'EDIT OPENAI-COMPATIBLE PROVIDER' : 'ADD OPENAI-COMPATIBLE PROVIDER'}</p><div className="form-grid"><label>CONNECTION NAME<input name="name" defaultValue={editing?.name ?? defaultName} required /></label><label>MODEL ID<input name="model" defaultValue={editing?.model ?? ''} placeholder="gpt-5 / deepseek-chat" required /></label><label className="wide">BASE URL<input name="baseUrl" defaultValue={editing?.baseUrl ?? 'https://api.openai.com/v1'} required /></label><label className="wide">API KEY<input name="apiKey" type="password" placeholder={editing ? 'Leave blank to keep the existing key' : 'sk-…'} required={!editing} /></label></div><input type="hidden" name="kind" value="openai-compatible"/>{status && <div className="form-status">{status}</div>}<button className="primary-button" disabled={busy}>{busy ? 'Encrypting…' : editing ? 'Update connection' : 'Save connection'}<span>→</span></button>{editing && <button type="button" className="text-button" onClick={() => { setEditing(null); setStatus(''); }}>Add another provider instead</button>}</form></section></div>;
}
