'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { APIError, request } from './api';

type AuthStatus = {
 authenticationRequired: boolean; setupRequired: boolean; registrationAvailable: boolean;
 passwordResetAvailable: boolean; turnstileSiteKey: string; loginChallengeRequired: boolean;
 minimumPasswordLength: number; accountMode: 'personal';
};
type View = 'checking' | 'open' | 'login' | 'register' | 'reset' | 'setup' | 'verify-register' | 'verify-reset' | 'connection-error';
type Turnstile = {
 render(element: HTMLElement, options: {sitekey: string; action: string; size: string; callback: (token: string) => void; 'expired-callback': () => void; 'error-callback': () => void}): string;
 remove(id: string): void;
};
declare global { interface Window { turnstile?: Turnstile } }
let turnstileLoader: Promise<void> | undefined;

function loadTurnstile() {
 if (window.turnstile) return Promise.resolve();
 if (!turnstileLoader) {
  turnstileLoader = new Promise<void>((resolve, reject) => {
   const script = document.createElement('script');
   script.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit';
   script.async = true;
   script.onload = () => window.turnstile ? resolve() : reject(new Error('人机验证组件未加载，请重试'));
   script.onerror = () => { script.remove(); reject(new Error('无法连接人机验证服务，请检查网络并重试')); };
   document.head.appendChild(script);
  }).catch((error: unknown) => { turnstileLoader = undefined; throw error; });
 }
 return turnstileLoader;
}

function HumanChallenge({sitekey, action, version, onToken}: {sitekey: string; action: string; version: number; onToken: (token: string) => void}) {
 const container = useRef<HTMLDivElement>(null);
 const [error, setError] = useState('');
 const [retry, setRetry] = useState(0);
 useEffect(() => {
  let cancelled = false;
  let widget: string | undefined;
  onToken('');
  void loadTurnstile().then(() => {
   if (cancelled || !container.current || !window.turnstile) return;
   widget = window.turnstile.render(container.current, {
    sitekey, action, size: 'flexible',
    callback: (token) => { if (!cancelled) { setError(''); onToken(token); } },
    'expired-callback': () => { if (!cancelled) onToken(''); },
    'error-callback': () => { if (!cancelled) { onToken(''); setError('人机验证未完成，请重试'); } },
   });
  }).catch((cause: unknown) => { if (!cancelled) setError(cause instanceof Error ? cause.message : '人机验证加载失败'); });
  return () => { cancelled = true; if (widget !== undefined) window.turnstile?.remove(widget); };
 }, [sitekey, action, version, retry, onToken]);
 return <div><div ref={container} aria-label="人机验证" />{error && <div role="alert" className="auth-error">{error}<button type="button" className="auth-link" onClick={() => { setError(''); setRetry((value) => value + 1); }}>重试</button></div>}</div>;
}

export default function AuthenticationGate({children}: {children: (options: {onLogout: () => Promise<void>; remoteAuthenticated: boolean}) => ReactNode}) {
 const [view, setView] = useState<View>('checking');
 const [status, setStatus] = useState<AuthStatus | null>(null);
 const [email, setEmail] = useState('');
 const [password, setPassword] = useState('');
 const [displayName, setDisplayName] = useState('');
 const [bootstrapToken, setBootstrapToken] = useState('');
 const [code, setCode] = useState('');
 const [challengeId, setChallengeId] = useState('');
 const [challengeToken, setChallengeToken] = useState('');
 const [challengeVersion, setChallengeVersion] = useState(0);
 const [error, setError] = useState('');
 const [notice, setNotice] = useState('');
 const [busy, setBusy] = useState(false);

 const acceptStatus = useCallback(async (readBack: AuthStatus, signal?: AbortSignal) => {
  if (signal?.aborted) return;
  setStatus(readBack);
  if (!readBack.authenticationRequired) { setView('open'); return; }
  try {
   await request('/auth/session', {signal});
   if (!signal?.aborted) setView('open');
  } catch (cause) {
   if (signal?.aborted) return;
   if (!(cause instanceof APIError) || cause.status !== 401) throw cause;
   setView(readBack.registrationAvailable ? 'register' : 'login');
  }
 }, []);
 const connectionError = useCallback((cause: unknown) => { setError(cause instanceof Error ? cause.message : '连接 O 服务失败'); setView('connection-error'); }, []);
 const check = () => request<AuthStatus>('/auth/status').then((value) => acceptStatus(value)).catch(connectionError);
 useEffect(() => {
  const controller = new AbortController();
  void request<AuthStatus>('/auth/status', {signal: controller.signal})
   .then((value) => acceptStatus(value, controller.signal))
   .catch((cause: unknown) => { if (!controller.signal.aborted) connectionError(cause); });
  const expired = () => { setError('登录会话已过期，请重新登录'); setPassword(''); setView('login'); };
  window.addEventListener('o:authentication-required', expired);
  return () => { controller.abort(); window.removeEventListener('o:authentication-required', expired); };
 }, [acceptStatus, connectionError]);

 function switchView(next: View) {
  setView(next); setError(''); setNotice(''); setPassword(''); setCode(''); setChallengeId(''); setChallengeToken(''); setChallengeVersion((n) => n + 1);
 }
 const logout = useCallback(async () => {
  await request('/auth/logout', {method: 'POST'});
  try { await request('/auth/session'); throw new Error('服务端仍接受原登录会话，退出未确认'); }
  catch (cause) { if (!(cause instanceof APIError) || cause.status !== 401) throw cause; }
  setPassword(''); setView('login'); setNotice('已退出登录');
 }, []);

 const verify = view === 'verify-register' || view === 'verify-reset';
 const action = view === 'register' ? 'register' : view === 'reset' ? 'reset' : 'login';
 const needsHuman = Boolean(status?.turnstileSiteKey) && (view === 'register' || view === 'reset' || (view === 'login' && status?.loginChallengeRequired));
 const newPassword = view === 'register' || view === 'setup' || view === 'verify-reset';
 const passwordVisible = view === 'login' || newPassword;
 const title = view === 'register' ? '创建你的 O 账户' : view === 'reset' ? '找回密码' : view === 'setup' ? '管理员初始设置' : verify ? '验证邮箱' : '登录 O';

 async function submit(event: FormEvent<HTMLFormElement>) {
  event.preventDefault();
  if (newPassword && Array.from(password).length < (status?.minimumPasswordLength ?? 15)) { setError('密码至少 15 个字符，可使用密码管理器生成'); return; }
  if (newPassword && new TextEncoder().encode(password).length > 1024) { setError('密码过长，最多 1024 个 UTF-8 字节'); return; }
  if (needsHuman && !challengeToken) { setError('请先完成人机验证'); return; }
  setBusy(true); setError(''); setNotice('');
  try {
   if (view === 'register' || view === 'reset') {
    const receipt = await request<{challengeId: string; status: string; message: string}>(view === 'register' ? '/auth/register/start' : '/auth/password-reset/start', {method: 'POST', body: JSON.stringify({email, ...(view === 'register' ? {password, displayName} : {}), challengeToken})});
    if (receipt.status !== 'verification_requested' || !receipt.challengeId) throw new Error('验证码申请状态无法确认');
    setChallengeId(receipt.challengeId); setCode(''); setPassword('');
    setNotice(receipt.message); setView(view === 'register' ? 'verify-register' : 'verify-reset');
    return;
   }
   if (view === 'verify-reset') {
    const result = await request<{status: string; message: string}>('/auth/password-reset/verify', {method: 'POST', body: JSON.stringify({challengeId, code, password})});
    if (result.status !== 'password_reset') throw new Error('密码修改状态未确认');
    setPassword(''); setView('login'); setNotice(result.message); return;
   }
   if (view === 'verify-register') {
    await request('/auth/register/verify', {method: 'POST', body: JSON.stringify({challengeId, code})});
   } else if (view === 'setup') {
    await request('/auth/setup', {method: 'POST', headers: {'X-O-Bootstrap-Token': bootstrapToken}, body: JSON.stringify({email, displayName, password})});
   } else {
    await request('/auth/login', {method: 'POST', body: JSON.stringify({email, password, challengeToken})});
   }
   await request('/auth/session');
   setPassword(''); setBootstrapToken(''); setView('open');
  } catch (cause) {
   if (cause instanceof APIError && cause.challengeRequired) setStatus((previous) => previous ? {...previous, loginChallengeRequired: true} : previous);
   setError(cause instanceof Error ? cause.message : '身份验证失败');
  } finally { setBusy(false); setChallengeToken(''); setChallengeVersion((n) => n + 1); }
 }

 if (view === 'open') return children({onLogout: logout, remoteAuthenticated: Boolean(status?.authenticationRequired)});
 if (view === 'checking') return <main className="auth-shell"><p role="status">正在连接 O…</p></main>;
 if (view === 'connection-error') return <main className="auth-shell"><section className="auth-card"><h1>暂时无法连接 O</h1><p role="alert" className="auth-error">{error}</p><button className="auth-primary" onClick={() => { setView('checking'); setError(''); void check(); }}>重新连接</button></section></main>;
 return <main className="auth-shell"><form className="auth-card" onSubmit={submit}>
  <div className="auth-brand">O</div><h1>{title}</h1>
  <p className="auth-description">{verify ? `输入发送到 ${email} 的 8 位验证码。` : view === 'setup' ? '使用服务器引导密钥初始化主账户。' : '登录后选择云端 Linux 或已连接的电脑，开始对话。'}</p>
  {view === 'setup' && <label>引导密钥<input required type="password" autoComplete="off" value={bootstrapToken} onChange={(e) => setBootstrapToken(e.target.value)} /></label>}
  {(view === 'register' || view === 'setup') && <label>名称<input value={displayName} maxLength={200} autoComplete="nickname" onChange={(e) => setDisplayName(e.target.value)} /></label>}
  {!verify && <label>邮箱<input required type="email" autoComplete="username" inputMode="email" value={email} onChange={(e) => setEmail(e.target.value)} /></label>}
  {verify && <label>邮箱验证码<input required pattern="[0-9]{8}" maxLength={8} inputMode="numeric" autoComplete="one-time-code" value={code} onChange={(e) => setCode(e.target.value.replace(/[^0-9]/g, ''))} /></label>}
  {passwordVisible && <div className="auth-field"><label htmlFor="auth-password">{view === 'verify-reset' ? '新密码' : '密码'}</label><input id="auth-password" required type="password" autoComplete={newPassword ? 'new-password' : 'current-password'} aria-describedby={newPassword ? 'auth-password-help' : undefined} value={password} onChange={(e) => setPassword(e.target.value)} />{newPassword && <small id="auth-password-help">至少 15 个字符，支持密码管理器和粘贴。</small>}</div>}
  {needsHuman && <HumanChallenge sitekey={status!.turnstileSiteKey} action={action} version={challengeVersion} onToken={setChallengeToken} />}
  {notice && <p role="status" className="auth-notice">{notice}</p>}
  {error && <p role="alert" className="auth-error">{error}</p>}
  <button disabled={busy || (needsHuman && !challengeToken)} className="auth-primary" type="submit">{busy ? '请稍候…' : verify ? view === 'verify-reset' ? '验证并更新密码' : '验证并登录' : view === 'register' || view === 'reset' ? '获取邮箱验证码' : view === 'setup' ? '创建账户并登录' : '登录'}</button>
  <nav className="auth-links" aria-label="账户操作">
   {view !== 'login' && <button disabled={busy} type="button" onClick={() => switchView('login')}>返回登录</button>}
   {view === 'login' && status?.registrationAvailable && <button disabled={busy} type="button" onClick={() => switchView('register')}>注册账户</button>}
   {view === 'login' && status?.passwordResetAvailable && <button disabled={busy} type="button" onClick={() => switchView('reset')}>忘记密码</button>}
   {verify && <button disabled={busy} type="button" onClick={() => switchView(view === 'verify-register' ? 'register' : 'reset')}>重新申请验证码</button>}
   {view === 'login' && status?.setupRequired && <button disabled={busy} type="button" onClick={() => switchView('setup')}>管理员初始设置</button>}
  </nav>
  {status?.setupRequired && !status.registrationAvailable && <p className="auth-footnote">此个人服务器尚未初始化。管理员可配置注册邮箱与邮件服务，或使用引导密钥完成初始设置。</p>}
  <p className="auth-footnote">一个主账户管理多台电脑。本地单机使用无需登录。</p>
 </form></main>;
}
