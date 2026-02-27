<script lang="ts">
  import { onMount } from 'svelte'
  import { getInstance, type Instance } from '../lib/api'

  interface Props {
    id: string
  }

  let { id }: Props = $props()
  let instance: Instance | null = $state(null)
  let error: string | null = $state(null)
  let pollTimer: ReturnType<typeof setInterval>

  async function load() {
    try {
      instance = await getInstance(id)
      error = null
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to load instance'
    }
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
    <div class="error">{error}</div>
  {:else if !instance}
    <div class="loading">Loading...</div>
  {:else}
    <div class="header">
      <h1>{instance.handle || instance.id}</h1>
      <span class="state-badge" class:running={instance.state === 'running'}
            class:paused={instance.state === 'paused'}
            class:stopped={instance.state === 'stopped'}
            class:starting={instance.state === 'starting'}>
        {instance.state}
      </span>
    </div>

    <div class="info-grid">
      <div class="info-item">
        <span class="field-label">ID</span>
        <code>{instance.id}</code>
      </div>
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
        <div class="info-item">
          <span class="field-label">Workspace</span>
          <code>{instance.workspace}</code>
        </div>
      {/if}
      {#if instance.endpoints && instance.endpoints.length > 0}
        <div class="info-item">
          <span class="field-label">Endpoints</span>
          <div>
            {#each instance.endpoints as ep}
              <a href="http://127.0.0.1:{ep.public_port}" target="_blank">
                :{ep.public_port} ({ep.protocol})
              </a>
            {/each}
          </div>
        </div>
      {/if}
    </div>

    <div class="placeholder">
      Logs, exec, and chat tabs coming soon.
    </div>
  {/if}
</div>

<style>
  .detail {
    max-width: 900px;
    margin: 0 auto;
  }

  .breadcrumb {
    font-size: 13px;
    color: var(--text-muted);
    margin-bottom: 16px;
  }
  .breadcrumb a {
    color: var(--accent);
  }
  .sep {
    margin: 0 6px;
  }

  .header {
    display: flex;
    align-items: center;
    gap: 12px;
    margin-bottom: 20px;
  }

  h1 {
    font-size: 20px;
    font-weight: 600;
    font-family: var(--font-mono);
  }

  .state-badge {
    padding: 2px 10px;
    border-radius: 10px;
    font-size: 12px;
    font-weight: 500;
  }
  .state-badge.running {
    background: rgba(63, 185, 80, 0.15);
    color: var(--green);
  }
  .state-badge.paused {
    background: rgba(210, 153, 34, 0.15);
    color: var(--yellow);
  }
  .state-badge.stopped {
    background: var(--bg-tertiary);
    color: var(--text-muted);
  }
  .state-badge.starting {
    background: rgba(88, 166, 255, 0.15);
    color: var(--accent);
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
  .info-item .field-label {
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

  .error {
    color: var(--red);
    padding: 20px;
    text-align: center;
  }

  .loading {
    color: var(--text-muted);
    padding: 40px;
    text-align: center;
  }

  .placeholder {
    margin-top: 24px;
    padding: 40px;
    text-align: center;
    color: var(--text-muted);
    border: 1px dashed var(--border);
    border-radius: var(--radius-lg);
  }
</style>
