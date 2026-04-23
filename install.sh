#!/usr/bin/env sh

# Copyright 2026 The Kubermatic Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Downloads and installs kubermatic-ee-downloader from the latest
# (or a pinned) GitHub release.
#
# Usage:
#   curl -sfL https://raw.githubusercontent.com/kubermatic/kubermatic-ee-downloader/main/install.sh | sh
#
# Environment variables:
#   VERSION     - pin a specific release tag, e.g. "v1.2.0" (default: latest)
#   INSTALL_DIR - directory to install the binary into   (default: current directory)

set -eu

# ─── Configuration ────────────────────────────────────────────────────────────

BINARY_NAME="kubermatic-ee-downloader"
GITHUB_REPO="kubermatic/kubermatic-ee-downloader"
INSTALL_DIR="${INSTALL_DIR:-.}"

# ─── Helpers ──────────────────────────────────────────────────────────────────

log()  { printf '[%s] %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >&2; }
fail() { printf '[%s] FATAL  %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >&2; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || fail "'$1' is required but not installed."
}

# ─── Dependency check ─────────────────────────────────────────────────────────

need curl
need tar
need sha256sum

# ─── OS / arch detection ──────────────────────────────────────────────────────

OS="$(uname -s)"
ARCH="$(uname -m)"

case "$OS" in
  Linux)  OS="linux"  ;;
  Darwin) OS="darwin" ;;
  *)      fail "Unsupported operating system: $OS" ;;
esac

case "$ARCH" in
  x86_64)          ARCH="amd64" ;;
  aarch64 | arm64) ARCH="arm64" ;;
  *)               fail "Unsupported architecture: $ARCH" ;;
esac

log "Platform        : ${OS}_${ARCH}"

# ─── Version resolution ───────────────────────────────────────────────────────

if [ -z "${VERSION:-}" ]; then
  log "Resolving latest release ..."
  VERSION="$(
    curl -sfL "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" \
      | grep '"tag_name"' \
      | head -1 \
      | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
  )"
  [ -n "$VERSION" ] || fail "Unable to resolve latest release tag. Set VERSION explicitly and retry."
  log "Resolved version: $VERSION"
else
  log "Pinned version  : $VERSION"
fi

# ─── Download ─────────────────────────────────────────────────────────────────

BASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}"
ARCHIVE="${BINARY_NAME}_${OS}_${ARCH}.tar.gz"
CHECKSUMS="checksums.txt"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

log "Downloading ${ARCHIVE} ..."
curl -sfL --retry 3 "${BASE_URL}/${ARCHIVE}" -o "${TMPDIR}/${ARCHIVE}" \
  || fail "Download failed — ${BASE_URL}/${ARCHIVE}"

log "Downloading checksums ..."
curl -sfL --retry 3 "${BASE_URL}/${CHECKSUMS}" -o "${TMPDIR}/${CHECKSUMS}" \
  || fail "Download failed — ${BASE_URL}/${CHECKSUMS}"

# ─── Checksum verification ────────────────────────────────────────────────────

log "Verifying checksum ..."
(
  cd "$TMPDIR"
  grep "${ARCHIVE}" "${CHECKSUMS}" | sha256sum --check --status -
) || fail "Checksum verification failed — archive may be corrupted or tampered with"
log "Checksum OK"

# ─── Extract & install ────────────────────────────────────────────────────────

log "Extracting ..."
tar -xzf "${TMPDIR}/${ARCHIVE}" -C "$TMPDIR"

BINARY="${TMPDIR}/${BINARY_NAME}"
[ -f "$BINARY" ] || fail "Binary '${BINARY_NAME}' not found in archive"
chmod +x "$BINARY"

log "Installing to ${INSTALL_DIR}/${BINARY_NAME} ..."
mv "$BINARY" "${INSTALL_DIR}/${BINARY_NAME}"

# ─── Done ─────────────────────────────────────────────────────────────────────

log "Done — ${BINARY_NAME} ${VERSION} installed to ${INSTALL_DIR}/${BINARY_NAME}"
