'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { X, Search, SlidersHorizontal } from 'lucide-react';
import { deleteGitCredential, getArtifactStorageCapacity, getGitCredentials, getSandboxConfiguration, request, runSandboxMaintenance, saveGitCredential, setSandboxDefaultBackend } from './api';
import type { ArtifactStorageCapacity, GitCredentialSummary, SandboxConfiguration } from './api';
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

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let value = bytes;
  let unit = -1;
  do { value /= 1024; unit += 1; } while (value >= 1024 && unit < units.length - 1);
  return `${value.toFixed(1)} ${units[unit]}`;
}

function gatewayName(provider: Provider): string {
  const suffix = ' · ' + provider.model;
  return provider.name.endsWith(suffix) ? provider.name.slice(0, -suffix.length) : provider.name;
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
  const [sandboxConfig, setSandboxConfig] = useState<SandboxConfiguration | null>(null);
  const [sandboxLoading, setSandboxLoading] = useState(true);
  const [sandboxBusy, setSandboxBusy] = useState(false);
  const [sandboxStatus, setSandboxStatus] = useState('');
  const [storageCapacity, setStorageCapacity] = useState<ArtifactStorageCapacity | null>(null);
  const [storageBusy, setStorageBusy] = useState(false);
  const [storageStatus, setStorageStatus] = useState('');
  const [gitCredentials, setGitCredentials] = useState<GitCredentialSummary[]>([]);
  const [gitCredentialLoading, setGitCredentialLoading] = useState(true);
  const [gitCredentialBusy, setGitCredentialBusy] = useState(false);
  const [gitCredentialStatus, setGitCredentialStatus] = useState('');
  const [gitHost, setGitHost] = useState('github.com');
  const [gitRepository, setGitRepository] = useState('');
  const [gitUsername, setGitUsername] = useState('');
  const [gitPassword, setGitPassword] = useState('');
  const editing = providers[0] ?? null;
  const [baseUrl, setBaseUrl] = useState(editing?.baseUrl ?? 'http://127.0.0.1:3000/v1');
  const [apiKey, setApiKey] = useState('');
  const [name, setName] = useState(editing ? gatewayName(editing) : 'One-API');
  const [probing, setProbing] = useState(false);
  const [remoteModels, setRemoteModels] = useState<string[]>([]);
  const [search, setSearch] = useState('');
  const [disabledModelIds, setDisabledModelIds] = useState<Set<string>>(() => new Set());
  const [modelConfigs] = useState<Record<string, ModelCapabilityConfig>>(() => {
    try {
      return JSON.parse(localStorage.getItem('axiom_model_configs') || '{}');
    } catch {
      return {};
    }
  });

  const [editingModel, setEditingModel] = useState<{
    provider?: Provider;
    modelId: string;
    mode: 'update' | 'activate' | 'draft';
  } | null>(null);
  const [editDisplayName, setEditDisplayName] = useState('');
  const [editVision, setEditVision] = useState(true);
  const [editContextTokens, setEditContextTokens] = useState(131072);
  const [selectedModels, setSelectedModels] = useState<Set<string>>(() => new Set());
  const [draftConfigs, setDraftConfigs] = useState<Record<string, ModelCapabilityConfig>>({});
  const migratedLegacyProviders = useRef(new Set<string>());

  useEffect(() => {
    let active = true;
    void getSandboxConfiguration().then((configuration) => {
      if (active) setSandboxConfig(configuration);
    }).catch((error: unknown) => {
      if (active) setSandboxStatus('读取沙箱配置失败：' + errorMessage(error));
    }).finally(() => {
      if (active) setSandboxLoading(false);
    });
    return () => { active = false; };
  }, []);

  useEffect(() => {
    let active = true;
    void getGitCredentials().then((credentials) => {
      if (active) setGitCredentials(credentials);
    }).catch((error: unknown) => {
      if (active) setGitCredentialStatus('读取 Git 凭据失败：' + errorMessage(error));
    }).finally(() => {
      if (active) setGitCredentialLoading(false);
    });
    return () => { active = false; };
  }, []);

  const saveGitCredentialEntry = async () => {
    setGitCredentialBusy(true);
    setGitCredentialStatus('正在加密保存 Git 凭据…');
    try {
      const id = crypto.randomUUID();
      const saved = await saveGitCredential({ id, host: gitHost, repository: gitRepository, username: gitUsername, password: gitPassword });
      const readBack = await getGitCredentials();
      if (!readBack.some((item) => item.id === saved.id && item.configured && item.host === saved.host && item.repository === saved.repository)) {
        throw new Error('保存后的 Git 凭据未能从后端读回');
      }
      setGitCredentials(readBack);
      setGitPassword('');
      setGitCredentialStatus(`已保存 ${saved.host}/${saved.repository}，凭据只会交给匹配的仓库命令。`);
    } catch (error: unknown) {
      setGitCredentialStatus('保存 Git 凭据失败：' + errorMessage(error));
    } finally {
      setGitCredentialBusy(false);
    }
  };

  const removeGitCredential = async (id: string) => {
    setGitCredentialBusy(true);
    try {
      await deleteGitCredential(id);
      const readBack = await getGitCredentials();
      if (readBack.some((item) => item.id === id)) throw new Error('删除后的 Git 凭据仍存在');
      setGitCredentials(readBack);
      setGitCredentialStatus('Git 凭据已删除并由后端读回确认。');
    } catch (error: unknown) {
      setGitCredentialStatus('删除 Git 凭据失败：' + errorMessage(error));
      try { setGitCredentials(await getGitCredentials()); } catch { /* Preserve the original failure for the user. */ }
    } finally {
      setGitCredentialBusy(false);
    }
  };

  const changeSandboxBackend = async (backend: string) => {
    setSandboxBusy(true);
    setSandboxStatus('正在保存默认命令沙箱…');
    try {
      const saved = await setSandboxDefaultBackend(backend);
      const readBack = await getSandboxConfiguration();
      if (saved.defaultBackend !== backend || readBack.defaultBackend !== backend) {
        throw new Error('后端读回值与所选沙箱不一致');
      }
      if (backend === 'windows-native' && readBack.native.health !== 'healthy') {
        throw new Error('Windows 原生沙箱健康检查失败：' + (readBack.native.reason || '未知原因'));
      }
      setSandboxConfig(readBack);
      setSandboxStatus('默认命令沙箱已从后端读回确认。');
    } catch (error: unknown) {
      setSandboxStatus('保存沙箱设置失败：' + errorMessage(error));
      try {
        setSandboxConfig(await getSandboxConfiguration());
      } catch (readError: unknown) {
        setSandboxStatus((current) => current + '；重新读取状态也失败：' + errorMessage(readError));
      }
    } finally {
      setSandboxBusy(false);
    }
  };

  const maintainSandbox = async (operation: 'install' | 'repair' | 'uninstall') => {
    if (operation === 'uninstall' && !window.confirm('卸载 Windows 原生命令沙箱？这会删除 O Agent 创建的沙箱账户和网络规则，不会删除工作区文件。')) return;
    setSandboxBusy(true);
    setSandboxStatus(operation === 'uninstall' ? '正在卸载沙箱…' : '正在请求管理员权限并检查沙箱…');
    try {
      const result = await runSandboxMaintenance(operation);
      const readBack = await getSandboxConfiguration();
      if (result.native.installation !== readBack.native.installation || result.native.health !== readBack.native.health) {
        throw new Error('沙箱维护结果与后端当前状态不一致');
      }
      if (operation === 'uninstall' ? readBack.native.installation !== 'absent' : readBack.native.health !== 'healthy') {
        throw new Error('沙箱维护尚未达到预期状态：' + (readBack.native.reason || readBack.native.health));
      }
      setSandboxConfig(readBack);
      setSandboxStatus(operation === 'uninstall' ? '沙箱卸载状态已由后端确认。' : 'Windows 原生沙箱已通过后端 Probe。');
    } catch (error: unknown) {
      let message = '沙箱维护失败：' + errorMessage(error);
      try {
        const readBack = await getSandboxConfiguration();
        setSandboxConfig(readBack);
        message += '。当前状态：' + readBack.native.installation + ' / ' + readBack.native.health;
        if (readBack.native.reason) message += '：' + readBack.native.reason;
      } catch (readError: unknown) {
        message += '；读取当前状态失败：' + errorMessage(readError);
      }
      setSandboxStatus(message);
    } finally {
      setSandboxBusy(false);
    }
  };

  const inspectArtifactStorage = async () => {
    setStorageBusy(true);
    setStorageStatus('正在读取成果存储和磁盘空间…');
    try {
      const readBack = await getArtifactStorageCapacity();
      if (!readBack.observedAt || !['local', 's3'].includes(readBack.backend) || readBack.remoteObjectsAuthoritative !== (readBack.backend === 's3') || readBack.objectBytes < 0 || readBack.stagingBytes < 0 || readBack.temporaryBytes < 0 || readBack.metadataDatabaseBytes < 0 || readBack.metadataWalBytes < 0 || readBack.metadataShmBytes < 0) {
        throw new Error('后端返回的成果存储容量数据无效');
      }
      setStorageCapacity(readBack);
      setStorageStatus('容量数据已由后端实时读取；这是观察信息，不限制项目或任务大小。');
    } catch (error: unknown) {
      setStorageStatus('读取成果存储容量失败：' + errorMessage(error));
    } finally {
      setStorageBusy(false);
    }
  };

  useEffect(() => {
    let legacyVision: Record<string, boolean> = {};
    try {
      const saved = JSON.parse(localStorage.getItem('axiom_model_vision') || '{}') as unknown;
      if (saved && typeof saved === 'object' && !Array.isArray(saved)) {
        legacyVision = saved as Record<string, boolean>;
      }
    } catch {}

    for (const provider of providers) {
      const legacy = modelConfigs[provider.model];
      const oldVision = typeof legacyVision[provider.model] === 'boolean'
        ? legacyVision[provider.model]
        : legacy?.supportsVision;
      const savedContext = provider.contextWindow || legacy?.contextWindow || 0;
      const needsContextMigration = provider.contextWindow <= 0 && savedContext > 0;
      const needsVisionMigration = provider.supportsVision == null && typeof oldVision === 'boolean';
      if ((!needsContextMigration && !needsVisionMigration) || migratedLegacyProviders.current.has(provider.id)) continue;

      migratedLegacyProviders.current.add(provider.id);
      void request<Provider>('/providers/' + encodeURIComponent(provider.id), {
        method: 'PUT',
        body: JSON.stringify({
          name: provider.name,
          kind: provider.kind,
          baseUrl: provider.baseUrl,
          model: provider.model,
          contextWindow: savedContext,
          supportsVision: provider.supportsVision ?? oldVision ?? true,
        }),
      }).then(onSaved).catch((error: unknown) => {
        setStatus('旧模型设置写入失败（' + provider.model + '）：' + errorMessage(error));
      });
    }
  }, [providers, modelConfigs, onSaved]);

  const getModelConfig = (modelId: string, provider?: Provider): ModelCapabilityConfig => {
    const legacy = modelConfigs[modelId];
    let supportsVision = provider?.supportsVision ?? legacy?.supportsVision ?? true;
    try {
      const vMap = JSON.parse(localStorage.getItem('axiom_model_vision') || '{}');
      if (provider?.supportsVision == null && vMap[modelId] === false) supportsVision = false;
    } catch {}
	return {
      ...legacy,
      supportsVision,
      contextWindow: provider?.contextWindow || legacy?.contextWindow || 131072,
    };
  };

  const openModelConfig = (modelId: string, provider?: Provider, mode: 'update' | 'activate' | 'draft' = 'update') => {
    const cfg = mode !== 'update' && draftConfigs[modelId]
      ? draftConfigs[modelId]
      : getModelConfig(modelId, provider);
    setEditingModel({ provider, modelId, mode });
    setEditDisplayName(cfg.customName || provider?.name || modelId);
    setEditVision(cfg.supportsVision);
    setEditContextTokens(cfg.contextWindow);
  };

  const handleSaveModelEdit = async () => {
    if (!editingModel) return;
    const { provider, modelId, mode } = editingModel;
    setBusy(true);
    try {
      if (mode === 'update' && provider) {
        const updated = await request<Provider>('/providers/' + encodeURIComponent(provider.id), {
          method: 'PUT',
          body: JSON.stringify({
            name: editDisplayName.trim() || provider.name,
            kind: provider.kind,
            baseUrl: provider.baseUrl,
            model: provider.model,
            contextWindow: editContextTokens,
            supportsVision: editVision,
          }),
        });
        onSaved(updated);
        setStatus('已保存模型 [' + modelId + '] 的视觉能力和上下文长度');
      } else if (mode === 'activate') {
        const created = await request<Provider>('/providers', {
          method: 'POST',
          body: JSON.stringify({
            name: `${name.trim() || 'One-API'} · ${modelId}`,
            kind: 'new-api',
            baseUrl: baseUrl || 'http://127.0.0.1:3000/v1',
            apiKey: apiKey || undefined,
            sourceProviderId: apiKey ? undefined : (editing?.baseUrl === baseUrl ? editing.id : undefined),
            model: modelId,
            contextWindow: editContextTokens,
            supportsVision: editVision,
          }),
        });
        onSaved(created);
        setSelectedModels((items) => {
          const next = new Set(items);
          next.delete(modelId);
          return next;
        });
        setDraftConfigs((items) => {
          const next = { ...items };
          delete next[modelId];
          return next;
        });
        setStatus('已配置并启用模型 ' + modelId);
      } else {
        setDraftConfigs((items) => ({
          ...items,
          [modelId]: { supportsVision: editVision, contextWindow: editContextTokens },
        }));
        setStatus('模型 ' + modelId + ' 的设置已暂存；批量启用后才会写入模型配置。');
      }
      setEditingModel(null);
    } catch (error: unknown) {
      setStatus('保存失败: ' + errorMessage(error));
    } finally {
      setBusy(false);
    }
  };

  const enableSelectedModels = async () => {
    const modelIds = [...selectedModels];
    if (!modelIds.length) return;
    const missingConfig = modelIds.find((modelId) => !draftConfigs[modelId]);
    if (missingConfig) {
      setStatus('请先为所选模型配置视觉能力和上下文长度：' + missingConfig);
      return;
    }
    setBusy(true);
    try {
      const created = await request<Provider[]>('/providers/batch', {
        method: 'POST',
        body: JSON.stringify({
          name: name.trim() || 'One-API',
          kind: 'new-api',
          baseUrl: baseUrl || 'http://127.0.0.1:3000/v1',
          apiKey: apiKey || undefined,
          sourceProviderId: apiKey ? undefined : (editing?.baseUrl === baseUrl ? editing.id : undefined),
          models: modelIds.map((modelId) => ({
            model: modelId,
            contextWindow: draftConfigs[modelId].contextWindow,
            supportsVision: draftConfigs[modelId].supportsVision,
          })),
        }),
      });
      for (const provider of created) onSaved(provider);
      setSelectedModels(new Set());
      setDraftConfigs((items) => {
        const next = { ...items };
        for (const modelId of modelIds) delete next[modelId];
        return next;
      });
      setStatus(`已配置并启用 ${created.length} 个模型`);
    } catch (error: unknown) {
      setStatus('批量启用失败: ' + errorMessage(error));
    } finally {
      setBusy(false);
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
      setStatus('已获取 ' + list.length + ' 个模型。启用前需要分别设置视觉能力和上下文长度。');
    } catch (error: unknown) {
      setStatus('拉取模型失败: ' + errorMessage(error));
    } finally {
      setProbing(false);
    }
  }

  async function toggleModel(modelId: string, currentEnabled: boolean) {
    if (!currentEnabled) {
      openModelConfig(modelId, undefined, 'activate');
      return;
    }
    setBusy(true);
    try {
      const existing = providers.find((p) => p.model === modelId);
      if (existing) {
        await request<void>(`/providers/${existing.id}`, { method: 'DELETE' });
        setDisabledModelIds((items) => {
          const next = new Set(items).add(modelId);
          return next;
        });
        onDeleted(existing.id);
        setSelectedModels((items) => {
          const next = new Set(items);
          next.delete(modelId);
          return next;
        });
        setStatus('已停用模型 ' + modelId);
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
            <p>可单独启用模型，也可以批量启用；每个模型分别保存视觉能力和上下文长度。</p>
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

          <section className="settings-sandbox-section" aria-labelledby="artifact-storage-title">
            <h4 id="artifact-storage-title">成果存储容量</h4>
            <p>按需读取成果后端、上传暂存文件和文件系统剩余空间。该信息用于容量运维，不设置 500MB 以上的任务或项目限制。</p>
            <button type="button" className="action-button" disabled={storageBusy} onClick={() => void inspectArtifactStorage()}>
              {storageBusy ? '读取中…' : '检查存储容量'}
            </button>
            {storageCapacity && (
              <div className="settings-sandbox-details" aria-live="polite">
                {storageCapacity.remoteObjectsAuthoritative ? (
                  <span>正式成果对象：由 S3 兼容存储承载（本机遗留/恢复副本 {formatBytes(storageCapacity.objectBytes)}；远端总容量由对象存储控制台提供）</span>
                ) : <span>正式成果对象：本机内容寻址存储 {formatBytes(storageCapacity.objectBytes)}</span>}
                <span>上传暂存：{formatBytes(storageCapacity.stagingBytes)}</span>
                <span>临时组装文件：{formatBytes(storageCapacity.temporaryBytes)}</span>
                <span>SQLite 元数据：{formatBytes(storageCapacity.metadataDatabaseBytes)}（WAL {formatBytes(storageCapacity.metadataWalBytes)}，SHM {formatBytes(storageCapacity.metadataShmBytes)}）</span>
                {storageCapacity.filesystemMeasured ? (
                  <span>{storageCapacity.remoteObjectsAuthoritative ? '本机暂存磁盘' : '所在磁盘'}：剩余 {formatBytes(storageCapacity.filesystemFreeBytes ?? 0)} / {formatBytes(storageCapacity.filesystemTotalBytes ?? 0)}</span>
                ) : <span>所在磁盘空间：当前平台未提供测量值</span>}
                <span>读取时间：{new Date(storageCapacity.observedAt).toLocaleString()}</span>
              </div>
            )}
            {storageStatus && <div className="settings-status" role="status" aria-live="polite">{storageStatus}</div>}
          </section>

          <section className="settings-sandbox-section" aria-labelledby="sandbox-settings-title">
            <h4 id="sandbox-settings-title">命令沙箱</h4>
            <p>exec_command 和 exec_script 使用这里选择的后端。Linux Bubblewrap 会隔离挂载、进程与默认网络命名空间；联网命令只允许明确列出的主机，并在执行前验证 systemd 出站过滤。CPU、内存和进程数有每命令 cgroup 限额；可写项目目录尚无每任务磁盘配额。Windows 原生沙箱维护需要管理员确认。</p>
            {sandboxConfig ? (
              <>
                <div className="settings-field">
                  <label className="field-label" htmlFor="sandbox-default-backend">默认命令沙箱</label>
                  <select
                    id="sandbox-default-backend"
                    className="text-input"
                    value={sandboxConfig.defaultBackend}
                    disabled={sandboxLoading || sandboxBusy}
                    onChange={(event) => void changeSandboxBackend(event.target.value)}
                  >
                    {sandboxConfig.platform === 'linux' ? (
                      <option value="bubblewrap" disabled={sandboxConfig.linux.health !== 'healthy'}>Linux Bubblewrap 命名空间</option>
                    ) : (
                      <>
                        <option value="appcontainer">Windows AppContainer</option>
                        <option value="windows-native" disabled={sandboxConfig.native.health !== 'healthy'}>Windows 原生低权限账户</option>
                      </>
                    )}
                  </select>
                </div>
                <div className="settings-sandbox-details">
                  {sandboxConfig.platform === 'linux' ? (
                    <>
                      <span>Bubblewrap：{sandboxConfig.linux.installation === 'absent' ? '未安装' : sandboxConfig.linux.installation}</span>
                      <span>健康状态：{sandboxConfig.linux.health === 'healthy' ? '正常' : '不可用'}</span>
                      {sandboxConfig.linux.reason && <span>原因：{sandboxConfig.linux.reason}</span>}
                      <span>联网出站过滤：{sandboxConfig.linux.networkEgressFiltering ? '已验证' : '未验证，联网命令将被拒绝'}</span>
                      {sandboxConfig.linux.networkEgressReason && <span>联网过滤原因：{sandboxConfig.linux.networkEgressReason}</span>}
                    </>
                  ) : (
                    <>
                      <span>原生沙箱：{sandboxConfig.native.installation === 'absent' ? '未安装' : sandboxConfig.native.installation}</span>
                      <span>健康状态：{sandboxConfig.native.health === 'healthy' ? '正常' : '不可用'}</span>
                      {sandboxConfig.native.reason && <span>原因：{sandboxConfig.native.reason}</span>}
                    </>
                  )}
                </div>
                {sandboxConfig.platform !== 'linux' && <div className="settings-sandbox-actions">
                  <button
                    type="button"
                    className="action-button"
                    disabled={sandboxBusy || sandboxLoading || !sandboxConfig.maintenanceAvailable || !sandboxConfig.runnerAvailable}
                    onClick={() => void maintainSandbox(sandboxConfig.native.installation === 'absent' ? 'install' : 'repair')}
                  >
                    {sandboxBusy ? '处理中…' : sandboxConfig.native.installation === 'absent' ? '安装原生沙箱' : '修复原生沙箱'}
                  </button>
                  {sandboxConfig.native.installation !== 'absent' && (
                    <button
                      type="button"
                      className="action-button"
                      disabled={sandboxBusy || sandboxLoading || !sandboxConfig.maintenanceAvailable}
                      onClick={() => void maintainSandbox('uninstall')}
                    >
                      卸载原生沙箱
                    </button>
                  )}
                </div>}
              </>
            ) : (
              <div className="settings-sandbox-details">{sandboxLoading ? '正在读取沙箱状态…' : '暂时没有可显示的沙箱状态。'}</div>
            )}
            {sandboxConfig?.platform !== 'linux' && !sandboxConfig?.maintenanceAvailable && !sandboxLoading && <p>当前安装包未包含 Windows 沙箱维护程序。</p>}
            {sandboxConfig?.maintenanceAvailable && !sandboxConfig.runnerAvailable && sandboxConfig.native.installation === 'absent' && <p>缺少可信命令 runner，当前无法安装原生沙箱。</p>}
            {sandboxStatus && <div className="settings-status" role="status" aria-live="polite">{sandboxStatus}</div>}
          </section>

          <section className="settings-sandbox-section" aria-labelledby="git-credentials-title">
            <h4 id="git-credentials-title">Git HTTPS 凭据</h4>
            <p>凭据在本机加密保存，只按 HTTPS 主机和仓库路径匹配；Windows 原生沙箱通过每条命令独立认证的 Host 通道提供。不要把 Token 放进仓库 URL。</p>
            <div className="settings-form-grid">
              <div className="settings-field">
                <label className="field-label" htmlFor="git-credential-host">主机</label>
                <input id="git-credential-host" className="text-input" value={gitHost} onChange={(event) => setGitHost(event.target.value)} placeholder="github.com" />
              </div>
              <div className="settings-field">
                <label className="field-label" htmlFor="git-credential-repository">仓库路径</label>
                <input id="git-credential-repository" className="text-input" value={gitRepository} onChange={(event) => setGitRepository(event.target.value)} placeholder="owner/repository" />
              </div>
              <div className="settings-field">
                <label className="field-label" htmlFor="git-credential-username">用户名</label>
                <input id="git-credential-username" className="text-input" value={gitUsername} onChange={(event) => setGitUsername(event.target.value)} autoComplete="off" />
              </div>
              <div className="settings-field">
                <label className="field-label" htmlFor="git-credential-password">Token / 密码</label>
                <input id="git-credential-password" className="text-input" type="password" value={gitPassword} onChange={(event) => setGitPassword(event.target.value)} autoComplete="new-password" />
              </div>
            </div>
            <button type="button" className="action-button" disabled={gitCredentialBusy || !gitHost.trim() || !gitRepository.trim() || !gitUsername.trim() || !gitPassword} onClick={() => void saveGitCredentialEntry()}>
              {gitCredentialBusy ? '处理中…' : '保存 Git 凭据'}
            </button>
            <div className="settings-sandbox-details" aria-live="polite">
              {gitCredentialLoading ? '正在读取已保存凭据…' : gitCredentials.length === 0 ? '没有已保存的 Git 凭据。' : gitCredentials.map((credential) => (
                <div key={credential.id} className="settings-sandbox-actions">
                  <span>{credential.host}/{credential.repository} · {credential.username}</span>
                  <button type="button" className="action-button" disabled={gitCredentialBusy} onClick={() => void removeGitCredential(credential.id)}>删除</button>
                </div>
              ))}
            </div>
            {gitCredentialStatus && <div className="settings-status" role="status" aria-live="polite">{gitCredentialStatus}</div>}
          </section>

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
                placeholder={editing?.hasApiKey ? '留空沿用现有 Token（更改接口地址时请重新填写）' : 'sk-...'}
              />
              <button type="button" className="action-button" disabled={probing} onClick={probeModels}>
                {probing ? '正在拉取...' : '拉取网关模型'}
              </button>
            </div>
          </div>

          <div className="settings-list-heading">
            <h4>可用模型列表 ({allModels.length} 个模型)</h4>
            <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
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
              <button
                type="button"
                className="action-button"
                disabled={busy || selectedModels.size === 0 || [...selectedModels].some((modelId) => !draftConfigs[modelId])}
                onClick={() => void enableSelectedModels()}
                title={selectedModels.size > 0 && [...selectedModels].some((modelId) => !draftConfigs[modelId]) ? '请先配置每个所选模型' : ''}
              >
                启用所选模型 ({selectedModels.size})
              </button>
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
                  const isSelected = selectedModels.has(modelId);
                  const isDraftConfigured = Boolean(draftConfigs[modelId]);
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
                          {!isEnabled && isSelected && (
                            <span className="settings-model-config-state">
                              {isDraftConfigured ? '已填写' : '待配置'}
                            </span>
                          )}
                          {isEnabled && (
                            <div className="settings-model-tags">
                              <button
                                type="button"
                                className={"model-tag-pill " + (modelCfg.supportsVision ? "vision-on" : "vision-off")}
                                onClick={(e) => {
                                  e.stopPropagation();
                                  if (prov) openModelConfig(modelId, prov, 'update');
                                }}
                                title="打开模型设置修改视觉能力和上下文长度"
                              >
                                {modelCfg.supportsVision ? "👁 视觉" : "🚫 纯文本"}
                              </button>
                              <span
                                className="model-tag-pill context"
                                title="当前模型上下文窗口上限 (点击配置修改)"
                                onClick={(e) => {
                                  e.stopPropagation();
                                  if (prov) openModelConfig(modelId, prov, 'update');
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
                              openModelConfig(modelId, prov, 'update');
                            }}
                            title="修改模型配置 (自定义别名、上下文窗口、视觉开关)"
                            aria-label={"编辑修改 " + modelId + " 配置"}
                          >
                            <SlidersHorizontal size={12} strokeWidth={2} />
                            <span>配置</span>
                          </button>
                        )}
                        {!isEnabled && (
                          <>
                            <label className="settings-model-select">
                              <input
                                type="checkbox"
                                checked={isSelected}
                                disabled={busy}
                                onChange={(event) => {
                                  setSelectedModels((items) => {
                                    const next = new Set(items);
                                    if (event.target.checked) next.add(modelId);
                                    else next.delete(modelId);
                                    return next;
                                  });
                                }}
                                aria-label={`选择 ${modelId} 以批量启用`}
                              />
                              <span>选择</span>
                            </label>
                            {isSelected && (
                              <button
                                type="button"
                                className="settings-model-edit-btn"
                                onClick={(e) => {
                                  e.stopPropagation();
                                  openModelConfig(modelId, undefined, 'draft');
                                }}
                                title="设置此模型的视觉能力和上下文长度"
                              >
                                <SlidersHorizontal size={12} strokeWidth={2} />
                                <span>配置</span>
                              </button>
                            )}
                          </>
                        )}
                        <button
                          type="button"
                          aria-label={isEnabled ? '停用 ' + modelId : '配置并启用 ' + modelId}
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
                <h4>{editingModel.mode === 'update' ? '模型配置' : '启用模型 · 配置能力'} · {editingModel.modelId}</h4>
                <button
                  type="button"
                  onClick={() => setEditingModel(null)}
                  style={{ background: 'none', border: 'none', cursor: 'pointer', color: 'var(--text-muted)' }}
                >
                  <X size={15} strokeWidth={2} />
                </button>
              </header>
              <div className="model-edit-body">
                {editingModel.mode === 'update' && (
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
                )}

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
                      min={1}
                      max={2000000}
                      value={editContextTokens}
                      onChange={(e) => setEditContextTokens(Math.max(1, Math.min(2000000, Number(e.target.value) || 131072)))}
                    />
                    <span style={{ fontSize: 11, color: 'var(--text-muted)' }}>Tokens（持久化对话约使用 55%，其余空间预留给系统指令、工具、推理和输出）</span>
                  </div>
                </div>
              </div>
              <footer className="model-edit-footer">
                <button type="button" className="text-button" disabled={busy} onClick={() => setEditingModel(null)}>
                  取消
                </button>
                <button type="button" className="action-button" disabled={busy} onClick={() => void handleSaveModelEdit()}>
                  {busy ? '保存中…' : editingModel.mode === 'activate' ? '保存并启用' : editingModel.mode === 'draft' ? '暂存设置' : '保存模型配置'}
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
