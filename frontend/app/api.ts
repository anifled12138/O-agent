declare global {
  interface Window {
    oDesktop?: Readonly<{ apiOrigin: string; platform: string; version: string; copyText?: (text: string) => Promise<boolean> | boolean }>;
  }
}

const desktopOrigin = typeof window !== 'undefined' ? window.oDesktop?.apiOrigin : undefined;
const configuredAPI = typeof process !== 'undefined' ? process.env.NEXT_PUBLIC_API_URL : undefined;

export const API = desktopOrigin ? `${desktopOrigin}/api/v1` : configuredAPI ?? 'http://127.0.0.1:9171/api/v1';
export const API_V2 = API.replace(/\/api\/v1\/?$/, '/api/v2');
export const ASSET_ORIGIN = new URL(API).origin;

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const target = /^https?:\/\//.test(path) ? path : `${API}${path}`;
  const response = await fetch(target, { ...init, headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) } });
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
      message = `本地 API 返回了 HTML（${response.status}）。请检查 API 地址以及 Go Host 是否正在运行。`;
    }
    throw new Error(message);
  }
  if (response.status === 204) return undefined as T;
  if (!contentType.includes('application/json')) throw new Error(`本地 API 返回了 ${contentType || '未知内容类型'}，而不是 JSON。`);
  return JSON.parse(text) as T;
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
}

export async function getUnifiedPlugins(): Promise<UnifiedPlugin[]> {
  return request<UnifiedPlugin[]>('/plugins');
}

export async function toggleUnifiedPlugin(id: string, enabled: boolean): Promise<{ id: string; enabled: boolean }> {
  return request<{ id: string; enabled: boolean }>(`/plugins/${encodeURIComponent(id)}/toggle`, {
    method: 'POST',
    body: JSON.stringify({ enabled }),
  });
}

export async function reloadUnifiedPlugins(): Promise<UnifiedPlugin[]> {
  return request<UnifiedPlugin[]>('/plugins/reload', {
    method: 'POST',
  });
}

export async function addMcpServerConfig(config: McpServerConfig): Promise<UnifiedPlugin[]> {
  return request<UnifiedPlugin[]>('/plugins/mcp', {
    method: 'POST',
    body: JSON.stringify(config),
  });
}

export async function removeMcpServerConfig(id: string): Promise<{ removed: string }> {
  return request<{ removed: string }>(`/plugins/mcp/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  });
}

export async function updateConversationTitle<T = Record<string, unknown>>(id: string, title: string): Promise<T> {
  return request<T>(`/conversations/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: JSON.stringify({ title }),
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
}): Promise<{ ok: boolean; targetDir: string; output?: string }> {
  return request<{ ok: boolean; targetDir: string; output?: string }>('/projects/git-clone', {
    method: 'POST',
    body: JSON.stringify(data),
  });
}

export async function updateConversationProject<T = Record<string, unknown>>(id: string, projectId: string): Promise<T> {
  return request<T>(`/conversations/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: JSON.stringify({ projectId }),
  });
}
