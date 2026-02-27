<script lang="ts">
  import { onMount } from 'svelte'
  import { getInstance, startInstance, disableInstance, deleteInstance, type Instance } from '../lib/api'
  import { addToast } from '../lib/store.svelte'
  import LogViewer from '../components/LogViewer.svelte'
  import CommandRunner from '../components/CommandRunner.svelte'
  import ChatPanel from '../components/ChatPanel.svelte'

  interface Props {
    id: string
  }

  let { id }: Props = $props()
  let instance: Instance | null = $state(null)
  let error: string | null = $state(null)
  let tab: 'info' | 'logs' | 'exec' | 'chat' = $state('info')
  let pollTimer: ReturnType<typeof setInterval>

  let canExec = $derived(instance?.enabled !== false)

  async function load() {
    try {
      instance = await getInstance(id)
      error = null
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to load instance'
    }
  }

  async function doAction(action: string) {
    if (!instance) return
    const ref = instance.handle || instance.id
    try {
      switch (action) {
        case 'enable': await startInstance(ref); break
        case 'disable': await disableInstance(ref); break
        case 'delete':
          if (!confirm(`Delete instance "${ref}"?`)) return
          await deleteInstance(ref)
          window.location.hash = '#/'
          break
      }
      addToast(`${action}: ${ref}`, 'success')
      setTimeout(load, 500)
    } catch (e) {
      addToast(`${action} failed: ${e instanceof Error ? e.message : 'unknown'}`, 'error')
    }
  }

  function uptime(inst: Instance): string {
    if (inst.state === 'stopped') return ''
    const created = new Date(inst.created_at).getTime()
    const secs = Math.floor((Date.now() - created) / 1000)
    if (secs < 60) return `${secs}s`
    const mins = Math.floor(secs / 60)
    if (mins < 60) return `${mins}m`
    const hours = Math.floor(mins / 60)
    if (hours < 24) return `${hours}h ${mins % 60}m`
    const days = Math.floor(hours / 24)
    return `${days}d ${hours % 24}h`
  }

  onMount(() => {
    load()
    pollTimer = setInterval(load, 5000)
    return () => clearInterval(pollTimer)
  })
</script>

<div class="detail">
  <div class="breadcrumb">
    <a href="#/">Instances</a>
    <span class="sep">/</span>
    <span>{id}</span>
  </div>

  {#if error}
    <div class="error-msg">{error}</div>
  {:else if !instance}
    <div class="loading">Loading...</div>
  {:else}
    <div class="header">
      <div class="header-left">
        <h1>{instance.handle || instance.id}</h1>
        <span class="state-badge {instance.enabled ? instance.state : 'disabled'}">{instance.enabled ? instance.state : 'disabled'}</span>
        {#if instance.state !== 'stopped'}
          <span class="uptime">{uptime(instance)}</span>
        {/if}
      </div>
      <div class="header-actions">
        {#if instance.enabled}
          <button class="btn btn-disable" onclick={() => doAction('disable')}>Disable</button>
        {:else}
          <button class="btn btn-enable" onclick={() => doAction('enable')}>Enable</button>
        {/if}
        <button class="btn-icon btn-delete" title="Delete" onclick={() => doAction('delete')}>&#x2715;</button>
      </div>
    </div>

    <div class="tabs">
      <button class="tab" class:active={tab === 'info'} onclick={() => tab = 'info'}>Info</button>
      <button class="tab" class:active={tab === 'logs'} onclick={() => tab = 'logs'}>Logs</button>
      <button class="tab" class:active={tab === 'exec'} onclick={() => tab = 'exec'}>Exec</button>
      <button class="tab" class:active={tab === 'chat'} onclick={() => tab = 'chat'}>Chat</button>
    </div>

    <div class="tab-content">
      {#if tab === 'info'}
        <div class="info-grid">
          <div class="info-item">
            <span class="field-label">ID</span>
            <code>{instance.id}</code>
          </div>
          {#if instance.handle}
            <div class="info-item">
              <span class="field-label">Handle</span>
              <code>{instance.handle}</code>
            </div>
          {/if}
          {#if instance.kit}
            <div class="info-item">
              <span class="field-label">Kit</span>
              <span>{instance.kit}</span>
            </div>
          {/if}
          {#if instance.image_ref}
            <div class="info-item">
              <span class="field-label">Image</span>
              <code>{instance.image_ref}</code>
            </div>
          {/if}
          <div class="info-item">
            <span class="field-label">Command</span>
            <code>{instance.command.join(' ')}</code>
          </div>
          <div class="info-item">
            <span class="field-label">Created</span>
            <span>{new Date(instance.created_at).toLocaleString()}</span>
          </div>
          {#if instance.workspace}
            <div class="info-item full-width">
              <span class="field-label">Workspace</span>
              <code>{instance.workspace}</code>
            </div>
          {/if}
          {#if instance.endpoints && instance.endpoints.length > 0}
            <div class="info-item full-width">
              <span class="field-label">Endpoints</span>
              <div class="endpoints">
                {#each instance.endpoints as ep}
                  <a href="http://127.0.0.1:{ep.public_port}" target="_blank" class="endpoint-link">
                    http://127.0.0.1:{ep.public_port}
                    <span class="endpoint-detail">→ :{ep.guest_port} ({ep.protocol})</span>
                  </a>
                {/each}
              </div>
            </div>
          {/if}
        </div>
      {:else if tab === 'logs'}
        <LogViewer instanceId={instance.handle || instance.id} />
      {:else if tab === 'exec'}
        <CommandRunner instanceId={instance.handle || instance.id} disabled={!canExec} />
      {:else if tab === 'chat'}
        <ChatPanel instanceId={instance.handle || instance.id} disabled={!canExec} />
      {/if}
    </div>
  {/if}
</div>

<style>
  .detail {
    max-width: 960px;
    margin: 0 auto;
  }

  .breadcrumb {
    font-size: 13px;
    color: var(--text-muted);
    margin-bottom: 16px;
  }
  .breadcrumb a { color: var(--accent); }
  .sep { margin: 0 6px; }

  .header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 16px;
  }

  .header-left {
    display: flex;
    align-items: center;
    gap: 10px;
  }

  h1 {
    font-size: 20px;
    font-weight: 600;
    font-family: var(--font-mono);
  }

  .uptime {
    font-size: 12px;
    color: var(--text-muted);
    font-family: var(--font-mono);
  }

  .state-badge {
    padding: 2px 10px;
    border-radius: 10px;
    font-size: 12px;
    font-weight: 500;
  }
  .state-badge.running { background: rgba(63, 185, 80, 0.15); color: var(--green); }
  .state-badge.paused { background: rgba(210, 153, 34, 0.15); color: var(--yellow); }
  .state-badge.stopped { background: var(--bg-tertiary); color: var(--text-muted); }
  .state-badge.starting { background: rgba(88, 166, 255, 0.15); color: var(--accent); }
  .state-badge.disabled { background: rgba(248, 81, 73, 0.15); color: var(--red); }

  .header-actions {
    display: flex;
    gap: 6px;
  }

  .btn {
    padding: 5px 14px;
    border-radius: var(--radius);
    border: 1px solid var(--border);
    background: var(--bg-tertiary);
    color: var(--text);
    font-size: 13px;
  }
  .btn:hover { background: var(--bg); border-color: var(--text-muted); }
  .btn-enable { color: var(--green); border-color: rgba(63, 185, 80, 0.3); }
  .btn-enable:hover { background: rgba(63, 185, 80, 0.1); border-color: var(--green); }
  .btn-disable { color: var(--orange); border-color: rgba(209, 134, 22, 0.3); }
  .btn-disable:hover { background: rgba(209, 134, 22, 0.1); border-color: var(--orange); }
  .btn-icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 28px;
    height: 28px;
    padding: 0;
    border-radius: var(--radius);
    border: 1px solid transparent;
    background: transparent;
    color: var(--text-muted);
    font-size: 14px;
    cursor: pointer;
  }
  .btn-icon:hover { background: var(--bg-tertiary); color: var(--text); border-color: var(--border); }
  .btn-delete:hover { color: var(--red); background: rgba(248, 81, 73, 0.1); border-color: rgba(248, 81, 73, 0.3); }

  .tabs {
    display: flex;
    gap: 0;
    border-bottom: 1px solid var(--border);
    margin-bottom: 16px;
  }

  .tab {
    padding: 8px 16px;
    border: none;
    background: none;
    color: var(--text-muted);
    font-size: 13px;
    font-weight: 500;
    border-bottom: 2px solid transparent;
    margin-bottom: -1px;
    transition: color 0.1s, border-color 0.1s;
  }
  .tab:hover { color: var(--text); }
  .tab.active {
    color: var(--text);
    border-bottom-color: var(--accent);
  }

  .info-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 12px;
    padding: 16px;
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
  }

  .info-item {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .info-item.full-width { grid-column: 1 / -1; }
  .field-label {
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
    font-weight: 600;
  }
  .info-item code {
    color: var(--text);
    word-break: break-all;
  }

  .endpoints {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .endpoint-link {
    font-family: var(--font-mono);
    font-size: 13px;
  }
  .endpoint-detail {
    color: var(--text-muted);
    font-size: 11px;
    margin-left: 6px;
  }

  .error-msg {
    color: var(--red);
    padding: 20px;
    text-align: center;
  }
  .loading {
    color: var(--text-muted);
    padding: 40px;
    text-align: center;
  }
</style>
