 'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { UserRound, X } from 'lucide-react';
import { APIError, request } from './api';
import { useDialogA11y } from './useDialogA11y';

type Account = {
  state: string; serverUrl: string; deviceName: string;
  user?: { email: string; displayName: string };
  verificationUrl?: string; error?: string; pendingRevocations: number;
};
const labels: Record<string, string> = {
  unavailable: '账户恢复失败', disconnecting: '正在停止远程任务', signed_out: '未登录',
  awaiting_login: '等待浏览器登录', login_expired: '登录已过期', connecting: '正在连接服务器',
  connected: '已连接', offline: '离线，正在重连', login_required: '需要重新登录',
};

export default function DeviceAccount() {
  const [account, setAccount] = useState<Account | null>(null);
  const [open, setOpen] = useState(false);
  const [error, setError] = useState('');
  const readEpoch = useRef(0);
  const close = useCallback(() => setOpen(false), []);
  const observeMutation = useCallback((state: Account) => {
    readEpoch.current++;
    setAccount(state);
    setError('');
  }, []);
  useEffect(() => {
    let stopped = false, unsupported = false, reading = false;
    async function refresh() {
      if (unsupported || reading) return;
      reading = true;
      const epoch = readEpoch.current;
      try {
        const state = await request<Account>('/account');
        if (!stopped && epoch === readEpoch.current) { setAccount(state); setError(''); }
      } catch (cause) {
        if (cause instanceof APIError && cause.status === 404) unsupported = true;
        else if (!stopped && epoch === readEpoch.current) setError(cause instanceof Error ? cause.message : '账户状态读取失败');
      } finally { reading = false; }
    }
    const initial = setTimeout(() => void refresh(), 0), timer = setInterval(() => void refresh(), 2000);
    return () => { stopped = true; clearTimeout(initial); clearInterval(timer); };
  }, []);
  if (!account && !error) return null;
  return <>
    <button type="button" className="sidebar-toggle-button" onClick={() => setOpen(true)} aria-label="账户与远程连接"
      title={error ? '账户状态读取失败' : account?.user ? `${account.user.email} · ${labels[account.state] ?? account.state}` : '登录账户，连接云端'}>
      <UserRound size={17} aria-hidden="true" />
    </button>
    {open && <AccountDialog account={account} error={error} onState={observeMutation} onClose={close} />}
  </>;
}

function AccountDialog({ account, error, onState, onClose }: {
  account: Account | null; error: string; onState: (a: Account) => void; onClose: () => void;
}) {
  const ref = useDialogA11y(onClose);
  const [server, setServer] = useState(account?.serverUrl ?? '');
  const [name, setName] = useState(account?.deviceName ?? '');
  const [busy, setBusy] = useState(false), [failure, setFailure] = useState('');
  async function openLogin(url: string) {
    if (window.oDesktop?.openExternal) await window.oDesktop.openExternal(url);
  }
  async function login(event: FormEvent) {
    event.preventDefault(); setBusy(true); setFailure('');
    try {
      const state = await request<Account>('/account/login', { method: 'POST', body: JSON.stringify({ serverUrl: server, deviceName: name }) });
      onState(state);
      if (!state.verificationUrl) throw new Error('未收到登录页面');
      if (window.oDesktop?.openExternal) await openLogin(state.verificationUrl);
    } catch (cause) { setFailure(cause instanceof Error ? cause.message : '登录失败'); }
    finally { setBusy(false); }
  }
  async function logout() {
    setBusy(true); setFailure('');
    try {
      await request<Account>('/account/logout', { method: 'POST', body: '{}' });
      const state = await request<Account>('/account');
      if (state.user || state.verificationUrl || state.state !== 'signed_out') throw new Error('退出状态未能确认');
      onState(state);
    } catch (cause) { setFailure(cause instanceof Error ? cause.message : '退出失败'); }
    finally { setBusy(false); }
  }
  return <div className="modal-backdrop" onClick={onClose}>
    <section ref={ref} role="dialog" tabIndex={-1} aria-modal="true" aria-labelledby="device-account-title"
      className="device-account-dialog" onClick={event => event.stopPropagation()}>
      <div className="device-account-heading">
        <h2 id="device-account-title">账户与远程连接</h2>
        <button type="button" className="icon-button" onClick={onClose} aria-label="关闭"><X size={18} aria-hidden="true" /></button>
      </div>
      <p>不登录也可以使用本地 Agent。登录你的服务器账户后，这台电脑会自动出现在同一账户的设备列表中。</p>
      <p role="status">{error ? '账户状态读取失败' : account ? labels[account.state] ?? account.state : '读取账户状态中'}</p>
      {account?.user ? <>
        <strong>{account.user.displayName} · {account.user.email}</strong>
        <p>{account.serverUrl}<br />{account.deviceName}</p>
        <button type="button" className="upc-btn-secondary" disabled={busy} onClick={() => void logout()}>退出账户并断开远程连接</button>
      </> : <form onSubmit={login}>
        <label>服务器地址<input type="url" autoComplete="url" placeholder="https://your-server.example" value={server}
          onChange={event => setServer(event.target.value)} required disabled={busy || Boolean(account?.verificationUrl)} /></label>
        <label>这台电脑的名称<input value={name} onChange={event => setName(event.target.value)} required disabled={busy || Boolean(account?.verificationUrl)} /></label>
        <button type="submit" className="auth-primary" disabled={busy || Boolean(account?.verificationUrl) || account?.state === 'unavailable'}>
          {busy ? '正在连接…' : '登录账户'}
        </button>
      </form>}
      {account?.verificationUrl && <>
        <p>在浏览器中登录并确认连接后，回到这里即可。你可以继续使用本地 Agent。</p>
        {window.oDesktop?.openExternal ?
          <button type="button" className="upc-btn-secondary" onClick={() => void openLogin(account.verificationUrl!).catch(cause => setFailure(cause.message))}>重新打开登录页面</button> :
          <a href={account.verificationUrl} target="_blank" rel="noopener noreferrer">打开登录页面</a>}
        <button type="button" className="upc-btn-secondary" disabled={busy} onClick={() => void logout()}>取消登录</button>
      </>}
      {account?.pendingRevocations ? <p role="status">本机已断开。服务器上的旧设备凭据正在撤销，网络恢复后会继续处理。</p> : null}
      {(failure || error || account?.error) && <p role="alert" className="device-account-error">{failure || error || account?.error}</p>}
    </section>
  </div>;
}

type Approval = { name: string; platform: string; approved: boolean };
export function DeviceApproval() {
  const [code, setCode] = useState(''), [info, setInfo] = useState<Approval | null>(null);
  const [error, setError] = useState(''), [busy, setBusy] = useState(false), [approved, setApproved] = useState(false);
  useEffect(() => {
    let stopped = false;
    const value = new URLSearchParams(window.location.search).get('device_login') ?? '';
    const initial = setTimeout(() => {
      setCode(value);
      void request<Approval>(`/auth/device/approval?code=${encodeURIComponent(value)}`)
        .then(data => { if (!stopped) { setInfo(data); setApproved(data.approved); } })
        .catch(cause => { if (!stopped) setError(cause instanceof Error ? cause.message : '无法读取设备登录请求'); });
    }, 0);
    return () => { stopped = true; clearTimeout(initial); };
  }, []);
  async function approve() {
    setBusy(true); setError('');
    try {
      await request('/auth/device/approve', { method: 'POST', body: JSON.stringify({ code }) });
      const state = await request<Approval>(`/auth/device/approval?code=${encodeURIComponent(code)}`);
      if (!state.approved) throw new Error('服务器未确认设备授权');
      setInfo(state); setApproved(true);
    } catch (cause) { setError(cause instanceof Error ? cause.message : '授权失败'); }
    finally { setBusy(false); }
  }
  return <main className="auth-shell"><section className="auth-card">
    <div className="auth-brand">O</div><h1>{approved ? '设备已授权' : '连接你的电脑'}</h1>
    {info && <p>{info.name} · {info.platform}</p>}
    <p>{approved ? '请回到本地 O Agent 查看连接状态。可以关闭此页面。' : '确认后，这台电脑将接收同一账户从网页发送的任务。只确认你自己刚刚发起的请求。'}</p>
    {!approved && info && <button type="button" className="auth-primary" onClick={() => void approve()} disabled={busy}>{busy ? '正在确认…' : '确认连接'}</button>}
    {error && <p role="alert">{error}</p>}<a href="./">返回 O</a>
  </section></main>;
}
