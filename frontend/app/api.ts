declare global {
  interface Window {
    oDesktop?: Readonly<{ apiOrigin: string; platform: string; version: string; copyText?: (text: string) => Promise<boolean> | boolean }>;
  }
}

const desktopOrigin = typeof window !== 'undefined' ? window.oDesktop?.apiOrigin : undefined;
const configuredAPI = typeof process !== 'undefined' ? process.env.NEXT_PUBLIC_API_URL : undefined;

// Browser clients follow their HTTPS gateway; Electron keeps its private Host.
const webOrigin = typeof window !== 'undefined' ? window.location.origin : 'http://127.0.0.1:3000';
export const API = desktopOrigin ? `${desktopOrigin}/api/v1` : new URL(configuredAPI || '/api/v1', webOrigin).href.replace(/\/$/, '');
export const API_V2 = API.replace(/\/api\/v1\/?$/, '/api/v2');
export const ASSET_ORIGIN = new URL(API).origin;

export interface NativeSandboxHealth {
  installation: 'absent' | 'installing' | 'installed' | string;
  health: 'healthy' | 'unhealthy' | string;
  backend: string;
  reason?: string;
  networkEgressFiltering?: boolean;
  networkEgressReason?: string;
  offlineUser?: string;
  onlineUser?: string;
  runner?: string;
  checkedAt: string;
}

export interface SandboxConfiguration {
  platform: string;
  defaultBackend: 'appcontainer' | 'windows-native' | 'bubblewrap' | string;
  native: NativeSandboxHealth;
  linux: NativeSandboxHealth;
  maintenanceAvailable: boolean;
  runnerAvailable: boolean;
}

export interface GitCredentialSummary {
  id: string;
  host: string;
  repository: string;
  username: string;
  configured: boolean;
}

export interface ArtifactStorageCapacity {
  backend: 'local' | 's3';
  remoteObjectsAuthoritative: boolean;
  objectBytes: number;
  stagingBytes: number;
  temporaryBytes: number;
  metadataDatabaseBytes: number;
  metadataWalBytes: number;
  metadataShmBytes: number;
  filesystemTotalBytes?: number;
  filesystemFreeBytes?: number;
  filesystemMeasured: boolean;
  observedAt: string;
}

export async function getArtifactStorageCapacity(): Promise<ArtifactStorageCapacity> {
  return request<ArtifactStorageCapacity>('/system/artifact-storage');
}

export async function getGitCredentials(): Promise<GitCredentialSummary[]> {
  return request<GitCredentialSummary[]>('/system/git-credentials');
}

export async function saveGitCredential(credential: { id: string; host: string; repository: string; username: string; password: string }): Promise<GitCredentialSummary> {
  return request<GitCredentialSummary>('/system/git-credentials', { method: 'PUT', body: JSON.stringify(credential) });
}

export async function deleteGitCredential(id: string): Promise<void> {
  await request<void>('/system/git-credentials/' + encodeURIComponent(id), { method: 'DELETE' });
}

export async function getSandboxConfiguration(): Promise<SandboxConfiguration> {
  return request<SandboxConfiguration>('/system/sandbox');
}

export async function setSandboxDefaultBackend(defaultBackend: SandboxConfiguration['defaultBackend']): Promise<SandboxConfiguration> {
  return request<SandboxConfiguration>('/system/sandbox', {
    method: 'PUT',
    body: JSON.stringify({ defaultBackend }),
  });
}

export async function runSandboxMaintenance(operation: 'install' | 'repair' | 'uninstall'): Promise<SandboxConfiguration> {
  return request<SandboxConfiguration>('/system/sandbox/maintenance', {
    method: 'POST',
    body: JSON.stringify({ operation }),
  });
}

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const target = /^https?:\/\//.test(path) ? path : `${API}${path}`;
  const response = await fetch(target, { cache: 'no-store', credentials: 'same-origin', ...init, headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) } });
  const contentType = response.headers.get('content-type')?.toLowerCase() ?? '';
  const text = response.status === 204 ? '' : await response.text();
  if (!response.ok) {
    let message = response.statusText || '请求失败';
    if (contentType.includes('application/json') && text) {
      try {
        const body = JSON.parse(text) as { error?: string | { message?: string } };
        message = typeof body.error === 'string' ? body.error : body.error?.message ?? message;
      } catch { /* The status remains the safe fallback. */ }
    } else if (text.trim().startsWith('<')) {
      message = `API 返回了 HTML（${response.status}）。请检查网关的 /api 转发以及 O Host 是否正在运行。`;
    }
    throw new Error(message);
  }
  if (response.status === 204) return undefined as T;
  if (!contentType.includes('application/json')) throw new Error(`API 返回了 ${contentType || '未知内容类型'}，而不是 JSON。`);
  return JSON.parse(text) as T;
}

export interface UserArtifact {
  id: string;
  sha256: string;
  byteSize: number;
  fileName: string;
  mediaType: string;
  uploadId: string;
  createdAt: string;
}

interface ArtifactUploadRecord {
  id: string;
  fileName: string;
  mediaType: string;
  expectedSize: number;
  expectedSha256?: string;
  chunkSize: number;
  chunkCount: number;
  status: string;
  artifactId?: string;
}

interface ArtifactUploadChunkRecord {
  index: number;
  sha256: string;
  byteSize: number;
}

const hexDigest = (bytes: ArrayBuffer): string => [...new Uint8Array(bytes)].map((value) => value.toString(16).padStart(2, '0')).join('');

function waitForUploadRetry(attempt: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason ?? new DOMException('上传已取消', 'AbortError'));
    const timer = setTimeout(resolve, Math.min(500 * (2 ** attempt), 8000));
    signal?.addEventListener('abort', () => {
      clearTimeout(timer);
      reject(signal.reason ?? new DOMException('上传已取消', 'AbortError'));
    }, { once: true });
  });
}

async function uploadChunkWithRetry(uploadId: string, index: number, digest: string, body: Blob, signal?: AbortSignal): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    if (signal?.aborted) throw signal.reason ?? new DOMException('上传已取消', 'AbortError');
    try {
      const response = await fetch(`${API}/artifacts/uploads/${encodeURIComponent(uploadId)}/chunks/${index}`, {
        method: 'PUT', cache: 'no-store', credentials: 'same-origin', redirect: 'error', signal,
        headers: { 'Content-Type': 'application/octet-stream', 'X-Chunk-SHA256': digest }, body,
      });
      if (response.ok) {
        const stored = await response.json() as ArtifactUploadChunkRecord;
        if (stored.index !== index || stored.sha256 !== digest || stored.byteSize !== body.size) throw new Error('服务器读回的上传分块与本地内容不一致。');
        return;
      }
      if (![408, 425, 429].includes(response.status) && response.status < 500) {
        const contentType = response.headers.get('content-type')?.toLowerCase() ?? '';
        const responseText = await response.text();
        let message = response.statusText || '上传分块失败';
        if (contentType.includes('application/json') && responseText) {
          try {
            const parsed = JSON.parse(responseText) as { error?: string | { message?: string } };
            message = typeof parsed.error === 'string' ? parsed.error : parsed.error?.message ?? message;
          } catch { /* retain the status fallback */ }
        }
        throw new Error(message);
      }
    } catch (error) {
      if (signal?.aborted) throw signal.reason ?? error;
      if (error instanceof Error && !['TypeError', 'NetworkError'].includes(error.name) && !/fetch failed/i.test(error.message)) throw error;
      if (attempt >= 6) throw new Error(`上传第 ${index + 1} 个分块多次失败；再次选择同一文件可从已保存分块继续。`, { cause: error });
    }
    if (attempt >= 6) throw new Error(`上传第 ${index + 1} 个分块多次失败；再次选择同一文件可从已保存分块继续。`);
    await waitForUploadRetry(attempt, signal);
  }
}

export async function uploadArtifactFile(
  file: File,
  idempotencyKey: string,
  options: { signal?: AbortSignal; onProgress?: (progress: { uploadedBytes: number; totalBytes: number; chunkIndex: number; chunkCount: number }) => void } = {},
): Promise<UserArtifact> {
  if (!file || !Number.isSafeInteger(file.size) || file.size < 0 || !idempotencyKey.trim()) throw new Error('文件或上传幂等键无效。');
  if (!globalThis.crypto?.subtle) throw new Error('当前浏览器不支持安全摘要计算，请使用 HTTPS 或本机安全环境。');
  const { upload } = await request<{ upload: ArtifactUploadRecord; chunkSize: number }>(`/artifacts/uploads`, {
    method: 'POST', headers: { 'Idempotency-Key': idempotencyKey },
    body: JSON.stringify({ fileName: file.name || 'upload.bin', mediaType: file.type || 'application/octet-stream', expectedSize: file.size }),
    signal: options.signal,
  });
  if (!upload?.id || upload.expectedSize !== file.size || !Number.isSafeInteger(upload.chunkSize) || upload.chunkSize <= 0 || upload.chunkSize > 8 * 1024 * 1024 || !Number.isSafeInteger(upload.chunkCount) || upload.chunkCount < 0) {
    throw new Error('文件上传记录未从服务端正确读回。');
  }
  const chunkSize = upload.chunkSize;
  const pages = new Map<number, ArtifactUploadChunkRecord>();
  let offset = 0;
  while (true) {
    const page = await request<{ chunks: ArtifactUploadChunkRecord[]; receivedChunkCount: number }>(`/artifacts/uploads/${encodeURIComponent(upload.id)}?chunkOffset=${offset}&chunkLimit=1000`, { signal: options.signal });
    if (!Array.isArray(page.chunks)) throw new Error('服务器没有读回可续传的分块清单。');
    for (const chunk of page.chunks) pages.set(chunk.index, chunk);
    offset += page.chunks.length;
    if (offset >= page.receivedChunkCount || page.chunks.length === 0) break;
  }
  let uploadedBytes = 0;
  for (let index = 0; index < upload.chunkCount; index += 1) {
    if (options.signal?.aborted) throw options.signal.reason ?? new DOMException('上传已取消', 'AbortError');
    const start = index * chunkSize;
    const blob = file.slice(start, Math.min(file.size, start + chunkSize));
    const digest = hexDigest(await crypto.subtle.digest('SHA-256', await blob.arrayBuffer()));
    const saved = pages.get(index);
    if (!saved || saved.sha256 !== digest || saved.byteSize !== blob.size) {
      if (upload.status === 'complete') throw new Error(`此上传幂等键已绑定另一份文件（第 ${index + 1} 块摘要不同）；请重新选择文件后再试。`);
      await uploadChunkWithRetry(upload.id, index, digest, blob, options.signal);
    }
    uploadedBytes += blob.size;
    options.onProgress?.({ uploadedBytes, totalBytes: file.size, chunkIndex: index + 1, chunkCount: upload.chunkCount });
  }
  const artifact = await request<UserArtifact>(`/artifacts/uploads/${encodeURIComponent(upload.id)}/finalize`, { method: 'POST', body: '{}', signal: options.signal });
  if (!artifact?.id || artifact.id !== (upload.status === 'complete' ? upload.artifactId : artifact.id) || artifact.byteSize !== file.size || !/^[a-f0-9]{64}$/i.test(artifact.sha256) || artifact.fileName !== upload.fileName) throw new Error('成果文件尚未通过服务端完整性读回校验。');
  return artifact;
}

export interface ArtifactShareLink {
  id: string;
  artifactId: string;
  createdAt: string;
  expiresAt: string;
  revokedAt?: string | null;
}

export async function createArtifactShareLink(artifactId: string, expiresInSeconds = 3600): Promise<{ link: ArtifactShareLink; path: string }> {
  const result = await request<{ link: ArtifactShareLink; path: string }>(`/artifacts/${encodeURIComponent(artifactId)}/share-links`, {
    method: 'POST',
    body: JSON.stringify({ expiresInSeconds }),
  });
  if (!result.link?.id || result.link.artifactId !== artifactId || !result.path.startsWith('/api/v1/shared/artifacts/')) {
    throw new Error('分享链接没有读回对应的成果和下载路径。');
  }
  return result;
}

export async function listArtifactShareLinks(artifactId: string): Promise<ArtifactShareLink[]> {
  const links = await request<ArtifactShareLink[]>(`/artifact-share-links?artifactId=${encodeURIComponent(artifactId)}`);
  if (!Array.isArray(links) || links.some((link) => !link.id || link.artifactId !== artifactId)) throw new Error('成果分享链接列表没有读回对应的成果。');
  return links;
}

export async function revokeArtifactShareLink(id: string): Promise<ArtifactShareLink> {
  const link = await request<ArtifactShareLink>(`/artifact-share-links/${encodeURIComponent(id)}`, { method: 'DELETE' });
  if (link.id !== id || !link.revokedAt) throw new Error('分享链接撤销状态没有读回。');
  return link;
}

export interface UnifiedPlugin {
  id: string;
  name: string;
  type: 'core' | 'skill' | 'mcp' | 'release' | 'model' | 'context';
  metadata?: Record<string, string>;
  description: string;
  status: 'enabled' | 'disabled' | 'error';
  error?: string;
  capabilities?: string[];
  mcpConfig?: {
    command: string;
    args?: string[];
    env?: Record<string, string>;
  };
}

export interface McpServerConfig {
  id: string;
  name: string;
  command: string;
  args?: string[];
  env?: Record<string, string>;
  enabled?: boolean;
}

export interface WebSearchPluginSettings {
  provider: string;
  configured: boolean;
  keyHint?: string;
}

export interface ContextCompactionSettings {
  mode: 'auto' | 'semantic' | 'provider_native' | 'extractive';
  triggerPercent: number;
  minimumGrowthBeforeRecompactTokens: number;
  recentContextTokens: number;
  compactionCallBudget: number;
  version: number;
}

export async function getContextCompactionSettings(): Promise<ContextCompactionSettings> {
  return request<ContextCompactionSettings>('/plugins/context-compactor/settings');
}

export async function saveContextCompactionSettings(settings: ContextCompactionSettings): Promise<ContextCompactionSettings> {
  return request<ContextCompactionSettings>('/plugins/context-compactor/settings', {
    method: 'PUT',
    body: JSON.stringify(settings),
  });
}

export async function getWebSearchPluginSettings(): Promise<WebSearchPluginSettings> {
  return request<WebSearchPluginSettings>('/plugins/web-search/settings');
}

export async function saveWebSearchPluginSettings(apiKey: string): Promise<WebSearchPluginSettings> {
  return request<WebSearchPluginSettings>('/plugins/web-search/settings', {
    method: 'PUT',
    body: JSON.stringify({ apiKey }),
  });
}

export async function clearWebSearchPluginSettings(): Promise<WebSearchPluginSettings> {
  return request<WebSearchPluginSettings>('/plugins/web-search/settings', { method: 'DELETE' });
}

export async function testWebSearchPlugin(): Promise<{ passed: boolean; resultCount: number; settings: WebSearchPluginSettings }> {
  return request('/plugins/web-search/test', { method: 'POST', body: '{}' });
}

export async function getUnifiedPlugins(): Promise<UnifiedPlugin[]> {
  return request<UnifiedPlugin[]>('/plugins');
}

export async function toggleUnifiedPlugin(id: string, enabled: boolean, confirmExternal = false): Promise<UnifiedPlugin> {
	return request<UnifiedPlugin>(`/plugins/${encodeURIComponent(id)}/toggle`, {
    method: 'POST',
    body: JSON.stringify({ enabled, confirmExternal }),
  });
}

export async function addMcpServerConfig(config: McpServerConfig): Promise<UnifiedPlugin[]> {
  return request<UnifiedPlugin[]>('/plugins/mcp', {
    method: 'POST',
    body: JSON.stringify({ config, confirmLaunch: true }),
  });
}

export async function removeMcpServerConfig(id: string): Promise<{ removed: string }> {
  const serverId = id.replace(/^mcp:/, '');
  return request<{ removed: string }>(`/plugins/mcp/${encodeURIComponent(serverId)}`, {
    method: 'DELETE',
    body: JSON.stringify({ confirmRemove: true }),
  });
}

export type ConversationPermissionProfile = 'read_only' | 'workspace_autonomous' | 'request_approval' | 'fully_autonomous';

export type ConversationDeletionReceipt = {
  conversationId: string;
  deletedAt: string;
  recoverUntil: string;
};

export type DeletedConversation = {
  id: string;
  title: string;
  projectId?: string;
  deletedAt: string;
  recoverUntil: string;
};

export type ToolUsageMetric = {
  toolName: string;
  projectId?: string;
  pluginId?: string;
  releaseId?: string;
  calls: number;
  completed: number;
  failures: number;
  permissionAllows: number;
  permissionDenials: number;
  permissionAsks: number;
  approvalRequests: number;
  approvalsPending: number;
  approvalsGranted: number;
  approvalsDenied: number;
  approvalsExpired: number;
  approvalsCancelled: number;
  pluginBuilds?: number;
  pluginBuildFailures?: number;
  pluginActivations?: number;
  pluginDeactivations?: number;
  pluginRollbacks?: number;
  pluginPermissionRequests?: number;
  pluginPermissionsGranted?: number;
  pluginRuntimeFailures?: number;
  pluginSourceChanges?: number;
  pluginUnusableMarks?: number;
  pluginBundleCleanups?: number;
  durationMillis: number;
  contextCompactions?: number;
  originalContextChars?: number;
  compactedContextChars?: number;
  lastUsedAt?: string;
};

export type PluginStorageUsage = {
  releaseCount: number;
  uniqueBundleCount: number;
  bundleBytes: number;
  missingBundleCount: number;
};

export async function getToolUsageMetrics(days: number): Promise<ToolUsageMetric[]> {
  return request<ToolUsageMetric[]>(`/observability/tools?days=${encodeURIComponent(days)}`);
}

export async function updateConversationPermissionProfile<T = unknown>(
  conversationId: string,
  profile: ConversationPermissionProfile
): Promise<T> {
  return request<T>(`/conversations/${encodeURIComponent(conversationId)}/permissions`, {
    method: 'PUT',
    body: JSON.stringify({ profile }),
  });
}

export type ApprovalRequest = {
  id: string;
  conversationId: string;
  turnId: string;
  toolCallId: string;
  toolName: string;
  source: string;
  effect: string;
  permissionProfile: ConversationPermissionProfile;
  pluginId?: string;
  releaseId?: string;
  resource?: string;
  impact?: string;
  reason: string;
  arguments: string;
  status: string;
  createdAt: string;
  expiresAt: string;
};

export async function getConversationApprovals(conversationId: string): Promise<ApprovalRequest[]> {
  return request<ApprovalRequest[]>(`/conversations/${encodeURIComponent(conversationId)}/approvals`);
}

export async function resolveAgentApproval(id: string, choice: 'approve' | 'deny'): Promise<ApprovalRequest> {
  return request<ApprovalRequest>(`/agent/approvals/${encodeURIComponent(id)}/decision`, {
    method: 'POST',
    body: JSON.stringify({ choice }),
  });
}

export async function updateConversationTitle<T = Record<string, unknown>>(id: string, title: string): Promise<T> {
  return request<T>(`/conversations/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: JSON.stringify({ title }),
  });
}

export async function deleteConversation(id: string): Promise<ConversationDeletionReceipt> {
  return request<ConversationDeletionReceipt>(`/conversations/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  });
}

export async function getDeletedConversations(): Promise<DeletedConversation[]> {
  return request<DeletedConversation[]>('/recovery/conversations');
}

export async function restoreConversation<T = Record<string, unknown>>(id: string): Promise<T> {
  return request<T>(`/recovery/conversations/${encodeURIComponent(id)}/restore`, {
    method: 'POST',
    body: '{}',
  });
}

export async function generateConversationTitle<T = Record<string, unknown>>(id: string, providerId?: string): Promise<T> {
  return request<T>(`/conversations/${encodeURIComponent(id)}/generate-title`, {
    method: 'POST',
    body: JSON.stringify({ providerId: providerId || undefined }),
  });
}

export interface Project {
  id: string;
  name: string;
  instructions?: string;
  instructionsEnabled?: boolean;
  workdir?: string;
  remoteRepoUrl?: string;
  remoteBranch?: string;
  repositoryProvider?: string;
  resolvedCommit?: string;
  measuredBytes?: number;
  createdAt: string;
  updatedAt: string;
}

export async function getProjects(): Promise<Project[]> {
  return request<Project[]>('/projects');
}

export async function createProject(data: {
  name: string;
  instructions?: string;
  instructionsEnabled?: boolean;
  workdir?: string;
  remoteRepoUrl?: string;
  remoteBranch?: string;
}): Promise<Project> {
  return request<Project>('/projects', {
    method: 'POST',
    body: JSON.stringify(data),
  });
}

export async function updateProject(
  id: string,
  data: {
    name?: string;
    instructions?: string;
    instructionsEnabled?: boolean;
    workdir?: string;
    remoteRepoUrl?: string;
    remoteBranch?: string;
  },
): Promise<Project> {
  return request<Project>(`/projects/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: JSON.stringify(data),
  });
}

export async function deleteProject(id: string): Promise<{ ok: boolean; deleted: string }> {
  return request<{ ok: boolean; deleted: string }>(`/projects/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    body: JSON.stringify({ confirmDelete: true }),
  });
}

export async function selectNativeDirectory(defaultPath?: string): Promise<string | null> {
  // 1. Electron Desktop IPC
  if (
    typeof window !== 'undefined' &&
    (window as unknown as { oDesktop?: { selectDirectory?: (opts?: { defaultPath?: string }) => Promise<string | null> } }).oDesktop?.selectDirectory
  ) {
    try {
      const selected = await (
        window as unknown as { oDesktop: { selectDirectory: (opts?: { defaultPath?: string }) => Promise<string | null> } }
      ).oDesktop.selectDirectory({ defaultPath });
      return selected || null;
    } catch {
      // fallback to backend
    }
  }

  // 2. Call backend system directory picker
  try {
    const res = await request<{ path?: string; canceled?: boolean }>('/system/select-directory', {
      method: 'POST',
      body: JSON.stringify({ defaultPath }),
    });
    if (res && res.path) {
      return res.path;
    }
  } catch {}

  return null;
}

export async function cloneProjectGit(data: {
  projectId?: string;
  repoUrl: string;
  targetDir?: string;
  branch?: string;
}): Promise<GitCloneResult> {
  return request<GitCloneResult>('/projects/git-clone', {
    method: 'POST',
    body: JSON.stringify(data),
  });
}

export interface GitCloneResult {
  targetDir: string;
  repositoryProvider: string;
  resolvedCommit: string;
  measuredBytes: number;
  measurementStatus: 'measured' | 'unknown';
  remoteBranch: string;
  recommendedTransferMode: 'direct' | 'incremental_or_artifact_link';
  directTransferBatchBytes: number;
}

export interface GitPublicationPreview {
  targetBranch: string;
  currentBranch: string;
  commitSha: string;
  remoteSha: string;
  worktreeClean: boolean;
  submodules: ProjectPublicationSubmodule[];
}

export interface ProjectPublicationSubmodule {
  path: string;
  repositoryUrl: string;
  commitSha: string;
  ref: string;
  remoteSha?: string;
  status: 'pending' | 'publishing' | 'published' | 'failed' | 'needs_reconciliation' | string;
  error?: string;
}

export interface ProjectPublication {
  id: string;
  projectId: string;
  idempotencyKey: string;
  targetBranch: string;
  expectedRemoteSha: string;
  commitSha: string;
  submodules?: ProjectPublicationSubmodule[];
  remoteSha?: string;
  status: 'publishing' | 'published' | 'failed' | 'needs_reconciliation' | string;
  error?: string;
  createdAt: string;
  updatedAt: string;
}

export async function previewProjectPublication(id: string, branch?: string): Promise<GitPublicationPreview> {
  const query = branch ? `?branch=${encodeURIComponent(branch)}` : '';
  return request<GitPublicationPreview>(`/projects/${encodeURIComponent(id)}/publication-preview${query}`);
}

export async function createProjectPublication(id: string, input: { targetBranch: string; expectedRemoteSha: string; commitSha: string }, idempotencyKey: string): Promise<ProjectPublication> {
  return request<ProjectPublication>(`/projects/${encodeURIComponent(id)}/publications`, {
    method: 'POST',
    headers: { 'Idempotency-Key': idempotencyKey },
    body: JSON.stringify(input),
  });
}

export async function reconcileProjectPublication(id: string, publicationId: string): Promise<ProjectPublication> {
  return request<ProjectPublication>(`/projects/${encodeURIComponent(id)}/publications/${encodeURIComponent(publicationId)}/reconcile`, { method: 'POST', body: '{}' });
}

export async function getProjectPublications(id: string): Promise<ProjectPublication[]> {
  return request<ProjectPublication[]>(`/projects/${encodeURIComponent(id)}/publications`);
}

export interface ExecutionNode {
  id: string;
  name: string;
  platform: string;
  capabilities: string[];
  resources: {
    memoryTotalBytes: number;
    memoryAvailableBytes: number;
    logicalCpus: number;
    maxConcurrentTasks: number;
  };
  connectivity: 'connected' | 'disconnected' | 'revoked' | string;
  lastSeen?: string;
  revokedAt?: string;
  createdAt: string;
  updatedAt: string;
}

export async function getExecutionNodes(): Promise<ExecutionNode[]> {
  return request<ExecutionNode[]>('/nodes');
}

export async function pairExecutionNode(input: { name: string; platform: string }): Promise<{ node: ExecutionNode; credential: string; credentialShownOnce: boolean }> {
  return request<{ node: ExecutionNode; credential: string; credentialShownOnce: boolean }>('/nodes', {
    method: 'POST',
    body: JSON.stringify({ ...input, capabilities: [] }),
  });
}

export async function revokeExecutionNode(id: string): Promise<ExecutionNode> {
  return request<ExecutionNode>(`/nodes/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

export interface ExecutionTask {
  id: string;
  logicalTaskId: string;
  parentTaskId?: string;
  segmentIndex: number;
  nodeId: string;
  status: string;
  attempt: number;
  sequence: number;
  cancelRequested: boolean;
  handoffRequested: boolean;
  progressPhase?: string;
  progressUpdatedAt?: string;
  error?: string;
  result?: Record<string, unknown>;
  recoveryResult?: Record<string, unknown>;
  importedProject?: Project;
  continuedConversation?: { id: string; title: string; projectId: string; parentConversationId: string };
  continuationTaskId?: string;
  continuationTaskStatus?: string;
  createdAt: string;
  updatedAt: string;
}

export async function submitLocalAgentTask(conversationId: string, targetNodeId: string, content: string, idempotencyKey: string, artifactIds: string[] = []): Promise<{ task: ExecutionTask; created: boolean }> {
  return request<{ task: ExecutionTask; created: boolean }>(`/conversations/${encodeURIComponent(conversationId)}/tasks/nodes`, {
    method: 'POST',
    headers: { 'Idempotency-Key': idempotencyKey },
    body: JSON.stringify({ targetNodeId, content, waitForNode: false, artifactIds }),
  });
}

export async function requestSafeTaskHandoff(taskId: string): Promise<ExecutionTask> {
  return request<ExecutionTask>(`/tasks/${encodeURIComponent(taskId)}/handoff`, { method: 'POST', body: '{}' });
}

export async function importLocalTaskProjectDelta(taskId: string, confirmOldNodeStopped = false): Promise<{ project: Project; created: boolean }> {
  return request<{ project: Project; created: boolean }>(`/tasks/${encodeURIComponent(taskId)}/import-project-delta`, {
    method: 'POST',
    body: JSON.stringify(confirmOldNodeStopped ? { confirmOldNodeStopped: true } : {}),
  });
}

export interface HandoffExternalEffect {
  toolCallId: string;
  tool: string;
  effect: string;
  state: 'completed' | 'reported_error' | 'result_unknown' | string;
}

export interface HandoffEffectResolution {
  toolCallId: string;
  outcome: 'confirmed_applied' | 'confirmed_not_applied' | 'unknown';
}

export async function previewLocalTaskContinuation(taskId: string): Promise<{ taskId: string; effects: HandoffExternalEffect[]; requiresAcknowledgement: boolean; requiresOldNodeStopConfirmation?: boolean; recoveryContinuation?: boolean }> {
  return request(`/tasks/${encodeURIComponent(taskId)}/continuation-preview`);
}

export async function continueLocalTaskInCloud(taskId: string, instruction?: string, effectResolutions: HandoffEffectResolution[] = [], confirmOldNodeStopped = false): Promise<{ conversation: NonNullable<ExecutionTask['continuedConversation']>; project: Project | null; task: ExecutionTask }> {
  return request<{ conversation: NonNullable<ExecutionTask['continuedConversation']>; project: Project | null; task: ExecutionTask }>(`/tasks/${encodeURIComponent(taskId)}/continue-in-cloud`, {
    method: 'POST',
    body: JSON.stringify({ ...(instruction ? { instruction } : {}), effectResolutions, ...(confirmOldNodeStopped ? { confirmOldNodeStopped: true } : {}) }),
  });
}

export async function updateConversationProject<T = Record<string, unknown>>(id: string, projectId: string): Promise<T> {
  return request<T>(`/conversations/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: JSON.stringify({ projectId }),
  });
}
