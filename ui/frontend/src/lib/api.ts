// API client for aegisd — talks to /api/v1/... which the Go server proxies
// to the aegisd unix socket.

const BASE = '/api/v1'

export interface Instance {
  id: string
  state: string
  enabled: boolean
  command: string[]
  handle?: string
  image_ref?: string
  kit?: string
  workspace?: string
  created_at: string
  stopped_at?: string
  last_active_at?: string
  active_connections?: number
  expose_ports?: number[]
  endpoints?: Endpoint[]
  gateway_running?: boolean
}

export interface Endpoint {
  guest_port: number
  public_port: number
  protocol: string
}

export interface SecretInfo {
  name: string
  created_at: string
}

export interface DaemonStatus {
  status: string
  backend: string
  capabilities: Record<string, unknown>
}

export interface Kit {
  name: string
  version?: string
  description?: string
  image?: string
}

class APIError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const opts: RequestInit = {
    method,
    headers: { 'Content-Type': 'application/json' },
  }
  if (body !== undefined) {
    opts.body = JSON.stringify(body)
  }
  const res = await fetch(BASE + path, opts)
  if (!res.ok) {
    let msg = res.statusText
    try {
      const data = await res.json()
      if (data.error) msg = data.error
    } catch {}
    throw new APIError(res.status, msg)
  }
  const text = await res.text()
  if (!text) return undefined as T
  return JSON.parse(text)
}

// Instances
export const listInstances = (state?: string) =>
  request<Instance[]>('GET', '/instances' + (state ? `?state=${state}` : ''))

export const getInstance = (id: string) =>
  request<Instance>('GET', `/instances/${encodeURIComponent(id)}`)

export const startInstance = (id: string) =>
  request<unknown>('POST', `/instances/${encodeURIComponent(id)}/start`)

export const disableInstance = (id: string) =>
  request<unknown>('POST', `/instances/${encodeURIComponent(id)}/disable`)

export const pauseInstance = (id: string) =>
  request<unknown>('POST', `/instances/${encodeURIComponent(id)}/pause`)

export const resumeInstance = (id: string) =>
  request<unknown>('POST', `/instances/${encodeURIComponent(id)}/resume`)

export const deleteInstance = (id: string) =>
  request<unknown>('DELETE', `/instances/${encodeURIComponent(id)}`)

// Secrets
export const listSecrets = () =>
  request<SecretInfo[]>('GET', '/secrets')

// Status
export const getStatus = () =>
  request<DaemonStatus>('GET', '/status')

// Tether (Agent Kit messaging)

export interface TetherFrame {
  v: number
  type: string
  ts: string
  seq: number
  session: { channel: string; id: string }
  msg_id?: string
  payload: Record<string, any>
}

export interface TetherSendResult {
  msg_id: string
  session_id: string
  ingress_seq: number
}

export interface TetherPollResult {
  frames: TetherFrame[]
  next_seq: number
  timed_out: boolean
}

export function tetherSend(id: string, sessionId: string, text: string): Promise<TetherSendResult> {
  const msgId = `ui-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`
  const frame = {
    v: 1,
    type: 'user.message',
    ts: new Date().toISOString(),
    session: { channel: 'ui', id: sessionId },
    msg_id: msgId,
    payload: { text },
  }
  return request<TetherSendResult>('POST', `/instances/${encodeURIComponent(id)}/tether`, frame)
}

export async function tetherPoll(
  id: string,
  sessionId: string,
  afterSeq: number,
  waitMs = 5000,
  signal?: AbortSignal,
): Promise<TetherPollResult> {
  const params = new URLSearchParams({
    channel: 'ui',
    session_id: sessionId,
    after_seq: String(afterSeq),
    wait_ms: String(waitMs),
  })
  const res = await fetch(
    `${BASE}/instances/${encodeURIComponent(id)}/tether/poll?${params}`,
    { signal },
  )
  if (!res.ok) {
    let msg = res.statusText
    try { const d = await res.json(); if (d.error) msg = d.error } catch {}
    throw new APIError(res.status, msg)
  }
  return res.json()
}
