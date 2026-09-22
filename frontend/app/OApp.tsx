'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Plus, MessageSquare, Layers, Settings as SettingsIcon, ArrowUp, Square, Sparkles, KeyRound, AlertCircle, ChevronDown, Check, X, Pencil, Folder, FolderPlus, MoreHorizontal, LogOut, ChevronRight } from 'lucide-react';
import UnifiedPluginCenter from './UnifiedPluginCenter';
import { Settings } from './SettingsModal';
import ProjectModal from './ProjectModal';
import { MarkdownView } from './MarkdownView';
import { API_V2, request, UnifiedPlugin, getUnifiedPlugins, updateConversationTitle, generateConversationTitle, Project, getProjects, updateConversationProject } from './api';

export type Provider = { id: string; name: string; kind: string; baseUrl: string; model: string; contextWindow: number; hasApiKey: boolean };
type Conversation = { id: string; title: string; providerId: string; agentGenerationId?: string; agentDefinitionDigest?: string; projectId?: string; updatedAt: string };
type Message = { id: string; role: 'user' | 'assistant'; content: string; createdAt: string };
type ConversationDetail = Conversation & { messages: Message[] };
type TraceEvent = { id: string; turnId: string; sequence: number; kind: string; details: Record<string, unknown>; createdAt: string };
type AgentTurn = { id: string; conversationId: string; inputMessageId: string; resultMessageId?: string; providerId: string; status: string; stopReason?: string; recoveryClass?: string; cancelRequested: boolean; lastSequence: number; startedAt: string; completedAt?: string };
type TurnReceipt = { turnId: string; conversationId: string; inputMessageId: string; status: string };

export function cleanTitleString(rawTitle?: string): string {
  if (!rawTitle) return '';
  let trimmed = rawTitle.trim();
  const chineseMatch = trimmed.match(/^【(.*?)】\s*(.*)$/);
  if (chineseMatch) {
    trimmed = chineseMatch[2].trim() || chineseMatch[1].trim();
  } else {
    const englishMatch = trimmed.match(/^\[(.*?)\]\s*(.*)$/);
    if (englishMatch) {
      trimmed = englishMatch[2].trim() || englishMatch[1].trim();
    }
  }
  trimmed = trimmed.replace(/^(?:会话标题|标题|Title|title)[:：]\s*/, '');
  return trimmed.trim();
}

export function shouldAutoTitle(title?: string): boolean {
  if (!title) return true;
  const t = title.trim();
  if (t === '新对话' || t === 'New mission' || t === '新的工作区' || t === '新任务') return true;
  if (t.startsWith('【') || t.startsWith('[')) return true;
  return false;
}

export default function OApp() {
  const [loading, setLoading] = useState(true);
  const [providers, setProviders] = useState<Provider[]>([]);
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [active, setActive] = useState<ConversationDetail | null>(null);
  const [selectedProviderId, setSelectedProviderId] = useState<string>('');
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [pluginsOpen, setPluginsOpen] = useState<false | 'all' | 'mcp' | 'skill' | 'core' | 'release'>(false);
  const [unifiedPlugins, setUnifiedPlugins] = useState<UnifiedPlugin[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [editingProject, setEditingProject] = useState<Project | null | undefined>(undefined);

  const isProjectPluginEnabled = useMemo(() => {
    const p = unifiedPlugins.find((item) => item.id === 'core:project_workspace');
    return p ? p.status === 'enabled' : true;
  }, [unifiedPlugins]);

  const activeProject = useMemo(() => {
    if (!active?.projectId) return null;
    return projects.find((p) => p.id === active.projectId) || null;
  }, [active?.projectId, projects]);

  const [headerProjectMenuOpen, setHeaderProjectMenuOpen] = useState(false);

  useEffect(() => {
    if (!headerProjectMenuOpen) return;
    function handleOutside(e: MouseEvent) {
      const target = e.target as HTMLElement;
      if (!target.closest('.header-project-dropdown') && !target.closest('.project-header-badge')) {
        setHeaderProjectMenuOpen(false);
      }
    }
    window.addEventListener('click', handleOutside);
    return () => window.removeEventListener('click', handleOutside);
  }, [headerProjectMenuOpen]);

  const refreshPlugins = useCallback(async () => {
    try {
      const [list, projList] = await Promise.all([
        getUnifiedPlugins(),
        getProjects().catch(() => [] as Project[]),
      ]);
      setUnifiedPlugins(list);
      if (projList) setProjects(projList);
    } catch {}
  }, []);

  // Per-conversation running state: conversationId -> { turnId: string, trace: TraceEvent[] }
  const [runningConvos, setRunningConvos] = useState<Record<string, { turnId: string; trace: TraceEvent[] }>>({});
  const runningConvosRef = useRef<Record<string, { turnId: string; trace: TraceEvent[] }>>({});
  runningConvosRef.current = runningConvos;

  const observersRef = useRef<Map<string, { turnId: string; controller: AbortController; promise: Promise<void> }>>(new Map());

  const [notice, setNotice] = useState('');
  const [trace, setTrace] = useState<TraceEvent[]>([]);
  const [turns, setTurns] = useState<AgentTurn[]>([]);
  const activeIdRef = useRef('');
  const activeRef = useRef<ConversationDetail | null>(null);
  activeRef.current = active;

  // Title editing state
  const [editingTitle, setEditingTitle] = useState(false);
  const [titleDraft, setTitleDraft] = useState('');
  const titleInputRef = useRef<HTMLInputElement>(null);
  const isSavingTitleRef = useRef(false);
  const isCancellingTitleRef = useRef(false);

  useEffect(() => {
    if (editingTitle && titleInputRef.current) {
      titleInputRef.current.focus();
      titleInputRef.current.select();
    }
  }, [editingTitle]);

  const isTitlePluginEnabled = useMemo(() => {
    const p = unifiedPlugins.find((item) => item.id === 'core:conversation_title');
    return p ? p.status === 'enabled' : true;
  }, [unifiedPlugins]);

  const autoUpdateTitle = useCallback(async (conversationId: string, providerId?: string) => {
    try {
      const updated = await generateConversationTitle<ConversationDetail>(conversationId, providerId);
      if (updated?.title) {
        const cleaned = cleanTitleString(updated.title);
        if (activeIdRef.current === conversationId) {
          setActive((prev) => (prev && prev.id === conversationId ? { ...prev, title: cleaned } : prev));
        }
        setConversations((items) => items.map((c) => (c.id === conversationId ? { ...c, title: cleaned } : c)));
      }
    } catch {}
  }, []);

  async function handleSaveTitle(newTitle: string, targetId?: string) {
    if (isCancellingTitleRef.current) {
      isCancellingTitleRef.current = false;
      return;
    }
    const convoId = targetId || activeRef.current?.id;
    if (!convoId) {
      setEditingTitle(false);
      return;
    }
    const trimmed = newTitle.trim();
    if (!trimmed) {
      setEditingTitle(false);
      return;
    }
    const currentTitle = activeRef.current?.title ? cleanTitleString(activeRef.current.title) : '';
    if (trimmed === currentTitle) {
      setEditingTitle(false);
      return;
    }
    if (isSavingTitleRef.current) return;
    isSavingTitleRef.current = true;
    try {
      const cleaned = cleanTitleString(trimmed) || trimmed;
      if (activeRef.current?.id === convoId) {
        setActive((prev) => (prev && prev.id === convoId ? { ...prev, title: cleaned } : prev));
      }
      setConversations((items) => items.map((c) => (c.id === convoId ? { ...c, title: cleaned } : c)));

      const updated = await updateConversationTitle<ConversationDetail>(convoId, cleaned);
      const serverCleaned = cleanTitleString(updated.title) || cleaned;
      if (activeRef.current?.id === convoId) {
        setActive((prev) => (prev && prev.id === convoId ? { ...prev, title: serverCleaned } : prev));
      }
      setConversations((items) => items.map((c) => (c.id === convoId ? { ...c, title: serverCleaned } : c)));
      setNotice('会话标题已更新');
    } catch (err) {
      setNotice(err instanceof Error ? err.message : '更新标题失败');
    } finally {
      isSavingTitleRef.current = false;
      setEditingTitle(false);
    }
  }

  useEffect(() => {
	const observers = observersRef.current;
    return () => {
	  for (const obs of observers.values()) {
        obs.controller.abort();
      }
	  observers.clear();
    };
  }, []);

  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'n') {
        e.preventDefault();
        newConversation();
      }
    }
    window.addEventListener('keydown', handleKeyDown);
    return () => window.removeEventListener('keydown', handleKeyDown);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    void Promise.all([
      request<Provider[]>('/providers'),
      request<Conversation[]>('/conversations'),
      getUnifiedPlugins().catch(() => [] as UnifiedPlugin[]),
      getProjects().catch(() => [] as Project[]),
    ])
      .then(([providerList, conversationList, pluginList, projectList]) => {
        setProviders(providerList);
        setConversations(conversationList);
        if (pluginList.length > 0) setUnifiedPlugins(pluginList);
        if (projectList) setProjects(projectList);
      })
      .catch((error) => setNotice(error instanceof Error ? error.message : '无法连接本地运行时'))
      .finally(() => setLoading(false));
  }, []);

  function observeTurn(turnId: string, conversationId: string): Promise<void> {
    const existing = observersRef.current.get(conversationId);
    if (existing && existing.turnId === turnId) {
      return existing.promise;
    }
    existing?.controller.abort();

    const controller = new AbortController();
    setRunningConvos((prev) => ({
      ...prev,
      [conversationId]: { turnId, trace: prev[conversationId]?.trace || [] },
    }));

    const promise = waitForTurn(
      turnId,
      (event) => {
        setRunningConvos((prev) => {
          const item = prev[conversationId] || { turnId, trace: [] };
          if (item.trace.some((t) => t.id === event.id)) return prev;
          return {
            ...prev,
            [conversationId]: { turnId, trace: [...item.trace, event] },
          };
        });
        if (activeIdRef.current === conversationId) {
          setTrace((items) => (items.some((item) => item.id === event.id) ? items : [...items, event]));
        }
        if (event.kind === 'turn.failed') {
          const errText = typeof event.details.error === 'string' ? event.details.error : 'Agent 运行失败';
          if (/image|vision|multimodal|400 Bad Request/i.test(errText)) {
            setNotice('模型调用失败：上游接口返回「' + errText + '」。若该模型不支持图片输入，可在左下角「模型设置」中将其标记为纯文本，或在右下角切换为支持多模态的模型。');
          } else {
            setNotice(errText);
          }
        }
		if (event.kind === 'turn.cancelled') setNotice('任务已停止，已完成的过程仍保留在运行记录中。');
		if (event.kind === 'turn.incomplete') setNotice('任务已达到执行步数上限；当前总结和已完成过程已保留，但任务尚未完成。');
		if (event.kind === 'turn.needs_reconciliation') setNotice('任务产生了需要确认的外部影响，处理后才能重试。');
      },
      controller.signal
    )
      .catch((error) => {
        if (!controller.signal.aborted) {
          setNotice(error instanceof Error ? error.message : '运行异常中断');
        }
      })
      .finally(() => {
        observersRef.current.delete(conversationId);
        setRunningConvos((prev) => {
          const next = { ...prev };
          delete next[conversationId];
          return next;
        });
        void refreshConversation(conversationId).then((refreshed) => {
          if (refreshed && shouldAutoTitle(refreshed.title) && refreshed.messages.length >= 1 && isTitlePluginEnabled) {
            void autoUpdateTitle(conversationId, refreshed.providerId || selectedProviderId);
          }
        });
      });

    observersRef.current.set(conversationId, { turnId, controller, promise });
    return promise;
  }

  async function refreshConversation(id: string): Promise<ConversationDetail | null> {
    try {
    const [detail, events, conversationTurns] = await Promise.all([
      request<ConversationDetail>(`/conversations/${id}`),
      request<TraceEvent[]>(`/conversations/${id}/trace`),
      request<AgentTurn[]>(`/conversations/${id}/turns`),
    ]);
    if (activeIdRef.current === id) {
      setActive(detail);
      setTrace(events);
      setTurns(conversationTurns);
      }
      setConversations((items) => items.map((item) => (item.id === detail.id ? detail : item)));
      return detail;
    } catch {
      return null;
    }
  }

  async function openConversation(id: string) {
    setEditingTitle(false);
    activeIdRef.current = id;
    const [detail, events, turns] = await Promise.all([
      request<ConversationDetail>(`/conversations/${id}`),
      request<TraceEvent[]>(`/conversations/${id}/trace`),
      request<AgentTurn[]>(`/conversations/${id}/turns`),
    ]);
    if (activeIdRef.current !== id) return;
    setActive(detail);
    setTurns(turns);
    setNotice('');

    const runningItem = runningConvosRef.current[id];
    if (runningItem) {
      setTrace(runningItem.trace);
    } else {
      setTrace(events);
    }

    if (shouldAutoTitle(detail.title) && detail.messages && detail.messages.length >= 1 && isTitlePluginEnabled) {
      void autoUpdateTitle(id, detail.providerId || selectedProviderId);
    }

    const running = turns.find((turn) => turn.status === 'running' || turn.status === 'cancelling');
    if (running) {
      void observeTurn(running.id, id);
    }
  }

  const handleMoveConversation = useCallback(async (conversationId: string, projectId: string) => {
    try {
      await updateConversationProject(conversationId, projectId);
      setConversations((items) =>
        items.map((c) => (c.id === conversationId ? { ...c, projectId: projectId || undefined } : c))
      );
      if (activeIdRef.current === conversationId) {
        setActive((prev) => (prev && prev.id === conversationId ? { ...prev, projectId: projectId || undefined } : prev));
      }
      const targetProject = projects.find((p) => p.id === projectId);
      setNotice(projectId ? `对话已归入「${targetProject?.name || '项目'}」` : '对话已移出项目');
    } catch (err) {
      setNotice(err instanceof Error ? err.message : '转移项目失败');
    }
  }, [projects]);

  async function handleNewInProject(projectId: string) {
    if (!providers.length) {
      setSettingsOpen(true);
      return;
    }
    try {
      const created = await request<Conversation>('/conversations', {
        method: 'POST',
        body: JSON.stringify({
          title: '新对话',
          providerId: selectedProviderId || providers[0]?.id,
          projectId,
        }),
      });
      setConversations((items) => [created, ...items]);
      await openConversation(created.id);
    } catch (err) {
      setNotice(err instanceof Error ? err.message : '创建对话失败');
    }
  }

  function newConversation() {
    // 1. 若当前已经是未发消息的新会话草稿状态，直接保持当前界面，不重复创建
    if (!activeRef.current || (activeRef.current.messages && activeRef.current.messages.length === 0)) {
      return;
    }
    // 2. 如果列表中已存在未发消息或标题为“新对话”的会话，直接切换过去，不新建多余会话
    const existingNew = conversations.find(
      (item) => cleanTitleString(item.title) === '新对话' || !cleanTitleString(item.title)
    );
    if (existingNew) {
      void openConversation(existingNew.id);
      return;
    }
    // 3. 否则切换到草稿态（待发送首条消息时再持久化）
    setEditingTitle(false);
    activeIdRef.current = '';
    setActive(null);
    setTrace([]);
    setTurns([]);
    setNotice('');
  }

  async function send(content: string) {
    if (!content.trim()) return;
    if (active && runningConvosRef.current[active.id]) return;

    setNotice('');
    try {
      let target = active;
      if (!target) {
        if (!providers.length) {
          setSettingsOpen(true);
          return;
        }
        const created = await request<Conversation>('/conversations', {
          method: 'POST',
          body: JSON.stringify({ title: '新对话', providerId: selectedProviderId || providers[0]?.id }),
        });
        target = { ...created, messages: [] };
        activeIdRef.current = created.id;
        setConversations((items) => [created, ...items]);
        setActive(target);
      }
      const pending: Message = { id: `pending-${Date.now()}`, role: 'user', content, createdAt: new Date().toISOString() };
      setActive({ ...target, messages: [...target.messages, pending] });
      const receipt = await request<TurnReceipt>(`${API_V2}/agent/conversations/${target.id}/turns`, {
        method: 'POST',
        body: JSON.stringify({ content }),
      });
      void observeTurn(receipt.turnId, target.id);
    } catch (error) {
      setNotice(error instanceof Error ? error.message : 'Agent 运行失败');
    }
  }

  async function cancelTurn() {
    if (!active) return;
    const runningItem = runningConvosRef.current[active.id];
    if (!runningItem) return;
    try {
      await request(`/agent/turns/${runningItem.turnId}/cancel`, { method: 'POST', body: JSON.stringify({ reason: 'user_requested' }) });
      setNotice('正在安全停止当前任务…');
    } catch (error) {
      setNotice(error instanceof Error ? error.message : '无法停止当前任务');
    }
  }


  const isCurrentSending = active ? !!runningConvos[active.id] : false;
  const currentTrace = active ? (runningConvos[active.id]?.trace ?? trace) : [];
  const backgroundRunningCount = useMemo(() => {
    return Object.keys(runningConvos).filter((id) => id !== active?.id).length;
  }, [runningConvos, active?.id]);

  if (loading) return <Splash />;
  return (
    <main className="app-shell">
      <Sidebar
        conversations={conversations}
        projects={projects}
        activeId={active?.id}
        runningConvos={runningConvos}
        isProjectPluginEnabled={isProjectPluginEnabled}
        onNew={newConversation}
        onNewInProject={handleNewInProject}
        onOpen={openConversation}
        onPlugins={() => setPluginsOpen('all')}
        onSettings={() => setSettingsOpen(true)}
        onCreateProject={() => setEditingProject(null)}
        onEditProject={(proj) => setEditingProject(proj)}
        onMoveConversation={handleMoveConversation}
      />
      <section className="workspace">
        <header className="workspace-header">
          {!active ? (
            <div className="header-title-static">
              <h1 className="header-title-text">新对话</h1>
            </div>
          ) : editingTitle ? (
            <div className="header-title-edit-container">
              <form
                className="header-title-edit-form"
                onSubmit={(e) => {
                  e.preventDefault();
                  void handleSaveTitle(titleDraft);
                }}
              >
                <input
                  ref={titleInputRef}
                  type="text"
                  className="header-title-inline-input"
                  value={titleDraft}
                  onChange={(e) => setTitleDraft(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Escape') {
                      e.preventDefault();
                      isCancellingTitleRef.current = true;
                      setEditingTitle(false);
                    }
                  }}
                  onBlur={() => {
                    if (!isCancellingTitleRef.current) {
                      void handleSaveTitle(titleDraft);
                    }
                  }}
                  placeholder="输入标题"
                  maxLength={50}
                  autoFocus
                />
                <button
                  type="submit"
                  className="header-title-btn-confirm"
                  onMouseDown={(e) => e.preventDefault()}
                  title="确认保存 (Enter)"
                  aria-label="确认保存标题"
                >
                  <Check size={13} strokeWidth={2.5} />
                </button>
                <button
                  type="button"
                  className="header-title-btn-cancel"
                  onMouseDown={(e) => {
                    e.preventDefault();
                    isCancellingTitleRef.current = true;
                  }}
                  onClick={() => {
                    isCancellingTitleRef.current = true;
                    setEditingTitle(false);
                  }}
                  title="取消 (Esc)"
                  aria-label="取消编辑"
                >
                  <X size={13} strokeWidth={2.5} />
                </button>
              </form>
            </div>
          ) : (
            <div className="header-title-display-container">
              <div className="header-title-main-row">
                <div
                  className="header-title-interactive-block"
                  onClick={() => {
                    setTitleDraft(cleanTitleString(active.title) || '新对话');
                    setEditingTitle(true);
                  }}
                  title="点击修改会话标题"
                  role="button"
                  tabIndex={0}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      setTitleDraft(cleanTitleString(active.title) || '新对话');
                      setEditingTitle(true);
                    }
                  }}
                >
                  <h1 className="header-title-text">{cleanTitleString(active.title) || '新对话'}</h1>
                  <span className="header-title-hover-icon" aria-hidden="true" title="点击修改标题">
                    <Pencil size={12} strokeWidth={2} />
                  </span>
                </div>
                {isProjectPluginEnabled && (
                  <div className="header-project-dropdown" style={{ position: 'relative', display: 'inline-flex', alignItems: 'center' }}>
                    {activeProject ? (
                      <button
                        type="button"
                        className="project-header-badge"
                        onClick={() => setHeaderProjectMenuOpen((prev) => !prev)}
                        title="点击选择归属项目或配置"
                      >
                        <Folder size={12} />
                        <span>{activeProject.name}</span>
                        {activeProject.instructionsEnabled && (
                          <span
                            title="已开启共享上下文设定"
                            style={{
                              width: 5,
                              height: 5,
                              borderRadius: '50%',
                              background: '#10b981',
                              display: 'inline-block',
                              marginLeft: 2,
                            }}
                          />
                        )}
                        <ChevronDown size={11} strokeWidth={2} style={{ opacity: 0.6 }} />
                      </button>
                    ) : (
                      <button
                        type="button"
                        className="project-header-badge unassigned"
                        onClick={() => setHeaderProjectMenuOpen((prev) => !prev)}
                        title="点击选择归属项目"
                      >
                        <Folder size={12} />
                        <span>未分配项目</span>
                        <ChevronDown size={11} strokeWidth={2} style={{ opacity: 0.6 }} />
                      </button>
                    )}

                    {headerProjectMenuOpen && (
                      <div
                        className="sidebar-popover-menu"
                        style={{ top: '100%', left: 0, right: 'auto', marginTop: 4, minWidth: 170 }}
                        onClick={(e) => e.stopPropagation()}
                      >
                        {projects.length > 0 ? (
                          projects.map((p) => {
                            const isSelected = active.projectId === p.id;
                            return (
                              <button
                                key={p.id}
                                type="button"
                                className={`sidebar-popover-item${isSelected ? ' active' : ''}`}
                                onClick={() => {
                                  if (!isSelected) handleMoveConversation(active.id, p.id);
                                  setHeaderProjectMenuOpen(false);
                                }}
                              >
                                <Folder size={13} />
                                <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{p.name}</span>
                                {isSelected && <Check size={12} strokeWidth={2.2} />}
                              </button>
                            );
                          })
                        ) : (
                          <div className="sidebar-popover-empty">暂无可用项目</div>
                        )}
                        <div className="sidebar-popover-divider" />
                        {activeProject && (
                          <>
                            <button
                              type="button"
                              className="sidebar-popover-item"
                              onClick={() => {
                                setEditingProject(activeProject);
                                setHeaderProjectMenuOpen(false);
                              }}
                            >
                              <SettingsIcon size={13} />
                              <span>项目配置...</span>
                            </button>
                            <button
                              type="button"
                              className="sidebar-popover-item danger"
                              onClick={() => {
                                handleMoveConversation(active.id, '');
                                setHeaderProjectMenuOpen(false);
                              }}
                            >
                              <LogOut size={13} />
                              <span>移出项目</span>
                            </button>
                            <div className="sidebar-popover-divider" />
                          </>
                        )}
                        <button
                          type="button"
                          className="sidebar-popover-item"
                          onClick={() => {
                            setEditingProject(null);
                            setHeaderProjectMenuOpen(false);
                          }}
                        >
                          <FolderPlus size={13} />
                          <span>新建项目</span>
                        </button>
                      </div>
                    )}
                  </div>
                )}
              </div>
            </div>
          )}
          <div className="header-actions">
            {isCurrentSending ? (
              <span className="status-pill active" aria-live="polite">
                <i className="status-dot" aria-hidden="true" />
                <span>任务运行中</span>
              </span>
            ) : backgroundRunningCount > 0 ? (
              <span className="status-pill background" aria-live="polite" title="后台任务运行中">
                <i className="status-dot" aria-hidden="true" />
                <span>后台 {backgroundRunningCount} 个任务运行中</span>
              </span>
            ) : (
              <span className="status-pill" aria-live="polite">
                <i className="status-dot" aria-hidden="true" />
                <span>就绪</span>
              </span>
            )}
          </div>
        </header>
        <div className="workspace-grid">
          <Chat
            active={active}
            providers={providers}
            selectedProviderId={selectedProviderId}
            onSelectProvider={(id) => {
              setSelectedProviderId(id);
              if (active) {
                setActive((prev) => prev ? { ...prev, providerId: id } : null);
                setConversations((items) => items.map((c) => c.id === active.id ? { ...c, providerId: id } : c));
              }
            }}
            sending={isCurrentSending}
            notice={notice}
            trace={trace}
            liveTrace={currentTrace}
            turns={turns}
            unifiedPlugins={unifiedPlugins}
            onNotice={setNotice}
            onSend={send}
            onCancel={cancelTurn}
            onConfigure={() => setSettingsOpen(true)}
            onOpenPlugins={(type) => setPluginsOpen(type || 'all')}
          />
        </div>
      </section>

      {settingsOpen && (
        <Settings
          providers={providers}
          onClose={() => setSettingsOpen(false)}
          onSaved={(next) => {
            setProviders((items) =>
              items.some((item) => item.id === next.id)
                ? items.map((item) => (item.id === next.id ? next : item))
                : [next, ...items]
            );
            setNotice('模型服务已保存');
          }}
          onDeleted={(providerId) => {
            setProviders((items) => items.filter((item) => item.id !== providerId));
            setNotice('模型已停用并删除');
          }}
        />
      )}

      {pluginsOpen && (
        <UnifiedPluginCenter
          initialType={pluginsOpen || 'all'}
          onClose={() => {
            setPluginsOpen(false);
            void refreshPlugins();
          }}
          onPluginsChanged={refreshPlugins}
        />
      )}

      {editingProject !== undefined && (
        <ProjectModal
          project={editingProject}
          onClose={() => setEditingProject(undefined)}
          onSaved={(savedProj) => {
            setProjects((items) =>
              items.some((p) => p.id === savedProj.id)
                ? items.map((p) => (p.id === savedProj.id ? savedProj : p))
                : [savedProj, ...items]
            );
            setNotice(editingProject ? '项目设置已保存' : '项目已创建');
          }}
          onDeleted={(deletedId) => {
            setProjects((items) => items.filter((p) => p.id !== deletedId));
            setConversations((items) =>
              items.map((c) => (c.projectId === deletedId ? { ...c, projectId: undefined } : c))
            );
            if (active?.projectId === deletedId) {
              setActive((prev) => (prev ? { ...prev, projectId: undefined } : prev));
            }
            setNotice('项目已删除，其下对话已移至普通列表');
          }}
        />
      )}
    </main>
  );
}

function waitForTurn(turnId: string, onEvent: (event: TraceEvent) => void, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const source = new EventSource(`${API_V2}/agent/turns/${encodeURIComponent(turnId)}/events`);
    signal?.addEventListener(
      'abort',
      () => {
        source.close();
        resolve();
      },
      { once: true }
    );
    source.onmessage = (message) => {
      try {
        const event = JSON.parse(message.data) as TraceEvent;
        onEvent(event);
        if (
		  event.kind === 'turn.completed' ||
		  event.kind === 'turn.incomplete' ||
		  event.kind === 'turn.failed' ||
          event.kind === 'turn.cancelled' ||
          event.kind === 'turn.needs_reconciliation'
        ) {
          source.close();
          resolve();
        }
      } catch (error) {
        source.close();
        reject(error);
      }
    };
    source.onerror = () => {
      if (source.readyState === EventSource.CLOSED) reject(new Error('任务完成前事件流意外关闭。'));
    };
  });
}

function Splash() {
  return (
    <div className="splash">
      <div className="brand-mark">O</div>
      <span>正在启动插件运行时…</span>
    </div>
  );
}

function deduplicateNewChats(list: Conversation[]): Conversation[] {
  const result: Conversation[] = [];
  let hasDefault = false;
  for (const item of list) {
    const displayTitle = cleanTitleString(item.title) || '新对话';
    if (displayTitle === '新对话') {
      if (!hasDefault) {
        result.push(item);
        hasDefault = true;
      }
    } else {
      result.push(item);
    }
  }
  return result;
}

function Sidebar({
  conversations,
  projects,
  activeId,
  runningConvos,
  isProjectPluginEnabled,
  onNew,
  onNewInProject,
  onOpen,
  onPlugins,
  onSettings,
  onCreateProject,
  onEditProject,
  onMoveConversation,
}: {
  runningConvos?: Record<string, unknown>;
  conversations: Conversation[];
  projects: Project[];
  activeId?: string;
  isProjectPluginEnabled: boolean;
  onNew: () => void;
  onNewInProject: (projectId: string) => void;
  onOpen: (id: string) => void;
  onPlugins: () => void;
  onSettings: () => void;
  onCreateProject: () => void;
  onEditProject: (project: Project) => void;
  onMoveConversation: (conversationId: string, projectId: string) => void;
}) {
  const [collapsedProjects, setCollapsedProjects] = useState<Record<string, boolean>>({});
  const [popoverConvoId, setPopoverConvoId] = useState<string | null>(null);

  useEffect(() => {
    if (!popoverConvoId) return;
    function handleOutside(e: MouseEvent) {
      const target = e.target as HTMLElement;
      if (!target.closest('.sidebar-popover-menu') && !target.closest('.sidebar-convo-action-btn')) {
        setPopoverConvoId(null);
      }
    }
    window.addEventListener('click', handleOutside);
    return () => window.removeEventListener('click', handleOutside);
  }, [popoverConvoId]);

  const toggleProjectCollapse = (projectId: string) => {
    setCollapsedProjects((prev) => ({
      ...prev,
      [projectId]: !prev[projectId],
    }));
  };

  const unassignedConversations = useMemo(() => {
    const raw = isProjectPluginEnabled ? conversations.filter((item) => !item.projectId) : conversations;
    return deduplicateNewChats(raw);
  }, [conversations, isProjectPluginEnabled]);

  return (
    <aside className="sidebar">
      <div className="brand">
        <div className="brand-mark">
          <Sparkles size={13} strokeWidth={2.2} />
        </div>
        <span className="brand-name">O Agent</span>
        <span className="brand-badge">0.2</span>
      </div>
      <button
        type="button"
        className={`new-button${!activeId ? ' active' : ''}`}
        onClick={onNew}
        aria-label="新建对话"
      >
        <Plus size={16} strokeWidth={2} aria-hidden="true" />
        <span className="new-button-label">新对话</span>
        <kbd aria-hidden="true">⌘ N</kbd>
      </button>

      <div className="sidebar-scroll-area">
        {/* 项目文件夹区域（置于对话栏上方，以“项目”命名） */}
        {isProjectPluginEnabled && (
          <section className="sidebar-section" aria-label="项目列表">
            <div className="sidebar-section-header">
              <span className="sidebar-section-title">项目</span>
              <button
                type="button"
                className="sidebar-section-action"
                onClick={onCreateProject}
                title="新建项目文件夹"
                aria-label="新建项目"
              >
                <Plus size={13} strokeWidth={2} />
              </button>
            </div>

            {projects.length === 0 ? (
              <button
                type="button"
                className="sidebar-project-empty-btn"
                onClick={onCreateProject}
                title="点击创建第一个项目文件夹"
              >
                <FolderPlus size={13} />
                <span>新建项目文件夹</span>
              </button>
            ) : (
              projects.map((proj) => {
                const isCollapsed = Boolean(collapsedProjects[proj.id]);
                const projConvos = deduplicateNewChats(conversations.filter((c) => c.projectId === proj.id));

                return (
                  <div key={proj.id} className="sidebar-project-item">
                    <div
                      className="sidebar-project-header"
                      onClick={() => toggleProjectCollapse(proj.id)}
                      title={`${proj.name}${proj.workdir ? ` (${proj.workdir})` : ''}`}
                    >
                      <span className={`sidebar-project-chevron${!isCollapsed ? ' expanded' : ''}`}>
                        <ChevronRight size={12} strokeWidth={2.2} />
                      </span>
                      <span className="sidebar-project-icon">
                        <Folder size={14} />
                      </span>
                      <span className="sidebar-project-name">{proj.name}</span>
                      <span className="sidebar-project-count" title={`${projConvos.length} 个对话`}>
                        {projConvos.length}
                      </span>

                      <div className="sidebar-project-actions" onClick={(e) => e.stopPropagation()}>
                        <button
                          type="button"
                          className="sidebar-project-action-btn"
                          onClick={() => {
                            setCollapsedProjects((prev) => ({ ...prev, [proj.id]: false }));
                            onNewInProject(proj.id);
                          }}
                          title="在该项目新建对话"
                          aria-label="新建对话"
                        >
                          <Plus size={13} strokeWidth={2} />
                        </button>
                        <button
                          type="button"
                          className="sidebar-project-action-btn"
                          onClick={() => onEditProject(proj)}
                          title="项目设置与共享设定"
                          aria-label="项目设置"
                        >
                          <SettingsIcon size={12} strokeWidth={2} />
                        </button>
                      </div>
                    </div>

                    {!isCollapsed && (
                      <div className="sidebar-project-children">
                        {projConvos.length === 0 ? (
                          <div
                            className="sidebar-project-empty-hint"
                            onClick={() => onNewInProject(proj.id)}
                          >
                            暂无对话，点击 + 开始
                          </div>
                        ) : (
                          projConvos.map((item) => {
                            const displayTitle = cleanTitleString(item.title) || '新对话';
                            const isMenuOpen = popoverConvoId === item.id;

                            return (
                              <div
                                key={item.id}
                                className={`sidebar-convo-row${item.id === activeId ? ' active' : ''}`}
                              >
                                <button
                                  type="button"
                                  className="sidebar-convo-btn"
                                  onClick={() => onOpen(item.id)}
                                  title={displayTitle}
                                >
                                  <MessageSquare size={13} strokeWidth={1.75} aria-hidden="true" />
                                  <span className="sidebar-convo-title">{displayTitle}</span>
                                  {!!runningConvos?.[item.id] && <span className="sidebar-running-dot" title="任务运行中" />}
                                </button>

                                <div className="sidebar-convo-tools">
                                  <button
                                    type="button"
                                    className="sidebar-convo-action-btn"
                                    onClick={(e) => {
                                      e.stopPropagation();
                                      setPopoverConvoId(isMenuOpen ? null : item.id);
                                    }}
                                    title="设置项目"
                                    aria-label="设置项目"
                                  >
                                    <MoreHorizontal size={13} />
                                  </button>
                                </div>

                                {isMenuOpen && (
                                  <div className="sidebar-popover-menu" onClick={(e) => e.stopPropagation()}>
                                    {projects.map((p) => {
                                      const isSelected = item.projectId === p.id;
                                      return (
                                        <button
                                          key={p.id}
                                          type="button"
                                          className={`sidebar-popover-item${isSelected ? ' active' : ''}`}
                                          onClick={() => {
                                            if (!isSelected) {
                                              onMoveConversation(item.id, p.id);
                                            }
                                            setPopoverConvoId(null);
                                          }}
                                        >
                                          <Folder size={13} />
                                          <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{p.name}</span>
                                          {isSelected && <Check size={12} strokeWidth={2.2} />}
                                        </button>
                                      );
                                    })}
                                    <div className="sidebar-popover-divider" />
                                    <button
                                      type="button"
                                      className="sidebar-popover-item danger"
                                      onClick={() => {
                                        onMoveConversation(item.id, '');
                                        setPopoverConvoId(null);
                                      }}
                                    >
                                      <LogOut size={13} />
                                      <span>移出项目</span>
                                    </button>
                                    <button
                                      type="button"
                                      className="sidebar-popover-item"
                                      onClick={() => {
                                        onCreateProject();
                                        setPopoverConvoId(null);
                                      }}
                                    >
                                      <FolderPlus size={13} />
                                      <span>新建项目</span>
                                    </button>
                                  </div>
                                )}
                              </div>
                            );
                          })
                        )}
                      </div>
                    )}
                  </div>
                );
              })
            )}
          </section>
        )}

        {/* 对话列表区域 */}
        <nav className="sidebar-section" aria-label="对话列表">
          <div className="sidebar-section-header">
            <span className="sidebar-section-title">对话</span>
          </div>
          {unassignedConversations.length === 0 ? (
            <div className="empty-nav">
              还没有对话。<br />
              从一个目标开始。
            </div>
          ) : (
            unassignedConversations.map((item) => {
              const displayTitle = cleanTitleString(item.title) || '新对话';
              const isMenuOpen = popoverConvoId === item.id;

              return (
                <div
                  key={item.id}
                  className={`sidebar-convo-row${item.id === activeId ? ' active' : ''}`}
                >
                  <button
                    type="button"
                    className="sidebar-convo-btn"
                    onClick={() => onOpen(item.id)}
                    title={displayTitle}
                  >
                    <MessageSquare size={14} strokeWidth={1.75} aria-hidden="true" />
                    <span className="sidebar-convo-title">{displayTitle}</span>
                    {!!runningConvos?.[item.id] && <span className="sidebar-running-dot" title="任务运行中" />}
                  </button>

                  {isProjectPluginEnabled && (
                    <div className="sidebar-convo-tools">
                      <button
                        type="button"
                        className="sidebar-convo-action-btn"
                        onClick={(e) => {
                          e.stopPropagation();
                          setPopoverConvoId(isMenuOpen ? null : item.id);
                        }}
                        title="设置项目"
                        aria-label="设置项目"
                      >
                        <MoreHorizontal size={13} />
                      </button>
                    </div>
                  )}

                  {isMenuOpen && (
                    <div className="sidebar-popover-menu" onClick={(e) => e.stopPropagation()}>
                      {projects.length > 0 ? (
                        projects.map((proj) => (
                          <button
                            key={proj.id}
                            type="button"
                            className="sidebar-popover-item"
                            onClick={() => {
                              onMoveConversation(item.id, proj.id);
                              setPopoverConvoId(null);
                            }}
                          >
                            <Folder size={13} />
                            <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{proj.name}</span>
                          </button>
                        ))
                      ) : (
                        <div className="sidebar-popover-empty">暂无可用项目</div>
                      )}
                      <div className="sidebar-popover-divider" />
                      <button
                        type="button"
                        className="sidebar-popover-item"
                        onClick={() => {
                          onCreateProject();
                          setPopoverConvoId(null);
                        }}
                      >
                        <FolderPlus size={13} />
                        <span>新建项目</span>
                      </button>
                    </div>
                  )}
                </div>
              );
            })
          )}
        </nav>
      </div>

      <div className="sidebar-foot">
        <button type="button" onClick={onPlugins} aria-label="打开插件与能力">
          <Layers size={16} strokeWidth={1.75} aria-hidden="true" />
          <span>插件与能力</span>
        </button>
        <button type="button" onClick={onSettings} aria-label="打开模型设置">
          <SettingsIcon size={16} strokeWidth={1.75} aria-hidden="true" />
          <span>模型设置</span>
        </button>
      </div>
    </aside>
  );
}

function compressImageFile(file: File, maxDim = 1600, quality = 0.82): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = (e) => {
      const img = new window.Image();
      img.onload = () => {
        let { width, height } = img;
        if (width > maxDim || height > maxDim) {
          if (width > height) {
            height = Math.round((height * maxDim) / width);
            width = maxDim;
          } else {
            width = Math.round((width * maxDim) / height);
            height = maxDim;
          }
        }
        const canvas = document.createElement('canvas');
        canvas.width = width;
        canvas.height = height;
        const ctx = canvas.getContext('2d');
        if (!ctx) return resolve(e.target?.result as string);
        ctx.drawImage(img, 0, 0, width, height);
        resolve(canvas.toDataURL('image/jpeg', quality));
      };
      img.onerror = () => resolve(e.target?.result as string);
      img.src = e.target?.result as string;
    };
    reader.onerror = reject;
    reader.readAsDataURL(file);
  });
}

const DRAFTS_STORAGE_KEY = 'axiom_conversation_drafts_v1';

export type DraftAttachment = {
  id: string;
  name: string;
  dataUrl: string;
};

export type ConversationDraft = {
  text: string;
  attachments: DraftAttachment[];
};

function loadStoredDrafts(): Record<string, ConversationDraft> {
  if (typeof window === 'undefined') return {};
  try {
    const raw = localStorage.getItem(DRAFTS_STORAGE_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw);
    if (parsed && typeof parsed === 'object') {
      return parsed;
    }
  } catch {}
  return {};
}

function persistDrafts(drafts: Record<string, ConversationDraft>) {
  if (typeof window === 'undefined') return;
  try {
    const cleaned: Record<string, ConversationDraft> = {};
    for (const [k, v] of Object.entries(drafts)) {
      if ((v?.text && v.text.trim()) || (v?.attachments && v.attachments.length > 0)) {
        cleaned[k] = {
          text: v.text || '',
          attachments: Array.isArray(v.attachments) ? v.attachments : [],
        };
      }
    }
    if (Object.keys(cleaned).length === 0) {
      localStorage.removeItem(DRAFTS_STORAGE_KEY);
    } else {
      localStorage.setItem(DRAFTS_STORAGE_KEY, JSON.stringify(cleaned));
    }
  } catch {
    try {
      const textOnly: Record<string, ConversationDraft> = {};
      for (const [k, v] of Object.entries(drafts)) {
        if (v?.text && v.text.trim()) {
          textOnly[k] = { text: v.text, attachments: [] };
        }
      }
      localStorage.setItem(DRAFTS_STORAGE_KEY, JSON.stringify(textOnly));
    } catch {}
  }
}

function Chat({
  active,
  providers,
  selectedProviderId,
  onSelectProvider,
  sending,
  notice,
  trace,
  liveTrace,
  turns,
  onSend,
  onCancel,
  onConfigure,
  onOpenPlugins,
  unifiedPlugins,
  onNotice,
}: {
  unifiedPlugins: UnifiedPlugin[];
  onNotice?: (msg: string) => void;
  active: ConversationDetail | null;
  providers: Provider[];
  selectedProviderId: string;
  onSelectProvider: (id: string) => void;
  sending: boolean;
  notice: string;
  trace: TraceEvent[];
  liveTrace: TraceEvent[];
  turns: AgentTurn[];
  onSend: (content: string) => void;
  onCancel: () => void;
  onConfigure: () => void;
  onOpenPlugins: (type?: 'all' | 'mcp' | 'skill' | 'core' | 'release') => void;
}) {
  const [modelMenuOpen, setModelMenuOpen] = useState(false);
  const modelMenuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (modelMenuRef.current && !modelMenuRef.current.contains(event.target as Node)) {
        setModelMenuOpen(false);
      }
    }
    if (modelMenuOpen) {
      document.addEventListener('mousedown', handleClickOutside);
      return () => document.removeEventListener('mousedown', handleClickOutside);
    }
  }, [modelMenuOpen]);

  const currentProvider = useMemo(() => {
    if (active?.providerId) {
      const p = providers.find((item) => item.id === active.providerId);
      if (p) return p;
    }
    if (selectedProviderId) {
      const p = providers.find((item) => item.id === selectedProviderId);
      if (p) return p;
    }
    return providers[0] || null;
  }, [active, selectedProviderId, providers]);
  const sessionId = active?.id || '__new__';

  const [draftsMap, setDraftsMap] = useState<Record<string, ConversationDraft>>(() => loadStoredDrafts());
  const draftsMapRef = useRef(draftsMap);

  useEffect(() => {
    draftsMapRef.current = draftsMap;
    const timer = setTimeout(() => {
      persistDrafts(draftsMap);
    }, 300);
    return () => clearTimeout(timer);
  }, [draftsMap]);

  useEffect(() => {
    const handleBeforeUnload = () => {
      persistDrafts(draftsMapRef.current);
    };
    window.addEventListener('beforeunload', handleBeforeUnload);
    return () => window.removeEventListener('beforeunload', handleBeforeUnload);
  }, []);

  const currentDraftObj = draftsMap[sessionId];
  const draft = currentDraftObj?.text ?? '';
  const attachments = currentDraftObj?.attachments ?? [];

  const setDraft = useCallback((textOrUpdater: string | ((prev: string) => string)) => {
    setDraftsMap((prev) => {
      const currentText = prev[sessionId]?.text ?? '';
      const nextText = typeof textOrUpdater === 'function' ? textOrUpdater(currentText) : textOrUpdater;
      if (nextText === currentText && prev[sessionId]) return prev;
      return {
        ...prev,
        [sessionId]: {
          text: nextText,
          attachments: prev[sessionId]?.attachments ?? [],
        },
      };
    });
  }, [sessionId]);

  const setAttachments = useCallback((
    attachmentsOrUpdater: DraftAttachment[] | ((prev: DraftAttachment[]) => DraftAttachment[])
  ) => {
    setDraftsMap((prev) => {
      const currentAtts = prev[sessionId]?.attachments ?? [];
      const nextAtts = typeof attachmentsOrUpdater === 'function'
        ? attachmentsOrUpdater(currentAtts)
        : attachmentsOrUpdater;
      return {
        ...prev,
        [sessionId]: {
          text: prev[sessionId]?.text ?? '',
          attachments: nextAtts,
        },
      };
    });
  }, [sessionId]);

  const addAttachmentForSession = useCallback((
    targetId: string,
    att: DraftAttachment
  ) => {
    setDraftsMap((prev) => ({
      ...prev,
      [targetId]: {
        text: prev[targetId]?.text ?? '',
        attachments: [...(prev[targetId]?.attachments ?? []), att],
      },
    }));
  }, []);

  const clearCurrentDraft = useCallback(() => {
    setDraftsMap((prev) => {
      if (!prev[sessionId]) return prev;
      const next = { ...prev };
      delete next[sessionId];
      return next;
    });
  }, [sessionId]);
  const suggestions = ['检查这个项目的架构', '设计一份可靠的执行计划', '根据证据定位一个问题'];

  const checkVisionSupport = (): boolean => {
    const modelId = currentProvider?.model || currentProvider?.name || '';
    if (!modelId) return true;
    try {
      const cfg = JSON.parse(localStorage.getItem('axiom_model_vision') || '{}') as Record<string, boolean>;
      if (cfg[modelId] === false) {
        onNotice?.('当前选择的模型 [' + modelId + '] 已在模型设置中关闭了视觉支持。若该模型支持图片输入，请在左下角「模型设置」中将其切换为「视觉开启」；或在底栏切换为支持多模态的模型。');
        return false;
      }
    } catch {}
    return true;
  };

  const handlePaste = (e: React.ClipboardEvent<HTMLTextAreaElement>) => {
    const items = e.clipboardData?.items;
    if (!items) return;
    const targetSessionId = sessionId;
    for (let i = 0; i < items.length; i++) {
      const item = items[i];
      if (item.type.startsWith('image/')) {
        if (!checkVisionSupport()) {
          e.preventDefault();
          return;
        }
        const file = item.getAsFile();
        if (file) {
          e.preventDefault();
          compressImageFile(file).then((compressedDataUrl) => {
            addAttachmentForSession(targetSessionId, {
              id: 'img-' + Date.now() + '-' + Math.random().toString(36).slice(2, 6),
              name: file.name || '粘贴图片',
              dataUrl: compressedDataUrl,
            });
          }).catch(() => {});
        }
      }
    }
  };

  const handleDrop = (e: React.DragEvent<HTMLDivElement>) => {
    e.preventDefault();
    const files = e.dataTransfer?.files;
    if (!files) return;
    const targetSessionId = sessionId;
    for (let i = 0; i < files.length; i++) {
      const file = files[i];
      if (file.type.startsWith('image/')) {
        if (!checkVisionSupport()) {
          return;
        }
        compressImageFile(file).then((compressedDataUrl) => {
          addAttachmentForSession(targetSessionId, {
            id: 'img-' + Date.now() + '-' + Math.random().toString(36).slice(2, 6),
            name: file.name || '拖拽图片',
            dataUrl: compressedDataUrl,
          });
        }).catch(() => {});
      }
    }
  };


  const isModelSelectorEnabled = useMemo(() => {
    const p = unifiedPlugins.find((item) => item.id === 'core:model_selector');
    return p ? p.status === 'enabled' : true;
  }, [unifiedPlugins]);
  const isRunInspectorEnabled = useMemo(() => {
    const p = unifiedPlugins.find((item) => item.id === 'core:run_inspector');
    return p ? p.status === 'enabled' : true;
  }, [unifiedPlugins]);
  const activeMcpCount = useMemo(() => unifiedPlugins.filter((p) => p.type === 'mcp' && p.status === 'enabled').length, [unifiedPlugins]);
  const activeSkillCount = useMemo(() => unifiedPlugins.filter((p) => p.type === 'skill' && p.status === 'enabled').length, [unifiedPlugins]);
  const activePluginCount = useMemo(
    () => unifiedPlugins.filter((p) => (p.type === 'core' || p.type === 'release') && p.status === 'enabled').length,
    [unifiedPlugins]
  );

  const submit = () => {
    const textValue = draft.trim();
    if (!sending && (textValue || attachments.length > 0)) {
      let fullContent = textValue;
      if (attachments.length > 0) {
        const imgPart = attachments.map((a) => '![' + a.name + '](' + a.dataUrl + ')').join('\n\n');
        fullContent = fullContent ? fullContent + '\n\n' + imgPart : imgPart;
      }
      clearCurrentDraft();
      onSend(fullContent);
    }
  };

  return (
    <div className="chat-column">
      <div className="messages">
        {!active?.messages.length ? (
          <div className="empty-chat">
            <div className="empty-hero">
              <div className="pulse-orbit">
                <Sparkles size={20} strokeWidth={2} />
              </div>
              <h2 className="empty-title">今天想探讨什么任务？</h2>
              <p className="empty-subtitle">输入目标与约束条件，本地 Agent 将自主调用工具与模型协同推进。</p>
            </div>
            {!providers.length ? (
              <button type="button" className="setup-card" onClick={onConfigure}>
                <div className="setup-card-icon">
                  <KeyRound size={16} strokeWidth={2} />
                </div>
                <div className="setup-card-body">
                  <b>连接模型服务</b>
                  <small>配置 One-API、Claude 或 OpenAI 兼容接口，即可开启完整能力</small>
                </div>
                <span className="setup-card-arrow">前往配置 →</span>
              </button>
            ) : (
              <div className="suggestions">
                {suggestions.map((text) => (
                  <button type="button" key={text} onClick={() => setDraft(text)}>
                    <span>{text}</span>
                    <span className="suggestion-arrow">↗</span>
                  </button>
                ))}
              </div>
            )}
          </div>
        ) : (
          active.messages.map((message) => {
            const messageTurn = message.role === 'assistant' ? turns.find((turn) => turn.resultMessageId === message.id) : undefined;
            return (
              <article key={message.id} className={`message ${message.role}`}>
                <div className="message-role">{message.role === 'user' ? '你' : 'O'}</div>
                <div className="message-body">
                  {isRunInspectorEnabled && messageTurn && (
                    <ActivityTrace turn={messageTurn} trace={trace.filter((event) => event.turnId === messageTurn.id)} />
                  )}
                  <MarkdownView content={message.content} />
                </div>
              </article>
            );
          })
        )}
        {sending && (
          <article className="message assistant running-message">
            <div className="message-role">O</div>
            <div className="thinking-stack">
              {isRunInspectorEnabled ? (
                <ActivityTrace
                  trace={liveTrace}
                  turn={turns.find((turn) => turn.status === 'running' || turn.status === 'cancelling')}
                  running
                />
              ) : (
                <div className="thinking-row">
                  <div className="thinking"><i /><i /><i /> 正在处理</div>
                </div>
              )}
            </div>
          </article>
        )}
      </div>
      {notice && (
        <div className="notice-wrap">
          <div className="notice" role="status">
            <AlertCircle size={14} strokeWidth={2} className="notice-icon" />
            <span className="notice-text">{notice}</span>
            <button type="button" onClick={() => onNotice?.('')} className="notice-close" aria-label="关闭提示">✕</button>
          </div>
        </div>
      )}
      <div className="composer-wrap" onDragOver={(e) => e.preventDefault()} onDrop={handleDrop}>
        <div className="composer">
        {attachments.length > 0 && (
          <div className="composer-attachments">
            {attachments.map((att, idx) => (
              <div key={att.id} className="composer-attachment-item">
                {/* Local data URLs cannot use the Next image optimization pipeline. */}
                {/* eslint-disable-next-line @next/next/no-img-element */}
                <img src={att.dataUrl} alt={att.name} className="composer-attachment-thumb" />
                <button
                  type="button"
                  className="composer-attachment-remove"
                  onClick={() => setAttachments((prev) => prev.filter((_, i) => i !== idx))}
                  title="移除图片"
                  aria-label="移除图片"
                >
                  <X size={11} strokeWidth={2.5} />
                </button>
              </div>
            ))}
          </div>
        )}
        <textarea
          key={sessionId}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onPaste={handlePaste}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey && !sending) {
              e.preventDefault();
              submit();
            }
          }}
          placeholder={
            !providers.length
              ? '请先配置模型服务…'
              : sending
              ? '可以先输入下一条消息，当前任务完成后发送…'
              : '描述目标、约束，或者下一步行动…'
          }
          disabled={!providers.length}
          aria-label="输入消息内容"
        />
        <div className="composer-row">
          <div className="composer-capability-bar" role="toolbar" aria-label="扩展能力入口">
            <button
              type="button"
              className="capability-btn"
              onClick={() => onOpenPlugins('mcp')}
              title="管理 MCP 服务"
              aria-label={`MCP 服务，已启用 ${activeMcpCount} 个`}
            >
              <span>MCP</span>
              <span className="cap-badge">{activeMcpCount}</span>
            </button>
            <button
              type="button"
              className="capability-btn"
              onClick={() => onOpenPlugins('skill')}
              title="管理 Markdown 技能"
              aria-label={`技能，已启用 ${activeSkillCount} 个`}
            >
              <span>Skill</span>
              <span className="cap-badge">{activeSkillCount}</span>
            </button>
            <button
              type="button"
              className="capability-btn"
              onClick={() => onOpenPlugins('core')}
              title="管理核心插件"
              aria-label={`插件，已启用 ${activePluginCount} 个`}
            >
              <span>Plugin</span>
              <span className="cap-badge">{activePluginCount}</span>
            </button>
          </div>
          <div className="composer-actions-right">
            {isModelSelectorEnabled && (
              <div className="model-selector-anchor" ref={modelMenuRef}>
              <button
                type="button"
                className={`model-select-pill ${modelMenuOpen ? 'active' : ''}`}
                onClick={() => setModelMenuOpen((v) => !v)}
                title={currentProvider ? `当前模型: ${currentProvider.model || currentProvider.name}` : '未配置模型服务'}
                aria-label="选择模型"
              >
                <span className="model-name-label">
                  {currentProvider ? (currentProvider.model || currentProvider.name) : '选择模型'}
                </span>
                <ChevronDown size={12} strokeWidth={2} className={`chevron-icon ${modelMenuOpen ? 'open' : ''}`} />
              </button>

              {modelMenuOpen && (
                <div className="model-dropdown-menu">
                  <div className="model-dropdown-header">
                    <span>切换模型</span>
                    <button
                      type="button"
                      className="model-dropdown-cfg-btn"
                      onClick={() => {
                        setModelMenuOpen(false);
                        onConfigure();
                      }}
                    >
                      管理模型 ⚙
                    </button>
                  </div>
                  <div className="model-dropdown-list">
                    {providers.length === 0 ? (
                      <div className="model-dropdown-empty">
                        <p>尚未启用任何模型服务</p>
                        <button
                          type="button"
                          onClick={() => {
                            setModelMenuOpen(false);
                            onConfigure();
                          }}
                        >
                          前往配置模型 →
                        </button>
                      </div>
                    ) : (
                      providers.map((p) => {
                        const isSelected = p.id === currentProvider?.id;
                        return (
                          <button
                            type="button"
                            key={p.id}
                            className={`model-option-item ${isSelected ? 'selected' : ''}`}
                            onClick={() => {
                              onSelectProvider(p.id);
                              setModelMenuOpen(false);
                            }}
                          >
                            <div className="model-option-info">
                              <span className="model-option-name">{p.model || p.name}</span>
                              <span className="model-option-sub">{p.name}</span>
                            </div>
                            {isSelected && <Check size={13} strokeWidth={2.5} className="model-check-icon" />}
                          </button>
                        );
                      })
                    )}
                  </div>
                </div>
              )}
            </div>
            )}

            {sending ? (
              <button type="button" className="composer-icon-button stop-btn" onClick={onCancel} title="停止任务" aria-label="停止">
                <Square size={12} fill="currentColor" strokeWidth={0} />
              </button>
            ) : (
              <button
                type="button"
                className="composer-icon-button send-btn"
                onClick={submit}
                disabled={(!draft.trim() && attachments.length === 0) || !providers.length}
                title="发送"
                aria-label="发送"
              >
                <ArrowUp size={15} strokeWidth={2.4} />
              </button>
            )}
          </div>
        </div>
      </div>
      </div>
    </div>
  );
}

function ActivityTrace({
  trace,
  turn,
  running = false,
}: {
  trace: TraceEvent[];
  turn?: AgentTurn;
  running?: boolean;
}) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!running) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [running]);

  const events = trace;
  const startedAt = turn?.startedAt || events[0]?.createdAt;
  const completedAt = turn?.completedAt || (!running ? events[events.length - 1]?.createdAt : undefined);
  const elapsed = startedAt ? Math.max(0, (completedAt ? Date.parse(completedAt) : now) - Date.parse(startedAt)) : 0;
  const steps = events.reduce((max, event) => {
    const step = typeof event.details?.step === 'number' ? event.details.step : 0;
    return Math.max(max, step);
  }, 0);
  const terminal = [...events].reverse().find((event) => event.kind.startsWith('turn.') && event.kind !== 'turn.started' && event.kind !== 'turn.cancel_requested');
  const metrics = asRecord(terminal?.details?.metrics);
  const liveTokenCount = events.reduce((sum, event) => {
    if (event.kind !== 'model.completed') return sum;
    const usage = asRecord(event.details?.usage);
    return sum + (typeof usage.totalTokens === 'number' ? usage.totalTokens : 0);
  }, 0);
  const tokenCount = typeof metrics.totalTokens === 'number' ? metrics.totalTokens : liveTokenCount;
  const status = running ? (turn?.status === 'cancelling' ? '正在停止' : '运行中') : turnStatusLabel(turn?.status || terminal?.kind || 'completed');
  const toolRuns = buildToolRuns(events);

  return (
    <details className={`activity-trace ${running ? 'is-running' : ''}`}>
      <summary>
        <span className="run-summary-text">
          {running && <span className="run-live-label">{status} · </span>}
          用时 {formatDuration(elapsed)} · 消耗 {tokenCount > 0 ? formatCompactNumber(tokenCount) : '0'} tokens · {steps || 0} 步
        </span>
        <ChevronDown size={14} strokeWidth={1.8} className="run-chevron" aria-hidden="true" />
      </summary>
      <div className="activity-trace-body">
        <div className="activity-trace-meta">
          <span>{startedAt ? `${formatClock(startedAt)} 开始` : '等待运行时事件'}</span>
          <span>{status} · 运行详情保存在本地会话中</span>
        </div>
        {toolRuns.length ? (
          <div className="tool-runs">
            {toolRuns.map((run) => <ToolRunDetail key={run.id} run={run} />)}
          </div>
        ) : events.length ? (
          <div className="activity-events compact-events">
            {events.filter((event) => event.kind === 'model.requested' || event.kind === 'model.completed' || event.kind.includes('failed')).map((event) => (
              <article key={event.id} className={eventTone(event.kind)}>
                <div className="activity-event-content">
                  <div className="activity-event-title"><b>{eventLabel(event.kind)}</b><span>{eventSummary(event)}</span></div>
                </div>
              </article>
            ))}
          </div>
        ) : (
          <p className="activity-empty">正在等待第一条运行事件…</p>
        )}
      </div>
    </details>
  );
}

type ToolRun = {
  id: string;
  name: string;
  step: number;
  argumentsValue: unknown;
  resultValue?: unknown;
  durationMillis?: number;
  ok?: boolean;
};

function buildToolRuns(events: TraceEvent[]): ToolRun[] {
  const runs = new Map<string, ToolRun>();
  for (const event of events) {
    if (event.kind !== 'tool.started' && event.kind !== 'tool.completed') continue;
    const id = textDetail(event.details, 'toolCallId') || event.id;
    const current = runs.get(id) || {
      id,
      name: textDetail(event.details, 'name') || 'tool',
      step: typeof event.details.step === 'number' ? event.details.step : 0,
      argumentsValue: undefined,
    };
    if (event.kind === 'tool.started') current.argumentsValue = event.details.arguments;
    if (event.kind === 'tool.completed') {
      current.resultValue = event.details.result;
      current.durationMillis = typeof event.details.durationMillis === 'number' ? event.details.durationMillis : undefined;
      current.ok = event.details.ok !== false;
    }
    runs.set(id, current);
  }
  return [...runs.values()];
}

function ToolRunDetail({ run }: { run: ToolRun }) {
  const args = asRecord(run.argumentsValue);
  const rawResult = asRecord(run.resultValue);
  const result = Object.prototype.hasOwnProperty.call(rawResult, 'result') ? rawResult.result : run.resultValue;
  const resultRecord = asRecord(result);
  const isShell = run.name === 'exec_command';
  const command = typeof args.cmd === 'string' ? args.cmd : '';
  const workdir = typeof args.workdir === 'string' ? args.workdir : '';
  const stdout = typeof resultRecord.stdout === 'string' ? resultRecord.stdout : '';
  const stderr = typeof resultRecord.stderr === 'string' ? resultRecord.stderr : '';
  const exitCode = typeof resultRecord.exitCode === 'number' ? resultRecord.exitCode : undefined;

  return (
    <section className="tool-run">
      <div className="tool-run-heading">
        <span>{isShell ? 'Shell' : run.name}</span>
        <small>第 {run.step || '—'} 步 · {run.ok === false ? '失败' : run.resultValue === undefined ? '运行中' : '完成'}{run.durationMillis !== undefined ? ` · ${formatDuration(run.durationMillis)}` : ''}</small>
      </div>
      {isShell ? (
        <>
          {workdir && <div className="tool-workdir">工作目录 {workdir}</div>}
          {command && <pre className="tool-command"><span aria-hidden="true">$ </span>{command}</pre>}
          {stdout && <OutputBlock label="输出" value={stdout} />}
          {stderr && <OutputBlock label="错误输出" value={stderr} tone="error" />}
          {exitCode !== undefined && <div className={`tool-exit ${exitCode === 0 ? '' : 'error'}`}>退出码 {exitCode}{resultRecord.outTruncated === true ? ' · 输出已截断' : ''}</div>}
          {!command && run.argumentsValue !== undefined && <OutputBlock label="参数" value={formatTraceValue(run.argumentsValue)} />}
          {!stdout && !stderr && run.resultValue !== undefined && exitCode === undefined && <OutputBlock label="反馈" value={formatTraceValue(result)} />}
        </>
      ) : (
        <>
          {run.argumentsValue !== undefined && <OutputBlock label="参数" value={formatTraceValue(run.argumentsValue)} />}
          {run.resultValue !== undefined && <OutputBlock label="反馈" value={formatTraceValue(result)} tone={run.ok === false ? 'error' : undefined} />}
        </>
      )}
    </section>
  );
}

function OutputBlock({ label, value, tone }: { label: string; value: string; tone?: 'error' }) {
  return (
    <div className={`tool-output ${tone === 'error' ? 'error' : ''}`}>
      <span>{label}</span>
      <pre>{value}</pre>
    </div>
  );
}

function formatTraceValue(value: unknown) {
  if (typeof value === 'string') return value;
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

function asRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value) ? (value as Record<string, unknown>) : {};
}

function textDetail(details: Record<string, unknown>, key: string) {
  return typeof details?.[key] === 'string' ? (details[key] as string) : '';
}

function turnStatusLabel(status: string) {
  const labels: Record<string, string> = {
	completed: '已完成',
	incomplete: '未完成',
    failed: '运行失败',
    cancelled: '已停止',
    needs_reconciliation: '等待确认',
	'turn.completed': '已完成',
	'turn.incomplete': '未完成',
    'turn.failed': '运行失败',
    'turn.cancelled': '已停止',
    'turn.needs_reconciliation': '等待确认',
  };
  return labels[status] || '已结束';
}

function formatDuration(milliseconds: number) {
  if (!Number.isFinite(milliseconds) || milliseconds < 1000) return `${Math.max(0, Math.round(milliseconds))} ms`;
  const seconds = Math.floor(milliseconds / 1000);
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  return `${minutes} 分 ${rest} 秒`;
}

function formatCompactNumber(value: number) {
  return new Intl.NumberFormat('zh-CN', { notation: 'compact', maximumFractionDigits: 1 }).format(value);
}

function formatClock(value: string) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(date);
}

function eventLabel(value: string) {
  const labels: Record<string, string> = {
    idle: '空闲',
    checkpointed: '已保存',
    'turn.started': '任务开始',
	'turn.completed': '任务完成',
	'turn.incomplete': '达到步数上限',
    'turn.failed': '任务失败',
    'turn.cancelled': '任务已取消',
    'turn.cancel_requested': '正在停止任务',
    'turn.needs_reconciliation': '等待确认',
    'model.requested': '正在请求模型',
    'model.started': '模型调用',
    'model.completed': '模型完成',
    'model.failed': '模型调用失败',
    'planner.requested': '正在请求规划',
    'planner.completed': '规划完成',
    'planner.failed': '规划失败',
    'tools.dispatched': '准备调用工具',
    'tools.completed': '工具批次完成',
    'tool.started': '工具调用',
    'tool.completed': '工具完成',
    'context.compacted': '上下文已压缩',
    'provider.compatibility_warning': '模型兼容性提示',
    'loop.step_limit_reached': '达到步骤上限',
  };
  return labels[value] ?? value;
}

function eventTone(kind: string) {
  if (kind.includes('failed')) return 'failed';
  if (kind.includes('cancelled') || kind.includes('warning')) return 'warning';
  if (kind.includes('completed')) return 'completed';
  return 'running';
}

function eventSummary(event: TraceEvent) {
  const details = event.details ?? {};
  const number = (key: string) => (typeof details[key] === 'number' ? (details[key] as number) : undefined);
  const text = (key: string) => (typeof details[key] === 'string' ? (details[key] as string) : '');
  const step = number('step');
  if (event.kind === 'model.requested')
    return `第 ${step ?? 1} 步 · ${number('messageCount') ?? 0} 条上下文 · ${number('toolDefinitionCount') ?? 0} 个可用工具`;
  if (event.kind === 'model.completed')
    return typeof details.durationMillis === 'number'
      ? `第 ${step ?? 1} 步 · ${number('toolCallCount') ?? 0} 个工具调用 · ${formatDuration(number('durationMillis') ?? 0)}`
      : `第 ${step ?? 1} 步 · ${number('toolCallCount') ?? 0} 个工具调用 · 输出 ${number('contentBytes') ?? 0} 字节`;
  if (event.kind === 'model.failed' || event.kind === 'planner.failed' || event.kind === 'turn.failed')
    return text('error') || '运行时未返回详细错误';
  if (event.kind === 'tool.started') {
    const argumentsRecord = asRecord(details.arguments);
    const command = typeof argumentsRecord.cmd === 'string' ? argumentsRecord.cmd : '';
    return command ? shortenLine(command, 180) : `${text('name') || '未命名工具'} · 第 ${step ?? 1} 步`;
  }
  if (event.kind === 'tool.completed')
    return `${text('name') || '未命名工具'} · ${details.ok === false ? '执行失败' : '执行成功'} · ${number('durationMillis') ?? 0} ms`;
  if (event.kind === 'tools.dispatched' || event.kind === 'tools.completed')
    return `第 ${step ?? 1} 步 · ${number('toolCallCount') ?? 0} 个工具`;
	if (event.kind === 'context.compacted') {
	  if (details.scope === 'turn') {
		return `本轮工具轨迹由 ${formatCompactNumber(number('originalChars') ?? 0)} 字符压缩到 ${formatCompactNumber(number('compactedChars') ?? 0)} 字符`;
	  }
	  return `已压缩提炼 ${number('compactedMessages') || number('omittedMessages') || 0} 条早期对话记忆，保留最新 ${number('retainedMessages') ?? 0} 条`;
	}
  if (event.kind === 'provider.compatibility_warning') return text('message') || text('code') || '供应商兼容性提示';
  if (event.kind === 'turn.started') return `${number('messageCount') ?? 0} 条消息 · ${number('pinnedTools') ?? 0} 个固定工具`;
	if (event.kind === 'turn.completed') {
    const metrics = asRecord(details.metrics);
    const duration = typeof metrics.durationMillis === 'number' ? metrics.durationMillis : 0;
    return duration > 0 ? `结果已保存 · 总耗时 ${formatDuration(duration)}` : '结果已经保存到当前对话';
	}
	if (event.kind === 'turn.incomplete') return '已保存当前总结，但任务尚未完成';
  if (event.kind === 'turn.cancel_requested') return '正在等待当前操作安全结束';
  if (event.kind === 'turn.cancelled') return '用户停止了当前任务';
  return step ? `第 ${step} 步` : '运行状态已更新';
}

function shortenLine(value: string, maxLength: number) {
  const oneLine = value.replace(/\s+/g, ' ').trim();
  return oneLine.length > maxLength ? `${oneLine.slice(0, maxLength)}…` : oneLine;
}
