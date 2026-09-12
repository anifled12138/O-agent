export const API = process.env.NEXT_PUBLIC_API_URL ?? 'http://127.0.0.1:8080/api/v1';
export const API_V2 = API.replace(/\/api\/v1\/?$/, '/api/v2');
export const ASSET_ORIGIN = new URL(API).origin;

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const target = /^https?:\/\//.test(path) ? path : `${API}${path}`;
  const response = await fetch(target, { ...init, credentials: 'include', headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) } });
  const contentType = response.headers.get('content-type')?.toLowerCase() ?? '';
  const text = response.status === 204 ? '' : await response.text();
  if (!response.ok) {
    let message = response.statusText || 'Request failed';
    if (contentType.includes('application/json') && text) {
      try {
        const body = JSON.parse(text) as { error?: string | { message?: string } };
        message = typeof body.error === 'string' ? body.error : body.error?.message ?? message;
      } catch { /* The status remains the safe fallback. */ }
    } else if (text.trim().startsWith('<')) {
      message = `The local API returned HTML (${response.status}). Check the API address and whether the Go host is running.`;
    }
    throw new Error(message);
  }
  if (response.status === 204) return undefined as T;
  if (!contentType.includes('application/json')) throw new Error(`The local API returned ${contentType || 'an unknown content type'} instead of JSON.`);
  return JSON.parse(text) as T;
}
