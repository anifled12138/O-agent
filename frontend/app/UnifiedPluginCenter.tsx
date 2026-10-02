'use client';

import { FormEvent, useCallback, useEffect, useState } from 'react';
import { X, Plus, KeyRound, Check, Trash2 } from 'lucide-react';
import {
  UnifiedPlugin,
  McpServerConfig,
  WebSearchPluginSettings,
  ContextCompactionSettings,
  getUnifiedPlugins,
  toggleUnifiedPlugin,
  addMcpServerConfig,
  removeMcpServerConfig,
  getWebSearchPluginSettings,
  saveWebSearchPluginSettings,
  clearWebSearchPluginSettings,
  testWebSearchPlugin,
  getContextCompactionSettings,
  saveContextCompactionSettings,
} from './api';
import { useDialogA11y } from './useDialogA11y';

type PluginFilter = 'all' | 'model' | 'mcp' | 'skill' | 'core' | 'release';

function normalizeFilter(value?: string): PluginFilter {
  return value === 'model' || value === 'skill' || value === 'core' || value === 'release' || value === 'all' ? value : 'mcp';
}

export default function UnifiedPluginCenter({
  onClose,
  initialType = 'all',
  onPluginsChanged,
}: {
  onClose: () => void;
  initialType?: string;
  onPluginsChanged?: () => void;
}) {
  const dialogRef = useDialogA11y(onClose);
  const [plugins, setPlugins] = useState<UnifiedPlugin[]>([]);
  const [filterType, setFilterType] = useState<PluginFilter>(() => normalizeFilter(initialType));
  const [busy, setBusy] = useState<string>('');
  const [error, setError] = useState<string>('');
  const [showAddMcp, setShowAddMcp] = useState<boolean>(false);
  const [removeConfirmationMcp, setRemoveConfirmationMcp] = useState('');
  const [removeMcpConsent, setRemoveMcpConsent] = useState(false);
  const [enableConfirmationMcp, setEnableConfirmationMcp] = useState('');
  const [enableMcpConsent, setEnableMcpConsent] = useState(false);
  const [mcpLaunchConsent, setMcpLaunchConsent] = useState(false);
  const [showWebSearchSettings, setShowWebSearchSettings] = useState(false);
  const [webSearchSettings, setWebSearchSettings] = useState<WebSearchPluginSettings | null>(null);
  const [webSearchApiKey, setWebSearchApiKey] = useState('');
  const [webSearchBusy, setWebSearchBusy] = useState('');
  const [webSearchError, setWebSearchError] = useState('');
  const [webSearchMessage, setWebSearchMessage] = useState('');
  const [confirmWebSearchClear, setConfirmWebSearchClear] = useState(false);
  const [showContextCompactorSettings, setShowContextCompactorSettings] = useState(false);
  const [contextCompactorDraft, setContextCompactorDraft] = useState<ContextCompactionSettings | null>(null);
  const [contextCompactorBusy, setContextCompactorBusy] = useState(false);
  const [contextCompactorError, setContextCompactorError] = useState('');
  const [contextCompactorMessage, setContextCompactorMessage] = useState('');

  // Form states for adding MCP
  const [mcpId, setMcpId] = useState('');
  const [mcpName, setMcpName] = useState('');
  const [mcpCommand, setMcpCommand] = useState('');
  const [mcpArgs, setMcpArgs] = useState('');
  const [mcpEnv, setMcpEnv] = useState('');

  const closeAddMcp = useCallback(() => {
    setShowAddMcp(false);
    setMcpLaunchConsent(false);
  }, []);

  const closeWebSearchSettings = useCallback(() => {
    setShowWebSearchSettings(false);
    setWebSearchApiKey('');
    setConfirmWebSearchClear(false);
  }, []);

  const closeContextCompactorSettings = useCallback(() => setShowContextCompactorSettings(false), []);

  const loadPlugins = useCallback(async () => {
    try {
      const data = await getUnifiedPlugins();
      setPlugins(data);
      return data;
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载插件列表失败');
      throw err;
    }
  }, []);

  useEffect(() => {
    let cancelled = false;
    void getUnifiedPlugins().then(
      (data) => {
        if (!cancelled) setPlugins(data);
      },
      (err: unknown) => {
        if (!cancelled) setError(err instanceof Error ? err.message : '加载插件列表失败');
      },
    );
    return () => {
      cancelled = true;
    };
  }, []);

  // Handle Escape key specifically for the sub-modal to avoid closing parent
  useEffect(() => {
    function handleSubModalKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape' && (showAddMcp || showWebSearchSettings || showContextCompactorSettings)) {
        e.stopPropagation();
        if (showAddMcp) closeAddMcp();
        else if (showWebSearchSettings) closeWebSearchSettings();
        else closeContextCompactorSettings();
      }
    }
    if (showAddMcp || showWebSearchSettings || showContextCompactorSettings) {
      window.addEventListener('keydown', handleSubModalKeyDown, true);
      return () => window.removeEventListener('keydown', handleSubModalKeyDown, true);
    }
  }, [showAddMcp, showWebSearchSettings, showContextCompactorSettings, closeAddMcp, closeWebSearchSettings, closeContextCompactorSettings]);

  async function handleToggle(p: UnifiedPlugin) {
    const nextState = p.status !== 'enabled';
    const isMcpStart = p.type === 'mcp' && nextState;
    if (isMcpStart && (enableConfirmationMcp !== p.id || !enableMcpConsent)) {
      setEnableConfirmationMcp(p.id);
      setEnableMcpConsent(false);
      return;
    }
    setBusy(`toggle:${p.id}`);
    try {
      const updated = await toggleUnifiedPlugin(p.id, nextState, isMcpStart && enableMcpConsent);
      const data = await loadPlugins();
      const observed = data.find((item) => item.id === p.id);
      const expectedStatus = nextState ? 'enabled' : 'disabled';
      if (updated.status !== expectedStatus || observed?.status !== expectedStatus) {
        throw new Error('插件状态读回与请求不一致，请重试或检查运行状态');
      }
      if (isMcpStart) { setEnableConfirmationMcp(''); setEnableMcpConsent(false); }
      onPluginsChanged?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : '切换插件状态失败');
    } finally {
      setBusy('');
    }
  }

  async function handleAddMcp(e: FormEvent) {
    e.preventDefault();
    if (!mcpId.trim() || !mcpCommand.trim()) {
      setError('服务标识与执行命令为必填项');
      return;
    }
    setBusy('add-mcp');
    try {
      const args = mcpArgs.trim() ? mcpArgs.split(/\s+/).filter(Boolean) : [];
      const envObj: Record<string, string> = {};
      if (mcpEnv.trim()) {
        const lines = mcpEnv.split(/\r?\n/);
        for (const line of lines) {
          const parts = line.split('=');
          if (parts.length >= 2) {
            envObj[parts[0].trim()] = parts.slice(1).join('=').trim();
          }
        }
      }

      const cfg: McpServerConfig = {
        id: mcpId.trim(),
        name: mcpName.trim() || mcpId.trim(),
        command: mcpCommand.trim(),
        args,
        env: Object.keys(envObj).length > 0 ? envObj : undefined,
        enabled: true,
      };

      await addMcpServerConfig(cfg);
      closeAddMcp();
      setMcpLaunchConsent(false);
      setMcpId('');
      setMcpName('');
      setMcpCommand('');
      setMcpArgs('');
      setMcpEnv('');
      await loadPlugins();
    } catch (err) {
      setError(err instanceof Error ? err.message : '添加 MCP 服务失败');
    } finally {
      setBusy('');
    }
  }

  async function handleRemoveMcp(id: string) {
    if (removeConfirmationMcp !== id) {
      setRemoveConfirmationMcp(id);
      setRemoveMcpConsent(false);
      return;
    }
    if (!removeMcpConsent) return;
    setBusy(`remove:${id}`);
    try {
      await removeMcpServerConfig(id);
      setRemoveConfirmationMcp('');
      setRemoveMcpConsent(false);
      await loadPlugins();
    } catch (err) {
      setError(err instanceof Error ? err.message : '删除 MCP 服务失败');
    } finally {
      setBusy('');
    }
  }

  async function openWebSearchSettings() {
    setWebSearchError('');
    setWebSearchMessage('');
    setWebSearchApiKey('');
    setConfirmWebSearchClear(false);
    setWebSearchBusy('load');
    try {
      setWebSearchSettings(await getWebSearchPluginSettings());
      setShowWebSearchSettings(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : '读取联网搜索配置失败');
    } finally {
      setWebSearchBusy('');
    }
  }

  async function handleSaveWebSearchSettings(event: FormEvent) {
    event.preventDefault();
    if (!webSearchApiKey.trim()) {
      setWebSearchError('请填写 Exa API Key。');
      return;
    }
    setWebSearchBusy('save');
    setWebSearchError('');
    setWebSearchMessage('');
    try {
      const saved = await saveWebSearchPluginSettings(webSearchApiKey.trim());
      if (!saved.configured) throw new Error('服务端未确认 API Key 已保存');
      setWebSearchSettings(saved);
      setWebSearchApiKey('');
      setWebSearchMessage(`密钥已在本机加密保存${saved.keyHint ? `（${saved.keyHint}）` : ''}。`);
      let data: UnifiedPlugin[];
      try {
        data = await loadPlugins();
      } catch (refreshError) {
        setWebSearchError(`密钥已保存，但插件目录读取失败：${refreshError instanceof Error ? refreshError.message : '请关闭后重新打开插件中心'}`);
        return;
      }
      if (data.find((item) => item.id === 'core:web_search')?.metadata?.configured !== 'true') {
        setWebSearchError('密钥已保存，但插件目录没有读回配置状态；请关闭后重新打开插件中心。');
      }
    } catch (err) {
      setWebSearchError(err instanceof Error ? err.message : '保存联网搜索配置失败');
    } finally {
      setWebSearchBusy('');
    }
  }

  async function handleTestWebSearch() {
    setWebSearchBusy('test');
    setWebSearchError('');
    setWebSearchMessage('');
    try {
      const result = await testWebSearchPlugin();
      if (!result.passed || !result.settings.configured) throw new Error('Exa Search 尚未确认可用');
      setWebSearchSettings(result.settings);
      setWebSearchMessage(`连接正常，返回 ${result.resultCount} 条测试结果。此次测试消耗了一次搜索请求和相应的内容额度。`);
    } catch (err) {
      setWebSearchError(err instanceof Error ? err.message : '联网搜索连接测试失败');
    } finally {
      setWebSearchBusy('');
    }
  }

  async function handleClearWebSearchKey() {
    const pluginEnabled = plugins.find((item) => item.id === 'core:web_search')?.status === 'enabled';
    if (pluginEnabled) {
      setWebSearchError('请先停用联网搜索插件，再移除密钥。');
      return;
    }
    if (!confirmWebSearchClear) {
      setConfirmWebSearchClear(true);
      setWebSearchMessage('再次点击“确认移除密钥”以删除本机保存的密钥。');
      return;
    }
    setWebSearchBusy('clear');
    setWebSearchError('');
    setWebSearchMessage('');
    try {
      const cleared = await clearWebSearchPluginSettings();
      if (cleared.configured) throw new Error('服务端仍报告密钥已配置');
      setWebSearchSettings(cleared);
      setConfirmWebSearchClear(false);
      setWebSearchMessage('本机保存的 API Key 已移除。');
      try {
        await loadPlugins();
      } catch (refreshError) {
        setWebSearchError(`密钥已移除，但插件目录读取失败：${refreshError instanceof Error ? refreshError.message : '请关闭后重新打开插件中心'}`);
      }
    } catch (err) {
      setWebSearchError(err instanceof Error ? err.message : '移除联网搜索密钥失败');
    } finally {
      setWebSearchBusy('');
    }
  }

  async function openContextCompactorSettings() {
    setContextCompactorError('');
    setContextCompactorMessage('');
    setContextCompactorBusy(true);
    try {
      const settings = await getContextCompactionSettings();
      setContextCompactorDraft(settings);
      setShowContextCompactorSettings(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : '读取上下文压缩设置失败');
    } finally {
      setContextCompactorBusy(false);
    }
  }

  async function handleSaveContextCompactorSettings(event: FormEvent) {
    event.preventDefault();
    if (!contextCompactorDraft) return;
    setContextCompactorBusy(true);
    setContextCompactorError('');
    setContextCompactorMessage('');
    try {
      const saved = await saveContextCompactionSettings(contextCompactorDraft);
      const readBack = await getContextCompactionSettings();
      if (saved.mode !== readBack.mode || saved.triggerPercent !== readBack.triggerPercent || saved.minimumGrowthBeforeRecompactTokens !== readBack.minimumGrowthBeforeRecompactTokens || saved.recentContextTokens !== readBack.recentContextTokens || saved.compactionCallBudget !== readBack.compactionCallBudget) {
        throw new Error('后端保存结果与运行时读回设置不一致');
      }
      setContextCompactorDraft(readBack);
      const catalog = await loadPlugins();
      const plugin = catalog.find((item) => item.id === 'core:context_compactor');
      if (plugin?.metadata?.mode !== readBack.mode || plugin.metadata?.triggerPercent !== String(readBack.triggerPercent) || plugin.metadata?.minimumGrowthBeforeRecompactTokens !== String(readBack.minimumGrowthBeforeRecompactTokens) || plugin.metadata?.recentContextTokens !== String(readBack.recentContextTokens) || plugin.metadata?.compactionCallBudget !== String(readBack.compactionCallBudget)) {
        throw new Error('压缩设置已写入，但插件运行目录未读回一致值');
      }
      setContextCompactorMessage('设置已保存，并由当前运行时读回确认。');
      onPluginsChanged?.();
    } catch (err) {
      setContextCompactorError(err instanceof Error ? err.message : '保存上下文压缩设置失败');
    } finally {
      setContextCompactorBusy(false);
    }
  }

  const filteredPlugins = plugins.filter((p) => {
    if (filterType === 'all') return true;
    return p.type === filterType;
  });

  const typeCounts = {
    all: plugins.length,
    core: plugins.filter((p) => p.type === 'core').length,
    skill: plugins.filter((p) => p.type === 'skill').length,
    mcp: plugins.filter((p) => p.type === 'mcp').length,
    release: plugins.filter((p) => p.type === 'release').length,
  };

  const modalConfig: Record<PluginFilter, { eyebrow: string; title: string; subtitle: string }> = {
    all: {
      eyebrow: 'ALL CAPABILITIES',
      title: '全部插件与能力',
      subtitle: '统一管理本地 Agent 支持的所有 MCP 服务、Markdown 技能与核心插件。',
    },
    mcp: {
      eyebrow: 'MCP PROTOCOL SERVERS',
      title: 'MCP 协议服务',
      subtitle: '管理并接入标准 Model Context Protocol 外部服务工具。',
    },
    skill: {
      eyebrow: 'AGENT SKILLS',
      title: 'Markdown 动态技能',
      subtitle: '查看和管理本地 Markdown 形式的 Agent 任务技能。',
    },
    core: {
      eyebrow: 'CORE CAPABILITIES',
      title: '核心插件与系统能力',
      subtitle: '内置的沙箱指令执行、环境交互及底层核心插件。',
    },
    model: {
      eyebrow: 'MODEL PROVIDERS',
      title: '模型服务',
      subtitle: '查看由模型服务配置提供的可用模型。',
    },
    release: {
      eyebrow: 'INSTALLED RELEASES',
      title: '已安装插件',
      subtitle: '查看由 Plugin Forge 构建并安装的扩展。',
    },
  };

  const currentConfig = modalConfig[filterType] || modalConfig.all;

  return (
    <div
      className="upc-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <section
        ref={dialogRef}
        className="upc-modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="upc-dialog-title"
        tabIndex={-1}
      >
        <header className="upc-header">
          <div>
            <div className="eyebrow">{currentConfig.eyebrow}</div>
            <h2 id="upc-dialog-title" className="upc-title">{currentConfig.title}</h2>
            <p className="upc-subtitle">{currentConfig.subtitle}</p>
          </div>
          <div className="upc-header-actions">
            {filterType === 'mcp' && (
              <button
                type="button"
                onClick={() => setShowAddMcp(true)}
                className="upc-btn-primary"
              >
                <Plus size={13} aria-hidden="true" />
                <span>接入 MCP 服务</span>
              </button>
            )}
            <button
              type="button"
              onClick={onClose}
              className="upc-btn-close"
              aria-label="关闭插件中心"
            >
              <X size={14} strokeWidth={2} aria-hidden="true" />
            </button>
          </div>
        </header>

        <div className="upc-tabs-bar" role="tablist" aria-label="插件分类">
          <div className="upc-tabs">
            {([
              { id: 'all', label: '全部', count: typeCounts.all },
              { id: 'mcp', label: 'MCP 服务', count: typeCounts.mcp },
              { id: 'skill', label: '技能 (Skills)', count: typeCounts.skill },
              { id: 'core', label: '核心插件 (Plugin)', count: typeCounts.core },
              { id: 'release', label: '已安装插件', count: typeCounts.release },
            ] as const).map((tab) => (
              <button
                type="button"
                key={tab.id}
                role="tab"
                aria-selected={filterType === tab.id}
                onClick={() => setFilterType(tab.id)}
                className={'upc-tab ' + (filterType === tab.id ? 'active' : '')}
              >
                {tab.label} ({tab.count})
              </button>
            ))}
          </div>
        </div>

        <div className="upc-body">
          {error && (
            <div className="upc-notice error" role="alert">
              <span>{error}</span>
              <button
                type="button"
                onClick={() => setError('')}
                aria-label="清除错误提示"
                style={{ background: 'none', border: 'none', cursor: 'pointer', color: 'inherit' }}
              >
                <X size={14} aria-hidden="true" />
              </button>
            </div>
          )}
          {filteredPlugins.length === 0 ? (
            <div className="upc-empty">未找到匹配分类的插件或技能。</div>
          ) : (
            <div className="upc-grid">
              {filteredPlugins.map((p) => {
                const isEnabled = p.status === 'enabled';
                const isWebSearch = p.id === 'core:web_search';
                const isContextCompactor = p.id === 'core:context_compactor';
                const webSearchConfigured = p.metadata?.configured === 'true';
                const statusLabel = isWebSearch && !webSearchConfigured ? '待配置' : p.status === 'error' ? '运行异常' : isEnabled ? '已启用' : '已停用';
                const typeLabel =
                  p.type === 'core'
                    ? 'Core 工具'
                    : p.type === 'skill'
                    ? 'Markdown Skill'
                    : p.type === 'mcp'
                    ? 'MCP 外部服务'
                    : 'Release 插件';

                return (
                  <div key={p.id} className="upc-card">
                    <div className="upc-card-top">
                      <span className="upc-tag">{typeLabel}</span>
                      <button
                        type="button"
                        role="switch"
                        aria-checked={isEnabled}
                        aria-label={`${p.name}，当前状态：${statusLabel}`}
                        aria-busy={busy === ('toggle:' + p.id)}
                        onClick={() => handleToggle(p)}
                        disabled={busy === ('toggle:' + p.id) || (isWebSearch && !webSearchConfigured)}
                        className={'upc-toggle ' + (isEnabled ? 'enabled' : '')}
                      >
                        {busy === ('toggle:' + p.id) ? '…' : statusLabel}
                      </button>
                    </div>

                    <div>
                      <h4 className="upc-card-title">{p.name}</h4>
                      <div style={{ fontSize: 11, color: '#9ca3af', fontFamily: 'var(--font-geist-mono)', marginTop: 2 }}>
                        {p.id}
                      </div>
                    </div>

                    <p className="upc-card-desc">{p.description || '暂无详细描述'}</p>

                    {isWebSearch && (
                      <div className="upc-card-footer">
                        <span style={{ fontSize: 11, color: '#9ca3af' }}>
                          {webSearchConfigured ? `API Key 已配置${p.metadata?.keyHint ? ` · ${p.metadata.keyHint}` : ''}` : '需要 Exa API Key'}
                        </span>
                        <button type="button" className="upc-btn-secondary" onClick={() => void openWebSearchSettings()} disabled={webSearchBusy === 'load'}>
                          <KeyRound size={13} aria-hidden="true" />
                          {webSearchConfigured ? '设置' : '配置'}
                        </button>
                      </div>
                    )}

                    {isContextCompactor && (
                      <div className="upc-card-footer">
                        <span style={{ fontSize: 11, color: '#9ca3af' }}>
                          {p.metadata?.mode || 'auto'} · {p.metadata?.triggerPercent || '75'}% 上限
                        </span>
                        <button type="button" className="upc-btn-secondary" onClick={() => void openContextCompactorSettings()} disabled={contextCompactorBusy}>
                          {contextCompactorBusy ? '读取中…' : '压缩策略'}
                        </button>
                      </div>
                    )}

                    {p.status === 'error' && (
                      <div className="upc-notice error" role="status">
                        {p.error || '配置已保存，但运行时未确认启动。'}
                      </div>
                    )}

                    {p.capabilities && p.capabilities.length > 0 && (
                      <div style={{ borderTop: '1px solid #f3f4f6', paddingTop: 8 }}>
                        <span style={{ fontSize: 11, color: '#9ca3af' }}>包含能力 / 工具:</span>
                        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginTop: 6 }}>
                          {p.capabilities.map((cap) => (
                            <span
                              key={cap}
                              className="upc-tag"
                              style={{ fontSize: 11, fontFamily: 'var(--font-geist-mono)' }}
                            >
                              {cap}
                            </span>
                          ))}
                        </div>
                      </div>
                    )}

                    {p.type === 'mcp' && (
                      <div className="upc-card-footer">
                        <span style={{ fontSize: 11, color: '#9ca3af' }}>MCP 外部连接</span>
                        {enableConfirmationMcp === p.id && p.status !== 'enabled' && (
                          <form onSubmit={(event) => { event.preventDefault(); void handleToggle(p); }}>
                            <label className="permission-consent">
                              <input type="checkbox" checked={enableMcpConsent} onChange={(event) => setEnableMcpConsent(event.target.checked)} required />
                              我授权启动此 MCP 外部进程。
                            </label>
                            <button type="submit" disabled={!enableMcpConsent || busy !== ''}>授权并启动</button>
                            <button type="button" onClick={() => setEnableConfirmationMcp('')}>取消</button>
                          </form>
                        )}
                        {removeConfirmationMcp === p.id ? (
                          <form onSubmit={(event) => { event.preventDefault(); void handleRemoveMcp(p.id); }}>
                            <label className="permission-consent">
                              <input type="checkbox" checked={removeMcpConsent} onChange={(event) => setRemoveMcpConsent(event.target.checked)} required />
                              删除此 MCP 配置并停止连接。
                            </label>
                            <button type="submit" disabled={busy !== '' || !removeMcpConsent} style={{ color: '#ef4444' }}>确认移除</button>
                            <button type="button" onClick={() => setRemoveConfirmationMcp('')}>取消</button>
                          </form>
                        ) : (
                          <button type="button" onClick={() => handleRemoveMcp(p.id)} disabled={busy !== ''} style={{ fontSize: 11, color: '#ef4444', background: 'transparent', border: 'none', cursor: 'pointer', padding: '4px 8px', minHeight: '28px' }}>
                            移除服务
                          </button>
                        )}
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </div>

        {showContextCompactorSettings && contextCompactorDraft && (
          <div className="upc-backdrop" style={{ zIndex: 110 }} onMouseDown={(event) => { if (event.target === event.currentTarget) closeContextCompactorSettings(); }}>
            <section className="upc-modal" role="dialog" aria-modal="true" aria-labelledby="context-compactor-title" tabIndex={-1} style={{ width: 'min(520px, 95vw)', height: 'auto', maxHeight: '88vh', padding: 24, gap: 16 }}>
              <header style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <div>
                  <div className="eyebrow">CONTEXT COMPACTION</div>
                  <h3 id="context-compactor-title" className="upc-title" style={{ margin: 0 }}>上下文压缩策略</h3>
                </div>
                <button type="button" onClick={closeContextCompactorSettings} className="upc-btn-close" aria-label="关闭压缩策略设置"><X size={18} aria-hidden="true" /></button>
              </header>
              <p className="upc-subtitle" style={{ margin: 0 }}>
                压缩阈值最高为 75%。system/developer 规则会从当前配置重建；语义摘要作为低信任历史资料，原始来源继续保留。
              </p>
              <form onSubmit={handleSaveContextCompactorSettings} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
                <label style={{ display: 'flex', flexDirection: 'column', gap: 6, fontSize: 12 }}>
                  策略模式
                  <select className="upc-input" value={contextCompactorDraft.mode} onChange={(event) => setContextCompactorDraft({ ...contextCompactorDraft, mode: event.target.value as ContextCompactionSettings['mode'] })}>
                    <option value="auto">自动：Responses 原生优先，安全时使用语义摘要</option>
                    <option value="semantic">语义摘要：引用来源并校验原文证据</option>
                    <option value="provider_native">仅 provider 原生：不兼容时停止</option>
                    <option value="extractive">确定性提取：不调用摘要模型</option>
                  </select>
                </label>
                <label style={{ display: 'flex', flexDirection: 'column', gap: 6, fontSize: 12 }}>
                  自动触发上限：{contextCompactorDraft.triggerPercent}%
                  <input type="range" min={10} max={75} step={1} value={contextCompactorDraft.triggerPercent} onChange={(event) => setContextCompactorDraft({ ...contextCompactorDraft, triggerPercent: Number(event.target.value) })} />
                  <span style={{ color: '#9ca3af' }}>实际预算还会受 provider 窗口和 host 安全余量限制。</span>
                </label>
                <label style={{ display: 'flex', flexDirection: 'column', gap: 6, fontSize: 12 }}>
                  重新压缩前的最小新增 token 数
                  <input className="upc-input" type="number" min={1000} max={200000} step={1000} value={contextCompactorDraft.minimumGrowthBeforeRecompactTokens} onChange={(event) => setContextCompactorDraft({ ...contextCompactorDraft, minimumGrowthBeforeRecompactTokens: Number(event.target.value) })} />
                </label>
                <label style={{ display: 'flex', flexDirection: 'column', gap: 6, fontSize: 12 }}>
                  保留最近历史上限（tokens）
                  <input className="upc-input" type="number" min={1000} max={100000} step={1000} value={contextCompactorDraft.recentContextTokens} onChange={(event) => setContextCompactorDraft({ ...contextCompactorDraft, recentContextTokens: Number(event.target.value) })} />
                  <span style={{ color: '#9ca3af' }}>优先保留最近完整对话和工具交互；实际上限还会按 context window 限制为 20%。当前用户请求单独保留。</span>
                </label>
                <label style={{ display: 'flex', flexDirection: 'column', gap: 6, fontSize: 12 }}>
                  每次压缩的模型调用上限
                  <input className="upc-input" type="number" min={1} max={32} step={1} value={contextCompactorDraft.compactionCallBudget} onChange={(event) => setContextCompactorDraft({ ...contextCompactorDraft, compactionCallBudget: Number(event.target.value) })} />
                  <span style={{ color: '#9ca3af' }}>摘要、证据审查和合并都会计入此上限，并另行给 Agent 下一次工作调用留额度。</span>
                </label>
                {contextCompactorError && <div className="upc-notice error" role="alert">{contextCompactorError}</div>}
                {contextCompactorMessage && <div className="upc-notice" role="status">{contextCompactorMessage}</div>}
                <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
                  <button type="button" className="upc-btn-secondary" onClick={closeContextCompactorSettings} disabled={contextCompactorBusy}>取消</button>
                  <button type="submit" className="upc-btn-primary" disabled={contextCompactorBusy}>{contextCompactorBusy ? '保存并读回中…' : '保存策略'}</button>
                </div>
              </form>
            </section>
          </div>
        )}

        {/* Modal: Add MCP Server */}
        {showAddMcp && (
          <div
            className="upc-backdrop"
            style={{ zIndex: 110 }}
            onMouseDown={(e) => {
              if (e.target === e.currentTarget) closeAddMcp();
            }}
          >
            <div
              className="upc-modal"
              role="dialog"
              aria-modal="true"
              aria-labelledby="add-mcp-title"
              tabIndex={-1}
              style={{ width: 'min(480px, 95vw)', height: 'auto', maxHeight: '88vh', padding: 24, gap: 16 }}
            >
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <h3 id="add-mcp-title" className="upc-title" style={{ margin: 0 }}>
                  接入外部 MCP 协议服务 (Stdio)
                </h3>
                <button
                  type="button"
                  onClick={closeAddMcp}
                  className="upc-btn-close"
                  aria-label="关闭添加 MCP 弹窗"
                >
                  <X size={18} strokeWidth={2} aria-hidden="true" />
                </button>
              </div>
              <p className="upc-subtitle" style={{ margin: 0 }}>
                配置通过标准输入输出 (stdio) 运行的外部 MCP 进程，Agent 将自动发现其 tools 并动态加入工具链。
              </p>
              <form onSubmit={handleAddMcp} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
                <label style={{ display: 'flex', flexDirection: 'column', gap: 4, fontSize: 12 }}>
                  服务唯一标识 (ID) *
                  <input
                    value={mcpId}
                    onChange={(e) => setMcpId(e.target.value)}
                    placeholder="如: github-mcp 或 sqlite-mcp"
                    required
                    className="upc-input"
                  />
                </label>
                <label style={{ display: 'flex', flexDirection: 'column', gap: 4, fontSize: 12 }}>
                  服务显示名称
                  <input
                    value={mcpName}
                    onChange={(e) => setMcpName(e.target.value)}
                    placeholder="如: GitHub 官方 MCP 工具"
                    className="upc-input"
                  />
                </label>
                <label style={{ display: 'flex', flexDirection: 'column', gap: 4, fontSize: 12 }}>
                  执行命令 (Executable) *
                  <input
                    value={mcpCommand}
                    onChange={(e) => setMcpCommand(e.target.value)}
                    placeholder="如: npx, node, python, 或二进制绝对路径"
                    required
                    className="upc-input"
                  />
                </label>
                <label style={{ display: 'flex', flexDirection: 'column', gap: 4, fontSize: 12 }}>
                  执行参数 (空格分隔)
                  <input
                    value={mcpArgs}
                    onChange={(e) => setMcpArgs(e.target.value)}
                    placeholder="如: -y @modelcontextprotocol/server-github"
                    className="upc-input"
                  />
                </label>
                <label style={{ display: 'flex', flexDirection: 'column', gap: 4, fontSize: 12 }}>
                  环境变量 (每行一个 KEY=VALUE)
                  <textarea
                    rows={3}
                    value={mcpEnv}
                    onChange={(e) => setMcpEnv(e.target.value)}
                    placeholder="GITHUB_PERSONAL_ACCESS_TOKEN=ghp_xxx"
                    className="upc-input"
                    style={{ resize: 'vertical' }}
                  />
                </label>

                <label className="permission-consent">
                  <input type="checkbox" checked={mcpLaunchConsent} onChange={(e) => setMcpLaunchConsent(e.target.checked)} required />
                  我确认启动上面配置的本机进程并建立 MCP 连接。
                </label>

                <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 12 }}>
                  <button type="button" onClick={closeAddMcp} className="upc-btn-secondary">
                    取消
                  </button>
                  <button type="submit" disabled={busy === 'add-mcp' || !mcpLaunchConsent} className="upc-btn-primary">
                    {busy === 'add-mcp' ? '正在连接…' : '添加并连接'}
                  </button>
                </div>
              </form>
            </div>
          </div>
        )}
        {showWebSearchSettings && (
          <div
            className="upc-backdrop"
            style={{ zIndex: 110 }}
            onMouseDown={(event) => {
              if (event.target === event.currentTarget) closeWebSearchSettings();
            }}
          >
            <div
              className="upc-modal"
              role="dialog"
              aria-modal="true"
              aria-labelledby="web-search-settings-title"
              tabIndex={-1}
              style={{ width: 'min(500px, 95vw)', height: 'auto', maxHeight: '88vh', padding: 24, gap: 16 }}
            >
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12 }}>
                <div>
                  <div className="eyebrow">WEB SEARCH PLUGIN</div>
                  <h3 id="web-search-settings-title" className="upc-title" style={{ margin: 0 }}>联网搜索 · Exa</h3>
                </div>
                <button type="button" onClick={closeWebSearchSettings} className="upc-btn-close" aria-label="关闭联网搜索设置">
                  <X size={18} strokeWidth={2} aria-hidden="true" />
                </button>
              </div>

              <div style={{ color: 'var(--text-muted)', fontSize: 12, lineHeight: 1.6 }}>
                <p style={{ margin: '0 0 8px' }}>
                  Exa Starter 目前提供注册赠送 $20 和每月 $10 免费额度，官方标注无需绑定付款方式。联网搜索时，查询词会发送给 Exa；搜索结果作为外部内容交给 Agent。
                </p>
                <p style={{ margin: 0 }}>
                  当前 Search 起价为每 1,000 次请求 $7；内容选项可能另计。免费额度用完后是否继续付费取决于 Exa 账户套餐和付款设置。<a href="https://exa.ai/pricing" target="_blank" rel="noreferrer">查看当前价格</a>
                </p>
              </div>

              {webSearchSettings?.configured && (
                <div className="upc-notice" style={{ background: '#f0fdf4', color: '#166534', border: '1px solid #bbf7d0', margin: 0 }}>
                  <span>已在本机加密保存 {webSearchSettings.keyHint || 'API Key'}。</span>
                  <Check size={14} aria-hidden="true" />
                </div>
              )}

              <form onSubmit={handleSaveWebSearchSettings} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
                <label style={{ display: 'flex', flexDirection: 'column', gap: 5, fontSize: 12 }}>
                  Exa API Key
                  <input
                    type="password"
                    value={webSearchApiKey}
                    onChange={(event) => setWebSearchApiKey(event.target.value)}
                    placeholder={webSearchSettings?.configured ? '输入新 Key 以替换当前 Key' : '粘贴 Exa API Key'}
                    autoComplete="new-password"
                    required
                    className="upc-input"
                  />
                  <span style={{ color: 'var(--text-faint)', fontSize: 11 }}>
                    从 <a href="https://dashboard.exa.ai" target="_blank" rel="noreferrer">Exa 控制台</a>创建。Key 会加密存储在本机，不会返回到页面。
                  </span>
                </label>

                {webSearchError && <div className="upc-notice error" role="alert" style={{ margin: 0 }}>{webSearchError}</div>}
                {webSearchMessage && <div className="upc-notice" role="status" style={{ background: '#f5f5f4', color: 'var(--text-muted)', margin: 0 }}>{webSearchMessage}</div>}
                <span style={{ color: 'var(--text-faint)', fontSize: 11 }}>测试连接会发起一次真实搜索并扣除搜索和内容额度；免费额度耗尽后是否收费取决于 Exa 账户设置。</span>

                <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', justifyContent: 'space-between', gap: 8, marginTop: 4 }}>
                  <div style={{ display: 'flex', gap: 8 }}>
                    {webSearchSettings?.configured && (
                      <button type="button" onClick={() => void handleClearWebSearchKey()} disabled={webSearchBusy !== ''} className="upc-btn-secondary" style={{ color: '#b91c1c' }}>
                        {confirmWebSearchClear ? '确认移除密钥' : <><Trash2 size={13} aria-hidden="true" />移除密钥</>}
                      </button>
                    )}
                    <button type="button" onClick={() => void handleTestWebSearch()} disabled={!webSearchSettings?.configured || webSearchBusy !== ''} className="upc-btn-secondary">
                      {webSearchBusy === 'test' ? '正在测试…' : '测试连接'}
                    </button>
                  </div>
                  <div style={{ display: 'flex', gap: 8 }}>
                    <button type="button" onClick={closeWebSearchSettings} className="upc-btn-secondary">关闭</button>
                    <button type="submit" disabled={webSearchBusy === 'save' || !webSearchApiKey.trim()} className="upc-btn-primary">
                      {webSearchBusy === 'save' ? '正在保存…' : '保存密钥'}
                    </button>
                  </div>
                </div>
              </form>
            </div>
          </div>
        )}
      </section>
    </div>
  );
}
