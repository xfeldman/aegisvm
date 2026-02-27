// Svelte 5 reactive state stores for the Aegis UI.

import { listInstances, type Instance } from './api'

// Toast notification system
export interface Toast {
  id: number
  message: string
  type: 'success' | 'error' | 'info'
}

let nextToastId = 0

// Reactive state using Svelte 5 $state rune (module-level)
// These are exported as getter functions since $state at module level
// needs to be accessed via functions.

let _instances: Instance[] = $state([])
let _loading: boolean = $state(false)
let _error: string | null = $state(null)
let _toasts: Toast[] = $state([])

export function getInstances(): Instance[] { return _instances }
export function isLoading(): boolean { return _loading }
export function getError(): string | null { return _error }
export function getToasts(): Toast[] { return _toasts }

export function addToast(message: string, type: Toast['type'] = 'info') {
  const id = nextToastId++
  _toasts = [..._toasts, { id, message, type }]
  setTimeout(() => {
    _toasts = _toasts.filter(t => t.id !== id)
  }, 4000)
}

// Exec history — persists across tab switches, keyed by instance ID
export interface ExecEntry {
  command: string
  output: string
  exitCode: number | null
  duration: string
  running: boolean
}

let _execHistory: Record<string, ExecEntry[]> = $state({})
let _cmdHistory: Record<string, string[]> = $state({})

export function getExecHistory(id: string): ExecEntry[] { return _execHistory[id] || [] }
export function setExecHistory(id: string, entries: ExecEntry[]) { _execHistory[id] = entries }
export function getCmdHistory(id: string): string[] { return _cmdHistory[id] || [] }
export function pushCmdHistory(id: string, cmd: string) {
  _cmdHistory[id] = [cmd, ...(_cmdHistory[id] || []).slice(0, 49)]
}

// Chat state — persists across tab switches, keyed by instance ID
export interface ChatMessage {
  role: 'user' | 'assistant'
  text: string
  images?: { media_type: string; blob: string }[]
  ts: string
  streaming?: boolean
}

export interface ChatState {
  messages: ChatMessage[]
  cursor: number          // after_seq for next poll
  thinking: string | null // presence indicator
}

const CHAT_STORAGE_KEY = 'aegis-chat-'

function loadChatState(id: string): ChatState {
  try {
    const raw = localStorage.getItem(CHAT_STORAGE_KEY + id)
    if (raw) {
      const saved = JSON.parse(raw)
      // Clear transient state from previous session
      saved.thinking = null
      if (saved.messages) {
        saved.messages = saved.messages.map((m: ChatMessage) => ({ ...m, streaming: false }))
      }
      return saved
    }
  } catch {}
  return { messages: [], cursor: 0, thinking: null }
}

function saveChatState(id: string, state: ChatState) {
  try {
    // Only persist messages and cursor, not transient thinking state
    localStorage.setItem(CHAT_STORAGE_KEY + id, JSON.stringify({
      messages: state.messages,
      cursor: state.cursor,
    }))
  } catch {}
}

let _chatStates: Record<string, ChatState> = $state({})
const _defaultChat: ChatState = { messages: [], cursor: 0, thinking: null }

// Pure read — safe inside $derived. No side effects.
export function getChatState(id: string): ChatState {
  return _chatStates[id] || _defaultChat
}

// Must be called from onMount / event handlers (not during render).
export function initChatState(id: string) {
  if (!_chatStates[id]) {
    _chatStates[id] = loadChatState(id)
  }
}

export function updateChatState(id: string, patch: Partial<ChatState>) {
  const current = _chatStates[id] || loadChatState(id)
  const updated = { ...current, ...patch }
  _chatStates[id] = updated
  saveChatState(id, updated)
}

export async function refreshInstances() {
  _loading = true
  _error = null
  try {
    _instances = await listInstances()
  } catch (e) {
    _error = e instanceof Error ? e.message : 'Failed to fetch instances'
    _instances = []
  } finally {
    _loading = false
  }
}
