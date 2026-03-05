<script lang="ts">
  import { onMount, tick } from 'svelte'
  import { tetherSend, tetherPollBack, setTetherWatermark, openInBrowser, type TetherFrame } from '../lib/api'
  import { getChatState, initChatState, updateChatState, addToast, clearUnreadMessages, type ChatMessage } from '../lib/store.svelte'
  import { marked } from 'marked'

  interface Props {
    instanceId: string
    disabled?: boolean
    exposedPorts?: number[]
    onOpenPort?: (port: number) => void
  }

  let { instanceId, disabled = false, exposedPorts = [], onOpenPort }: Props = $props()

  // Configure marked: no async, sanitize output by stripping dangerous tags
  marked.setOptions({ async: false, breaks: true })

  function renderMarkdown(text: string): string {
    return marked.parse(text) as string
  }

  function handleMessageClick(e: MouseEvent) {
    const a = (e.target as HTMLElement).closest('a')
    if (!a) return
    e.preventDefault()

    const href = a.getAttribute('href') || ''
    // Check if it's a localhost link matching an exposed port
    const m = href.match(/^https?:\/\/(?:localhost|127\.0\.0\.1):(\d+)/)
    if (m) {
      const port = parseInt(m[1])
      if (exposedPorts.includes(port) && onOpenPort) {
        onOpenPort(port)
        return
      }
    }
    openInBrowser(href)
  }

  let state = $derived(getChatState(instanceId))
  let input = $state('')
  let autoScroll = $state(true)
  let messagesEl: HTMLElement
  let abortCtrl: AbortController | null = null
  let oldestSeq = $state(Infinity)
  let hasMore = $state(true)
  let loadingMore = $state(false)
  let ready = $state(false)
  let lightboxSrc: string | null = $state(null)

  function openLightbox(src: string) { lightboxSrc = src }
  function closeLightbox() { lightboxSrc = null }

  const SESSION_ID = 'default'

  async function scrollToBottom() {
    if (!autoScroll) return
    await tick()
    if (messagesEl) {
      messagesEl.scrollTop = messagesEl.scrollHeight
    }
  }

  function onScroll() {
    if (!messagesEl) return
    const atBottom = messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight < 40
    autoScroll = atBottom
    // Load older messages when scrolled to top
    if (messagesEl.scrollTop < 50 && hasMore && !loadingMore) {
      loadOlder()
    }
  }

  // Convert history frames (user.message + assistant.done) to ChatMessages.
  // Pure function — no streaming state, no side effects.
  function framesToMessages(frames: TetherFrame[]): ChatMessage[] {
    const msgs: ChatMessage[] = []
    for (const f of frames) {
      if (f.type === 'user.message') {
        const text = (f as any).content?.text || f.payload?.text || ''
        if (text) msgs.push({ role: 'user', text, ts: f.ts || '' })
      } else if (f.type === 'assistant.done') {
        msgs.push({
          role: 'assistant',
          text: f.payload?.text || '',
          images: f.payload?.images,
          ts: f.ts || '',
        })
      }
    }
    return msgs
  }

  async function loadOlder() {
    if (loadingMore || !hasMore || oldestSeq <= 1) return
    loadingMore = true
    try {
      const result = await tetherPollBack(instanceId, SESSION_ID, oldestSeq, 50)
      if (result.frames.length === 0) {
        hasMore = false
      } else {
        const prevHeight = messagesEl?.scrollHeight || 0
        const prevTop = messagesEl?.scrollTop || 0
        const older = framesToMessages(result.frames)
        if (older.length > 0) {
          const { messages } = getChatState(instanceId)
          updateChatState(instanceId, { messages: [...older, ...messages] })
          await tick()
          if (messagesEl) {
            messagesEl.scrollTop = prevTop + (messagesEl.scrollHeight - prevHeight)
          }
        }
        oldestSeq = result.next_seq
        if (result.frames.length < 50) hasMore = false
      }
    } catch {}
    loadingMore = false
    // If still near top after inserting (e.g. tall message fills viewport), keep loading
    await tick()
    if (messagesEl && messagesEl.scrollTop < 50 && hasMore) {
      loadOlder()
    }
  }

  // Handle a single live stream frame — manages streaming state (deltas, presence).
  function handleStreamFrame(frame: TetherFrame) {
    let { messages, thinking } = getChatState(instanceId)
    messages = [...messages]

    switch (frame.type) {
      case 'user.message': {
        const text = (frame as any).content?.text || frame.payload?.text || ''
        if (text) messages.push({ role: 'user', text, ts: frame.ts || '' })
        break
      }
      case 'status.presence':
        thinking = frame.payload?.state || 'thinking'
        break
      case 'reasoning.delta': {
        const text = frame.payload?.text || ''
        const last = messages[messages.length - 1]
        if (last?.role === 'assistant' && last.streaming) {
          messages[messages.length - 1] = { ...last, reasoning: (last.reasoning || '') + text }
        } else {
          messages.push({ role: 'assistant', text: '', reasoning: text, ts: frame.ts || '', streaming: true })
        }
        thinking = null
        break
      }
      case 'reasoning.done': {
        const last = messages[messages.length - 1]
        if (last?.role === 'assistant') {
          messages[messages.length - 1] = { ...last, reasoningDone: true }
        }
        break
      }
      case 'assistant.delta': {
        const text = frame.payload?.text || ''
        const last = messages[messages.length - 1]
        if (last?.role === 'assistant' && last.streaming) {
          messages[messages.length - 1] = { ...last, text: last.text + text }
        } else {
          messages.push({ role: 'assistant', text, ts: frame.ts || '', streaming: true })
        }
        break
      }
      case 'assistant.done': {
        const last = messages[messages.length - 1]
        const done: ChatMessage = {
          role: 'assistant', text: frame.payload?.text || '',
          images: frame.payload?.images, ts: frame.ts || '',
        }
        if (last?.role === 'assistant' && last.streaming) {
          messages[messages.length - 1] = { ...done, reasoning: last.reasoning, reasoningDone: last.reasoningDone }
        } else {
          messages.push(done)
        }
        thinking = null
        break
      }
      default:
        return // unknown frame type — no state change
    }

    updateChatState(instanceId, { messages, thinking })
    scrollToBottom()
  }

  // Live stream — real-time frame delivery via NDJSON stream.
  // Replaces polling for instant message display.
  function startStream() {
    if (abortCtrl) return
    abortCtrl = new AbortController()

    const url = `/api/v1/instances/${encodeURIComponent(instanceId)}/tether/stream`
    fetch(url, { signal: abortCtrl.signal }).then(async (resp) => {
      if (!resp.ok || !resp.body) return
      const reader = resp.body.getReader()
      const decoder = new TextDecoder()
      let buf = ''

      while (true) {
        const { done, value } = await reader.read()
        if (done) break
        buf += decoder.decode(value, { stream: true })

        // Process complete NDJSON lines
        let nl: number
        while ((nl = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, nl).trim()
          buf = buf.slice(nl + 1)
          if (!line) continue
          try {
            const frame = JSON.parse(line)
            // Only process frames for our channel/session.
            // Skip user.message — already added locally by send().
            if (frame.session?.channel === 'ui' && frame.session?.id === SESSION_ID && frame.type !== 'user.message') {
              handleStreamFrame(frame)
            }
          } catch {}
        }
      }
    }).catch(() => {})
  }

  function stopStream() {
    if (abortCtrl) {
      abortCtrl.abort()
      abortCtrl = null
    }
  }

  async function send() {
    const text = input.trim()
    if (!text || disabled) return

    input = ''

    // Add user message locally for instant display
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
      await tetherSend(instanceId, SESSION_ID, text)
    } catch (e) {
      addToast(`Send failed: ${e instanceof Error ? e.message : 'unknown'}`, 'error')
    }
  }

  function onKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      send()
      // Reset height after send
      const ta = e.target as HTMLTextAreaElement
      if (ta) { ta.style.height = 'auto' }
    }
  }

  function autoResize(e: Event) {
    const ta = e.target as HTMLTextAreaElement
    ta.style.height = 'auto'
    ta.style.height = Math.min(ta.scrollHeight, 72) + 'px' // 3 lines ≈ 72px
  }

  // On mount: load recent messages from the end (reverse), then connect stream.
  // No full history replay — older messages load on scroll-to-top.
  onMount(() => {
    initChatState(instanceId)

    async function loadRecent() {
      updateChatState(instanceId, { messages: [], cursor: 0, thinking: null })

      try {
        const result = await tetherPollBack(instanceId, SESSION_ID, Number.MAX_SAFE_INTEGER, 200)
        if (result.frames.length > 0) {
          // Replay all frames to reconstruct exact state (including thinking, tool use, streaming)
          for (const frame of result.frames) {
            handleStreamFrame(frame)
          }
          oldestSeq = result.next_seq
          hasMore = result.frames.length >= 200

          const maxSeq = Math.max(...result.frames.map(f => f.seq || 0))
          if (maxSeq > 0) setTetherWatermark(instanceId, 'ui', maxSeq).catch(() => {})
        } else {
          hasMore = false
        }
        clearUnreadMessages(instanceId)

        // Wait for DOM update, then wait for all images to load before scrolling
        await tick()
        if (messagesEl) {
          const imgs = messagesEl.querySelectorAll('img')
          if (imgs.length > 0) {
            await Promise.all(Array.from(imgs).map(img =>
              img.complete ? Promise.resolve() : new Promise(r => { img.onload = r; img.onerror = r })
            ))
          }
          messagesEl.scrollTop = messagesEl.scrollHeight
        }
        ready = true

        // Connect to live stream for real-time frames
        startStream()
      } catch {
        ready = true
      }
    }

    loadRecent()

    return () => stopStream()
  })
</script>

<svelte:window
  onresize={() => { if (autoScroll && messagesEl) messagesEl.scrollTop = messagesEl.scrollHeight }}
  onkeydown={(e) => { if (e.key === 'Escape' && lightboxSrc) closeLightbox() }}
/>

<div class="chat" class:ready>
  <div class="messages" bind:this={messagesEl} onscroll={onScroll}>
    {#if loadingMore}
      <div class="loading-more">Loading older messages...</div>
    {/if}
    {#if state.messages.length === 0 && !state.thinking}
      <div class="empty">Send a message to the agent.</div>
    {/if}
    {#each state.messages as msg}
      <!-- svelte-ignore a11y_no_static_element_interactions, a11y_click_events_have_key_events -->
      <div class="message {msg.role}" onclick={msg.role === 'assistant' ? handleMessageClick : undefined}>
        <div class="message-role">{msg.role === 'user' ? 'You' : 'Agent'}</div>
        {#if msg.role === 'assistant'}
          {#if msg.reasoning}
            <details class="reasoning" open={!msg.reasoningDone && msg.streaming}>
              <summary>
                Thinking{#if !msg.reasoningDone && msg.streaming}...{/if}
                <span class="reasoning-size">({msg.reasoning.length} chars)</span>
              </summary>
              <div class="reasoning-text">{msg.reasoning}</div>
            </details>
          {/if}
          <div class="message-text markdown">{@html renderMarkdown(msg.text)}{#if msg.streaming}<span class="cursor">|</span>{/if}</div>
        {:else}
          <div class="message-text">{msg.text}</div>
        {/if}
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
    <textarea
      class="chat-input"
      bind:value={input}
      onkeydown={onKeydown}
      oninput={autoResize}
      placeholder={disabled ? 'Instance is disabled' : 'Message the agent...'}
      {disabled}
      rows="1"
    ></textarea>
    <button class="send-btn" onclick={send} disabled={disabled || !input.trim()}>Send</button>
  </div>
</div>

{#if lightboxSrc}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="lightbox" onclick={closeLightbox}>
    <button class="lightbox-close" onclick={closeLightbox}>&times;</button>
    <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
    <img src={lightboxSrc} alt="Preview" class="lightbox-img" onclick={(e) => e.stopPropagation()} />
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
    visibility: hidden;
  }
  .chat.ready {
    visibility: visible;
  }
  .loading-more {
    text-align: center;
    padding: 8px;
    color: var(--text-muted);
    font-size: 12px;
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

  .reasoning {
    font-size: 12px;
    margin-bottom: 6px;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow: hidden;
  }
  .reasoning summary {
    padding: 4px 8px;
    cursor: pointer;
    color: var(--text-muted);
    font-weight: 500;
    user-select: none;
  }
  .reasoning-size {
    font-weight: normal;
    opacity: 0.5;
  }
  .reasoning-text {
    padding: 6px 8px;
    white-space: pre-wrap;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 11px;
    max-height: 200px;
    overflow-y: auto;
    border-top: 1px solid var(--border);
  }

  .message-text {
    white-space: pre-wrap;
    word-break: break-word;
  }
  .message-text.markdown {
    white-space: normal;
  }
  .message-text.markdown :global(p) {
    margin: 0 0 8px;
  }
  .message-text.markdown :global(p:last-child) {
    margin-bottom: 0;
  }
  .message-text.markdown :global(a) {
    color: var(--accent);
    text-decoration: underline;
    cursor: pointer;
  }
  .message-text.markdown :global(code) {
    font-family: var(--font-mono);
    font-size: 12px;
    background: var(--bg-tertiary);
    padding: 1px 4px;
    border-radius: 3px;
  }
  .message-text.markdown :global(pre) {
    background: var(--bg-tertiary);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 8px 10px;
    overflow-x: auto;
    margin: 6px 0;
  }
  .message-text.markdown :global(pre code) {
    background: none;
    padding: 0;
  }
  .message-text.markdown :global(ul), .message-text.markdown :global(ol) {
    margin: 4px 0;
    padding-left: 20px;
  }
  .message-text.markdown :global(li) {
    margin: 2px 0;
  }
  .message-text.markdown :global(blockquote) {
    border-left: 3px solid var(--border);
    margin: 6px 0;
    padding: 2px 10px;
    color: var(--text-muted);
  }
  .message-text.markdown :global(h1), .message-text.markdown :global(h2), .message-text.markdown :global(h3) {
    margin: 8px 0 4px;
    font-size: 14px;
    font-weight: 600;
  }
  .message-text.markdown :global(hr) {
    border: none;
    border-top: 1px solid var(--border);
    margin: 8px 0;
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
    align-items: flex-end;
    gap: 8px;
    padding: 8px 12px;
    background: var(--bg-secondary);
    border-top: 1px solid var(--border);
  }

  .chat-input {
    flex: 1;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 8px 12px;
    color: var(--text);
    font-size: 13px;
    font-family: inherit;
    outline: none;
    resize: none;
    line-height: 1.4;
    max-height: 72px;
    overflow-y: auto;
  }
  .chat-input:focus {
    border-color: var(--accent);
  }
  .chat-input:disabled {
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
