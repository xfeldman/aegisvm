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
  kit_version?: string
  harness_version?: string
  workspace?: string
  secret_keys?: string[]
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
  version?: string
  backend: string
  capabilities: Record<string, unknown>
}

export interface KitConfigFile {
  path: string
  location: 'workspace' | 'host'
  label?: string
  default?: Record<string, any>
}

export interface Kit {
  name: string
  version?: string
  description?: string
  image?: string
  defaults?: { command?: string[] }
  referenced_env?: string[]
  config?: KitConfigFile[]
}

export interface CreateInstanceRequest {
  command: string[]
  handle?: string
  kit?: string
  image_ref?: string
  workspace?: string
  memory_mb?: number
  secrets?: string[]
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

// Open URL in system browser (works in both desktop app and browser contexts)
export function openInBrowser(url: string) {
  if (document.documentElement.classList.contains('desktop-app')) {
    fetch('/open-url', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: 'url=' + encodeURIComponent(url),
    })
  } else {
    window.open(url, '_blank')
  }
}

// Instances
export const listInstances = (state?: string) =>
  request<Instance[]>('GET', '/instances' + (state ? `?state=${state}` : ''))

export const getInstance = (id: string) =>
  request<Instance>('GET', `/instances/${encodeURIComponent(id)}`)

export const startInstance = (id: string) =>
  request<unknown>('POST', `/instances/${encodeURIComponent(id)}/start`)

export const restartInstance = (id: string) =>
  request<unknown>('POST', `/instances/${encodeURIComponent(id)}/restart`)

export const disableInstance = (id: string) =>
  request<unknown>('POST', `/instances/${encodeURIComponent(id)}/disable`)

export const pauseInstance = (id: string) =>
  request<unknown>('POST', `/instances/${encodeURIComponent(id)}/pause`)

export const resumeInstance = (id: string) =>
  request<unknown>('POST', `/instances/${encodeURIComponent(id)}/resume`)

export const deleteInstance = (id: string) =>
  request<unknown>('DELETE', `/instances/${encodeURIComponent(id)}`)

// Instances — create
export const createInstance = (req: CreateInstanceRequest) =>
  request<{ id: string; state: string; command: string[]; handle?: string; kit?: string }>('POST', '/instances', req)

export const exposePort = (id: string, guestPort: number, protocol = 'http') =>
  request<Endpoint>('POST', `/instances/${encodeURIComponent(id)}/expose`, { port: guestPort, protocol })

export const updateInstanceSecrets = (id: string, secrets: string[]) =>
  request<{ secret_keys: string[]; restart_required?: boolean }>('PUT', `/instances/${encodeURIComponent(id)}/secrets`, { secrets })

// Secrets
export const listSecrets = () =>
  request<SecretInfo[]>('GET', '/secrets')

export const setSecret = (name: string, value: string) =>
  request<SecretInfo>('PUT', `/secrets/${encodeURIComponent(name)}`, { value })

export const deleteSecret = (name: string) =>
  request<unknown>('DELETE', `/secrets/${encodeURIComponent(name)}`)

// Kits
export const listKits = () =>
  request<Kit[]>('GET', '/kits')

// Status
export const getStatus = () =>
  request<DaemonStatus>('GET', '/status')

// Kit config files (host-side, ~/.aegis/kits/{handle}/)
export async function readKitConfig(id: string, file: string): Promise<string> {
  const res = await fetch(`${BASE}/instances/${encodeURIComponent(id)}/kit-config?file=${encodeURIComponent(file)}`)
  if (!res.ok) {
    let msg = res.statusText
    try { const d = await res.json(); if (d.error) msg = d.error } catch {}
    throw new APIError(res.status, msg)
  }
  return res.text()
}

export async function writeKitConfig(id: string, file: string, content: string): Promise<void> {
  const res = await fetch(`${BASE}/instances/${encodeURIComponent(id)}/kit-config?file=${encodeURIComponent(file)}`, {
    method: 'POST',
    body: content,
  })
  if (!res.ok) {
    let msg = res.statusText
    try { const d = await res.json(); if (d.error) msg = d.error } catch {}
    throw new APIError(res.status, msg)
  }
}

// Workspace directory listing
export interface FileEntry {
  name: string
  is_dir: boolean
  size: number
}

// Tether watermarks (server-side read position / high-water mark)
export const getTetherWatermark = (id: string, channel: string) =>
  request<{ seq: number }>('GET', `/instances/${encodeURIComponent(id)}/tether/watermark?channel=${encodeURIComponent(channel)}`)

export const setTetherWatermark = (id: string, channel: string, seq: number) =>
  request<{ seq: number }>('POST', `/instances/${encodeURIComponent(id)}/tether/watermark?channel=${encodeURIComponent(channel)}`, { seq })

export async function listWorkspaceDir(id: string, path = '.'): Promise<FileEntry[]> {
  const res = await fetch(`${BASE}/instances/${encodeURIComponent(id)}/workspace/tree?path=${encodeURIComponent(path)}`)
  if (!res.ok) {
    let msg = res.statusText
    try { const d = await res.json(); if (d.error) msg = d.error } catch {}
    throw new APIError(res.status, msg)
  }
  return res.json()
}

// Workspace files
export async function readWorkspaceFile(id: string, path: string): Promise<string> {
  const res = await fetch(`${BASE}/instances/${encodeURIComponent(id)}/workspace?path=${encodeURIComponent(path)}`)
  if (!res.ok) {
    let msg = res.statusText
    try { const d = await res.json(); if (d.error) msg = d.error } catch {}
    throw new APIError(res.status, msg)
  }
  return res.text()
}

export async function writeWorkspaceFile(id: string, path: string, content: string): Promise<void> {
  const res = await fetch(`${BASE}/instances/${encodeURIComponent(id)}/workspace?path=${encodeURIComponent(path)}`, {
    method: 'POST',
    body: content,
  })
  if (!res.ok) {
    let msg = res.statusText
    try { const d = await res.json(); if (d.error) msg = d.error } catch {}
    throw new APIError(res.status, msg)
  }
}

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

export interface BlobRef {
  blob: string
  media_type: string
  size: number
}

export async function uploadBlob(id: string, file: File): Promise<BlobRef> {
  const data = await file.arrayBuffer()
  const res = await fetch(`${BASE}/instances/${encodeURIComponent(id)}/blob`, {
    method: 'POST',
    headers: { 'Content-Type': file.type || 'image/png' },
    body: data,
  })
  if (!res.ok) throw new Error(await res.text())
  return res.json()
}

export function tetherSend(id: string, sessionId: string, text: string, images?: BlobRef[]): Promise<TetherSendResult> {
  const msgId = `ui-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`
  const payload: Record<string, unknown> = { text }
  if (images && images.length > 0) {
    payload.images = images
  }
  const frame = {
    v: 1,
    type: 'user.message',
    ts: new Date().toISOString(),
    session: { channel: 'ui', id: sessionId },
    msg_id: msgId,
    payload,
  }
  return request<TetherSendResult>('POST', `/instances/${encodeURIComponent(id)}/tether`, frame)
}

export function tetherCancel(id: string, sessionId: string): Promise<void> {
  const frame = {
    v: 1,
    type: 'control.cancel',
    ts: new Date().toISOString(),
    session: { channel: 'ui', id: sessionId },
  }
  return request<void>('POST', `/instances/${encodeURIComponent(id)}/tether`, frame)
}

export async function tetherPoll(
  id: string,
  sessionId: string,
  afterSeq: number,
  waitMs = 5000,
  signal?: AbortSignal,
  channel = 'ui',
): Promise<TetherPollResult> {
  const params = new URLSearchParams({
    channel,
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

// Reverse poll: fetch frames before a given seq (for loading older history).
// Returns frames in chronological order (oldest first).
export async function tetherPollBack(
  id: string,
  sessionId: string,
  beforeSeq: number,
  limit = 50,
  channel = 'ui',
): Promise<TetherPollResult> {
  const params = new URLSearchParams({
    channel,
    session_id: sessionId,
    before_seq: String(beforeSeq),
    limit: String(limit),
  })
  const res = await fetch(
    `${BASE}/instances/${encodeURIComponent(id)}/tether/poll?${params}`,
  )
  if (!res.ok) {
    let msg = res.statusText
    try { const d = await res.json(); if (d.error) msg = d.error } catch {}
    throw new APIError(res.status, msg)
  }
  return res.json()
}
