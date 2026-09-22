'use client';

import { useMemo, useState } from 'react';
import { X, Search, SlidersHorizontal } from 'lucide-react';
import { request } from './api';
import type { Provider } from './OApp';
import { useDialogA11y } from './useDialogA11y';

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : '未知错误';
}

export interface ModelCapabilityConfig {
  customName?: string;
  supportsVision: boolean;
  contextWindow: number;
}

function formatTokens(tokens: number): string {
  if (tokens >= 1000000) return (tokens / 1000000).toFixed(0) + "M";
  if (tokens >= 1000) return (tokens / 1000).toFixed(0) + "K";
  return String(tokens);
}

export function Settings({
  providers,
  onClose,
  onSaved,
  onDeleted,
}: {
  providers: Provider[];
  onClose: () => void;
  onSaved: (provider: Provider) => void;
  onDeleted: (providerId: string) => void;
}) {
  const dialogRef = useDialogA11y(onClose);
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState('');
  const editing = providers[0] ?? null;
  const [baseUrl, setBaseUrl] = useState(editing?.baseUrl ?? 'http://127.0.0.1:3000/v1');
  const [apiKey, setApiKey] = useState('');
  const [name, setName] = useState(editing?.name ?? 'One-API');
  const [probing, setProbing] = useState(false);
  const [remoteModels, setRemoteModels] = useState<string[]>([]);
  const [search, setSearch] = useState('');
  const [disabledModelIds, setDisabledModelIds] = useState<Set<string>>(() => new Set());
  const [modelConfigs, setModelConfigs] = useState<Record<string, ModelCapabilityConfig>>(() => {
    try {
      return JSON.parse(localStorage.getItem('axiom_model_configs') || '{}');
    } catch {
      return {};
    }
  });

  const [editingModel, setEditingModel] = useState<{ provider: Provider; modelId: string } | null>(null);
  const [editDisplayName, setEditDisplayName] = useState('');
  const [editVision, setEditVision] = useState(true);
  const [editContextTokens, setEditContextTokens] = useState(131072);

	const getModelConfig = (modelId: string, provider?: Provider): ModelCapabilityConfig => {
	if (modelConfigs[modelId]) {
	  return { ...modelConfigs[modelId], contextWindow: provider?.contextWindow || modelConfigs[modelId].contextWindow };
	}
    let isV = true;
    try {
      const vMap = JSON.parse(localStorage.getItem('axiom_model_vision') || '{}');
      if (vMap[modelId] === false) isV = false;
    } catch {}
	return { supportsVision: isV, contextWindow: provider?.contextWindow || 131072 };
  };

  const saveModelConfig = (modelId: string, patch: Partial<ModelCapabilityConfig>) => {
    const current = getModelConfig(modelId);
    const updated = { ...current, ...patch };
    const next = { ...modelConfigs, [modelId]: updated };
    setModelConfigs(next);
    localStorage.setItem('axiom_model_configs', JSON.stringify(next));
    if (patch.supportsVision !== undefined) {
      try {
        const vMap = JSON.parse(localStorage.getItem('axiom_model_vision') || '{}');
        vMap[modelId] = patch.supportsVision;
        localStorage.setItem('axiom_model_vision', JSON.stringify(vMap));
      } catch {}
    }
  };

  const openEditModel = (p: Provider) => {
	const cfg = getModelConfig(p.model, p);
    setEditingModel({ provider: p, modelId: p.model });
    setEditDisplayName(cfg.customName || p.name);
    setEditVision(cfg.supportsVision);
    setEditContextTokens(cfg.contextWindow);
  };

  const handleSaveModelEdit = async () => {
    if (!editingModel) return;
    const { provider: p, modelId } = editingModel;
	try {
	  const updated = await request<Provider>('/providers/' + encodeURIComponent(p.id), {
		method: 'PUT',
		body: JSON.stringify({
		  name: editDisplayName.trim() || p.name,
		  kind: p.kind,
		  baseUrl: p.baseUrl,
		  model: p.model,
		  contextWindow: editContextTokens || 131072,
		}),
	  });
	  saveModelConfig(modelId, {
		customName: updated.name,
		supportsVision: editVision,
		contextWindow: updated.contextWindow || editContextTokens || 131072,
	  });
	  onSaved(updated);
	  setStatus('已更新模型 [' + modelId + '] 的定制配置');
	  setEditingModel(null);
	} catch (error: unknown) {
	  setStatus('更新失败: ' + errorMessage(error));
	}
  };

  async function probeModels() {
    setProbing(true);
    setStatus('正在从 One-API 网关拉取模型列表...');
    try {
      let data: { models?: string[] };
      if (editing?.id && !apiKey) {
        data = await request<{ models?: string[] }>(`/providers/${editing.id}/models`);
      } else {
        data = await request<{ models?: string[] }>('/providers/probe-models', {
          method: 'POST',
          body: JSON.stringify({ baseUrl, apiKey }),
        });
      }
      const list: string[] = data.models || [];
      setRemoteModels(list);
      setStatus('成功获取 ' + list.length + ' 个可用模型，所有模型默认停用，请点击启用所需要的模型');
    } catch (error: unknown) {
      setStatus('拉取模型失败: ' + errorMessage(error));
    } finally {
      setProbing(false);
    }
  }

  async function toggleModel(modelId: string, currentEnabled: boolean) {
    setBusy(true);
    try {
      if (currentEnabled) {
        const existing = providers.find((p) => p.model === modelId);
        if (existing) {
          await request<void>(`/providers/${existing.id}`, { method: 'DELETE' });
          setDisabledModelIds((items) => new Set(items).add(modelId));
          onDeleted(existing.id);
          setStatus('已停用模型 ' + modelId);
        }
      } else {
        const payload = {
          name: name || 'One-API (' + modelId + ')',
          kind: 'new-api',
          baseUrl: baseUrl || 'http://127.0.0.1:3000/v1',
		  apiKey: apiKey || undefined,
		  model: modelId,
		  contextWindow: getModelConfig(modelId).contextWindow,
        };
        const created = await request<Provider>('/providers', {
          method: 'POST',
          body: JSON.stringify(payload),
        });
        setDisabledModelIds((items) => {
          const next = new Set(items);
          next.delete(modelId);
          return next;
        });
        onSaved(created);
        setStatus('已成功启用模型 ' + modelId);
      }
    } catch (error: unknown) {
      setStatus('操作失败: ' + errorMessage(error));
    } finally {
      setBusy(false);
    }
  }

  const enabledModelMap = useMemo(() => {
    const map = new Set<string>();
    for (const p of providers) {
      if (p.model && !disabledModelIds.has(p.model)) map.add(p.model);
    }
    return map;
  }, [disabledModelIds, providers]);

  const allModels = useMemo(() => {
    const set = new Set<string>(remoteModels);
    for (const p of providers) {
      if (p.model) set.add(p.model);
    }
    return Array.from(set).sort();
  }, [remoteModels, providers]);

  const filteredModels = useMemo(() => {
    if (!search.trim()) return allModels;
    const q = search.toLowerCase();
    return allModels.filter((m) => m.toLowerCase().includes(q));
  }, [allModels, search]);

  return (
    <div
      className="settings-dialog-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <section
        ref={dialogRef}
        className="settings-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="settings-dialog-title"
        tabIndex={-1}
      >
        <header className="settings-header">
          <div>
            <h3 id="settings-dialog-title">One-API 大模型网关配置</h3>
            <p>统一接入所有商业与开源模型，海量模型默认停用，按需点击启用</p>
          </div>
          <button
            type="button"
            className="icon-button"
            onClick={onClose}
            aria-label="关闭模型设置"
            style={{ minHeight: 36, minWidth: 36, display: 'inline-flex', alignItems: 'center', justifyContent: 'center' }}
          >
            <X size={18} strokeWidth={2} aria-hidden="true" />
          </button>
        </header>

        <div className="settings-content">
          {status && (
            <div className="settings-status" role="status" aria-live="polite">
              {status}
            </div>
          )}

          <div className="settings-form-grid">
            <div className="settings-field">
              <label className="field-label" htmlFor="provider-name">网关服务名称</label>
              <input id="provider-name" className="text-input" value={name} onChange={(e) => setName(e.target.value)} placeholder="One-API" />
            </div>
            <div className="settings-field">
              <label className="field-label" htmlFor="provider-base-url">网关接口地址 (Base URL)</label>
              <input id="provider-base-url" className="text-input" value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} placeholder="http://127.0.0.1:3000/v1" />
            </div>
          </div>

          <div className="settings-field">
            <label className="field-label" htmlFor="provider-api-key">访问令牌 (API Key / Token)</label>
            <div className="settings-token-row">
              <input
                id="provider-api-key"
                className="text-input"
                type="password"
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
                placeholder={editing?.hasApiKey ? '保持现有 Token（留空不改）' : 'sk-...'}
              />
              <button type="button" className="action-button" disabled={probing} onClick={probeModels}>
                {probing ? '正在拉取...' : '拉取网关模型'}
              </button>
            </div>
          </div>

          <div className="settings-list-heading">
            <h4>可用模型列表 ({allModels.length} 个模型)</h4>
            <div style={{ position: 'relative', display: 'flex', alignItems: 'center' }}>
              <label className="sr-only" htmlFor="provider-model-search">搜索模型名称</label>
              <input
                id="provider-model-search"
                className="text-input settings-search"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="搜索模型名称..."
                style={{ paddingLeft: 28 }}
              />
              <Search size={14} style={{ position: 'absolute', left: 8, color: '#9ca3af', pointerEvents: 'none' }} aria-hidden="true" />
            </div>
          </div>

          <div className="settings-model-list">
            {filteredModels.length === 0 ? (
              <div className="settings-model-empty">
                {allModels.length === 0 ? '尚未拉取模型列表，请点击上方“拉取网关模型”按钮获取' : '未找到匹配的模型'}
              </div>
            ) : (
              <div className="settings-model-grid">
                {filteredModels.map((modelId) => {
                  const isEnabled = enabledModelMap.has(modelId);
				  const prov = providers.find((p) => p.model === modelId);
				  const modelCfg = getModelConfig(modelId, prov);
                  return (
                    <div
                      key={modelId}
                      className={"settings-model-card" + (isEnabled ? " enabled" : "")}
                    >
                      <div className="settings-model-meta">
                        <div className="settings-model-name">
                          {modelCfg.customName && modelCfg.customName !== modelId ? (
                            <>
                              <span>{modelCfg.customName}</span>
                              <span style={{ fontSize: 10, color: '#9ca3af', marginLeft: 6 }}>({modelId})</span>
                            </>
                          ) : (
                            modelId
                          )}
                        </div>
                        <div className="settings-model-status">
                          <span>{isEnabled ? '● 已启用' : '○ 停用中'}</span>
                          {isEnabled && (
                            <div className="settings-model-tags">
                              <button
                                type="button"
                                className={"model-tag-pill " + (modelCfg.supportsVision ? "vision-on" : "vision-off")}
                                onClick={(e) => {
                                  e.stopPropagation();
                                  saveModelConfig(modelId, { supportsVision: !modelCfg.supportsVision });
                                }}
                                title="点击快速切换该模型的视觉/纯文本属性"
                              >
                                {modelCfg.supportsVision ? "👁 视觉" : "🚫 纯文本"}
                              </button>
                              <span
                                className="model-tag-pill context"
                                title="当前模型上下文窗口上限 (点击配置修改)"
                                onClick={(e) => {
                                  e.stopPropagation();
                                  if (prov) openEditModel(prov);
                                }}
                              >
                                {formatTokens(modelCfg.contextWindow)}
                              </span>
                            </div>
                          )}
                        </div>
                      </div>

                      <div className="settings-model-actions">
                        {isEnabled && prov && (
                          <button
                            type="button"
                            className="settings-model-edit-btn"
                            onClick={(e) => {
                              e.stopPropagation();
                              openEditModel(prov);
                            }}
                            title="修改模型配置 (自定义别名、上下文窗口、视觉开关)"
                            aria-label={"编辑修改 " + modelId + " 配置"}
                          >
                            <SlidersHorizontal size={12} strokeWidth={2} />
                            <span>配置</span>
                          </button>
                        )}
                        <button
                          type="button"
                          role="switch"
                          aria-checked={isEnabled}
                          aria-label={modelId + '，当前状态：' + (isEnabled ? '已启用' : '已停用')}
                          aria-busy={busy}
                          disabled={busy}
                          onClick={() => toggleModel(modelId, isEnabled)}
                          className={"settings-model-action" + (isEnabled ? " danger" : "")}
                        >
                          {isEnabled ? '停用' : '启用'}
                        </button>
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        </div>

                {editingModel && (
          <div
            className="model-edit-backdrop"
            onMouseDown={(e) => {
              if (e.target === e.currentTarget) setEditingModel(null);
            }}
          >
            <div className="model-edit-modal">
              <header className="model-edit-header">
                <h4>修改模型配置 · {editingModel.modelId}</h4>
                <button
                  type="button"
                  onClick={() => setEditingModel(null)}
                  style={{ background: 'none', border: 'none', cursor: 'pointer', color: 'var(--text-muted)' }}
                >
                  <X size={15} strokeWidth={2} />
                </button>
              </header>
              <div className="model-edit-body">
                <div className="model-edit-field">
                  <label className="model-edit-label">显示别名 / 昵称</label>
                  <input
                    className="text-input"
                    value={editDisplayName}
                    onChange={(e) => setEditDisplayName(e.target.value)}
                    placeholder={editingModel.modelId}
                  />
                  <span className="model-edit-desc">在对话和底栏下拉框中展示的友好名称</span>
                </div>

                <div className="model-edit-field">
                  <label className="model-edit-label">模型类型与能力</label>
                  <div className="model-edit-radio-group">
                    <button
                      type="button"
                      className={"model-edit-radio-card" + (editVision ? " selected" : "")}
                      onClick={() => setEditVision(true)}
                    >
                      <span className="model-edit-radio-title">👁 多模态视觉模型</span>
                      <span className="model-edit-radio-sub">支持识图与图片理解 (可粘贴截图)</span>
                    </button>
                    <button
                      type="button"
                      className={"model-edit-radio-card" + (!editVision ? " selected" : "")}
                      onClick={() => setEditVision(false)}
                    >
                      <span className="model-edit-radio-title">🚫 纯文本 / 代码模型</span>
                      <span className="model-edit-radio-sub">仅纯文本交互，禁止图片输入防报错</span>
                    </button>
                  </div>
                </div>

                <div className="model-edit-field">
                  <label className="model-edit-label">上下文窗口大小 (Context Window)</label>
                  <div className="context-window-pills">
                    {[
                      { label: '8K', tokens: 8192 },
                      { label: '16K', tokens: 16384 },
                      { label: '32K', tokens: 32768 },
                      { label: '64K', tokens: 65536 },
                      { label: '128K', tokens: 131072 },
                      { label: '200K', tokens: 200000 },
                      { label: '1M', tokens: 1000000 },
                    ].map((item) => (
                      <button
                        key={item.label}
                        type="button"
                        className={"context-pill-btn" + (editContextTokens === item.tokens ? " active" : "")}
                        onClick={() => setEditContextTokens(item.tokens)}
                      >
                        {item.label}
                      </button>
                    ))}
                  </div>
                  <div style={{ marginTop: 6, display: 'flex', alignItems: 'center', gap: 8 }}>
                    <input
                      type="number"
                      className="text-input"
                      style={{ width: 140 }}
                      value={editContextTokens}
                      onChange={(e) => setEditContextTokens(Number(e.target.value) || 131072)}
                    />
                    <span style={{ fontSize: 11, color: 'var(--text-muted)' }}>Tokens（持久化对话约使用 55%，其余空间预留给系统指令、工具、推理和输出）</span>
                  </div>
                </div>
              </div>
              <footer className="model-edit-footer">
                <button type="button" className="text-button" onClick={() => setEditingModel(null)}>
                  取消
                </button>
                <button type="button" className="action-button" onClick={handleSaveModelEdit}>
                  保存模型配置
                </button>
              </footer>
            </div>
          </div>
        )}

        <footer className="settings-footer">
          <div>已启用模型将直接作为 Agent 对话与调用的候选模型</div>
          <button type="button" className="action-button" onClick={onClose}>完成</button>
        </footer>
      </section>
    </div>
  );
}
