'use client';

import { useCallback, useEffect, useState, useSyncExternalStore } from 'react';
import { Download, WifiOff, X } from 'lucide-react';
import { useDialogA11y } from './useDialogA11y';

interface InstallPrompt extends Event {
  prompt(): Promise<void>;
  userChoice: Promise<{ outcome: 'accepted' | 'dismissed' }>;
}

function browserState() {
  if (window.oDesktop) return 0;
  const standalone = window.matchMedia('(display-mode: standalone)').matches || !!(navigator as Navigator & { standalone?: boolean }).standalone;
  return 1 | (navigator.onLine ? 2 : 0) | (standalone ? 4 : 0);
}
function subscribeBrowserState(onChange: () => void) {
  const display = window.matchMedia('(display-mode: standalone)');
  display.addEventListener('change', onChange);
  window.addEventListener('online', onChange);
  window.addEventListener('offline', onChange);
  return () => {
    display.removeEventListener('change', onChange);
    window.removeEventListener('online', onChange);
    window.removeEventListener('offline', onChange);
  };
}
const serverBrowserState = () => 0;

export default function WebAppControls() {
  const browser = useSyncExternalStore(subscribeBrowserState, browserState, serverBrowserState);
  const isWeb = (browser & 1) !== 0;
  const online = (browser & 2) !== 0;
  const standalone = (browser & 4) !== 0;
  const [installPrompt, setInstallPrompt] = useState<InstallPrompt | null>(null);
  const [helpOpen, setHelpOpen] = useState(false);
  const [error, setError] = useState('');
  const [prompting, setPrompting] = useState(false);

  useEffect(() => {
    if (window.oDesktop || !window.visualViewport) return;
    const viewport = window.visualViewport;
    const updateHeight = () => {
      if (window.matchMedia('(max-width: 720px), (max-height: 500px) and (pointer: coarse)').matches && viewport.scale === 1) {
        document.documentElement.style.setProperty('--o-web-viewport-height', `${viewport.height}px`);
      } else {
        document.documentElement.style.removeProperty('--o-web-viewport-height');
      }
    };
    updateHeight();
    viewport.addEventListener('resize', updateHeight);
    window.addEventListener('resize', updateHeight);
    return () => {
      viewport.removeEventListener('resize', updateHeight);
      window.removeEventListener('resize', updateHeight);
      document.documentElement.style.removeProperty('--o-web-viewport-height');
    };
  }, []);

  useEffect(() => {
    if (window.oDesktop) return;
    const onPrompt = (event: Event) => {
      event.preventDefault();
      setInstallPrompt(event as InstallPrompt);
    };
    const onInstalled = () => setInstallPrompt(null);
    window.addEventListener('beforeinstallprompt', onPrompt);
    window.addEventListener('appinstalled', onInstalled);
    let mounted = true;
    if ('serviceWorker' in navigator && window.isSecureContext) {
      navigator.serviceWorker.register('/sw.js', { scope: '/', updateViaCache: 'none' }).catch(() => {
        if (mounted) setError('离线提示组件未能启用。你仍可在线使用 O，恢复网络后刷新重试。');
      });
    }
    return () => {
      mounted = false;
      window.removeEventListener('beforeinstallprompt', onPrompt);
      window.removeEventListener('appinstalled', onInstalled);
    };
  }, []);

  const closeHelp = useCallback(() => setHelpOpen(false), []);
  async function install() {
    if (!installPrompt) { setHelpOpen(true); return; }
    setPrompting(true);
    try {
      await installPrompt.prompt();
      await installPrompt.userChoice;
      // The browser owns installation. Do not infer success from acceptance.
      setInstallPrompt(null);
    } catch {
      setError('浏览器未能打开安装窗口，可按下方步骤添加到主屏幕。');
      setHelpOpen(true);
    } finally {
      setPrompting(false);
    }
  }

  if (!isWeb) return null;
  return (
    <div className="web-app-controls">
      {!online && <span className="web-network-notice" role="status" title="当前设备离线。任务是否继续运行，以执行电脑上的状态为准；恢复网络后重新查看。"><WifiOff size={15} aria-hidden="true" />设备离线</span>}
      {(!standalone || error) && <button type="button" className="web-install-button" onClick={() => void install()} disabled={prompting} title={error || '添加到主屏幕'}><Download size={15} aria-hidden="true" />{error ? '安装帮助' : '添加到主屏幕'}</button>}
      {helpOpen && <InstallHelp onClose={closeHelp} error={error} secure={window.isSecureContext} />}
    </div>
  );
}

function InstallHelp({ onClose, error, secure }: { onClose: () => void; error: string; secure: boolean }) {
  const ref = useDialogA11y(onClose);
  return (
    <div className="modal-backdrop web-install-backdrop" onClick={onClose}>
      <section className="web-install-dialog" ref={ref} role="dialog" aria-modal="true" aria-labelledby="web-install-title" tabIndex={-1} onClick={(event) => event.stopPropagation()}>
        <header><h2 id="web-install-title">把 O 放到主屏幕</h2><button type="button" onClick={onClose} aria-label="关闭安装说明"><X size={20} aria-hidden="true" /></button></header>
        <p>用主屏幕图标打开 O，直接回到会话和任务工作台。</p>
        {error && <p className="web-install-error" role="alert">{error}</p>}
        {!secure && <p className="web-install-error">当前地址不支持完整安装。请通过 HTTPS 地址打开；本机开发可使用 localhost。</p>}
        <dl>
          <dt>iPhone / iPad</dt><dd>在 Safari 打开此页面，点分享，选择「添加到主屏幕」，按提示确认。</dd>
          <dt>Android</dt><dd>在 Chrome 打开此页面，从菜单选择「安装应用」或「添加到主屏幕」。</dd>
          <dt>电脑</dt><dd>在 Chrome 或 Edge 使用地址栏中的安装入口，也可以继续在浏览器使用。</dd>
        </dl>
        <p className="web-install-note">发起任务和查看最新状态需要网络。关闭页面后，任务状态以执行电脑为准。</p>
        <button className="web-install-done" type="button" onClick={onClose}>知道了</button>
      </section>
    </div>
  );
}
