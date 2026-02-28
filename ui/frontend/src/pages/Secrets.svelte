<script lang="ts">
  import { onMount } from 'svelte'
  import { listSecrets, setSecret, deleteSecret, type SecretInfo } from '../lib/api'
  import { addToast, showConfirm } from '../lib/store.svelte'

  let secrets: SecretInfo[] = $state([])
  let loading = $state(true)
  let showAdd = $state(false)
  let newName = $state('')
  let newValue = $state('')
  let saving = $state(false)

  async function load() {
    try {
      secrets = await listSecrets()
    } catch (e) {
      addToast(`Failed to load secrets: ${e instanceof Error ? e.message : 'unknown'}`, 'error')
    } finally {
      loading = false
    }
  }

  async function add() {
    const name = newName.trim()
    const value = newValue.trim()
    if (!name || !value) return

    saving = true
    try {
      await setSecret(name, value)
      addToast(`Secret "${name}" saved`, 'success')
      newName = ''
      newValue = ''
      showAdd = false
      await load()
    } catch (e) {
      addToast(`Save failed: ${e instanceof Error ? e.message : 'unknown'}`, 'error')
    } finally {
      saving = false
    }
  }

  async function remove(name: string) {
    if (!await showConfirm(`Delete secret "${name}"?`)) return
    try {
      await deleteSecret(name)
      addToast(`Deleted "${name}"`, 'success')
      await load()
    } catch (e) {
      addToast(`Delete failed: ${e instanceof Error ? e.message : 'unknown'}`, 'error')
    }
  }

  function onKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') add()
    if (e.key === 'Escape') { showAdd = false; newName = ''; newValue = '' }
  }

  onMount(() => { load() })
</script>

<div class="page">
  <div class="page-header">
    <h1>Secrets</h1>
    {#if !showAdd}
      <button class="btn-add" onclick={() => showAdd = true}>+ Add</button>
    {/if}
  </div>

  {#if showAdd}
    <div class="add-form">
      <input
        type="text"
        bind:value={newName}
        placeholder="SECRET_NAME"
        onkeydown={onKeydown}
      />
      <input
        type="password"
        bind:value={newValue}
        placeholder="Value"
        onkeydown={onKeydown}
      />
      <button class="btn btn-save" onclick={add} disabled={saving || !newName.trim() || !newValue.trim()}>
        {saving ? 'Saving...' : 'Save'}
      </button>
      <button class="btn btn-cancel" onclick={() => { showAdd = false; newName = ''; newValue = '' }}>Cancel</button>
    </div>
  {/if}

  {#if loading}
    <div class="empty">Loading...</div>
  {:else if secrets.length === 0}
    <div class="empty">No secrets yet. Add one to get started.</div>
  {:else}
    <div class="secret-table">
      {#each secrets as secret}
        <div class="secret-row">
          <code class="secret-name">{secret.name}</code>
          <span class="secret-date">{new Date(secret.created_at).toLocaleDateString()}</span>
          <button class="btn-delete" onclick={() => remove(secret.name)}>&times;</button>
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .page {
    max-width: 640px;
    margin: 0 auto;
  }

  .page-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 16px;
  }

  h1 {
    font-size: 20px;
    font-weight: 600;
  }

  .btn-add {
    padding: 5px 14px;
    border-radius: var(--radius);
    border: 1px solid var(--accent);
    background: transparent;
    color: var(--accent);
    font-size: 13px;
    font-weight: 500;
  }
  .btn-add:hover { background: rgba(88, 166, 255, 0.1); }

  .add-form {
    display: flex;
    gap: 8px;
    margin-bottom: 16px;
    padding: 12px;
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
  }

  .add-form input {
    flex: 1;
    padding: 7px 10px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text);
    font-size: 13px;
    font-family: var(--font-mono);
    outline: none;
  }
  .add-form input:focus { border-color: var(--accent); }

  .btn {
    padding: 7px 14px;
    border-radius: var(--radius);
    border: 1px solid var(--border);
    font-size: 13px;
    white-space: nowrap;
  }
  .btn-save {
    background: var(--accent);
    border-color: var(--accent);
    color: #fff;
  }
  .btn-save:hover:not(:disabled) { background: var(--accent-hover); }
  .btn-save:disabled { opacity: 0.5; cursor: not-allowed; }
  .btn-cancel {
    background: transparent;
    color: var(--text-muted);
  }
  .btn-cancel:hover { color: var(--text); }

  .secret-table {
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    overflow: hidden;
  }

  .secret-row {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 10px 16px;
    border-bottom: 1px solid var(--border);
  }
  .secret-row:last-child { border-bottom: none; }

  .secret-name {
    flex: 1;
    font-size: 13px;
    color: var(--text);
  }

  .secret-date {
    font-size: 12px;
    color: var(--text-muted);
    flex-shrink: 0;
  }

  .btn-delete {
    width: 26px;
    height: 26px;
    border-radius: var(--radius);
    border: 1px solid transparent;
    background: transparent;
    color: var(--text-muted);
    font-size: 16px;
    display: flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
  }
  .btn-delete:hover { color: var(--red); background: rgba(248, 81, 73, 0.1); border-color: rgba(248, 81, 73, 0.3); }

  .empty {
    color: var(--text-muted);
    text-align: center;
    padding: 40px;
  }
</style>
