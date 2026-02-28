//go:build !uifrontend

// Package ui embeds the frontend build output for production serving.
package ui

import "embed"

// Frontend is an empty FS when building without the frontend.
// Run 'make ui' to build with the embedded Svelte app.
var Frontend embed.FS
