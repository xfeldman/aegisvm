// Package ui embeds the frontend build output for production serving.
package ui

import "embed"

// Frontend holds the compiled Svelte app from ui/frontend/dist/.
// In development (before npm run build), this will be empty — the
// aegis ui --dev flag proxies to the Vite dev server instead.
//
//go:embed all:frontend/dist
var Frontend embed.FS
