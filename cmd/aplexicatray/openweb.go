//go:build tray

package main

import (
	"log"
	"strings"

	"github.com/aplexica/aplexica/internal/i18n"
)

// openWebUI is the click handler for the "Open Aplexica" menu item.
// It RPCs the running daemon (`aplexica web open`) which mints a
// bootstrap URL and launches the user's default browser at it.
//
// We deliberately invoke the CLI subcommand rather than re-implementing
// the UDS dial in this binary: the tray's existing runAplexica helper
// already handles per-platform exec quirks, and shelling out keeps the
// tray binary small (no UDS client, no auth state).
//
// On failure (daemon down, web UI disabled, port-info missing) we log
// and fall back to a one-line stderr notice — the menu item itself
// stays clickable so the user can retry after starting the daemon.
func (t *tray) openWebUI() {
	out, err := runAplexica(t.aplexicaPath, "web", "open")
	if err != nil {
		log.Printf("open web ui: %v: %s", err, out)
		// Best-effort surface a dialog when the daemon path returned
		// a clean "web UI not running" error so the user gets a
		// human-readable hint without trawling logs.
		_ = showInfoDialog(
			i18n.T("tray_menu_open_web"),
			i18n.T("tray_tooltip_open_web_unavailable")+"\n\n"+strings.TrimSpace(string(out)),
		)
		return
	}
	// `aplexica web open` already invokes the browser; no further
	// work needed here. The CLI also prints the URL to stdout for
	// users running the command directly; capture it for the log.
	if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
		log.Printf("open web ui: %s", trimmed)
	}
}

// updateOpenWebState refreshes the "Open Aplexica" menu item's
// enabled/disabled state from the current snapshot:
//   - daemon up: enabled, label = "Open Aplexica"
//   - daemon stopped: disabled, label = "Open Aplexica"
//
// Called from apply() after lastSnap is refreshed. We can't distinguish
// "web disabled by config" from "web binding failed" at the tray level —
// both surface as a missing portinfo.json. Treating either as "web
// disabled" is acceptable in V1; the user gets a clear menu hint either
// way and the underlying CLI error surfaces in the log.
func (t *tray) updateOpenWebState() {
	if t.miOpenWeb == nil {
		return
	}
	// t.mu is held by the caller (apply); the per-field reads here
	// are part of the same critical section.
	daemonUp := t.lastSnap.DaemonAvailable
	t.miOpenWeb.SetTitle(i18n.T("tray_menu_open_web"))
	if !daemonUp {
		t.miOpenWeb.Disable()
		return
	}
	t.miOpenWeb.Enable()
}
