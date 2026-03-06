<script lang="ts">
  import { onMount } from 'svelte'
  import { listKits, listSecrets, createInstance, exposePort, type Kit, type SecretInfo } from '../lib/api'
  import { addToast } from '../lib/store.svelte'

  let kits: Kit[] = $state([])
  let secrets: SecretInfo[] = $state([])
  let creating = $state(false)

  // Form fields
  let name = $state('')
  let selectedKit = $state('')
  let image = $state('')
  let command = $state('')
  let memory = $state(512)
  let workspace = $state('')
  let selectedSecrets: Record<string, boolean> = $state({})
  let ports: string[] = $state([''])
  let kitDropdownOpen = $state(false)
  let imageDropdownOpen = $state(false)

  const popularImages = [
    { ref: 'alpine:3.21', desc: 'Minimal Alpine Linux', size: '~4 MB', mem: 256 },
    { ref: 'python:3.12-alpine', desc: 'Python 3.12', size: '~17 MB', mem: 512 },
    { ref: 'python:3.13-alpine', desc: 'Python 3.13', size: '~18 MB', mem: 512 },
    { ref: 'node:22-alpine', desc: 'Node.js 22 LTS', size: '~42 MB', mem: 512 },
    { ref: 'node:23-alpine', desc: 'Node.js 23', size: '~44 MB', mem: 512 },
    { ref: 'golang:1.23-alpine', desc: 'Go 1.23', size: '~85 MB', mem: 1024 },
    { ref: 'ruby:3.3-alpine', desc: 'Ruby 3.3', size: '~28 MB', mem: 512 },
    { ref: 'rust:1.82-alpine', desc: 'Rust 1.82', size: '~95 MB', mem: 1024 },
    { ref: 'bun:1-alpine', desc: 'Bun runtime', size: '~36 MB', mem: 512 },
    { ref: 'denoland/deno:alpine', desc: 'Deno runtime', size: '~42 MB', mem: 512 },
  ]

  let filteredImages = $derived(
    image.trim()
      ? popularImages.filter(img =>
          img.ref.toLowerCase().includes(image.trim().toLowerCase()) ||
          img.desc.toLowerCase().includes(image.trim().toLowerCase())
        )
      : popularImages
  )

  let activeKit = $derived(kits.find(k => k.name === selectedKit))

  const handlePattern = /^[a-zA-Z0-9][a-zA-Z0-9._-]*$/
  let handleError = $derived(
    name.trim() && !handlePattern.test(name.trim())
      ? 'Must start with a letter or digit, then letters, digits, dots, dashes, or underscores only'
      : null
  )

  function isReferenced(secretName: string): boolean {
    return activeKit?.referenced_env?.includes(secretName) ?? false
  }

  function onKitChange() {
    const kit = kits.find(k => k.name === selectedKit)
    if (kit) {
      image = kit.image || ''
      command = kit.defaults?.command?.join(' ') || ''
      // Auto-select secrets referenced by kit configs
      if (kit.referenced_env) {
        for (const key of kit.referenced_env) {
          if (secrets.some(s => s.name === key)) {
            selectedSecrets[key] = true
          }
        }
        selectedSecrets = { ...selectedSecrets }
      }
    } else {
      image = ''
      command = ''
    }
  }

  function addPort() {
    ports = [...ports, '']
  }

  function removePort(idx: number) {
    ports = ports.filter((_, i) => i !== idx)
  }

  async function submit() {
    const cmd = command.trim()
    if (!cmd) {
      addToast('Command is required', 'error')
      return
    }

    creating = true
    try {
      const secretList = Object.entries(selectedSecrets)
        .filter(([, v]) => v)
        .map(([k]) => k)

      const result = await createInstance({
        command: cmd.split(/\s+/),
        handle: name.trim() || undefined,
        kit: selectedKit || undefined,
        image_ref: image.trim() || undefined,
        workspace: workspace.trim() || undefined,
        memory_mb: memory || undefined,
        secrets: secretList.length > 0 ? secretList : undefined,
      })

      // Expose ports
      const ref = result.handle || result.id
      for (const p of ports) {
        const port = parseInt(p.trim(), 10)
        if (port > 0) {
          try {
            await exposePort(ref, port)
          } catch (e) {
            addToast(`Port ${port}: ${e instanceof Error ? e.message : 'failed'}`, 'error')
          }
        }
      }

      addToast(`Created ${ref}`, 'success')
      window.location.hash = `#/instance/${encodeURIComponent(ref)}`
    } catch (e) {
      addToast(`Create failed: ${e instanceof Error ? e.message : 'unknown'}`, 'error')
    } finally {
      creating = false
    }
  }

  function handleClickOutside(e: MouseEvent) {
    const target = e.target as HTMLElement
    if (!target.closest('.custom-select')) kitDropdownOpen = false
    if (!target.closest('.image-field')) imageDropdownOpen = false
  }

  onMount(() => {
    listKits().then(k => kits = k).catch(() => {})
    listSecrets().then(s => secrets = s).catch(() => {})
    document.addEventListener('click', handleClickOutside)
    return () => document.removeEventListener('click', handleClickOutside)
  })
</script>

<div class="page">
  <div class="breadcrumb">
    <a href="#/">Dashboard</a>
    <span class="sep">/</span>
    <span>New Instance</span>
  </div>

  <h1>New Instance</h1>

  <div class="form">
    <div class="field">
      <label for="name">Name</label>
      <input id="name" type="text" bind:value={name} placeholder="my-instance (optional)" class:input-error={!!handleError} autocapitalize="off" autocomplete="off" autocorrect="off" spellcheck="false" />
      {#if handleError}
        <span class="field-error">{handleError}</span>
      {/if}
    </div>

    <div class="field">
      <label>Kit</label>
      <div class="custom-select">
        <button class="custom-select-trigger" onclick={() => kitDropdownOpen = !kitDropdownOpen} type="button">
          <span>{selectedKit || 'None'}</span>
          <svg width="12" height="12" viewBox="0 0 12 12" fill="currentColor"><path d="M2.5 4.5L6 8l3.5-3.5z"/></svg>
        </button>
        {#if kitDropdownOpen}
          <div class="custom-select-menu">
            <button class="custom-select-option" class:selected={selectedKit === ''} onclick={() => { selectedKit = ''; kitDropdownOpen = false; onKitChange() }} type="button">None</button>
            {#each kits as kit}
              <button class="custom-select-option" class:selected={selectedKit === kit.name} onclick={() => { selectedKit = kit.name; kitDropdownOpen = false; onKitChange() }} type="button">
                {kit.name}{kit.description ? ` — ${kit.description}` : ''}
              </button>
            {/each}
          </div>
        {/if}
      </div>
    </div>

    <div class="field">
      <label for="image">Image</label>
      <div class="image-field">
        <input id="image" type="text" bind:value={image} placeholder="python:3.12-alpine" autocapitalize="off" autocomplete="off" autocorrect="off" spellcheck="false"
          onfocus={() => imageDropdownOpen = true} />
        <button class="image-dropdown-toggle" onclick={() => imageDropdownOpen = !imageDropdownOpen} type="button">
          <svg width="12" height="12" viewBox="0 0 12 12" fill="currentColor"><path d="M2.5 4.5L6 8l3.5-3.5z"/></svg>
        </button>
        {#if imageDropdownOpen && filteredImages.length > 0}
          <div class="image-dropdown">
            {#each filteredImages as img}
              <button class="image-option" onclick={() => { image = img.ref; memory = img.mem; imageDropdownOpen = false }} type="button">
                <span class="image-option-ref">{img.ref}</span>
                <span class="image-option-desc">{img.desc}</span>
                <span class="image-option-size">{img.size}</span>
              </button>
            {/each}
          </div>
        {/if}
      </div>
    </div>

    <div class="field">
      <label for="command">Command</label>
      <input id="command" type="text" bind:value={command} placeholder="echo hello" disabled={!!activeKit?.defaults?.command} autocapitalize="off" autocomplete="off" autocorrect="off" spellcheck="false" />
      {#if activeKit?.defaults?.command}
        <span class="hint">Set by kit</span>
      {/if}
    </div>

    <div class="field-row">
      <div class="field">
        <label for="memory">Memory (MB)</label>
        <input id="memory" type="number" bind:value={memory} min="128" step="128" />
      </div>
      <div class="field">
        <label for="workspace">Workspace</label>
        <input id="workspace" type="text" bind:value={workspace} placeholder="Auto-created if empty" autocapitalize="off" autocomplete="off" autocorrect="off" spellcheck="false" />
      </div>
    </div>

    {#if secrets.length > 0}
      <div class="field">
        <span class="field-label">Secrets</span>
        <div class="secret-list">
          {#each secrets as secret}
            <label class="secret-item" class:referenced={isReferenced(secret.name)}>
              <input type="checkbox" bind:checked={selectedSecrets[secret.name]} />
              <span>{secret.name}</span>
              {#if isReferenced(secret.name)}
                <span class="referenced-badge">referenced by kit</span>
              {/if}
            </label>
          {/each}
        </div>
      </div>
    {/if}

    <div class="field">
      <span class="field-label">Expose Ports <span class="optional">(optional)</span></span>
      {#each ports as port, idx}
        <div class="port-row">
          <input type="text" bind:value={ports[idx]} placeholder="8080" />
          {#if ports.length > 1}
            <button class="btn-remove" onclick={() => removePort(idx)}>&times;</button>
          {/if}
        </div>
      {/each}
      <button class="btn-add" onclick={addPort}>+ Add port</button>
    </div>

    <div class="actions">
      <a href="#/" class="btn btn-cancel">Cancel</a>
      <button class="btn btn-create" onclick={submit} disabled={creating || !command.trim() || !!handleError}>
        {creating ? 'Creating...' : 'Create'}
      </button>
    </div>
  </div>
</div>

<style>
  .page {
    max-width: 640px;
    margin: 0 auto;
  }

  .breadcrumb {
    font-size: 13px;
    color: var(--text-muted);
    margin-bottom: 16px;
  }
  .breadcrumb a { color: var(--accent); }
  .sep { margin: 0 6px; }

  h1 {
    font-size: 20px;
    font-weight: 600;
    margin-bottom: 20px;
  }

  .form {
    display: flex;
    flex-direction: column;
    gap: 16px;
    padding: 20px;
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .field-row {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 12px;
  }

  label, .field-label {
    font-size: 12px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
  }
  .optional {
    font-weight: 400;
    text-transform: none;
    letter-spacing: normal;
  }

  input[type="text"], input[type="number"], select {
    padding: 7px 10px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text);
    font-size: 13px;
    font-family: var(--font-mono);
    outline: none;
  }
  input:focus, select:focus { border-color: var(--accent); }
  input:disabled { opacity: 0.5; }
  input.input-error { border-color: var(--red); }
  input.input-error:focus { border-color: var(--red); }
  .field-error { font-size: 11px; color: var(--red); }

  select { font-family: inherit; }

  .hint {
    font-size: 11px;
    color: var(--text-muted);
  }

  .secret-list {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 8px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }

  .secret-item {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
    font-weight: normal;
    text-transform: none;
    letter-spacing: normal;
    color: var(--text);
    cursor: pointer;
  }
  .secret-item.referenced { color: var(--accent); }
  .secret-item input[type="checkbox"] { cursor: pointer; }

  .referenced-badge {
    font-size: 10px;
    padding: 1px 6px;
    border-radius: 8px;
    background: rgba(88, 166, 255, 0.15);
    color: var(--accent);
  }

  .port-row {
    display: flex;
    gap: 6px;
    margin-bottom: 4px;
  }
  .port-row input { flex: 1; }

  .btn-remove {
    width: 28px;
    height: 28px;
    border-radius: var(--radius);
    border: 1px solid var(--border);
    background: transparent;
    color: var(--text-muted);
    font-size: 16px;
    display: flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
  }
  .btn-remove:hover { color: var(--red); border-color: var(--red); }

  .btn-add {
    align-self: flex-start;
    padding: 2px 10px;
    border: none;
    background: none;
    color: var(--accent);
    font-size: 12px;
  }
  .btn-add:hover { text-decoration: underline; }

  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    margin-top: 8px;
  }

  .btn {
    padding: 7px 18px;
    border-radius: var(--radius);
    border: 1px solid var(--border);
    font-size: 13px;
    font-weight: 500;
    text-decoration: none;
  }
  .btn-cancel {
    background: transparent;
    color: var(--text-muted);
  }
  .btn-cancel:hover { color: var(--text); text-decoration: none; }
  .btn-create {
    background: var(--accent);
    border-color: var(--accent);
    color: #fff;
    cursor: pointer;
  }
  .btn-create:hover:not(:disabled) { background: var(--accent-hover); }
  .btn-create:disabled { opacity: 0.5; cursor: not-allowed; }

  /* Custom select (kit dropdown) */
  .custom-select { position: relative; }
  .custom-select-trigger {
    width: 100%;
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 7px 10px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text);
    font-size: 13px;
    font-family: var(--font-mono);
    cursor: pointer;
    text-align: left;
  }
  .custom-select-trigger:hover { border-color: var(--accent); }
  .custom-select-trigger svg { color: var(--text-muted); flex-shrink: 0; }
  .custom-select-menu {
    position: absolute;
    top: calc(100% + 4px);
    left: 0; right: 0;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    z-index: 20;
    overflow: hidden;
    box-shadow: 0 8px 24px rgba(0,0,0,0.4);
  }
  .custom-select-option {
    display: block;
    width: 100%;
    padding: 7px 10px;
    background: none;
    border: none;
    color: var(--text);
    font-size: 13px;
    font-family: var(--font-mono);
    text-align: left;
    cursor: pointer;
  }
  .custom-select-option:hover { background: rgba(255,255,255,0.05); }
  .custom-select-option.selected { color: var(--accent); }

  /* Image field with dropdown */
  .image-field { position: relative; display: flex; }
  .image-field input { flex: 1; border-top-right-radius: 0; border-bottom-right-radius: 0; }
  .image-dropdown-toggle {
    padding: 0 8px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-left: none;
    border-radius: 0 var(--radius) var(--radius) 0;
    color: var(--text-muted);
    cursor: pointer;
    display: flex;
    align-items: center;
  }
  .image-dropdown-toggle:hover { color: var(--text); }
  .image-dropdown {
    position: absolute;
    top: calc(100% + 4px);
    left: 0; right: 0;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    z-index: 20;
    overflow: hidden;
    box-shadow: 0 8px 24px rgba(0,0,0,0.4);
    max-height: 280px;
    overflow-y: auto;
  }
  .image-option {
    display: flex;
    width: 100%;
    padding: 7px 10px;
    background: none;
    border: none;
    color: var(--text);
    font-size: 13px;
    text-align: left;
    cursor: pointer;
    gap: 8px;
    align-items: baseline;
  }
  .image-option:hover { background: rgba(255,255,255,0.05); }
  .image-option-ref { font-family: var(--font-mono); color: var(--text); }
  .image-option-desc { font-size: 11px; color: var(--text-muted); flex: 1; }
  .image-option-size { font-size: 11px; color: var(--text-muted); opacity: 0.6; white-space: nowrap; }
</style>
