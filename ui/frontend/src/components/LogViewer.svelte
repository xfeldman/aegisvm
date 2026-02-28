<script lang="ts">
  import { onMount } from 'svelte'

  interface Props {
    instanceId: string
  }

  let { instanceId }: Props = $props()

  interface LogEntry {
    ts: string
    stream: string
    line: string
    exec_id?: string
  }

  let lines: LogEntry[] = $state([])
  let autoScroll = $state(true)
  let streamFilter = $state('all') // 'all' | 'stdout' | 'stderr'
  let container: HTMLElement

  let filtered = $derived(
    streamFilter === 'all' ? lines : lines.filter(l => l.stream === streamFilter)
  )

  function scrollToBottom() {
    if (container && autoScroll) {
      requestAnimationFrame(() => {
        container.scrollTop = container.scrollHeight
      })
    }
  }

  function onScroll() {
    if (!container) return
    const atBottom = container.scrollHeight - container.scrollTop - container.clientHeight < 40
    autoScroll = atBottom
  }

  function formatTime(ts: string): string {
    const d = new Date(ts)
    return d.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })
  }

  function clear() {
    lines = []
  }

  onMount(() => {
    const controller = new AbortController()

    async function streamLogs() {
      try {
        const res = await fetch(`/api/v1/instances/${encodeURIComponent(instanceId)}/logs?follow=true&tail=200`, {
          signal: controller.signal,
        })
        if (!res.ok || !res.body) return

        const reader = res.body.getReader()
        const decoder = new TextDecoder()
        let buffer = ''

        while (true) {
          const { done, value } = await reader.read()
          if (done) break
          buffer += decoder.decode(value, { stream: true })
          const parts = buffer.split('\n')
          buffer = parts.pop() || ''
          for (const part of parts) {
            if (!part.trim()) continue
            try {
              const entry: LogEntry = JSON.parse(part)
              if (entry.line !== undefined) {
                lines = [...lines, entry]
                scrollToBottom()
              }
            } catch {}
          }
        }
      } catch (e) {
        if (e instanceof DOMException && e.name === 'AbortError') return
      }
    }

    streamLogs()
    return () => controller.abort()
  })
</script>

<div class="log-viewer">
  <div class="log-toolbar">
    <div class="filter-group">
      <button class="filter-btn" class:active={streamFilter === 'all'} onclick={() => streamFilter = 'all'}>All</button>
      <button class="filter-btn" class:active={streamFilter === 'stdout'} onclick={() => streamFilter = 'stdout'}>stdout</button>
      <button class="filter-btn" class:active={streamFilter === 'stderr'} onclick={() => streamFilter = 'stderr'}>stderr</button>
    </div>
    <div class="toolbar-right">
      <button class="tool-btn" class:active={autoScroll} onclick={() => { autoScroll = !autoScroll; if (autoScroll) scrollToBottom() }}>
        auto-scroll {autoScroll ? 'on' : 'off'}
      </button>
      <button class="tool-btn" onclick={clear}>clear</button>
    </div>
  </div>
  <div class="log-output" bind:this={container} onscroll={onScroll}>
    {#if filtered.length === 0}
      <div class="empty">No logs yet.</div>
    {:else}
      {#each filtered as entry}
        <div class="log-line" class:stderr={entry.stream === 'stderr'}>
          <span class="ts">{formatTime(entry.ts)}</span>
          <span class="text">{entry.line}</span>
        </div>
      {/each}
    {/if}
  </div>
</div>

<style>
  .log-viewer {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 300px;
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    overflow: hidden;
  }

  .log-toolbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 6px 12px;
    background: var(--bg-tertiary);
    border-bottom: 1px solid var(--border);
    flex-shrink: 0;
  }

  .filter-group {
    display: flex;
    gap: 2px;
  }

  .filter-btn, .tool-btn {
    padding: 2px 8px;
    border-radius: var(--radius);
    border: 1px solid transparent;
    background: transparent;
    color: var(--text-muted);
    font-size: 12px;
    font-family: var(--font-mono);
  }
  .filter-btn:hover, .tool-btn:hover {
    background: var(--bg);
    color: var(--text);
  }
  .filter-btn.active {
    background: var(--bg);
    color: var(--text);
    border-color: var(--border);
  }
  .tool-btn.active {
    color: var(--accent);
  }

  .toolbar-right {
    display: flex;
    gap: 8px;
  }

  .log-output {
    flex: 1;
    overflow-y: auto;
    padding: 8px 12px;
    background: var(--bg);
    font-family: var(--font-mono);
    font-size: 12px;
    line-height: 1.6;
  }

  .log-line {
    display: flex;
    gap: 10px;
    white-space: pre-wrap;
    word-break: break-all;
  }
  .log-line.stderr {
    color: var(--yellow);
  }

  .ts {
    color: var(--text-muted);
    flex-shrink: 0;
    user-select: none;
  }

  .text {
    flex: 1;
  }

  .empty {
    color: var(--text-muted);
    padding: 20px;
    text-align: center;
  }
</style>
