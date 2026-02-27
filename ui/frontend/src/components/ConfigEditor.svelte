<script lang="ts">
  import { onMount } from 'svelte'
  import { readWorkspaceFile, writeWorkspaceFile, tetherSend } from '../lib/api'
  import { addToast } from '../lib/store.svelte'

  interface Props {
    instanceId: string
  }

  let { instanceId }: Props = $props()

  let content = $state('')
  let original = $state('')
  let loading = $state(true)
  let saving = $state(false)
  let jsonError: string | null = $state(null)
  let notFound = $state(false)
  let showExample = $state(false)
  let textareaEl: HTMLTextAreaElement = $state(null!)
  let codeEl: HTMLElement = $state(null!)

  const CONFIG_PATH = '.aegis/agent.json'

  const EXAMPLE_CONFIG = `{
  "model": "openai/gpt-4.1",
  "max_tokens": 4096,
  "mcp": {
    "playwright": {
      "command": "npx",
      "args": ["@playwright/mcp@latest"]
    }
  },
  "disabled_tools": ["image_generate"],
  "memory": {
    "inject_mode": "relevant"
  }
}`

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

  // Single-pass JSON tokenizer — avoids regex chaining issues
  function highlight(json: string): string {
    let result = ''
    let i = 0
    const len = json.length

    while (i < len) {
      const ch = json[i]

      if (ch === '"') {
        // Read full string
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
        // Check if this string is a key (followed by optional whitespace + colon)
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
        // Punctuation, whitespace — escape and pass through
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
  let exampleHighlighted = $derived(highlight(EXAMPLE_CONFIG))

  function onInput() {
    jsonError = validate(content)
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
      content = content.substring(0, start) + '  ' + content.substring(end)
      requestAnimationFrame(() => {
        textareaEl.selectionStart = textareaEl.selectionEnd = start + 2
      })
    }
  }

  async function load() {
    loading = true
    try {
      const text = await readWorkspaceFile(instanceId, CONFIG_PATH)
      content = text
      original = text
      notFound = false
    } catch (e: any) {
      if (e?.status === 404) {
        content = '{}\n'
        original = '{}\n'
        notFound = true
        showExample = true
      } else {
        addToast(`Load config: ${e instanceof Error ? e.message : 'failed'}`, 'error')
      }
    } finally {
      loading = false
      jsonError = validate(content)
    }
  }

  async function save(restart = false) {
    jsonError = validate(content)
    if (jsonError) {
      addToast('Fix JSON errors before saving', 'error')
      return
    }

    saving = true
    try {
      const formatted = JSON.stringify(JSON.parse(content), null, 2) + '\n'
      await writeWorkspaceFile(instanceId, CONFIG_PATH, formatted)
      content = formatted
      original = formatted
      notFound = false

      if (restart) {
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

<div class="config-wrapper">
  <div class="editor">
    {#if loading}
      <div class="empty">Loading config...</div>
    {:else}
      <div class="toolbar">
        <span class="filepath">{CONFIG_PATH}</span>
        {#if notFound}
          <span class="badge">new file</span>
        {/if}
        <div class="toolbar-right">
          <button class="btn" onclick={() => save()} disabled={saving || !!jsonError || !dirty}>
            {saving ? 'Saving...' : 'Save'}
          </button>
          <button class="btn btn-primary" onclick={() => save(true)} disabled={saving || !!jsonError}>
            Save + Restart
          </button>
        </div>
      </div>
      <div class="editor-body" class:has-error={!!jsonError}>
        <pre class="highlight" bind:this={codeEl}><code>{@html highlighted}</code>{'\n'}</pre>
        <textarea
          class="input-overlay"
          bind:this={textareaEl}
          bind:value={content}
          oninput={onInput}
          onscroll={syncScroll}
          onkeydown={onKeydown}
          spellcheck="false"
        ></textarea>
      </div>
      {#if jsonError}
        <div class="error-bar">{jsonError}</div>
      {/if}
    {/if}
  </div>

  <div class="example-section">
    <button class="example-toggle" onclick={() => showExample = !showExample}>
      <span class="chevron" class:open={showExample}>&#9656;</span>
      Example configuration
    </button>
    {#if showExample}
      <pre class="example-code highlight"><code>{@html exampleHighlighted}</code></pre>
    {/if}
  </div>
</div>

<style>
  .config-wrapper {
    display: flex;
    flex-direction: column;
    gap: 12px;
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

  .badge {
    font-size: 10px;
    padding: 1px 6px;
    border-radius: 8px;
    background: rgba(88, 166, 255, 0.15);
    color: var(--accent);
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
    font-family: var(--font-mono);
    font-size: 13px;
    line-height: 1.6;
    tab-size: 2;
    white-space: pre-wrap;
    word-break: break-word;
    overflow: auto;
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

  /* Example section */
  .example-section {
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    overflow: hidden;
  }

  .example-toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    width: 100%;
    padding: 8px 12px;
    background: var(--bg-secondary);
    border: none;
    color: var(--text-muted);
    font-size: 12px;
    text-align: left;
    cursor: pointer;
  }
  .example-toggle:hover { color: var(--text); }

  .chevron {
    font-size: 10px;
    transition: transform 0.15s;
    display: inline-block;
  }
  .chevron.open { transform: rotate(90deg); }

  .example-code {
    margin: 0;
    padding: 12px;
    background: var(--bg);
    border-top: 1px solid var(--border);
    font-size: 13px;
    line-height: 1.6;
    position: relative;
    overflow-x: auto;
    color: var(--text);
    opacity: 0.7;
  }
</style>
