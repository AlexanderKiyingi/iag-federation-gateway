#!/usr/bin/env sh
# Copy shared/platform-go from the meta-repo into third_party/ for standalone builds.
# Run from iag-dms repo root:
#   sh scripts/sync-platform-go.sh
# Or from meta-repo:
#   sh services/operations/dms/scripts/sync-platform-go.sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
SRC="${IAG_PLATFORM_GO_SRC:-}"

if [ -z "$SRC" ]; then
  if [ -d "$ROOT/../../../shared/platform-go" ]; then
    SRC="$ROOT/../../../shared/platform-go"
  elif [ -d "$ROOT/../../shared/platform-go" ]; then
    SRC="$ROOT/../../shared/platform-go"
  else
    echo "Set IAG_PLATFORM_GO_SRC to the shared/platform-go directory" >&2
    exit 1
  fi
fi

DEST="$ROOT/third_party/platform-go"
mkdir -p "$DEST"
rm -rf "$DEST"/*
cp -R "$SRC"/. "$DEST/"
echo "Synced platform-go from $SRC to $DEST"
