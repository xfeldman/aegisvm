<script lang="ts">
  import { onMount } from 'svelte'
  import InstanceList from '../components/InstanceList.svelte'
  import { getInstances, isLoading, getError, refreshInstances } from '../lib/store'
  import { getStatus, type DaemonStatus } from '../lib/api'

  let instances = $derived(getInstances())
  let loading = $derived(isLoading())
  let error = $derived(getError())
  let status: DaemonStatus | null = $state(null)
  let pollTimer: ReturnType<typeof setInterval>

  // Computed status counts
  let running = $derived(instances.filter(i => i.state === 'running').length)
  let paused = $derived(instances.filter(i => i.state === 'paused').length)
  let stopped = $derived(instances.filter(i => i.state === 'stopped').length)
  let starting = $derived(instances.filter(i => i.state === 'starting').length)

  onMount(() => {
    refreshInstances()
    getStatus().then(s => status = s).catch(() => {})
    pollTimer = setInterval(refreshInstances, 5000)
    return () => clearInterval(pollTimer)
  })
</script>

<div class="dashboard">
  <div class="dashboard-header">
    <h1>Instances</h1>
    {#if status}
      <span class="daemon-status">
        {status.backend}
      </span>
    {/if}
  </div>

  <div class="status-bar">
    <div class="status-item">
      <span class="status-dot running"></span>
      Running: {running}
    </div>
    {#if starting > 0}
      <div class="status-item">
        <span class="status-dot starting"></span>
        Starting: {starting}
      </div>
    {/if}
    <div class="status-item">
      <span class="status-dot paused"></span>
      Paused: {paused}
    </div>
    <div class="status-item">
      <span class="status-dot stopped-dot"></span>
      Stopped: {stopped}
    </div>
    <div class="status-item total">
      Total: {instances.length}
    </div>
  </div>

  {#if error}
    <div class="error-banner">
      {error}
      <button class="retry-btn" onclick={refreshInstances}>Retry</button>
    </div>
  {:else if loading && instances.length === 0}
    <div class="empty-state">Loading instances...</div>
  {:else if instances.length === 0}
    <div class="empty-state">
      <p>No instances yet.</p>
      <p class="hint">Run <code>aegis instance start --name myvm -- echo hello</code> to create one.</p>
    </div>
  {:else}
    <InstanceList {instances} />
  {/if}
</div>

<style>
  .dashboard {
    max-width: 1100px;
    margin: 0 auto;
  }

  .dashboard-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 16px;
  }

  h1 {
    font-size: 20px;
    font-weight: 600;
  }

  .daemon-status {
    font-size: 12px;
    color: var(--text-muted);
    padding: 3px 10px;
    border-radius: var(--radius);
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    font-family: var(--font-mono);
  }

  .status-bar {
    display: flex;
    gap: 20px;
    margin-bottom: 16px;
    padding: 10px 16px;
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    font-size: 13px;
  }

  .status-item {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--text-muted);
  }

  .status-item.total {
    margin-left: auto;
  }

  .status-dot {
    display: inline-block;
    width: 8px;
    height: 8px;
    border-radius: 50%;
  }
  .status-dot.running { background: var(--green); }
  .status-dot.starting { background: var(--accent); }
  .status-dot.paused { background: var(--yellow); }
  .status-dot.stopped-dot { background: var(--text-muted); }

  .error-banner {
    padding: 12px 16px;
    background: rgba(248, 81, 73, 0.1);
    border: 1px solid var(--red);
    border-radius: var(--radius-lg);
    color: var(--red);
    display: flex;
    align-items: center;
    gap: 12px;
  }

  .retry-btn {
    padding: 4px 12px;
    border-radius: var(--radius);
    border: 1px solid var(--red);
    background: transparent;
    color: var(--red);
    font-size: 12px;
  }
  .retry-btn:hover {
    background: rgba(248, 81, 73, 0.15);
  }

  .empty-state {
    text-align: center;
    padding: 60px 20px;
    color: var(--text-muted);
  }
  .empty-state p {
    margin-bottom: 8px;
  }
  .hint {
    font-size: 13px;
  }
  .hint code {
    background: var(--bg-secondary);
    padding: 2px 6px;
    border-radius: 3px;
    border: 1px solid var(--border);
  }
</style>
