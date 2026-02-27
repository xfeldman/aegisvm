<script lang="ts">
  import type { Instance } from '../lib/api'
  import { disableInstance, deleteInstance } from '../lib/api'
  import { addToast, refreshInstances } from '../lib/store.svelte'

  interface Props {
    instances: Instance[]
  }

  let { instances }: Props = $props()

  function stateColor(inst: Instance): string {
    if (!inst.enabled) return 'var(--red)'
    switch (inst.state) {
      case 'running': return 'var(--green)'
      case 'paused': return 'var(--yellow)'
      case 'starting': return 'var(--accent)'
      case 'stopped': return 'var(--text-muted)'
      default: return 'var(--text-muted)'
    }
  }

  function stateLabel(inst: Instance): string {
    if (!inst.enabled) return 'disabled'
    return inst.state
  }

  function displayName(inst: Instance): string {
    return inst.handle || inst.id
  }

  function uptime(inst: Instance): string {
    if (inst.state === 'stopped') return ''
    const created = new Date(inst.created_at).getTime()
    const now = Date.now()
    const secs = Math.floor((now - created) / 1000)
    if (secs < 60) return `${secs}s`
    const mins = Math.floor(secs / 60)
    if (mins < 60) return `${mins}m`
    const hours = Math.floor(mins / 60)
    if (hours < 24) return `${hours}h ${mins % 60}m`
    const days = Math.floor(hours / 24)
    return `${days}d ${hours % 24}h`
  }

  async function doAction(action: string, inst: Instance) {
    const name = displayName(inst)
    try {
      const ref = inst.handle || inst.id
      switch (action) {
        case 'disable': await disableInstance(ref); break
        case 'delete':
          if (!confirm(`Delete instance "${name}"?`)) return
          await deleteInstance(ref)
          break
      }
      addToast(`${action}: ${name}`, 'success')
      // Small delay to let state propagate
      setTimeout(() => refreshInstances(), 500)
    } catch (e) {
      addToast(`${action} failed: ${e instanceof Error ? e.message : 'unknown error'}`, 'error')
    }
  }
</script>

<div class="instance-list">
  <div class="header-row">
    <div class="col-status"></div>
    <div class="col-name">Name</div>
    <div class="col-kit">Kit</div>
    <div class="col-state">State</div>
    <div class="col-ports">Ports</div>
    <div class="col-uptime">Uptime</div>
    <div class="col-actions">Actions</div>
  </div>

  {#each instances as inst (inst.id)}
    <div class="instance-row">
      <div class="col-status">
        <span class="status-dot" style="background: {stateColor(inst)}"></span>
      </div>
      <div class="col-name">
        <a href="#/instance/{inst.handle || inst.id}" class="instance-name">{displayName(inst)}</a>
        {#if inst.image_ref}
          <span class="image-ref">{inst.image_ref}</span>
        {/if}
      </div>
      <div class="col-kit">
        {#if inst.kit}
          <span class="kit-badge">{inst.kit}</span>
        {/if}
      </div>
      <div class="col-state">
        <span class="state" style="color: {stateColor(inst)}">{stateLabel(inst)}</span>
      </div>
      <div class="col-ports">
        {#if inst.endpoints && inst.endpoints.length > 0}
          {#each inst.endpoints as ep}
            <a href="http://127.0.0.1:{ep.public_port}" target="_blank" class="port-link">
              :{ep.public_port}
            </a>
          {/each}
        {/if}
      </div>
      <div class="col-uptime">
        <span class="uptime">{uptime(inst)}</span>
      </div>
      <div class="col-actions">
        {#if !inst.enabled}
          <button class="btn btn-sm btn-danger" onclick={() => doAction('delete', inst)}>Delete</button>
        {:else if inst.state === 'starting'}
          <span class="text-muted">starting...</span>
        {:else}
          <button class="btn btn-sm" onclick={() => doAction('disable', inst)}>Disable</button>
          <button class="btn btn-sm btn-danger" onclick={() => doAction('delete', inst)}>Delete</button>
        {/if}
      </div>
    </div>
  {/each}
</div>

<style>
  .instance-list {
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    overflow: hidden;
  }

  .header-row {
    display: grid;
    grid-template-columns: 24px 1fr 80px 80px 120px 80px 160px;
    gap: 8px;
    padding: 8px 16px;
    background: var(--bg-tertiary);
    font-size: 12px;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    font-weight: 600;
  }

  .instance-row {
    display: grid;
    grid-template-columns: 24px 1fr 80px 80px 120px 80px 160px;
    gap: 8px;
    padding: 10px 16px;
    align-items: center;
    border-top: 1px solid var(--border);
    transition: background 0.1s;
  }
  .instance-row:hover {
    background: var(--bg-secondary);
  }

  .status-dot {
    display: inline-block;
    width: 8px;
    height: 8px;
    border-radius: 50%;
  }

  .instance-name {
    font-weight: 500;
    font-family: var(--font-mono);
    font-size: 13px;
  }

  .image-ref {
    display: block;
    font-size: 11px;
    color: var(--text-muted);
    font-family: var(--font-mono);
  }

  .kit-badge {
    display: inline-block;
    padding: 1px 6px;
    border-radius: 10px;
    background: var(--bg-tertiary);
    border: 1px solid var(--border);
    font-size: 11px;
    color: var(--text-muted);
  }

  .state {
    font-size: 12px;
    font-weight: 500;
  }

  .port-link {
    font-family: var(--font-mono);
    font-size: 12px;
    margin-right: 4px;
  }

  .uptime {
    font-family: var(--font-mono);
    font-size: 12px;
    color: var(--text-muted);
  }

  .text-muted {
    color: var(--text-muted);
    font-size: 12px;
  }

  .btn {
    padding: 3px 10px;
    border-radius: var(--radius);
    border: 1px solid var(--border);
    background: var(--bg-tertiary);
    color: var(--text);
    font-size: 12px;
    transition: background 0.1s, border-color 0.1s;
  }
  .btn:hover {
    background: var(--bg);
    border-color: var(--text-muted);
  }
  .btn-sm {
    padding: 2px 8px;
  }
  .btn-danger {
    color: var(--red);
    border-color: rgba(248, 81, 73, 0.3);
  }
  .btn-danger:hover {
    background: rgba(248, 81, 73, 0.1);
    border-color: var(--red);
  }

  .col-actions {
    display: flex;
    gap: 4px;
  }
</style>
