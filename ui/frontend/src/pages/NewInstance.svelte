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

  let activeKit = $derived(kits.find(k => k.name === selectedKit))

  function onKitChange() {
    const kit = kits.find(k => k.name === selectedKit)
    if (kit) {
      image = kit.image || ''
      command = kit.defaults?.command?.join(' ') || ''
      // Auto-select required secrets
      if (kit.required_secrets) {
        for (const group of kit.required_secrets) {
          for (const s of group) {
            if (secrets.some(sec => sec.name === s)) {
              selectedSecrets[s] = true
            }
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

  function isRequired(secretName: string): boolean {
    if (!activeKit?.required_secrets) return false
    return activeKit.required_secrets.some(group => group.includes(secretName))
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

    {#if secrets.length > 0}
      <div class="field">
        <span class="field-label">Secrets</span>
        <div class="secret-list">
          {#each secrets as secret}
            <label class="secret-item" class:required={isRequired(secret.name)}>
              <input type="checkbox" bind:checked={selectedSecrets[secret.name]} />
              <span>{secret.name}</span>
              {#if isRequired(secret.name)}
                <span class="required-badge">required</span>
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

  select {
    font-family: inherit;
  }

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
  .secret-item.required { color: var(--accent); }
  .secret-item input[type="checkbox"] { cursor: pointer; }

  .required-badge {
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
