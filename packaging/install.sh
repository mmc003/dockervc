#!/bin/sh
# dockervc installer for Linux/macOS
#
# Usage (from the extracted release folder containing the dockervc binary):
#   sudo ./install.sh            # installs to /usr/local/bin
#   ./install.sh --prefix ~/bin  # installs elsewhere
set -eu

PREFIX="/usr/local/bin"
if [ "${1:-}" = "--prefix" ] && [ -n "${2:-}" ]; then
    PREFIX="$2"
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ ! -f "$SCRIPT_DIR/dockervc" ]; then
    echo "ERROR: dockervc binary not found next to install.sh ($SCRIPT_DIR)." >&2
    echo "Download the release .tar.gz, extract it fully, and run this script from inside the extracted folder." >&2
    exit 1
fi

mkdir -p "$PREFIX"
install -m 0755 "$SCRIPT_DIR/dockervc" "$PREFIX/dockervc"
echo "Installed dockervc to $PREFIX/dockervc"
"$PREFIX/dockervc" version

if ! command -v docker >/dev/null 2>&1; then
    echo "WARNING: 'docker' not found on PATH — install Docker Engine first." >&2
fi

echo
echo "Next steps:"
echo "  dockervc init                 # one-time, creates the snapshot store"
echo "  dockervc snapshot -m \"first\"  # take your first snapshot"
