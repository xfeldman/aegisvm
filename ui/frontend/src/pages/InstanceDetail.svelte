<script lang="ts">
  import { onMount } from 'svelte'
  import { getInstance, startInstance, disableInstance, deleteInstance, openInBrowser, listSecrets, updateInstanceSecrets, type Instance, type SecretInfo } from '../lib/api'
  import { addToast, showConfirm, consumePendingPort, loadOpenPorts, saveOpenPorts } from '../lib/store.svelte'
  import LogViewer from '../components/LogViewer.svelte'
  import CommandRunner from '../components/CommandRunner.svelte'
  import ChatPanel from '../components/ChatPanel.svelte'
  import ConfigEditor from '../components/ConfigEditor.svelte'
  import WorkspaceBrowser from '../components/WorkspaceBrowser.svelte'

  interface Props {
    id: string
  }

  let { id }: Props = $props()
  let instance: Instance | null = $state(null)
  let error: string | null = $state(null)
  let tab: string = $state('info')
  let openPorts: number[] = $state([])
  let iframeKey: number = $state(0)
  let pollTimer: ReturnType<typeof setInterval>

  let canExec = $derived(instance?.enabled !== false)

  let secretsInitialized = false

  async function load() {
    try {
      instance = await getInstance(id)
      error = null
      // Sync bound keys on first load only (don't overwrite user edits on poll)
      if (!secretsInitialized) {
        syncBoundKeys()
        secretsInitialized = true
      }
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
          if (!await showConfirm(`Delete instance "${ref}"?`)) return
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

  let iframeEl: HTMLIFrameElement | undefined = $state(undefined)
  let iframeUrl: string = $state('')
  let addressInput: string = $state('')

  function openPort(port: number) {
    if (!openPorts.includes(port)) {
      openPorts = [...openPorts, port]
    }
    tab = `port:${port}`
    iframeUrl = `http://127.0.0.1:${port}/`
    addressInput = '/'
    saveOpenPorts(id, openPorts)
  }

  function closePort(port: number) {
    openPorts = openPorts.filter(p => p !== port)
    if (tab === `port:${port}`) tab = 'info'
    saveOpenPorts(id, openPorts)
  }

  function activePort(): number | null {
    return tab.startsWith('port:') ? parseInt(tab.slice(5)) : null
  }

  function navigateTo(path: string) {
    let p = path.trim()
    if (!p.startsWith('/')) p = '/' + p
    const port = activePort()
    iframeUrl = `http://127.0.0.1:${port}${p}`
    addressInput = p
    iframeKey++
  }

  function refreshIframe() {
    iframeKey++
  }

  function iframeBack() {
    try { iframeEl?.contentWindow?.history.back() } catch {}
  }

  function iframeForward() {
    try { iframeEl?.contentWindow?.history.forward() } catch {}
  }

  // --- Propagated secrets ---
  let allSecrets: SecretInfo[] = $state([])
  let boundKeys: string[] = $state([])
  let originalBoundKeys: string[] = $state([])
  let secretsDirty = $derived(JSON.stringify([...boundKeys].sort()) !== JSON.stringify([...originalBoundKeys].sort()))
  let savingSecrets = $state(false)
  let restartRequired = $state(false)

  async function loadSecrets() {
    try {
      const secrets = await listSecrets()
      allSecrets = secrets
    } catch {}
  }

  function syncBoundKeys() {
    if (instance) {
      boundKeys = instance.secret_keys ? [...instance.secret_keys] : []
      originalBoundKeys = [...boundKeys]
      restartRequired = false
    }
  }

  function toggleSecret(name: string) {
    if (boundKeys.includes(name)) {
      boundKeys = boundKeys.filter(k => k !== name)
    } else {
      boundKeys = [...boundKeys, name]
    }
  }

  async function saveSecrets() {
    if (!instance) return
    const ref = instance.handle || instance.id
    savingSecrets = true
    try {
      const result = await updateInstanceSecrets(ref, boundKeys)
      originalBoundKeys = [...boundKeys]
      if (result.restart_required) {
        restartRequired = true
      }
      addToast('Secrets updated', 'success')
    } catch (e) {
      addToast(`Save failed: ${e instanceof Error ? e.message : 'unknown'}`, 'error')
    } finally {
      savingSecrets = false
    }
  }

  async function doRestart() {
    if (!instance) return
    const ref = instance.handle || instance.id
    try {
      await startInstance(ref)
      restartRequired = false
      addToast('Restart requested', 'success')
      setTimeout(load, 500)
    } catch (e) {
      addToast(`Restart failed: ${e instanceof Error ? e.message : 'unknown'}`, 'error')
    }
  }

  onMount(() => {
    // Restore persisted port tabs
    const saved = loadOpenPorts(id)
    if (saved.length) openPorts = saved

    // Open port tab if navigated from InstanceList
    const pending = consumePendingPort()
    if (pending) openPort(pending)

    load()
    loadSecrets()
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
      {#if instance.kit}
        <button class="tab" class:active={tab === 'config'} onclick={() => tab = 'config'}>Kit Config</button>
      {/if}
      {#if instance.workspace}
        <button class="tab" class:active={tab === 'files'} onclick={() => tab = 'files'}>Files</button>
      {/if}
      <button class="tab" class:active={tab === 'logs'} onclick={() => tab = 'logs'}>Logs</button>
      <button class="tab" class:active={tab === 'exec'} onclick={() => tab = 'exec'}>Exec</button>
      <button class="tab" class:active={tab === 'chat'} onclick={() => tab = 'chat'}>Chat</button>
      {#each openPorts as port}
        <button class="tab" class:active={tab === `port:${port}`} onclick={() => tab = `port:${port}`}>
          <span class="port-tab-label">:{port}</span><span class="port-tab-close" role="button" tabindex="0" onclick={(e) => { e.stopPropagation(); closePort(port) }} onkeydown={(e) => { if (e.key === 'Enter') { e.stopPropagation(); closePort(port) } }}>&times;</span>
        </button>
      {/each}
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
              <span>{instance.kit} {#if instance.kit_version}<span class="text-muted">{instance.kit_version}</span>{/if}</span>
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
          {#if instance.harness_version}
            <div class="info-item">
              <span class="field-label">Harness</span>
              <span>{instance.harness_version}</span>
            </div>
          {/if}
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
                  <div class="endpoint-row">
                    <button class="endpoint-link" onclick={() => openPort(ep.public_port)}>
                      http://127.0.0.1:{ep.public_port}
                      <span class="endpoint-detail">&rarr; :{ep.guest_port} ({ep.protocol})</span>
                    </button>
                    <button class="open-external" title="Open in browser" onclick={() => openInBrowser(`http://127.0.0.1:${ep.public_port}`)}>&nearr;</button>
                  </div>
                {/each}
              </div>
            </div>
          {/if}
        </div>

        {#if allSecrets.length > 0}
          <div class="secrets-panel">
            <span class="field-label">Propagated Secrets</span>
            <div class="secrets-list">
              {#each allSecrets as secret}
                <label class="secret-item">
                  <input type="checkbox" checked={boundKeys.includes(secret.name)} onchange={() => toggleSecret(secret.name)} />
                  <code>{secret.name}</code>
                </label>
              {/each}
            </div>
            <div class="secrets-actions">
              <button class="btn" onclick={saveSecrets} disabled={savingSecrets || !secretsDirty}>
                {savingSecrets ? 'Saving...' : 'Save'}
              </button>
              {#if restartRequired}
                <button class="btn btn-restart" onclick={doRestart}>Restart to apply</button>
              {/if}
            </div>
          </div>
        {/if}
      {:else if tab === 'logs'}
        <LogViewer instanceId={instance.handle || instance.id} />
      {:else if tab === 'exec'}
        <CommandRunner instanceId={instance.handle || instance.id} disabled={!canExec} />
      {:else if tab === 'chat'}
        <ChatPanel instanceId={instance.handle || instance.id} disabled={!canExec} exposedPorts={instance.endpoints?.map(ep => ep.public_port) || []} onOpenPort={openPort} />
      {:else if tab === 'config' && instance.kit}
        <ConfigEditor instanceId={instance.handle || instance.id} kitName={instance.kit} />
      {:else if tab === 'files' && instance.workspace}
        <WorkspaceBrowser instanceId={instance.handle || instance.id} />
      {:else if activePort()}
        {@const port = activePort()}
        <div class="iframe-toolbar">
          <div class="iframe-nav">
            <button class="iframe-btn" title="Back" onclick={iframeBack}>&lsaquo;</button>
            <button class="iframe-btn" title="Forward" onclick={iframeForward}>&rsaquo;</button>
            <button class="iframe-btn" title="Refresh" onclick={refreshIframe}>&#x21bb;</button>
          </div>
          <input class="iframe-path-input" bind:value={addressInput} placeholder="/"
            onkeydown={(e) => { if (e.key === 'Enter') navigateTo(addressInput) }} />
          <button class="iframe-btn" title="Open in browser" onclick={() => openInBrowser(`http://127.0.0.1:${port}${addressInput}`)}>&nearr;</button>
        </div>
        <div class="iframe-container">
          {#key iframeKey}
            <iframe bind:this={iframeEl} src={iframeUrl || `http://127.0.0.1:${port}/`} title="Port {port}"></iframe>
          {/key}
        </div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .detail {
    max-width: 960px;
    margin: 0 auto;
    display: flex;
    flex-direction: column;
    height: 100%;
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
    flex-shrink: 0;
  }

  .tab-content {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
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
  .endpoint-row {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .endpoint-link {
    font-family: var(--font-mono);
    font-size: 13px;
    background: none;
    border: none;
    color: var(--accent);
    padding: 0;
    cursor: pointer;
    text-align: left;
  }
  .endpoint-link:hover { text-decoration: underline; }
  .endpoint-detail {
    color: var(--text-muted);
    font-size: 11px;
    margin-left: 6px;
  }
  .open-external {
    background: none;
    border: none;
    color: var(--text-muted);
    font-size: 14px;
    cursor: pointer;
    padding: 2px 4px;
    border-radius: var(--radius);
    line-height: 1;
  }
  .open-external:hover { color: var(--accent); background: var(--bg-tertiary); }

  /* Port tab extras */
  .port-tab-label { font-family: var(--font-mono); }
  .port-tab-close {
    margin-left: 4px;
    opacity: 0.4;
    cursor: pointer;
  }
  .port-tab-close:hover { opacity: 1; color: var(--red); }

  /* Iframe preview */
  .iframe-toolbar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 12px;
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg) var(--radius-lg) 0 0;
    flex-shrink: 0;
  }
  .iframe-nav {
    display: flex;
    gap: 2px;
  }
  .iframe-path-input {
    flex: 1;
    font-family: var(--font-mono);
    font-size: 12px;
    color: var(--text);
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 4px 8px;
    outline: none;
    min-width: 0;
  }
  .iframe-path-input:focus { border-color: var(--accent); }
  .iframe-btn {
    background: none;
    border: 1px solid transparent;
    color: var(--text-muted);
    font-size: 15px;
    cursor: pointer;
    padding: 2px 6px;
    border-radius: var(--radius);
    line-height: 1;
  }
  .iframe-btn:hover { color: var(--text); background: var(--bg-tertiary); border-color: var(--border); }
  .iframe-container {
    flex: 1;
    min-height: 0;
    border: 1px solid var(--border);
    border-top: none;
    border-radius: 0 0 var(--radius-lg) var(--radius-lg);
    overflow: hidden;
  }
  .iframe-container iframe {
    width: 100%;
    height: 100%;
    border: none;
    background: var(--bg);
  }

  .text-muted { color: var(--text-muted); font-size: 12px; }

  /* Propagated secrets */
  .secrets-panel {
    margin-top: 12px;
    padding: 12px 16px;
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .secrets-list {
    display: flex;
    flex-wrap: wrap;
    gap: 6px 16px;
  }
  .secret-item {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 13px;
    cursor: pointer;
  }
  .secret-item code { color: var(--text); }
  .secrets-actions {
    display: flex;
    gap: 6px;
    align-items: center;
    margin-top: 4px;
  }
  .btn-restart {
    color: var(--accent);
    border-color: rgba(88, 166, 255, 0.3);
  }
  .btn-restart:hover { background: rgba(88, 166, 255, 0.1); border-color: var(--accent); }

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
