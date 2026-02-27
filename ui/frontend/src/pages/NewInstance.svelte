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
  let envEntries: { key: string; value: string }[] = $state([])
  let ports: string[] = $state([''])

  let activeKit = $derived(kits.find(k => k.name === selectedKit))

  function onKitChange() {
    const kit = kits.find(k => k.name === selectedKit)
    if (kit) {
      image = kit.image || ''
      command = kit.defaults?.command?.join(' ') || ''
      // Pre-fill env entries from referenced_env
      if (kit.referenced_env) {
        const existing = new Set(envEntries.map(e => e.key))
        for (const key of kit.referenced_env) {
          if (!existing.has(key)) {
            envEntries = [...envEntries, { key, value: '' }]
          }
        }
      }
    } else {
      image = ''
      command = ''
    }
  }

  function addEnv() {
    envEntries = [...envEntries, { key: '', value: '' }]
  }

  function removeEnv(idx: number) {
    envEntries = envEntries.filter((_, i) => i !== idx)
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
      // Build secrets and env from entries
      // Bare key (no value) or value starting with "secret." → secrets list
      // KEY=value → explicit env
      const secretKeys: string[] = []
      const envVars: Record<string, string> = {}

      for (const entry of envEntries) {
        const key = entry.key.trim()
        const val = entry.value.trim()
        if (!key) continue

        if (!val) {
          // Bare key → secret lookup
          secretKeys.push(key)
        } else if (val.startsWith('secret.')) {
          // Mapped secret
          secretKeys.push(`${key}=${val}`)
        } else {
          // Literal value
          envVars[key] = val
        }
      }

      const result = await createInstance({
        command: cmd.split(/\s+/),
        handle: name.trim() || undefined,
        kit: selectedKit || undefined,
        image_ref: image.trim() || undefined,
        workspace: workspace.trim() || undefined,
        memory_mb: memory || undefined,
        secrets: secretKeys.length > 0 ? secretKeys : undefined,
        env: Object.keys(envVars).length > 0 ? envVars : undefined,
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

  onMount(() => {
    listKits().then(k => kits = k).catch(() => {})
    listSecrets().then(s => secrets = s).catch(() => {})
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
      <input id="name" type="text" bind:value={name} placeholder="my-instance (optional)" />
    </div>

    <div class="field">
      <label for="kit">Kit</label>
      <select id="kit" bind:value={selectedKit} onchange={onKitChange}>
        <option value="">None</option>
        {#each kits as kit}
          <option value={kit.name}>{kit.name}{kit.description ? ` — ${kit.description}` : ''}</option>
        {/each}
      </select>
    </div>

    <div class="field">
      <label for="image">Image</label>
      <input id="image" type="text" bind:value={image} placeholder="python:3.12-alpine" />
    </div>

    <div class="field">
      <label for="command">Command</label>
      <input id="command" type="text" bind:value={command} placeholder="echo hello" disabled={!!activeKit?.defaults?.command} />
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
        <input id="workspace" type="text" bind:value={workspace} placeholder="Auto-created if empty" />
      </div>
    </div>

    <div class="field">
      <span class="field-label">Environment</span>
      <span class="hint">Leave value empty to inject from secret store. Use <code>secret.name</code> for mapped secrets.</span>
      {#each envEntries as entry, idx}
        <div class="env-row">
          <input
            type="text"
            class="env-key"
            bind:value={envEntries[idx].key}
            placeholder="KEY"
            list="secret-names"
          />
          <span class="env-eq">=</span>
          <input type="text" class="env-val" bind:value={envEntries[idx].value} placeholder="(from secret store)" />
          <button class="btn-remove" onclick={() => removeEnv(idx)}>&times;</button>
        </div>
      {/each}
      <button class="btn-add" onclick={addEnv}>+ Add env</button>
      <datalist id="secret-names">
        {#each secrets as s}
          <option value={s.name}></option>
        {/each}
      </datalist>
    </div>

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
      <button class="btn btn-create" onclick={submit} disabled={creating || !command.trim()}>
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

  select { font-family: inherit; }

  .hint {
    font-size: 11px;
    color: var(--text-muted);
  }
  .hint code {
    background: var(--bg);
    padding: 1px 4px;
    border-radius: 3px;
    font-size: 11px;
  }

  .env-row {
    display: flex;
    align-items: center;
    gap: 4px;
    margin-bottom: 4px;
  }
  .env-key {
    flex: 2;
  }
  .env-eq {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 13px;
    user-select: none;
    flex-shrink: 0;
  }
  .env-val {
    flex: 3;
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
</style>
