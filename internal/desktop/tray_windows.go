//go:build windows

package desktop

import "github.com/energye/systray"

// The notification-area icon is served by systray, whose Windows backend is
// plain win32 syscalls: no cgo, no network.

// startTray brings up the tray icon on its own OS thread. systray.Run blocks
// inside its message loop, so it runs in a goroutine for the app's lifetime;
// stopTray ends it at shutdown.
func (a *App) startTray() {
	goSafe("the notification area icon", func() { systray.Run(a.trayReady, nil) })
}

// stopTray removes the tray icon. Safe to call even if the tray never came up.
func (a *App) stopTray() {
	systray.Quit()
}

// trayReady builds the icon and menu once systray's loop is up.
func (a *App) trayReady() {
	if len(a.trayIcon) > 0 {
		systray.SetIcon(a.trayIcon)
	}
	systray.SetTooltip("Pelton")

	for _, item := range a.trayMenuItems(a.showWindow) {
		if item.action == nil {
			systray.AddSeparator()
			continue
		}
		systray.AddMenuItem(item.label, "").Click(item.action)
	}

	// left click reopens the window; the menu only shows on right click.
	systray.SetOnClick(func(systray.IMenu) {
		a.showWindow()
	})
	systray.SetOnRClick(func(m systray.IMenu) {
		_ = m.ShowMenu()
	})
}
