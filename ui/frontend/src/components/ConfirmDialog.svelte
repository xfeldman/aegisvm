<script lang="ts">
  import { getConfirmMessage, getConfirmResolve } from '../lib/store.svelte'
  let message = $derived(getConfirmMessage())
  let resolve = $derived(getConfirmResolve())
</script>

{#if resolve}
  <div class="overlay" role="presentation" onclick={() => resolve?.(false)} onkeydown={(e) => { if (e.key === 'Escape') resolve?.(false) }}>
    <div class="dialog" role="alertdialog" aria-modal="true" tabindex="-1" onclick={(e) => e.stopPropagation()} onkeydown={() => {}}>
      <p>{message}</p>
      <div class="actions">
        <button class="cancel" onclick={() => resolve?.(false)}>Cancel</button>
        <button class="ok" onclick={() => resolve?.(true)}>OK</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .overlay { position: fixed; inset: 0; background: rgba(0,0,0,0.5); display: flex; align-items: center; justify-content: center; z-index: 9999; }
  .dialog { background: var(--bg-secondary); color: var(--text); border: 1px solid var(--border); border-radius: var(--radius-lg); padding: 24px; min-width: 300px; max-width: 400px; font-size: 14px; }
  p { margin: 0 0 20px; line-height: 1.5; }
  .actions { display: flex; gap: 8px; justify-content: flex-end; }
  button { padding: 5px 14px; border-radius: var(--radius); cursor: pointer; font-size: 13px; transition: background 0.15s, border-color 0.15s, color 0.15s; }
  .cancel { border: 1px solid var(--border); background: var(--bg-tertiary); color: var(--text); }
  .cancel:hover { background: var(--bg-secondary); border-color: var(--text-muted); }
  .ok { border: 1px solid rgba(248, 81, 73, 0.3); background: var(--bg-tertiary); color: var(--red); font-weight: 600; }
  .ok:hover { background: rgba(248, 81, 73, 0.1); border-color: var(--red); }
</style>
