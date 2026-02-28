<script lang="ts">
  import { onMount } from 'svelte'
  import { addToast, getExecHistory, setExecHistory, getCmdHistory, pushCmdHistory, type ExecEntry } from '../lib/store.svelte'

  interface Props {
    instanceId: string
    disabled?: boolean
  }

  let { instanceId, disabled = false }: Props = $props()

  let history = $derived(getExecHistory(instanceId))
  let input = $state('')
  let historyIdx = $state(-1)
  let autoScroll = $state(true)
  let outputEl: HTMLElement

  function scrollToBottom() {
    if (outputEl && autoScroll) {
      requestAnimationFrame(() => {
        outputEl.scrollTop = outputEl.scrollHeight
      })
    }
  }

  function onScroll() {
    if (!outputEl) return
    const atBottom = outputEl.scrollHeight - outputEl.scrollTop - outputEl.clientHeight < 40
    autoScroll = atBottom
  }

  onMount(() => {
    scrollToBottom()
  })

  async function run() {
    const cmd = input.trim()
    if (!cmd || disabled) return

    input = ''
    pushCmdHistory(instanceId, cmd)
    historyIdx = -1

    const idx = history.length
    setExecHistory(instanceId, [...history, {
      command: cmd,
      output: '',
      exitCode: null,
      duration: '',
      running: true,
    }])
    scrollToBottom()

    const startTime = performance.now()

    function update(patch: Partial<ExecEntry>) {
      setExecHistory(instanceId, getExecHistory(instanceId).map((e, i) => i === idx ? { ...e, ...patch } : e))
      scrollToBottom()
    }

    try {
      const res = await fetch(`/api/v1/instances/${encodeURIComponent(instanceId)}/exec`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ command: ['sh', '-c', cmd] }),
      })

      if (!res.ok) {
        const data = await res.json().catch(() => ({ error: res.statusText }))
        update({ output: data.error || res.statusText, exitCode: -1, running: false, duration: formatDuration(performance.now() - startTime) })
        return
      }

      // Stream NDJSON response
      const reader = res.body!.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      let output = ''

      function processLines(text: string) {
        buffer += text
        const parts = buffer.split('\n')
        buffer = parts.pop() || ''
        let exitCode: number | null = null
        let done = false
        for (const part of parts) {
          if (!part.trim()) continue
          try {
            const frame = JSON.parse(part)
            if (frame.line !== undefined) {
              output += (output ? '\n' : '') + frame.line
            }
            if (frame.done) {
              exitCode = frame.exit_code ?? 0
              done = true
            }
          } catch {}
        }
        const patch: Partial<ExecEntry> = { output }
        if (done) {
          patch.exitCode = exitCode
          patch.running = false
          patch.duration = formatDuration(performance.now() - startTime)
        }
        update(patch)
      }

      while (true) {
        const { done, value } = await reader.read()
        if (done) break
        processLines(decoder.decode(value, { stream: true }))
      }
      if (buffer.trim()) {
        processLines(buffer + '\n')
      }

      // If stream ended without a done frame
      const current = getExecHistory(instanceId)[idx]
      if (current?.running) {
        update({ running: false, duration: formatDuration(performance.now() - startTime), exitCode: current.exitCode ?? 0 })
      }
    } catch (e) {
      update({ output: e instanceof Error ? e.message : 'exec failed', exitCode: -1, running: false, duration: formatDuration(performance.now() - startTime) })
    }
  }

  function formatDuration(ms: number): string {
    if (ms < 1000) return `${Math.round(ms)}ms`
    return `${(ms / 1000).toFixed(1)}s`
  }

  function onKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      run()
    } else if (e.key === 'ArrowUp') {
      const cmds = getCmdHistory(instanceId)
      if (cmds.length > 0 && historyIdx < cmds.length - 1) {
        historyIdx++
        input = cmds[historyIdx]
      }
    } else if (e.key === 'ArrowDown') {
      const cmds = getCmdHistory(instanceId)
      if (historyIdx > 0) {
        historyIdx--
        input = cmds[historyIdx]
      } else {
        historyIdx = -1
        input = ''
      }
    }
  }

  function copyOutput(text: string) {
    navigator.clipboard.writeText(text)
    addToast('Copied to clipboard', 'success')
  }
</script>

<div class="runner">
  <div class="output" bind:this={outputEl} onscroll={onScroll}>
    {#if history.length === 0}
      <div class="empty">Run a command inside the VM.</div>
    {/if}
    {#each history as entry}
      <div class="exec-block">
        <div class="exec-header">
          <span class="prompt">$</span>
          <span class="cmd">{entry.command}</span>
          <span class="meta">
            {#if entry.running}
              <span class="running-indicator">running...</span>
            {:else}
              {#if entry.exitCode === 0}
                <span class="exit-ok">0</span>
              {:else}
                <span class="exit-err">{entry.exitCode}</span>
              {/if}
              <span class="duration">{entry.duration}</span>
              <button class="copy-btn" onclick={() => copyOutput(entry.output)}>copy</button>
            {/if}
          </span>
        </div>
        {#if entry.output}
          <pre class="exec-output">{entry.output}</pre>
        {/if}
      </div>
    {/each}
  </div>
  <div class="input-bar">
    <span class="input-prompt">$</span>
    <input
      type="text"
      bind:value={input}
      onkeydown={onKeydown}
      placeholder={disabled ? 'Instance must be running' : 'Enter command...'}
      {disabled}
    />
    <button class="run-btn" onclick={run} disabled={disabled || !input.trim()}>Run</button>
  </div>
</div>

<style>
  .runner {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 300px;
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    overflow: hidden;
  }

  .output {
    flex: 1;
    overflow-y: auto;
    padding: 12px;
    background: var(--bg);
    font-family: var(--font-mono);
    font-size: 12px;
  }

  .empty {
    color: var(--text-muted);
    padding: 20px;
    text-align: center;
  }

  .exec-block {
    margin-bottom: 12px;
  }

  .exec-header {
    display: flex;
    align-items: baseline;
    gap: 6px;
    margin-bottom: 2px;
  }

  .prompt {
    color: var(--accent);
    font-weight: 600;
    user-select: none;
  }

  .cmd {
    color: var(--text);
    font-weight: 500;
  }

  .meta {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 11px;
    flex-shrink: 0;
  }

  .exit-ok {
    color: var(--green);
  }
  .exit-err {
    color: var(--red);
  }
  .duration {
    color: var(--text-muted);
  }
  .running-indicator {
    color: var(--accent);
  }

  .copy-btn {
    padding: 1px 6px;
    border-radius: var(--radius);
    border: 1px solid var(--border);
    background: transparent;
    color: var(--text-muted);
    font-size: 11px;
    font-family: var(--font-mono);
  }
  .copy-btn:hover {
    color: var(--text);
    border-color: var(--text-muted);
  }

  .exec-output {
    margin: 0;
    padding: 4px 0 4px 16px;
    color: var(--text);
    white-space: pre-wrap;
    word-break: break-all;
    line-height: 1.5;
  }

  .input-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    background: var(--bg-secondary);
    border-top: 1px solid var(--border);
  }

  .input-prompt {
    color: var(--accent);
    font-family: var(--font-mono);
    font-weight: 600;
    user-select: none;
  }

  input {
    flex: 1;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 6px 10px;
    color: var(--text);
    font-family: var(--font-mono);
    font-size: 13px;
    outline: none;
  }
  input:focus {
    border-color: var(--accent);
  }
  input:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .run-btn {
    padding: 6px 14px;
    border-radius: var(--radius);
    border: 1px solid var(--border);
    background: var(--bg-tertiary);
    color: var(--text);
    font-size: 13px;
  }
  .run-btn:hover:not(:disabled) {
    background: var(--bg);
    border-color: var(--accent);
  }
  .run-btn:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
