package main

import (
	"context"
	"fmt"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/xfeldman/aegisvm/internal/client"
)

// setupSystemTray configures the system tray icon, menu, and window behavior.
//
// Behavior:
//   - Left-click tray icon → toggle window visibility
//   - Right-click → show menu with instance list + quit
//   - Close window (X button) → hide to tray, app stays running
//   - "Quit AegisVM" in menu → app exits, aegisd keeps running
func setupSystemTray(app *application.App, window *application.WebviewWindow) {
	tray := app.SystemTray.New()

	// Template icon: macOS tints it for dark/light mode automatically.
	tray.SetTemplateIcon(generateTrayIcon())
	tray.SetTooltip("AegisVM")

	// Build initial menu (updated every 10s with live instance data).
	menu := buildTrayMenu(app, window, nil)
	tray.SetMenu(menu)

	// Left-click: toggle window visibility.
	tray.OnClick(func() {
		if window.IsVisible() {
			window.Hide()
		} else {
			window.Show()
		}
	})

	// Intercept window close → hide to tray instead of quitting.
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		e.Cancel()
		window.Hide()
	})

	// Poll instances to keep tray menu up to date.
	go pollTrayInstances(app, tray, window)
}

// buildTrayMenu creates a tray menu with the current instance list.
func buildTrayMenu(app *application.App, window *application.WebviewWindow, instances []client.Instance) *application.Menu {
	menu := application.NewMenu()

	menu.Add("Open Dashboard").OnClick(func(ctx *application.Context) {
		window.Show()
	})

	menu.AddSeparator()

	// Instance list
	if len(instances) == 0 {
		menu.Add("No instances").SetEnabled(false)
	} else {
		for _, inst := range instances {
			name := inst.Handle
			if name == "" && len(inst.ID) >= 12 {
				name = inst.ID[:12]
			}
			stateIcon := stateIndicator(inst.State)
			label := fmt.Sprintf("%s %s (%s)", stateIcon, name, inst.State)
			menu.Add(label)
		}
	}

	menu.AddSeparator()

	menu.Add("Quit AegisVM").OnClick(func(ctx *application.Context) {
		app.Quit()
	})

	return menu
}

// stateIndicator returns a Unicode dot/circle for the instance state.
func stateIndicator(state string) string {
	switch state {
	case "running":
		return "\u25CF" // ● green (colored by terminal, solid here)
	case "paused":
		return "\u25CB" // ○
	case "stopped":
		return "\u25CB" // ○
	case "disabled":
		return "\u2298" // ⊘
	default:
		return "\u25CB" // ○
	}
}

// pollTrayInstances periodically fetches instance data and rebuilds the tray menu.
func pollTrayInstances(app *application.App, tray *application.SystemTray, window *application.WebviewWindow) {
	c := client.NewDefault()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Initial update after a short delay (let the app settle).
	time.Sleep(2 * time.Second)
	updateTrayMenu(c, app, tray, window)

	for range ticker.C {
		updateTrayMenu(c, app, tray, window)
	}
}

func updateTrayMenu(c *client.Client, app *application.App, tray *application.SystemTray, window *application.WebviewWindow) {
	instances, err := c.ListInstances(context.Background(), "")
	if err != nil {
		return // Silently skip — daemon might be restarting
	}

	menu := buildTrayMenu(app, window, instances)
	tray.SetMenu(menu)
}
