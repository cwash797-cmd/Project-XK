// API client for the admin panel.

export interface Client {
  id: string;
  name: string;
  comment?: string;
  room: { subdomain: string; room_id: string; hash?: string };
  shared_key: string;
  sub_token: string;
  quota: Quota;
  created_at: string;
}

export interface Quota {
  speed_mbps?: number;
  traffic_gb?: number;
  used_bytes?: number;
  expires_at?: string;
}

export interface ProcessState {
  client_id: string;
  running: boolean;
  started_at?: string;
  exited_at?: string;
  exit_err?: string;
  restarts: number;
}

export interface LogLine {
  t: string;
  line: string;
}

export interface SSEEvent {
  type: 'state' | 'log' | string;
  data: any;
}

// ── Low-level fetch ────────────────────────────────────────────────────────

async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: 'include',
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
    ...init
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(body.error ?? res.statusText);
  }
  if (res.status === 204) return undefined as T;
  return res.json();
}

// ── API calls ──────────────────────────────────────────────────────────────

export const api = {
  // Auth
  me:     () => apiFetch<{ configured: boolean }>('/api/auth/me'),
  setup:  (password: string) =>
    apiFetch<{ ok: boolean }>('/api/auth/setup', { method: 'POST', body: JSON.stringify({ password }) }),
  login:  (password: string) =>
    apiFetch<{ ok: boolean }>('/api/auth/login', { method: 'POST', body: JSON.stringify({ password }) }),
  logout: () => apiFetch<{ ok: boolean }>('/api/auth/logout', { method: 'POST' }),
  changePassword: (current: string, newPwd: string) =>
    apiFetch<{ ok: boolean }>('/api/auth/password', {
      method: 'POST', body: JSON.stringify({ current, new: newPwd })
    }),

  // Clients
  listClients: () => apiFetch<Client[]>('/api/clients'),
  createClient: (data: {
    name: string; comment?: string; subdomain: string;
    room_id: string; hash?: string; quota?: Quota;
  }) => apiFetch<Client>('/api/clients', { method: 'POST', body: JSON.stringify(data) }),
  deleteClient: (id: string) => apiFetch<void>(`/api/clients/${id}`, { method: 'DELETE' }),

  // Process actions
  startClient:   (id: string) => apiFetch<{ ok: boolean }>(`/api/clients/${id}/start`,       { method: 'POST' }),
  stopClient:    (id: string) => apiFetch<{ ok: boolean }>(`/api/clients/${id}/stop`,        { method: 'POST' }),
  restartClient: (id: string) => apiFetch<{ ok: boolean }>(`/api/clients/${id}/restart`,     { method: 'POST' }),
  rotateKey:     (id: string) => apiFetch<{ ok: boolean }>(`/api/clients/${id}/rotate-key`,  { method: 'POST' }),
  rotateRoom:    (id: string) => apiFetch<{ ok: boolean }>(`/api/clients/${id}/rotate-room`, { method: 'POST' }),

  // State & Logs
  state: () => apiFetch<ProcessState[]>('/api/state'),
  logs:  (id: string) => apiFetch<{ logs: LogLine[] }>(`/api/logs/${id}`)
};

// ── SSE ────────────────────────────────────────────────────────────────────

/**
 * Connect to /api/events and call `onEvent` for each message.
 * Returns a cleanup function that closes the EventSource.
 */
export function connectSSE(onEvent: (e: SSEEvent) => void): () => void {
  const es = new EventSource('/api/events', { withCredentials: true });

  es.onmessage = (raw) => {
    try {
      const evt: SSEEvent = JSON.parse(raw.data);
      onEvent(evt);
    } catch {
      // ignore malformed lines
    }
  };

  es.onerror = () => {
    // Browser will auto-reconnect per the retry hint we send.
  };

  return () => es.close();
}

// ── Helpers ────────────────────────────────────────────────────────────────

export function subUrl(id: string, token: string): string {
  return `${window.location.origin}/sub/${id}/${token}`;
}

export function subQrUrl(id: string, token: string): string {
  const sub = encodeURIComponent(subUrl(id, token));
  return `https://api.qrserver.com/v1/create-qr-code/?size=256x256&data=${sub}`;
}

export function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MB`;
  return `${(n / 1024 ** 3).toFixed(2)} GB`;
}

export function fmtTime(iso?: string): string {
  if (!iso) return '—';
  return new Date(iso).toLocaleString();
}
