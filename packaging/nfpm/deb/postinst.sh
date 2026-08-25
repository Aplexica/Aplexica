#!/bin/sh
# postinst — runs after dpkg installs the aplexica .deb on Debian/Ubuntu.
# Reloads the systemd user manager so new units are visible, but does not
# enable them automatically; user opts in via `aplexica setup` or
# `systemctl --user enable --now aplexicad.service`.

set -e

case "$1" in
    configure)
        # Reload user units for every active user session, best-effort.
        # `systemctl --global daemon-reload` is not a supported operation
        # either. Reload each live user manager instead; a user whose manager
        # is not running picks the new units up at next login regardless.
        if command -v systemctl >/dev/null 2>&1 && command -v loginctl >/dev/null 2>&1; then
            loginctl list-users --no-legend 2>/dev/null | while read -r _uid user _rest; do
                [ -n "$user" ] || continue
                systemctl --user --machine="${user}@.host" daemon-reload 2>/dev/null || true
            done
        fi

        # Print a friendly notice.
        cat <<'EOF'

Aplexica installed.

Next steps:
  aplexica setup                # interactive: enable tray, web UI, open browser

Or wire up services manually for the current user:
  systemctl --user enable --now aplexicad.service

The daemon starts the local web UI and launches the tray when a graphical
session is available. Local web UI: click the tray icon → Open Aplexica, or run
`aplexica web open`.
User data lives in ~/.aplexica/. Logs at ~/.aplexica/logs/.

EOF
        ;;
    abort-upgrade|abort-remove|abort-deconfigure)
        ;;
esac

exit 0
