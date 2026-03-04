//go:build uifrontend

package main

import (
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// setupSystemTray configures the system tray icon, menu, and window behavior.
//
// Behavior:
//   - Click tray icon (left or right) → show menu
//   - Close window (X button) → hide to tray, app stays running
//   - "Go to Dashboard" → show + focus window
//   - "Restart Daemon" → restart aegisd
//   - "Quit AegisVM" → aegis down + app exit
func setupSystemTray(app *application.App, window *application.WebviewWindow) {
	tray := app.SystemTray.New()

	// macOS: template icon (system tints for dark/light mode).
	// Linux: regular icon (libappindicator doesn't support template icons).
	icon := generateTrayIcon()
	tray.SetTemplateIcon(icon)
	tray.SetIcon(icon)
	tray.SetTooltip("AegisVM")

	tray.SetMenu(buildTrayMenu(app, window))

	// Both clicks open the menu.
	tray.OnClick(func() {
		tray.OpenMenu()
	})

	// Intercept window close → hide to tray instead of quitting.
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		e.Cancel()
		window.Hide()
	})
}

// buildTrayMenu creates the tray menu reflecting current window state.
func buildTrayMenu(app *application.App, window *application.WebviewWindow) *application.Menu {
	menu := application.NewMenu()

	menu.Add("Go to Dashboard").OnClick(func(ctx *application.Context) {
		window.Show()
		window.Focus()
	})

	menu.AddSeparator()

	menu.Add("Restart Daemon").OnClick(func(ctx *application.Context) {
		go restartDaemon()
	})

	menu.AddSeparator()

	menu.Add("Quit AegisVM").OnClick(func(ctx *application.Context) {
		go func() {
			stopDaemon()
			app.Quit()
		}()
	})

	return menu
}
