// API client for ktalk-panel backend.

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

export const api = {
  // Auth
  me: () => apiFetch<{ configured: boolean }>('/api/auth/me'),
  setup: (password: string) =>
    apiFetch<{ ok: boolean }>('/api/auth/setup', {
      method: 'POST',
      body: JSON.stringify({ password })
    }),
  login: (password: string) =>
    apiFetch<{ ok: boolean }>('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ password })
    }),
  logout: () => apiFetch<{ ok: boolean }>('/api/auth/logout', { method: 'POST' }),
  changePassword: (current: string, newPwd: string) =>
    apiFetch<{ ok: boolean }>('/api/auth/password', {
      method: 'POST',
      body: JSON.stringify({ current, new: newPwd })
    }),

  // Clients
  listClients: () => apiFetch<Client[]>('/api/clients'),
  createClient: (data: {
    name: string;
    comment?: string;
    subdomain: string;
    room_id: string;
    hash?: string;
    quota?: Quota;
  }) => apiFetch<Client>('/api/clients', { method: 'POST', body: JSON.stringify(data) }),
  deleteClient: (id: string) =>
    apiFetch<void>(`/api/clients/${id}`, { method: 'DELETE' }),

  // Actions
  startClient: (id: string) =>
    apiFetch<{ ok: boolean }>(`/api/clients/${id}/start`, { method: 'POST' }),
  stopClient: (id: string) =>
    apiFetch<{ ok: boolean }>(`/api/clients/${id}/stop`, { method: 'POST' }),
  restartClient: (id: string) =>
    apiFetch<{ ok: boolean }>(`/api/clients/${id}/restart`, { method: 'POST' }),
  rotateKey: (id: string) =>
    apiFetch<{ ok: boolean }>(`/api/clients/${id}/rotate-key`, { method: 'POST' }),
  rotateRoom: (id: string) =>
    apiFetch<{ ok: boolean }>(`/api/clients/${id}/rotate-room`, { method: 'POST' }),

  // State & Logs
  state: () => apiFetch<ProcessState[]>('/api/state'),
  logs: (id: string) => apiFetch<{ logs: LogLine[] }>(`/api/logs/${id}`)
};

export function subUrl(id: string, token: string): string {
  return `${window.location.origin}/sub/${id}/${token}`;
}

export function subQrUrl(id: string, token: string): string {
  const sub = encodeURIComponent(subUrl(id, token));
  return `https://api.qrserver.com/v1/create-qr-code/?size=256x256&data=${sub}`;
}
