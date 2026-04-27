#!/usr/bin/env bash
# naozhi installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/KevinZhao/naozhi/master/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/KevinZhao/naozhi/master/install.sh | bash -s -- --uninstall
#
# Env vars:
#   NAOZHI_VERSION  pin to a specific release tag (default: latest)
#   NAOZHI_PREFIX   install prefix (default: $HOME/.local)
#
# Design notes:
#   - Entire script wraps in main() + trailing invocation so a half-downloaded
#     pipe cannot execute a truncated top-level statement. See README "Security
#     considerations" for the `curl | bash` safety model.
#   - No sudo, no writes outside $NAOZHI_PREFIX and ~/.naozhi.
#   - No rc-file mutation: if PATH lacks the install dir we print instructions
#     instead of silently patching .zshrc / .bashrc.
#   - checksum is fetched from the same release's SHA256SUMS so this script
#     itself never needs to be re-published per release.

set -euo pipefail

REPO="KevinZhao/naozhi"
BINARY="naozhi"

# ----- ANSI helpers (disabled when stdout is not a tty) ---------------------

if [ -t 1 ]; then
    BOLD=$'\033[1m'
    DIM=$'\033[2m'
    RED=$'\033[31m'
    GREEN=$'\033[32m'
    YELLOW=$'\033[33m'
    RESET=$'\033[0m'
else
    BOLD="" DIM="" RED="" GREEN="" YELLOW="" RESET=""
fi

log()  { printf '%s\n' "$*"; }
info() { printf '%s==>%s %s\n' "$BOLD" "$RESET" "$*"; }
warn() { printf '%swarn:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

# ----- Dependency checks ----------------------------------------------------

have() { command -v "$1" >/dev/null 2>&1; }

pick_downloader() {
    # Prefer curl (shared with our invocation), fall back to wget.
    # Sets global DL_CMD used later in fetch().
    if have curl; then
        DL_CMD="curl"
    elif have wget; then
        DL_CMD="wget"
    else
        die "need curl or wget to download naozhi"
    fi
}

# fetch <url> <dest>   — dest may be "-" to emit to stdout.
fetch() {
    local url="$1" dest="$2"
    case "$DL_CMD" in
        curl)
            # -f fails on HTTP errors (so we don't save a 404 HTML page).
            # -L follows redirects (GitHub Release assets go via 302).
            # -S shows error on failure even when -s is set.
            # --retry adds a modest amount of resilience.
            if [ "$dest" = "-" ]; then
                curl -fsSL --retry 3 --retry-delay 2 "$url"
            else
                curl -fsSL --retry 3 --retry-delay 2 -o "$dest" "$url"
            fi
            ;;
        wget)
            if [ "$dest" = "-" ]; then
                wget -q -O - "$url"
            else
                wget -q -O "$dest" "$url"
            fi
            ;;
    esac
}

# ----- Platform detection ---------------------------------------------------

detect_os() {
    local uname_s
    uname_s="$(uname -s)"
    case "$uname_s" in
        Darwin) echo "darwin" ;;
        Linux)  echo "linux" ;;
        # MSYS/Cygwin are out of scope for the Go binary's systemd/launchd
        # deps; refuse rather than ship a subtly broken install.
        MINGW*|MSYS*|CYGWIN*)
            die "Windows is not supported by this installer. Download the release asset manually from https://github.com/$REPO/releases"
            ;;
        *) die "unsupported OS: $uname_s" ;;
    esac
}

detect_arch() {
    # Explicit mapping: uname -m returns a menagerie of names across
    # distros/devices; map them to the GOARCH values our release assets use.
    local uname_m
    uname_m="$(uname -m)"
    case "$uname_m" in
        arm64|aarch64)   echo "arm64" ;;
        x86_64|amd64)    echo "amd64" ;;
        *) die "unsupported architecture: $uname_m (supported: arm64, amd64)" ;;
    esac
}

# ----- Version resolution ---------------------------------------------------

# resolve_version prints the release tag to install. Defaults to latest but
# honours $NAOZHI_VERSION when the caller pins a specific tag. Uses the
# `/releases/latest` redirect instead of the GitHub API so an anonymous
# request is not subject to the 60 req/hr limit.
resolve_version() {
    if [ -n "${NAOZHI_VERSION:-}" ]; then
        echo "$NAOZHI_VERSION"
        return
    fi

    local latest_url="https://github.com/$REPO/releases/latest"
    local resolved

    case "$DL_CMD" in
        curl)
            # -o /dev/null: drop body, we only need the final URL
            # -w '%{url_effective}': print the URL after following redirects
            resolved="$(curl -fsSL -o /dev/null -w '%{url_effective}' "$latest_url" 2>/dev/null || true)"
            ;;
        wget)
            # wget --spider follows redirects and prints Location headers on
            # stderr; the last one is the tag we want.
            resolved="$(wget --spider --max-redirect=5 "$latest_url" 2>&1 \
                | awk '/^Location:/ {print $2}' | tail -n1 || true)"
            ;;
    esac

    # Expected final form: https://github.com/<owner>/<repo>/releases/tag/<version>
    if [[ "$resolved" =~ /releases/tag/([^/]+)$ ]]; then
        echo "${BASH_REMATCH[1]}"
        return
    fi

    die "could not resolve latest release; pin one manually: NAOZHI_VERSION=v0.0.3 curl ... | bash"
}

# ----- Install / uninstall --------------------------------------------------

install_naozhi() {
    pick_downloader

    local prefix="${NAOZHI_PREFIX:-$HOME/.local}"
    local bin_dir="$prefix/bin"
    local install_path="$bin_dir/$BINARY"

    local os arch version
    os="$(detect_os)"
    arch="$(detect_arch)"
    version="$(resolve_version)"

    info "Installing $BINARY $version ($os/$arch)"
    log "  prefix: $prefix"

    mkdir -p "$bin_dir"

    local asset="naozhi-${os}-${arch}"
    local base="https://github.com/$REPO/releases/download/${version}"

    # Use a per-run temp dir so a failed install doesn't leave half-downloaded
    # files in the user's bin dir. trap removes it on any exit path.
    local tmp
    tmp="$(mktemp -d)"
    # shellcheck disable=SC2064  # we want $tmp expanded now, not at trap time
    trap "rm -rf '$tmp'" EXIT

    info "Downloading binary"
    fetch "$base/$asset" "$tmp/$asset"

    info "Verifying checksum"
    # checksums.txt is produced by release.yml (cat dist/*.sha256 >
    # dist/checksums.txt) with one "<hash>  <filename>" line per asset.
    fetch "$base/checksums.txt" "$tmp/checksums.txt"

    local expected
    expected="$(grep "  ${asset}\$" "$tmp/checksums.txt" | awk '{print $1}')"
    if [ -z "$expected" ]; then
        die "checksum entry for $asset missing from checksums.txt"
    fi

    local actual
    if have sha256sum; then
        actual="$(sha256sum "$tmp/$asset" | awk '{print $1}')"
    elif have shasum; then
        # macOS ships shasum (Perl), not sha256sum.
        actual="$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')"
    else
        die "need sha256sum or shasum to verify download"
    fi

    if [ "$expected" != "$actual" ]; then
        die "checksum mismatch: expected $expected, got $actual"
    fi

    info "Installing to $install_path"
    chmod +x "$tmp/$asset"
    mv "$tmp/$asset" "$install_path"

    # macOS: curl downloads do NOT get com.apple.quarantine in practice, but
    # some corporate MDM profiles tag everything in $HOME. Clearing the
    # attribute is a no-op when it's absent, so always attempt it on darwin.
    if [ "$os" = "darwin" ]; then
        xattr -d com.apple.quarantine "$install_path" 2>/dev/null || true
    fi

    log ""
    log "${GREEN}✓${RESET} $BINARY $version installed"

    # PATH hint — only when the bin dir is not already reachable. We do NOT
    # mutate shell rc files: writes to ~/.zshrc / ~/.bashrc are hard to undo
    # cleanly and surprise users who manage their shells carefully.
    case ":$PATH:" in
        *":$bin_dir:"*) : ;;
        *)
            log ""
            log "${YELLOW}!${RESET} $bin_dir is not on your PATH. Add it by running:"
            log ""
            log "    ${BOLD}echo 'export PATH=\"$bin_dir:\$PATH\"' >> ~/.zshrc${RESET}"
            log "    # (use ~/.bashrc for bash)"
            log ""
            log "  Then restart your shell or: ${BOLD}source ~/.zshrc${RESET}"
            ;;
    esac

    log ""
    log "Run ${BOLD}$BINARY${RESET} to start with defaults, or"
    log "    ${BOLD}$BINARY --help${RESET} for options."
    log ""
    log "${DIM}To uninstall:${RESET}"
    log "${DIM}  curl -fsSL https://raw.githubusercontent.com/$REPO/master/install.sh | bash -s -- --uninstall${RESET}"
}

uninstall_naozhi() {
    local prefix="${NAOZHI_PREFIX:-$HOME/.local}"
    local install_path="$prefix/bin/$BINARY"

    if [ -e "$install_path" ]; then
        rm -f "$install_path"
        log "${GREEN}✓${RESET} removed $install_path"
    else
        warn "no binary at $install_path (already uninstalled?)"
    fi

    # Deliberately do NOT delete ~/.naozhi (config, sessions, tokens,
    # per-session JSONL shim state). Uninstall is for the binary; user
    # data survives re-install. Tell the operator how to purge if desired.
    if [ -d "$HOME/.naozhi" ]; then
        log ""
        log "${DIM}User data preserved at ~/.naozhi (config, sessions, shim state).${RESET}"
        log "${DIM}To fully purge: rm -rf ~/.naozhi${RESET}"
    fi
}

# ----- Entry point ----------------------------------------------------------

main() {
    case "${1:-install}" in
        install|"") install_naozhi ;;
        --uninstall|uninstall) uninstall_naozhi ;;
        -h|--help|help)
            cat <<EOF
naozhi installer

Usage:
  curl -fsSL https://raw.githubusercontent.com/$REPO/master/install.sh | bash
  curl -fsSL https://raw.githubusercontent.com/$REPO/master/install.sh | bash -s -- --uninstall

Environment:
  NAOZHI_VERSION  pin to a release tag (e.g. v0.0.3); defaults to latest
  NAOZHI_PREFIX   install prefix (default: \$HOME/.local, binary goes to \$prefix/bin)

Examples:
  # Install latest
  curl -fsSL .../install.sh | bash

  # Install specific version
  curl -fsSL .../install.sh | NAOZHI_VERSION=v0.0.3 bash

  # Install into /opt/naozhi/bin (still no sudo as long as you own it)
  curl -fsSL .../install.sh | NAOZHI_PREFIX=/opt/naozhi bash

  # Uninstall
  curl -fsSL .../install.sh | bash -s -- --uninstall
EOF
            ;;
        *) die "unknown command: $1 (try --help)" ;;
    esac
}

# MUST be the last statement in this file. `curl | bash` streams the script
# line by line; if the connection drops mid-download the truncated script
# will be missing this invocation and therefore execute nothing.
main "$@"
