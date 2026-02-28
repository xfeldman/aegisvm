<script lang="ts">
  import { onMount } from 'svelte'
  import { tetherSend, tetherPoll, type TetherFrame } from '../lib/api'
  import { getChatState, initChatState, updateChatState, addToast, type ChatMessage } from '../lib/store.svelte'

  interface Props {
    instanceId: string
    disabled?: boolean
  }

  let { instanceId, disabled = false }: Props = $props()

  let state = $derived(getChatState(instanceId))
  let input = $state('')
  let autoScroll = $state(true)
  let messagesEl: HTMLElement
  let abortCtrl: AbortController | null = null
  let polling = false
  let lightboxSrc: string | null = $state(null)

  function openLightbox(src: string) { lightboxSrc = src }
  function closeLightbox() { lightboxSrc = null }
  function onLightboxKey(e: KeyboardEvent) { if (e.key === 'Escape') closeLightbox() }

  const SESSION_ID = 'default'

  function scrollToBottom() {
    if (messagesEl && autoScroll) {
      requestAnimationFrame(() => {
        messagesEl.scrollTop = messagesEl.scrollHeight
      })
    }
  }

  function onScroll() {
    if (!messagesEl) return
    const atBottom = messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight < 40
    autoScroll = atBottom
  }

  function processFrames(frames: TetherFrame[]) {
    let { messages, thinking } = getChatState(instanceId)
    messages = [...messages]
    let changed = false

    for (const frame of frames) {
      if (frame.type === 'user.message') {
        const text = frame.content?.text || frame.payload?.text || ''
        if (text) {
          messages.push({
            role: 'user',
            text,
            ts: frame.ts || new Date().toISOString(),
          })
          changed = true
        }
      } else if (frame.type === 'status.presence') {
        thinking = frame.payload?.state || 'thinking'
        changed = true
      } else if (frame.type === 'assistant.delta') {
        const text = frame.payload?.text || ''
        const last = messages[messages.length - 1]
        if (last && last.role === 'assistant' && last.streaming) {
          messages[messages.length - 1] = { ...last, text: last.text + text }
        } else {
          messages.push({
            role: 'assistant',
            text,
            ts: frame.ts || new Date().toISOString(),
            streaming: true,
          })
        }
        changed = true
      } else if (frame.type === 'assistant.done') {
        const text = frame.payload?.text || ''
        const images = frame.payload?.images
        const last = messages[messages.length - 1]
        if (last && last.role === 'assistant' && last.streaming) {
          messages[messages.length - 1] = {
            ...last,
            text,
            images,
            streaming: false,
          }
        } else {
          messages.push({
            role: 'assistant',
            text,
            ts: frame.ts || new Date().toISOString(),
            images,
            streaming: false,
          })
        }
        thinking = null
        changed = true
      }
    }

    if (changed) {
      updateChatState(instanceId, { messages, thinking })
      scrollToBottom()
    }
  }

  async function startPollLoop(fromSeq: number) {
    if (polling) return
    polling = true
    abortCtrl = new AbortController()
    let cursor = fromSeq

    try {
      while (polling) {
        const result = await tetherPoll(instanceId, SESSION_ID, cursor, 5000, abortCtrl.signal)
        cursor = result.next_seq
        updateChatState(instanceId, { cursor })

        if (result.frames.length > 0) {
          processFrames(result.frames)
        }

        // Stop if we got assistant.done
        const done = result.frames.some(f => f.type === 'assistant.done')
        if (done) break

        if (result.timed_out) continue
      }
    } catch (e) {
      if (e instanceof DOMException && e.name === 'AbortError') return
      // Non-abort errors — stop polling silently
    } finally {
      polling = false
    }
  }

  function stopPolling() {
    polling = false
    if (abortCtrl) {
      abortCtrl.abort()
      abortCtrl = null
    }
  }

  async function send() {
    const text = input.trim()
    if (!text || disabled) return

    input = ''

    // Add user message to state
    const { messages } = getChatState(instanceId)
    updateChatState(instanceId, {
      messages: [...messages, {
        role: 'user',
        text,
        ts: new Date().toISOString(),
      }],
    })
    scrollToBottom()

    try {
      const result = await tetherSend(instanceId, SESSION_ID, text)
      updateChatState(instanceId, { cursor: result.ingress_seq })
      startPollLoop(result.ingress_seq)
    } catch (e) {
      addToast(`Send failed: ${e instanceof Error ? e.message : 'unknown'}`, 'error')
    }
  }

  function onKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      send()
    }
  }

  // On mount: reload full conversation from tether (server is authoritative).
  // Tether stores both user.message and assistant.* frames, so all clients
  // (desktop app, browser, MCP) see the same history.
  onMount(() => {
    initChatState(instanceId)

    async function catchUp() {
      // Start from 0 — tether has the complete conversation.
      let cursor = 0
      updateChatState(instanceId, { messages: [], cursor: 0, thinking: null })

      try {
        // Drain all available frames (API paginates at ~50)
        while (true) {
          const result = await tetherPoll(instanceId, SESSION_ID, cursor, 0)
          if (result.frames.length > 0) {
            processFrames(result.frames)
          }
          cursor = result.next_seq
          updateChatState(instanceId, { cursor })
          if (result.frames.length === 0 || result.timed_out) break
        }

        // If last message is streaming (interrupted mid-stream), resume polling
        const { messages } = getChatState(instanceId)
        const last = messages[messages.length - 1]
        if (last && last.role === 'assistant' && last.streaming) {
          startPollLoop(cursor)
        }
      } catch {
        // Instance may not support tether — ignore
      }
    }

    scrollToBottom()
    catchUp().then(scrollToBottom)

    return () => stopPolling()
  })
</script>

<div class="chat">
  <div class="messages" bind:this={messagesEl} onscroll={onScroll}>
    {#if state.messages.length === 0 && !state.thinking}
      <div class="empty">Send a message to the agent.</div>
    {/if}
    {#each state.messages as msg}
      <div class="message {msg.role}">
        <div class="message-role">{msg.role === 'user' ? 'You' : 'Agent'}</div>
        <div class="message-text">{msg.text}{#if msg.streaming}<span class="cursor">|</span>{/if}</div>
        {#if msg.images && msg.images.length > 0}
          <div class="message-images">
            {#each msg.images as img}
              {@const src = `/api/v1/instances/${instanceId}/workspace?path=.aegis/blobs/${img.blob}`}
              <button class="image-btn" onclick={() => openLightbox(src)}>
                <img {src} alt="Generated content" class="tether-image" />
              </button>
            {/each}
          </div>
        {/if}
      </div>
    {/each}
    {#if state.thinking}
      <div class="thinking">
        <span class="thinking-dot"></span>
        <span class="thinking-label">{state.thinking}</span>
      </div>
    {/if}
  </div>
  <div class="input-bar">
    <input
      type="text"
      bind:value={input}
      onkeydown={onKeydown}
      placeholder={disabled ? 'Instance is disabled' : 'Message the agent...'}
      {disabled}
    />
    <button class="send-btn" onclick={send} disabled={disabled || !input.trim()}>Send</button>
  </div>
</div>

{#if lightboxSrc}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="lightbox" onclick={closeLightbox} onkeydown={onLightboxKey}>
    <button class="lightbox-close" onclick={closeLightbox}>&times;</button>
    <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
    <img src={lightboxSrc} alt="Preview" class="lightbox-img" onclick={(e) => e.stopPropagation()} onkeydown={(e) => { if (e.key === 'Escape') closeLightbox() }} />
  </div>
{/if}

<style>
  .chat {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 300px;
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    overflow: hidden;
  }

  .messages {
    flex: 1;
    overflow-y: auto;
    padding: 16px;
    background: var(--bg);
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .empty {
    color: var(--text-muted);
    padding: 20px;
    text-align: center;
    margin: auto;
  }

  .message {
    max-width: 85%;
    padding: 8px 12px;
    border-radius: var(--radius-lg);
    font-size: 13px;
    line-height: 1.5;
  }

  .message.user {
    align-self: flex-end;
    background: var(--accent);
    color: #fff;
  }

  .message.assistant {
    align-self: flex-start;
    background: var(--bg-secondary);
    border: 1px solid var(--border);
  }

  .message-role {
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.03em;
    margin-bottom: 2px;
    opacity: 0.7;
  }

  .message-text {
    white-space: pre-wrap;
    word-break: break-word;
  }

  .cursor {
    animation: blink 0.8s step-end infinite;
    color: var(--accent);
  }

  @keyframes blink {
    50% { opacity: 0; }
  }

  .message-images {
    margin-top: 8px;
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }

  .image-btn {
    padding: 0;
    border: none;
    background: none;
    cursor: zoom-in;
    display: block;
  }

  .tether-image {
    max-width: 300px;
    max-height: 200px;
    border-radius: var(--radius);
    border: 1px solid var(--border);
    transition: opacity 0.15s;
  }
  .image-btn:hover .tether-image {
    opacity: 0.85;
  }

  .lightbox {
    position: fixed;
    inset: 0;
    z-index: 1000;
    background: rgba(0, 0, 0, 0.85);
    display: flex;
    align-items: center;
    justify-content: center;
    backdrop-filter: blur(4px);
  }

  .lightbox-close {
    position: absolute;
    top: 16px;
    right: 20px;
    background: none;
    border: none;
    color: #fff;
    font-size: 32px;
    line-height: 1;
    opacity: 0.7;
    cursor: pointer;
  }
  .lightbox-close:hover { opacity: 1; }

  .lightbox-img {
    max-width: 90vw;
    max-height: 90vh;
    border-radius: var(--radius-lg);
    box-shadow: 0 8px 32px rgba(0, 0, 0, 0.5);
  }

  .thinking {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    color: var(--text-muted);
    font-size: 12px;
  }

  .thinking-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--accent);
    animation: pulse 1.2s ease-in-out infinite;
  }

  @keyframes pulse {
    0%, 100% { opacity: 0.3; transform: scale(0.8); }
    50% { opacity: 1; transform: scale(1); }
  }

  .thinking-label {
    font-family: var(--font-mono);
  }

  .input-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    background: var(--bg-secondary);
    border-top: 1px solid var(--border);
  }

  input {
    flex: 1;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 8px 12px;
    color: var(--text);
    font-size: 13px;
    outline: none;
  }
  input:focus {
    border-color: var(--accent);
  }
  input:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .send-btn {
    padding: 8px 16px;
    border-radius: var(--radius);
    border: 1px solid var(--border);
    background: var(--bg-tertiary);
    color: var(--text);
    font-size: 13px;
    font-weight: 500;
  }
  .send-btn:hover:not(:disabled) {
    background: var(--bg);
    border-color: var(--accent);
  }
  .send-btn:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
