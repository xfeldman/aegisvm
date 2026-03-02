<script lang="ts">
  import { onMount } from 'svelte'
  import { listWorkspaceDir, readWorkspaceFile, writeWorkspaceFile, type FileEntry } from '../lib/api'
  import { addToast } from '../lib/store.svelte'
  import hljs from 'highlight.js/lib/core'
  import python from 'highlight.js/lib/languages/python'
  import javascript from 'highlight.js/lib/languages/javascript'
  import typescript from 'highlight.js/lib/languages/typescript'
  import json from 'highlight.js/lib/languages/json'
  import yaml from 'highlight.js/lib/languages/yaml'
  import shell from 'highlight.js/lib/languages/shell'
  import bash from 'highlight.js/lib/languages/bash'
  import xml from 'highlight.js/lib/languages/xml'
  import css from 'highlight.js/lib/languages/css'
  import markdown from 'highlight.js/lib/languages/markdown'
  import dockerfile from 'highlight.js/lib/languages/dockerfile'

  hljs.registerLanguage('python', python)
  hljs.registerLanguage('javascript', javascript)
  hljs.registerLanguage('typescript', typescript)
  hljs.registerLanguage('json', json)
  hljs.registerLanguage('yaml', yaml)
  hljs.registerLanguage('shell', shell)
  hljs.registerLanguage('bash', bash)
  hljs.registerLanguage('xml', xml)
  hljs.registerLanguage('css', css)
  hljs.registerLanguage('markdown', markdown)
  hljs.registerLanguage('dockerfile', dockerfile)

  interface Props {
    instanceId: string
  }

  let { instanceId }: Props = $props()

  // Tree state
  interface TreeNode {
    name: string
    path: string
    isDir: boolean
    size: number
    children?: TreeNode[]
    expanded?: boolean
    loaded?: boolean
  }

  let tree: TreeNode[] = $state([])
  let treeLoading = $state(true)

  // Editor state
  let currentFile: string | null = $state(null)
  let content = $state('')
  let original = $state('')
  let editorLoading = $state(false)
  let textareaEl: HTMLTextAreaElement | null = $state(null)
  let codeEl: HTMLElement | null = $state(null)

  let dirty = $derived(content !== original)

  async function loadDir(path: string): Promise<TreeNode[]> {
    const entries = await listWorkspaceDir(instanceId, path)
    return entries.map(e => ({
      name: e.name,
      path: path === '.' ? e.name : `${path}/${e.name}`,
      isDir: e.is_dir,
      size: e.size,
      children: e.is_dir ? [] : undefined,
      expanded: false,
      loaded: false,
    }))
  }

  async function toggleDir(node: TreeNode) {
    if (!node.isDir) return
    if (node.expanded) {
      node.expanded = false
      return
    }
    if (!node.loaded) {
      try {
        node.children = await loadDir(node.path)
        node.loaded = true
      } catch (e) {
        addToast(`Failed to list ${node.path}`, 'error')
        return
      }
    }
    node.expanded = true
  }

  async function openFile(path: string) {
    if (currentFile === path) return
    editorLoading = true
    try {
      const text = await readWorkspaceFile(instanceId, path)
      currentFile = path
      content = text
      original = text
    } catch (e) {
      addToast(`Failed to open ${path}`, 'error')
    } finally {
      editorLoading = false
    }
  }

  async function saveFile() {
    if (!currentFile || !dirty) return
    try {
      await writeWorkspaceFile(instanceId, currentFile, content)
      original = content
      addToast(`Saved ${currentFile}`, 'success')
    } catch (e) {
      addToast(`Save failed: ${e instanceof Error ? e.message : 'unknown'}`, 'error')
    }
  }

  function highlightCode(text: string, filename: string): string {
    const ext = filename.split('.').pop()?.toLowerCase() || ''
    const langMap: Record<string, string> = {
      py: 'python', js: 'javascript', ts: 'typescript', tsx: 'typescript',
      jsx: 'javascript', json: 'json', yml: 'yaml', yaml: 'yaml',
      sh: 'bash', bash: 'bash', zsh: 'bash', html: 'xml', xml: 'xml',
      css: 'css', md: 'markdown', dockerfile: 'dockerfile',
    }
    const lang = langMap[ext]
    // Special case: Dockerfile without extension
    const basename = filename.split('/').pop() || ''
    const effectiveLang = lang || (basename === 'Dockerfile' ? 'dockerfile' : null)

    if (effectiveLang) {
      try {
        return hljs.highlight(text, { language: effectiveLang }).value
      } catch {}
    }
    // Fallback: auto-detect
    try {
      return hljs.highlightAuto(text).value
    } catch {}
    return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  }

  function onKeydown(e: KeyboardEvent) {
    // Cmd/Ctrl+S to save
    if ((e.metaKey || e.ctrlKey) && e.key === 's') {
      e.preventDefault()
      saveFile()
      return
    }
    // Tab inserts 2 spaces
    if (e.key === 'Tab') {
      e.preventDefault()
      const ta = textareaEl
      if (!ta) return
      const start = ta.selectionStart
      const end = ta.selectionEnd
      content = content.substring(0, start) + '  ' + content.substring(end)
      requestAnimationFrame(() => { ta.selectionStart = ta.selectionEnd = start + 2 })
    }
  }

  function syncScroll() {
    if (textareaEl && codeEl) {
      codeEl.scrollTop = textareaEl.scrollTop
      codeEl.scrollLeft = textareaEl.scrollLeft
    }
  }

  function formatSize(bytes: number): string {
    if (bytes < 1024) return `${bytes}B`
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)}K`
    return `${(bytes / (1024 * 1024)).toFixed(1)}M`
  }

  onMount(async () => {
    try {
      tree = await loadDir('.')
    } catch (e) {
      addToast('Failed to load workspace', 'error')
    } finally {
      treeLoading = false
    }
  })
</script>

<div class="browser">
  <div class="tree-panel">
    <div class="tree-header">Files</div>
    <div class="tree-content">
      {#if treeLoading}
        <div class="tree-loading">Loading...</div>
      {:else}
        {#snippet renderNodes(nodes: TreeNode[], depth: number)}
          {#each nodes as node}
            <button
              class="tree-node"
              class:active={currentFile === node.path}
              style="padding-left: {8 + depth * 14}px"
              onclick={() => node.isDir ? toggleDir(node) : openFile(node.path)}
            >
              <span class="tree-icon">{node.isDir ? (node.expanded ? '▾' : '▸') : ' '}</span>
              <span class="tree-name" class:dir={node.isDir}>{node.name}</span>
              {#if !node.isDir}
                <span class="tree-size">{formatSize(node.size)}</span>
              {/if}
            </button>
            {#if node.isDir && node.expanded && node.children}
              {@render renderNodes(node.children, depth + 1)}
            {/if}
          {/each}
        {/snippet}
        {@render renderNodes(tree, 0)}
      {/if}
    </div>
  </div>

  <div class="editor-panel">
    {#if !currentFile}
      <div class="editor-empty">Select a file to view</div>
    {:else if editorLoading}
      <div class="editor-empty">Loading...</div>
    {:else}
      <div class="editor-header">
        <span class="editor-filename">{currentFile}{#if dirty}<span class="dirty-dot">*</span>{/if}</span>
        <button class="editor-save" disabled={!dirty} onclick={saveFile}>Save</button>
      </div>
      <div class="editor-area">
        <pre class="editor-highlight" bind:this={codeEl}>{@html highlightCode(content, currentFile)}{'\n'}</pre>
        <textarea
          class="editor-textarea"
          bind:this={textareaEl}
          bind:value={content}
          onkeydown={onKeydown}
          onscroll={syncScroll}
          spellcheck={false}
        ></textarea>
      </div>
    {/if}
  </div>
</div>

<style>
  .browser {
    display: flex;
    flex: 1;
    min-height: 0;
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    overflow: hidden;
  }

  /* Tree panel */
  .tree-panel {
    width: 220px;
    flex-shrink: 0;
    display: flex;
    flex-direction: column;
    border-right: 1px solid var(--border);
    background: var(--bg-secondary);
  }
  .tree-header {
    padding: 8px 12px;
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
    border-bottom: 1px solid var(--border);
    flex-shrink: 0;
  }
  .tree-content {
    flex: 1;
    overflow-y: auto;
    padding: 4px 0;
  }
  .tree-loading {
    padding: 16px;
    color: var(--text-muted);
    font-size: 12px;
    text-align: center;
  }
  .tree-node {
    display: flex;
    align-items: center;
    gap: 4px;
    width: 100%;
    padding: 3px 8px;
    border: none;
    background: none;
    color: var(--text);
    font-size: 12px;
    font-family: var(--font-mono);
    cursor: pointer;
    text-align: left;
  }
  .tree-node:hover { background: var(--bg-tertiary); }
  .tree-node.active { background: var(--bg-tertiary); color: var(--accent); }
  .tree-icon {
    width: 10px;
    flex-shrink: 0;
    font-size: 10px;
    color: var(--text-muted);
  }
  .tree-name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .tree-name.dir { color: var(--accent); }
  .tree-size { color: var(--text-muted); font-size: 10px; flex-shrink: 0; }

  /* Editor panel */
  .editor-panel {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    background: var(--bg);
  }
  .editor-empty {
    flex: 1;
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--text-muted);
    font-size: 13px;
  }
  .editor-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 6px 12px;
    border-bottom: 1px solid var(--border);
    background: var(--bg-secondary);
    flex-shrink: 0;
  }
  .editor-filename {
    font-family: var(--font-mono);
    font-size: 12px;
    color: var(--text);
  }
  .dirty-dot { color: var(--orange); margin-left: 4px; }
  .editor-save {
    padding: 3px 10px;
    border-radius: var(--radius);
    border: 1px solid var(--border);
    background: var(--bg-tertiary);
    color: var(--text);
    font-size: 11px;
  }
  .editor-save:hover:not(:disabled) { border-color: var(--accent); }
  .editor-save:disabled { opacity: 0.3; cursor: default; }

  /* Editor area: overlay textarea on highlighted pre */
  .editor-area {
    flex: 1;
    min-height: 0;
    position: relative;
    overflow: hidden;
  }
  .editor-highlight, .editor-textarea {
    position: absolute;
    inset: 0;
    margin: 0;
    padding: 12px;
    font-family: var(--font-mono);
    font-size: 13px;
    line-height: 1.5;
    white-space: pre;
    overflow: auto;
    tab-size: 2;
    border: none;
    border-radius: 0;
  }
  .editor-highlight {
    color: var(--text);
    pointer-events: none;
    z-index: 0;
  }
  .editor-textarea {
    color: transparent;
    caret-color: var(--text);
    background: transparent;
    resize: none;
    outline: none;
    z-index: 1;
    -webkit-text-fill-color: transparent;
  }

  /* highlight.js token colors (GitHub dark theme) */
  .editor-highlight :global(.hljs-keyword) { color: #ff7b72; }
  .editor-highlight :global(.hljs-built_in) { color: #ffa657; }
  .editor-highlight :global(.hljs-type) { color: #ffa657; }
  .editor-highlight :global(.hljs-literal) { color: #79c0ff; }
  .editor-highlight :global(.hljs-number) { color: #79c0ff; }
  .editor-highlight :global(.hljs-string) { color: #a5d6ff; }
  .editor-highlight :global(.hljs-regexp) { color: #a5d6ff; }
  .editor-highlight :global(.hljs-symbol) { color: #79c0ff; }
  .editor-highlight :global(.hljs-variable) { color: #ffa657; }
  .editor-highlight :global(.hljs-attr) { color: #79c0ff; }
  .editor-highlight :global(.hljs-params) { color: #e6edf3; }
  .editor-highlight :global(.hljs-comment) { color: #8b949e; font-style: italic; }
  .editor-highlight :global(.hljs-doctag) { color: #8b949e; }
  .editor-highlight :global(.hljs-meta) { color: #79c0ff; }
  .editor-highlight :global(.hljs-section) { color: #79c0ff; font-weight: bold; }
  .editor-highlight :global(.hljs-title) { color: #d2a8ff; }
  .editor-highlight :global(.hljs-name) { color: #7ee787; }
  .editor-highlight :global(.hljs-tag) { color: #7ee787; }
  .editor-highlight :global(.hljs-selector-class) { color: #d2a8ff; }
  .editor-highlight :global(.hljs-selector-id) { color: #79c0ff; }
  .editor-highlight :global(.hljs-attribute) { color: #79c0ff; }
  .editor-highlight :global(.hljs-template-variable) { color: #ffa657; }
  .editor-highlight :global(.hljs-addition) { color: #aff5b4; background: rgba(63,185,80,0.1); }
  .editor-highlight :global(.hljs-deletion) { color: #ffa198; background: rgba(248,81,73,0.1); }
</style>
