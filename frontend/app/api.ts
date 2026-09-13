declare global {
  interface Window {
    oDesktop?: Readonly<{ apiOrigin: string; platform: string; version: string }>;
  }
}

const desktopOrigin = typeof window !== 'undefined' ? window.oDesktop?.apiOrigin : undefined;
const configuredAPI = typeof process !== 'undefined' ? process.env.NEXT_PUBLIC_API_URL : undefined;

export const API = desktopOrigin ? `${desktopOrigin}/api/v1` : configuredAPI ?? 'http://127.0.0.1:8080/api/v1';
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
