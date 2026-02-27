<script lang="ts">
  import { onMount } from 'svelte'
  import {
    listKits, readWorkspaceFile, writeWorkspaceFile,
    readKitConfig, writeKitConfig, tetherSend,
    type Kit, type KitConfigFile,
  } from '../lib/api'
  import { addToast } from '../lib/store.svelte'

  interface Props {
    instanceId: string
    kitName: string
  }

  let { instanceId, kitName }: Props = $props()

  let configs: KitConfigFile[] = $state([])
  let activeIdx = $state(0)
  let loading = $state(true)

  // Per-config editor state
  let contents: string[] = $state([])
  let originals: string[] = $state([])
  let errors: (string | null)[] = $state([])
  let saving = $state(false)
  let textareaEl: HTMLTextAreaElement = $state(null!)
  let codeEl: HTMLElement = $state(null!)

  let active = $derived(configs[activeIdx])
  let content = $derived(contents[activeIdx] ?? '')
  let original = $derived(originals[activeIdx] ?? '')
  let jsonError = $derived(errors[activeIdx] ?? null)
  let dirty = $derived(content !== original)

  function validate(text: string): string | null {
    if (!text.trim()) return null
    try {
      JSON.parse(text)
      return null
    } catch (e) {
      return e instanceof Error ? e.message : 'Invalid JSON'
    }
  }

  function highlight(json: string): string {
    let result = ''
    let i = 0
    const len = json.length

    while (i < len) {
      const ch = json[i]

      if (ch === '"') {
        let str = '"'
        i++
        while (i < len) {
          if (json[i] === '\\' && i + 1 < len) {
            str += json[i] + json[i + 1]
            i += 2
          } else if (json[i] === '"') {
            str += '"'
            i++
            break
          } else {
            str += json[i]
            i++
          }
        }
        const escaped = str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
        let j = i
        while (j < len && (json[j] === ' ' || json[j] === '\t')) j++
        if (j < len && json[j] === ':') {
          result += `<span class="hl-key">${escaped}</span>`
        } else {
          result += `<span class="hl-str">${escaped}</span>`
        }
      } else if (ch === '-' || (ch >= '0' && ch <= '9')) {
        let num = ''
        while (i < len && /[\d.eE+\-]/.test(json[i])) {
          num += json[i]
          i++
        }
        result += `<span class="hl-num">${num}</span>`
      } else if (json.startsWith('true', i)) {
        result += '<span class="hl-bool">true</span>'
        i += 4
      } else if (json.startsWith('false', i)) {
        result += '<span class="hl-bool">false</span>'
        i += 5
      } else if (json.startsWith('null', i)) {
        result += '<span class="hl-null">null</span>'
        i += 4
      } else {
        if (ch === '<') result += '&lt;'
        else if (ch === '>') result += '&gt;'
        else if (ch === '&') result += '&amp;'
        else result += ch
        i++
      }
    }
    return result
  }

  let highlighted = $derived(highlight(content))

  function setContent(text: string) {
    contents = contents.map((c, i) => i === activeIdx ? text : c)
    errors = errors.map((e, i) => i === activeIdx ? validate(text) : e)
  }

  function onInput(e: Event) {
    setContent((e.target as HTMLTextAreaElement).value)
    syncScroll()
  }

  function syncScroll() {
    if (codeEl && textareaEl) {
      codeEl.scrollTop = textareaEl.scrollTop
      codeEl.scrollLeft = textareaEl.scrollLeft
    }
  }

  function onKeydown(e: KeyboardEvent) {
    if (e.key === 'Tab') {
      e.preventDefault()
      const start = textareaEl.selectionStart
      const end = textareaEl.selectionEnd
      const newContent = content.substring(0, start) + '  ' + content.substring(end)
      setContent(newContent)
      requestAnimationFrame(() => {
        textareaEl.selectionStart = textareaEl.selectionEnd = start + 2
      })
    }
  }

  function switchTab(idx: number) {
    activeIdx = idx
  }

  async function readConfig(cfg: KitConfigFile): Promise<string> {
    if (cfg.location === 'workspace') {
      return readWorkspaceFile(instanceId, cfg.path)
    } else {
      return readKitConfig(instanceId, cfg.path)
    }
  }

  async function writeConfig(cfg: KitConfigFile, data: string): Promise<void> {
    if (cfg.location === 'workspace') {
      await writeWorkspaceFile(instanceId, cfg.path, data)
    } else {
      await writeKitConfig(instanceId, cfg.path, data)
    }
  }

  async function load() {
    loading = true
    try {
      const kits = await listKits()
      const kit = kits.find(k => k.name === kitName)
      if (!kit?.config?.length) {
        configs = []
        loading = false
        return
      }
      configs = kit.config

      const loaded: string[] = []
      const origs: string[] = []
      const errs: (string | null)[] = []

      for (const cfg of kit.config) {
        try {
          const text = await readConfig(cfg)
          loaded.push(text)
          origs.push(text)
        } catch {
          loaded.push('{}\n')
          origs.push('{}\n')
        }
        errs.push(null)
      }

      contents = loaded
      originals = origs
      errors = errs
    } catch {
      configs = []
    } finally {
      loading = false
    }
  }

  async function save(restart = false) {
    if (!active || jsonError) {
      addToast('Fix JSON errors before saving', 'error')
      return
    }

    saving = true
    try {
      const formatted = JSON.stringify(JSON.parse(content), null, 2) + '\n'
      await writeConfig(active, formatted)
      contents = contents.map((c, i) => i === activeIdx ? formatted : c)
      originals = originals.map((o, i) => i === activeIdx ? formatted : o)

      if (restart && active.location === 'workspace') {
        await tetherSend(instanceId, 'default', 'Run self_restart to apply the updated configuration.')
        addToast('Config saved — restart requested', 'success')
      } else {
        addToast('Config saved', 'success')
      }
    } catch (e) {
      addToast(`Save failed: ${e instanceof Error ? e.message : 'unknown'}`, 'error')
    } finally {
      saving = false
    }
  }

  onMount(() => { load() })
</script>

{#if loading}
  <div class="empty">Loading config...</div>
{:else if configs.length === 0}
  <div class="empty">No configuration files declared by this kit.</div>
{:else}
  <div class="config-wrapper">
    {#if configs.length > 1}
      <div class="config-tabs">
        {#each configs as cfg, idx}
          <button class="config-tab" class:active={activeIdx === idx} onclick={() => switchTab(idx)}>
            {cfg.label || cfg.path}
            <span class="location-badge">{cfg.location}</span>
          </button>
        {/each}
      </div>
    {/if}

    <div class="editor">
      <div class="toolbar">
        <span class="filepath">{active.path}</span>
        <span class="location-badge">{active.location}</span>
        <div class="toolbar-right">
          <button class="btn" onclick={() => save()} disabled={saving || !!jsonError || !dirty}>
            {saving ? 'Saving...' : 'Save'}
          </button>
          {#if active.location === 'workspace'}
            <button class="btn btn-primary" onclick={() => save(true)} disabled={saving || !!jsonError}>
              Save + Restart
            </button>
          {/if}
        </div>
      </div>
      <div class="editor-body" class:has-error={!!jsonError}>
        <pre class="highlight" bind:this={codeEl}>{@html highlighted}{'\n'}</pre>
        <textarea
          class="input-overlay"
          bind:this={textareaEl}
          value={content}
          oninput={onInput}
          onscroll={syncScroll}
          onkeydown={onKeydown}
          spellcheck="false"
        ></textarea>
      </div>
      {#if jsonError}
        <div class="error-bar">{jsonError}</div>
      {/if}
    </div>



  </div>
{/if}

<style>
  .config-wrapper {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .config-tabs {
    display: flex;
    gap: 0;
    border-bottom: 1px solid var(--border);
  }

  .config-tab {
    padding: 6px 14px;
    border: none;
    background: none;
    color: var(--text-muted);
    font-size: 13px;
    font-weight: 500;
    border-bottom: 2px solid transparent;
    margin-bottom: -1px;
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .config-tab:hover { color: var(--text); }
  .config-tab.active {
    color: var(--text);
    border-bottom-color: var(--accent);
  }

  .location-badge {
    font-size: 10px;
    padding: 1px 5px;
    border-radius: 6px;
    background: var(--bg-tertiary);
    color: var(--text-muted);
    font-weight: 400;
  }

  .editor {
    display: flex;
    flex-direction: column;
    height: 400px;
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    overflow: hidden;
  }

  .toolbar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    background: var(--bg-tertiary);
    border-bottom: 1px solid var(--border);
    flex-shrink: 0;
  }

  .filepath {
    font-family: var(--font-mono);
    font-size: 12px;
    color: var(--text-muted);
  }

  .toolbar-right {
    margin-left: auto;
    display: flex;
    gap: 6px;
  }

  .btn {
    padding: 4px 12px;
    border-radius: var(--radius);
    border: 1px solid var(--border);
    background: var(--bg);
    color: var(--text);
    font-size: 12px;
    font-family: var(--font-mono);
  }
  .btn:hover:not(:disabled) { border-color: var(--text-muted); }
  .btn:disabled { opacity: 0.4; cursor: not-allowed; }

  .btn-primary {
    background: var(--accent);
    border-color: var(--accent);
    color: #fff;
  }
  .btn-primary:hover:not(:disabled) { background: var(--accent-hover); }

  .editor-body {
    flex: 1;
    position: relative;
    overflow: hidden;
    background: var(--bg);
  }
  .editor-body.has-error {
    box-shadow: inset 0 0 0 1px var(--red);
  }

  .highlight, .input-overlay {
    position: absolute;
    inset: 0;
    margin: 0;
    padding: 12px;
    border: none;
    font-family: var(--font-mono);
    font-size: 13px;
    line-height: 1.6;
    tab-size: 2;
    white-space: pre-wrap;
    word-wrap: break-word;
    overflow-wrap: break-word;
    overflow: auto;
    letter-spacing: normal;
    text-rendering: auto;
  }

  .highlight {
    pointer-events: none;
    color: var(--text);
  }

  .highlight :global(.hl-key) { color: #79c0ff; }
  .highlight :global(.hl-str) { color: #a5d6ff; }
  .highlight :global(.hl-num) { color: #79c0ff; }
  .highlight :global(.hl-bool) { color: #ff7b72; }
  .highlight :global(.hl-null) { color: #8b949e; }

  .input-overlay {
    color: transparent;
    caret-color: var(--text);
    background: transparent;
    border: none;
    outline: none;
    resize: none;
    -webkit-text-fill-color: transparent;
  }

  .error-bar {
    padding: 6px 12px;
    background: rgba(248, 81, 73, 0.1);
    border-top: 1px solid var(--red);
    color: var(--red);
    font-size: 12px;
    font-family: var(--font-mono);
    flex-shrink: 0;
  }

  .empty {
    color: var(--text-muted);
    text-align: center;
    padding: 40px;
  }

</style>
