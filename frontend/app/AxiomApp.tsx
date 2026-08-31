'use client';

import { FormEvent, useCallback, useEffect, useMemo, useState } from 'react';

type User = { id: string; email: string; displayName: string };
type Provider = { id: string; name: string; kind: string; baseUrl: string; model: string; hasApiKey: boolean };
type Conversation = { id: string; title: string; providerId: string; updatedAt: string };
type Message = { id: string; role: 'user' | 'assistant'; content: string; createdAt: string };
type ConversationDetail = Conversation & { messages: Message[] };
type Plugin = { id: string; version: string; description: string; state: string; capabilities: string[] };

const API = process.env.NEXT_PUBLIC_API_URL ?? 'http://127.0.0.1:8080/api/v1';

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
  const [sending, setSending] = useState(false);
  const [notice, setNotice] = useState('');

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
  async function openConversation(id: string) { setActive(await request<ConversationDetail>(`/conversations/${id}`)); }
  async function newConversation() {
    if (!providers.length) { setSettingsOpen(true); return; }
    const created = await request<Conversation>('/conversations', { method: 'POST', body: JSON.stringify({ title: 'New mission', providerId: providers[0].id }) });
    setConversations((items) => [created, ...items]); setActive({ ...created, messages: [] });
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
      setActive(refreshed); setConversations((items) => items.map((item) => item.id === refreshed.id ? refreshed : item));
    } catch (error) { setNotice(error instanceof Error ? error.message : 'Agent turn failed'); }
    finally { setSending(false); }
  }

  if (loading) return <Splash />;
  if (!user) return <AuthScreen onAuthenticated={async (next) => { setUser(next); await hydrate(); }} />;
  return <main className="app-shell">
    <Sidebar user={user} conversations={conversations} activeId={active?.id} onNew={newConversation} onOpen={openConversation} onSettings={() => setSettingsOpen(true)} onLogout={async () => { await request('/auth/logout', { method: 'POST' }); setUser(null); }} />
    <section className="workspace"><header className="workspace-header"><div><span className="eyebrow">LOCAL AGENT / MISSION</span><h1>{active?.title ?? 'Untitled workspace'}</h1></div><div className="header-actions"><span className="status-pill"><i /> runtime online</span><button className="icon-button" onClick={() => setSettingsOpen(true)} aria-label="Open settings">⌘</button></div></header>
      <div className="workspace-grid"><Chat active={active} providers={providers} sending={sending} notice={notice} onSend={send} onConfigure={() => setSettingsOpen(true)} /><RuntimePanel plugins={plugins} provider={providers.find((p) => p.id === active?.providerId) ?? providers[0]} messageCount={active?.messages.length ?? 0} /></div>
    </section>
    {settingsOpen && <Settings providers={providers} onClose={() => setSettingsOpen(false)} onSaved={(next) => { setProviders((items) => items.some((item) => item.id === next.id) ? items.map((item) => item.id === next.id ? next : item) : [next, ...items]); setNotice('Provider saved'); }} />}
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

function Sidebar({ user, conversations, activeId, onNew, onOpen, onSettings, onLogout }: { user: User; conversations: Conversation[]; activeId?: string; onNew: () => void; onOpen: (id: string) => void; onSettings: () => void; onLogout: () => void }) {
  return <aside className="sidebar"><div className="brand"><div className="brand-mark">A</div><span>AXIOM</span><small>0.1</small></div><button className="new-button" onClick={onNew}><span>＋</span> New mission <kbd>⌘ N</kbd></button><nav><p>MISSIONS</p>{conversations.length === 0 ? <div className="empty-nav">No missions yet.<br/>Start with an objective.</div> : conversations.map((item) => <button key={item.id} className={item.id === activeId ? 'active' : ''} onClick={() => onOpen(item.id)}><i>◫</i><span>{item.title}</span></button>)}</nav><div className="sidebar-foot"><button onClick={onSettings}><i>⚙</i><span>Provider settings</span></button><div className="operator"><span>{user.displayName.slice(0,2).toUpperCase()}</span><div><b>{user.displayName}</b><small>{user.email}</small></div><button onClick={onLogout} title="Sign out">↗</button></div></div></aside>;
}

function Chat({ active, providers, sending, notice, onSend, onConfigure }: { active: ConversationDetail | null; providers: Provider[]; sending: boolean; notice: string; onSend: (content: string) => void; onConfigure: () => void }) {
  const [draft, setDraft] = useState(''); const suggestions = ['Review this repository architecture', 'Design a reliable execution plan', 'Trace a bug with evidence']; const submit = () => { const value = draft; if (value.trim()) { setDraft(''); onSend(value); } };
  return <div className="chat-column"><div className="messages">{!active?.messages.length ? <div className="empty-chat"><div className="pulse-orbit"><span>A</span></div><span className="eyebrow">AGENT READY</span><h2>What are we building?</h2><p>Give Axiom a concrete outcome. The runtime will retain the mission state and route it through your configured model.</p>{!providers.length ? <button className="setup-card" onClick={onConfigure}><span>01</span><div><b>Connect a model provider</b><small>Add any OpenAI-compatible API endpoint</small></div><i>→</i></button> : <div className="suggestions">{suggestions.map((text) => <button key={text} onClick={() => setDraft(text)}>{text}<span>↗</span></button>)}</div>}</div> : active.messages.map((message) => <article key={message.id} className={`message ${message.role}`}><div className="message-role">{message.role === 'user' ? 'YOU' : 'AXIOM'}</div><div className="message-body">{message.content}</div></article>)}{sending && <article className="message assistant"><div className="message-role">AXIOM</div><div className="thinking"><i/><i/><i/> reasoning</div></article>}</div>{notice && <div className="notice">{notice}</div>}<div className="composer"><textarea value={draft} onChange={(e) => setDraft(e.target.value)} onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); } }} placeholder={providers.length ? 'Describe the outcome, constraints, or next move…' : 'Configure a model provider to begin…'} disabled={!providers.length || sending}/><div className="composer-row"><span>Enter to run · Shift Enter for newline</span><button onClick={submit} disabled={!draft.trim() || sending}>Run <i>↑</i></button></div></div></div>;
}

function RuntimePanel({ plugins, provider, messageCount }: { plugins: Plugin[]; provider?: Provider; messageCount: number }) {
  const running = plugins.filter((item) => item.state === 'running').length;
  return <aside className="runtime-panel"><div className="panel-title"><div><span className="eyebrow">LIVE INSPECTOR</span><h3>Runtime</h3></div><span className="live-dot">LIVE</span></div><div className="runtime-metric"><span>Plugin graph</span><b>{running}<small> / {plugins.length} running</small></b><div className="meter"><i style={{width: plugins.length ? `${running/plugins.length*100}%` : '0%'}}/></div></div><div className="runtime-section"><p>ACTIVE MODEL</p>{provider ? <div className="model-card"><div className="model-icon">M</div><div><b>{provider.model}</b><small>{provider.name} · API</small></div><i>●</i></div> : <div className="muted-card">No provider mounted</div>}</div><div className="runtime-section"><p>TURN STATE</p><dl><div><dt>Phase</dt><dd>{messageCount ? 'checkpointed' : 'idle'}</dd></div><div><dt>Messages</dt><dd>{messageCount}</dd></div><div><dt>Policy</dt><dd>runtime.agent.v1</dd></div></dl></div><div className="runtime-section plugin-list"><p>PLUGIN GRAPH</p>{plugins.slice(0,6).map((item) => <div key={item.id}><i className={item.state}/><span>{item.id.replace('core.','').replace('.v1','')}</span><small>{item.version}</small></div>)}</div><div className="runtime-foot"><span>STATE</span><b>LOCAL / ENCRYPTED</b></div></aside>;
}

function Settings({ providers, onClose, onSaved }: { providers: Provider[]; onClose: () => void; onSaved: (provider: Provider) => void }) {
  const [busy, setBusy] = useState(false); const [status, setStatus] = useState(''); const [editing, setEditing] = useState<Provider | null>(providers[0] ?? null); const defaultName = useMemo(() => providers.length ? `Provider ${providers.length + 1}` : 'Primary model', [providers.length]);
  async function save(event: FormEvent<HTMLFormElement>) { event.preventDefault(); setBusy(true); setStatus(''); const form = new FormData(event.currentTarget); try { const saved = await request<Provider>(editing ? `/providers/${editing.id}` : '/providers', { method: editing ? 'PUT' : 'POST', body: JSON.stringify(Object.fromEntries(form)) }); onSaved(saved); setEditing(saved); setStatus('Provider saved. Run Test to verify the model ID.'); } catch (error) { setStatus(error instanceof Error ? error.message : 'Could not save provider'); } finally { setBusy(false); } }
  async function test(id: string) { setStatus('Testing provider…'); try { await request(`/providers/${id}/test`, { method: 'POST' }); setStatus('Connection healthy.'); } catch (error) { setStatus(error instanceof Error ? error.message : 'Connection failed'); } }
  return <div className="modal-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}><section className="settings-modal"><header><div><span className="eyebrow">SYSTEM / PROVIDERS</span><h2>Model connections</h2></div><button onClick={onClose}>×</button></header><p className="modal-lead">Connect an API model. Credentials are encrypted at rest by the local Go service and are never returned to the browser.</p>{!!providers.length && <div className="provider-list">{providers.map((item) => <div key={item.id}><span className="model-icon">M</span><div><b>{item.name}</b><small>{item.model} · {item.baseUrl}</small></div><div className="provider-actions"><button onClick={() => { setEditing(item); setStatus(''); }}>Edit</button><button onClick={() => test(item.id)}>Test</button></div></div>)}</div>}<form key={editing?.id ?? 'new'} onSubmit={save} className="provider-form"><p>{editing ? 'EDIT OPENAI-COMPATIBLE PROVIDER' : 'ADD OPENAI-COMPATIBLE PROVIDER'}</p><div className="form-grid"><label>CONNECTION NAME<input name="name" defaultValue={editing?.name ?? defaultName} required /></label><label>MODEL ID<input name="model" defaultValue={editing?.model ?? ''} placeholder="gpt-5 / deepseek-chat" required /></label><label className="wide">BASE URL<input name="baseUrl" defaultValue={editing?.baseUrl ?? 'https://api.openai.com/v1'} required /></label><label className="wide">API KEY<input name="apiKey" type="password" placeholder={editing ? 'Leave blank to keep the existing key' : 'sk-…'} required={!editing} /></label></div><input type="hidden" name="kind" value="openai-compatible"/>{status && <div className="form-status">{status}</div>}<button className="primary-button" disabled={busy}>{busy ? 'Encrypting…' : editing ? 'Update connection' : 'Save connection'}<span>→</span></button>{editing && <button type="button" className="text-button" onClick={() => { setEditing(null); setStatus(''); }}>Add another provider instead</button>}</form></section></div>;
}
