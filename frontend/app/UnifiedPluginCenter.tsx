'use client';

import { FormEvent, useCallback, useEffect, useState } from 'react';
import { X, Plus, RotateCw } from 'lucide-react';
import {
  UnifiedPlugin,
  McpServerConfig,
  getUnifiedPlugins,
  toggleUnifiedPlugin,
  reloadUnifiedPlugins,
  addMcpServerConfig,
  removeMcpServerConfig,
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

  // Form states for adding MCP
  const [mcpId, setMcpId] = useState('');
  const [mcpName, setMcpName] = useState('');
  const [mcpCommand, setMcpCommand] = useState('');
  const [mcpArgs, setMcpArgs] = useState('');
  const [mcpEnv, setMcpEnv] = useState('');

  const loadPlugins = useCallback(async () => {
    try {
      const data = await getUnifiedPlugins();
      setPlugins(data);
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载插件列表失败');
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
      if (e.key === 'Escape' && showAddMcp) {
        e.stopPropagation();
        setShowAddMcp(false);
      }
    }
    if (showAddMcp) {
      window.addEventListener('keydown', handleSubModalKeyDown, true);
      return () => window.removeEventListener('keydown', handleSubModalKeyDown, true);
    }
  }, [showAddMcp]);

  async function handleToggle(p: UnifiedPlugin) {
    const nextState = p.status !== 'enabled';
    setBusy(`toggle:${p.id}`);
    try {
      await toggleUnifiedPlugin(p.id, nextState);
      await loadPlugins();
      onPluginsChanged?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : '切换插件状态失败');
    } finally {
      setBusy('');
    }
  }

  async function handleReload() {
    setBusy('reload');
    try {
      const data = await reloadUnifiedPlugins();
      setPlugins(data);
      onPluginsChanged?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : '重载插件失败');
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
      };

      await addMcpServerConfig(cfg);
      setShowAddMcp(false);
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
    if (!confirm(`确定要移除 MCP 服务 "${id}" 吗？`)) return;
    setBusy(`remove:${id}`);
    try {
      await removeMcpServerConfig(id);
      await loadPlugins();
    } catch (err) {
      setError(err instanceof Error ? err.message : '删除 MCP 服务失败');
    } finally {
      setBusy('');
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
      subtitle: '查看、管理或重载本地 Markdown 形式的 Agent 任务技能。',
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
            <button
              type="button"
              onClick={handleReload}
              disabled={busy !== ''}
              className="upc-btn-secondary"
              aria-label="热重载所有插件"
            >
              <RotateCw size={12} className={busy === 'reload' ? 'spin' : ''} aria-hidden="true" />
              <span>{busy === 'reload' ? '正在重载…' : '热重载'}</span>
            </button>
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
                        aria-label={`${p.name}，当前状态：${isEnabled ? '已启用' : '已停用'}`}
                        aria-busy={busy === ('toggle:' + p.id)}
                        onClick={() => handleToggle(p)}
                        disabled={busy === ('toggle:' + p.id)}
                        className={'upc-toggle ' + (isEnabled ? 'enabled' : '')}
                      >
                        {busy === ('toggle:' + p.id) ? '…' : isEnabled ? '已启用' : '已停用'}
                      </button>
                    </div>

                    <div>
                      <h4 className="upc-card-title">{p.name}</h4>
                      <div style={{ fontSize: 11, color: '#9ca3af', fontFamily: 'var(--font-geist-mono)', marginTop: 2 }}>
                        {p.id}
                      </div>
                    </div>

                    <p className="upc-card-desc">{p.description || '暂无详细描述'}</p>

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
                        <button
                          type="button"
                          onClick={() => handleRemoveMcp(p.id)}
                          disabled={busy !== ''}
                          style={{
                            fontSize: 11,
                            color: '#ef4444',
                            background: 'transparent',
                            border: 'none',
                            cursor: 'pointer',
                            padding: '4px 8px',
                            minHeight: '28px',
                          }}
                        >
                          移除服务
                        </button>
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </div>

        {/* Modal: Add MCP Server */}
        {showAddMcp && (
          <div
            className="upc-backdrop"
            style={{ zIndex: 110 }}
            onMouseDown={(e) => {
              if (e.target === e.currentTarget) setShowAddMcp(false);
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
                  onClick={() => setShowAddMcp(false)}
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

                <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 12 }}>
                  <button type="button" onClick={() => setShowAddMcp(false)} className="upc-btn-secondary">
                    取消
                  </button>
                  <button type="submit" disabled={busy === 'add-mcp'} className="upc-btn-primary">
                    {busy === 'add-mcp' ? '正在连接…' : '添加并连接'}
                  </button>
                </div>
              </form>
            </div>
          </div>
        )}
      </section>
    </div>
  );
}
