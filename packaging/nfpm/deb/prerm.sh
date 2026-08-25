#!/bin/sh
# prerm — runs before dpkg removes the aplexica .deb. Best-effort stop
# of the user services. Leaves ~/.aplexica/ data intact.

set -e

case "$1" in
    remove|upgrade|deconfigure)
        # `systemctl --global` only supports enable/disable/mask/preset — it
        # answers "--global is not supported for this operation." for stop, and
        # the previous code swallowed that, so apt removed /usr/bin/aplexica
        # out from under a running daemon. Enumerate live sessions instead.
        if command -v systemctl >/dev/null 2>&1 && command -v loginctl >/dev/null 2>&1; then
            loginctl list-users --no-legend 2>/dev/null | while read -r _uid user _rest; do
                [ -n "$user" ] || continue
                systemctl --user --machine="${user}@.host" \
                    stop aplexicatray.service aplexicad.service 2>/dev/null || true
            done
        fi

        cat <<'NOTICE'

If an Aplexica daemon or tray is still running for a logged-in user, stop it
from that user's own session before completing removal:

  aplexica daemon uninstall
  aplexica tray uninstall

Data under ~/.aplexica/ is left in place.

NOTICE
        ;;
    failed-upgrade)
        ;;
esac

exit 0
