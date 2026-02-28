//go:build uifrontend

// Package ui embeds the frontend build output for production serving.
package ui

import "embed"

// Frontend holds the compiled Svelte app from ui/frontend/dist/.
//
//go:embed all:frontend/dist
var Frontend embed.FS
