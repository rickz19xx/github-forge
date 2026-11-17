#!/bin/sh
# build.sh — rebuild the committed linux/amd64 binary from go/, byte for byte.
#
# The entry runs `node main.ts`, which starts ./forge. This is the command that produced the
# committed file: -trimpath keeps the source paths out, -s -w drops the symbol table and DWARF,
# -buildvcs=false keeps a VCS stamp (which embeds the build's own clock) out so the output is
# reproducible, and CGO_ENABLED=0 makes it statically linked.
set -eu
cd "$(dirname "$0")"

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -buildvcs=false -trimpath -ldflags="-s -w" -o forge ./go

echo "built ./forge ($(wc -c < forge) bytes)"
