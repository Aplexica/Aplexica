//go:build tray

package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// openPath opens an arbitrary filesystem path in the user's default file
// manager. macOS: open <path>; linux: xdg-open <path>; windows:
// explorer <path>. Best-effort — failures are logged by callers.
func openPath(p string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", p).Start()
	case "windows":
		return exec.Command("explorer", p).Start()
	default:
		return exec.Command("xdg-open", p).Start()
	}
}

// safeProjectID reports whether id is composed solely of characters safe to
// interpolate into a terminal command. Legitimate project IDs are
// host/owner/repo slugs or "local:" identifiers; anything outside this set
// (space, $ ` ( ) ; & | < > etc.) indicates a hostile or malformed ID that
// must not reach openTerminalRun.
func safeProjectID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '/', r == ':', r == '@':
		default:
			return false
		}
	}
	return true
}

// runAplexica runs `aplexica <args...>` synchronously and returns the
// combined stdout+stderr. Used for control actions like `daemon stop`
// where we want to await the result.
func runAplexica(aplexicaPath string, args ...string) ([]byte, error) {
	return exec.Command(aplexicaPath, args...).CombinedOutput()
}

// askDuration prompts the user for a Go-style duration string via a
// platform-native input dialog and returns the parsed result. v0.49.0;
// powers the "Pause sync for… → Custom…" menu item.
//
// Returns (0, errCancelled) if the user dismissed the dialog without
// entering a value. Returns (0, parseErr) if time.ParseDuration rejects
// the input.
//
//   - darwin  : osascript "display dialog ... default answer"; parses
//     "text returned:..." from osascript stdout.
//   - linux   : kdialog --inputbox → zenity --entry → fallback to stdin
//     read (which only works when launched from a terminal).
//   - windows : PowerShell Microsoft.VisualBasic.Interaction.InputBox.
func askDuration(prompt, defaultStr string) (time.Duration, error) {
	resp, err := promptString(prompt, defaultStr)
	if err != nil {
		return 0, err
	}
	resp = strings.TrimSpace(resp)
	if resp == "" {
		return 0, errPromptCancelled
	}
	d, err := time.ParseDuration(resp)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w (try 30m, 2h, 1h30m)", resp, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be > 0, got %v", d)
	}
	return d, nil
}

// errPromptCancelled is the sentinel returned when the user dismisses
// the prompt without entering a value. Callers should treat it as a
// soft no-op (don't surface as an error to the user).
var errPromptCancelled = fmt.Errorf("prompt cancelled")

// promptString shows a platform-native text-input dialog and returns
// the entered text. The dialog title is always "Aplexica" so the user
// can recognize the source across the various platform dialog styles.
func promptString(prompt, defaultStr string) (string, error) {
	const title = "Aplexica"
	switch runtime.GOOS {
	case "darwin":
		script := `display dialog ` + osascriptQuote(prompt) +
			` default answer ` + osascriptQuote(defaultStr) +
			` with title ` + osascriptQuote(title) +
			` buttons {"Cancel","OK"} default button "OK"`
		out, err := exec.Command("osascript", "-e", script).Output()
		if err != nil {
			// osascript exits with status 1 when the user clicks Cancel.
			return "", errPromptCancelled
		}
		// Output format: "button returned:OK, text returned:1h30m\n"
		s := string(out)
		const marker = "text returned:"
		if i := strings.Index(s, marker); i >= 0 {
			return strings.TrimRight(s[i+len(marker):], "\n"), nil
		}
		return "", errPromptCancelled
	case "windows":
		ps := `Add-Type -AssemblyName Microsoft.VisualBasic | Out-Null; ` +
			`Write-Output ([Microsoft.VisualBasic.Interaction]::InputBox(` +
			psSingleQuote(prompt) + `, ` + psSingleQuote(title) + `, ` +
			psSingleQuote(defaultStr) + `))`
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
		if err != nil {
			return "", errPromptCancelled
		}
		return strings.TrimRight(string(out), "\r\n"), nil
	default:
		if _, err := exec.LookPath("kdialog"); err == nil {
			out, err := exec.Command("kdialog", "--title", title,
				"--inputbox", prompt, defaultStr).Output()
			if err != nil {
				return "", errPromptCancelled
			}
			return strings.TrimRight(string(out), "\n"), nil
		}
		if _, err := exec.LookPath("zenity"); err == nil {
			out, err := exec.Command("zenity", "--entry",
				"--title="+title, "--text="+prompt,
				"--entry-text="+defaultStr).Output()
			if err != nil {
				return "", errPromptCancelled
			}
			return strings.TrimRight(string(out), "\n"), nil
		}
		return "", fmt.Errorf("no input-dialog helper available (install kdialog or zenity)")
	}
}

// showInfoDialog displays a small modal info dialog with the given
// title + body. Best-effort across platforms (v0.40.0):
//   - darwin  : osascript "display dialog"
//   - linux   : zenity --info; falls back to notify-send when zenity
//     is absent (notify-send produces a toast, not a modal,
//     but at least surfaces the message)
//   - windows : PowerShell System.Windows.Forms.MessageBox
//
// Returns nil even if the dialog is closed by the user — failures
// only fire when the helper binary is missing entirely.
func showInfoDialog(title, body string) error {
	switch runtime.GOOS {
	case "darwin":
		// osascript "display dialog" syntax. Both fields are quoted as
		// AppleScript strings; embedded double-quotes are doubled.
		script := fmt.Sprintf(
			`display dialog %s with title %s buttons {"OK"} default button "OK"`,
			osascriptQuote(body), osascriptQuote(title))
		return exec.Command("osascript", "-e", script).Start()
	case "windows":
		// MessageBox.Show needs System.Windows.Forms loaded. Use
		// Add-Type to make sure it's available in the session.
		ps := fmt.Sprintf(
			`Add-Type -AssemblyName System.Windows.Forms | Out-Null; `+
				`[System.Windows.Forms.MessageBox]::Show(%s, %s) | Out-Null`,
			psSingleQuote(body), psSingleQuote(title))
		return exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Start()
	default:
		// Linux dialog fallback chain. Order chosen to match the
		// likeliest desktop environment first:
		//   1. kdialog  — KDE Plasma's native helper, ships by default
		//                 on KDE; identical look-and-feel.
		//   2. zenity   — GNOME's GTK-based helper, also widely installed
		//                 outside GNOME on Debian/Ubuntu desktops.
		//   3. gdialog  — older GTK helper, occasionally present on
		//                 minimal installs.
		//   4. xmessage — X11's plain-text fallback; almost always
		//                 available on X-equipped systems.
		//   5. notify-send — toast, not modal, but at least surfaces
		//                 the message when no dialog tool exists.
		// Returns an error only when none of the five tools are on PATH.
		if _, err := exec.LookPath("kdialog"); err == nil {
			return exec.Command("kdialog", "--title", title,
				"--msgbox", body).Start()
		}
		if _, err := exec.LookPath("zenity"); err == nil {
			return exec.Command("zenity", "--info",
				"--title="+title, "--text="+body).Start()
		}
		if _, err := exec.LookPath("gdialog"); err == nil {
			return exec.Command("gdialog", "--title", title,
				"--msgbox", body).Start()
		}
		if _, err := exec.LookPath("xmessage"); err == nil {
			return exec.Command("xmessage", "-center",
				"-title", title, body).Start()
		}
		if _, err := exec.LookPath("notify-send"); err == nil {
			return exec.Command("notify-send", title, body).Start()
		}
		return fmt.Errorf("no dialog helper available (install kdialog, zenity, gdialog, xmessage, or libnotify)")
	}
}

// osascriptQuote produces an AppleScript string literal for embedding
// in `osascript -e`. AppleScript string syntax uses double quotes, with
// internal double-quotes escaped via backslash. The outer command-line
// double-quotes are NOT a concern because we pass the whole script as
// one argv element to exec.Command.
func osascriptQuote(s string) string {
	var b []byte
	b = append(b, '"')
	for _, c := range s {
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		default:
			b = append(b, byte(c))
		}
	}
	b = append(b, '"')
	return string(b)
}

// psSingleQuote mirrors internal/trayinstall.psQuote: wraps in
// PowerShell single-quotes with internal single-quotes doubled. Kept
// duplicated locally to avoid the tray binary importing internal/.
func psSingleQuote(s string) string {
	out := "'"
	for _, c := range s {
		if c == '\'' {
			out += "''"
		} else {
			out += string(c)
		}
	}
	out += "'"
	return out
}
