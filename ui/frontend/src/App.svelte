<script lang="ts">
  import Dashboard from './pages/Dashboard.svelte'
  import InstanceDetail from './pages/InstanceDetail.svelte'
  import NewInstance from './pages/NewInstance.svelte'
  import Secrets from './pages/Secrets.svelte'
  import Toast from './components/Toast.svelte'

  let hash = $state(window.location.hash || '#/')

  function onHashChange() {
    hash = window.location.hash || '#/'
  }

  // Parse route
  function getRoute(h: string): { page: string; param?: string } {
    const path = h.slice(1) || '/'
    if (path.startsWith('/instance/')) {
      return { page: 'instance', param: path.slice('/instance/'.length) }
    }
    if (path === '/new') return { page: 'new' }
    if (path === '/secrets') return { page: 'secrets' }
    return { page: 'dashboard' }
  }

  let route = $derived(getRoute(hash))
</script>

<svelte:window onhashchange={onHashChange} />

<div class="layout">
  <header class="topbar">
    <a href="#/" class="logo">
      <span class="logo-text">aegis</span>
    </a>
    <nav>
      <a href="#/" class="nav-link" class:active={route.page === 'dashboard'}>Dashboard</a>
      <a href="#/secrets" class="nav-link" class:active={route.page === 'secrets'}>Secrets</a>
    </nav>
  </header>

  <main class="content">
    {#if route.page === 'dashboard'}
      <Dashboard />
    {:else if route.page === 'instance' && route.param}
      <InstanceDetail id={route.param} />
    {:else if route.page === 'new'}
      <NewInstance />
    {:else if route.page === 'secrets'}
      <Secrets />
    {/if}
  </main>
</div>

<Toast />

<style>
  .layout {
    display: flex;
    flex-direction: column;
    height: 100%;
  }

  .topbar {
    display: flex;
    align-items: center;
    gap: 24px;
    padding: 0 20px;
    height: 48px;
    background: var(--bg-secondary);
    border-bottom: 1px solid var(--border);
    flex-shrink: 0;
  }

  .logo {
    display: flex;
    align-items: center;
    gap: 8px;
    text-decoration: none;
  }
  .logo-text {
    font-size: 16px;
    font-weight: 600;
    color: var(--text);
    font-family: var(--font-mono);
  }

  nav {
    display: flex;
    gap: 4px;
  }

  .nav-link {
    padding: 6px 12px;
    border-radius: var(--radius);
    color: var(--text-muted);
    font-size: 13px;
    text-decoration: none;
    transition: background 0.15s, color 0.15s;
  }
  .nav-link:hover {
    background: var(--bg-tertiary);
    color: var(--text);
    text-decoration: none;
  }
  .nav-link.active {
    background: var(--bg-tertiary);
    color: var(--text);
  }

  .content {
    flex: 1;
    overflow-y: auto;
    padding: 24px;
  }
</style>
