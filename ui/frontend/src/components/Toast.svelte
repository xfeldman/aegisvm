<script lang="ts">
  import { getToasts } from '../lib/store'

  let toasts = $derived(getToasts())
</script>

{#if toasts.length > 0}
  <div class="toast-container">
    {#each toasts as toast (toast.id)}
      <div class="toast toast-{toast.type}">
        {toast.message}
      </div>
    {/each}
  </div>
{/if}

<style>
  .toast-container {
    position: fixed;
    bottom: 20px;
    right: 20px;
    display: flex;
    flex-direction: column;
    gap: 8px;
    z-index: 1000;
  }

  .toast {
    padding: 10px 16px;
    border-radius: var(--radius);
    font-size: 13px;
    max-width: 360px;
    animation: slideIn 0.2s ease-out;
    box-shadow: 0 4px 12px rgba(0, 0, 0, 0.4);
  }

  .toast-success {
    background: #1a3a2a;
    border: 1px solid var(--green);
    color: var(--green);
  }
  .toast-error {
    background: #3a1a1a;
    border: 1px solid var(--red);
    color: var(--red);
  }
  .toast-info {
    background: var(--bg-tertiary);
    border: 1px solid var(--border);
    color: var(--text);
  }

  @keyframes slideIn {
    from { transform: translateX(100%); opacity: 0; }
    to { transform: translateX(0); opacity: 1; }
  }
</style>
