#!/bin/sh
# dockervc uninstaller for Linux/macOS
#
# Removes the dockervc executable(s). Your snapshot store is intentionally
# KEPT — snapshots are your data, so deleting them stays a decision you make
# by hand (the script prints where each store lives, and the rm command).
#
# Usage (from the extracted release folder, or anywhere else):
#   sudo ./uninstall.sh            # removes every dockervc on PATH plus
#                                  # /usr/local/bin (the install.sh default)
#   ./uninstall.sh --prefix ~/bin  # only remove a custom-prefix install
set -eu

PREFIX=""
if [ "${1:-}" = "--prefix" ] && [ -n "${2:-}" ]; then
    PREFIX="$2"
elif [ -n "${1:-}" ]; then
    echo "usage: $0 [--prefix <dir>]" >&2
    exit 1
fi

SUDO_HINT="sudo $0${PREFIX:+ --prefix $PREFIX}"

# remove_bin <path>: drop one dockervc binary, or explain how to retry.
removed=0
remove_bin() {
    if [ ! -f "$1" ]; then
        return 0
    fi
    if rm -f "$1" 2>/dev/null; then
        echo "Removed $1"
        removed=$((removed + 1))
    else
        echo "ERROR: cannot remove $1 — try: $SUDO_HINT" >&2
        exit 1
    fi
}

# walk_dir <dir>: remove dir/dockervc once, even if PATH lists the dir twice.
seen=" "
walk_dir() {
    dir="${1%/}"
    case "$seen" in *" $dir "*) return 0 ;; esac
    seen="$seen $dir "
    remove_bin "$dir/dockervc"
}

if [ -n "$PREFIX" ]; then
    remove_bin "${PREFIX%/}/dockervc"
else
    oldIFS=$IFS
    IFS=:
    for dir in $PATH:/usr/local/bin; do
        IFS=$oldIFS
        if [ -z "$dir" ]; then
            dir=.
        fi
        if [ -d "$dir" ]; then
            walk_dir "$dir"
        fi
    done
    IFS=$oldIFS
fi

echo
if [ "$removed" -gt 0 ]; then
    echo "dockervc removed. (If your shell still finds it, run: hash -r)"
else
    echo "No dockervc executable found — nothing removed."
fi

# Report (never delete) snapshot stores. Under sudo, HOME may point at
# root's home — find the invoking user's home too, best effort.
real_home=$HOME
if [ -n "${SUDO_USER:-}" ] && [ "$(id -u)" = "0" ]; then
    if h=$(getent passwd "$SUDO_USER" 2>/dev/null | cut -d: -f6) && [ -n "$h" ]; then
        real_home=$h
    elif h=$(dscl . -read "/Users/$SUDO_USER" NFSHomeDirectory 2>/dev/null | awk '{print $NF}') && [ -n "$h" ]; then
        real_home=$h
    fi
fi

report=""
stores_seen=" "
note_store() {
    case "$stores_seen" in *" $1 "*) return 0 ;; esac
    stores_seen="$stores_seen $1 "
    if [ -d "$1" ]; then
        size=$(du -sh "$1" 2>/dev/null | awk '{print $1}' || true)
        if [ -z "$size" ]; then
            size="?"
        fi
        report="${report}  $1  ($size) — kept, that is your data; to delete: rm -rf $1
"
    fi
}
note_store "${DOCKERVC_HOME:-}"
note_store /var/lib/dockervc
note_store "$HOME/.dockervc"
note_store "$real_home/.dockervc"

echo
if [ -n "$report" ]; then
    echo "Snapshot stores left in place:"
    printf '%s' "$report"
else
    echo "No snapshot store found — machine is clean."
fi
