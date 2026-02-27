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
