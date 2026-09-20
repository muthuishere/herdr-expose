#!/usr/bin/env bash
#
# Build herdr-expose: the React app first, then the Go binary with that app
# embedded inside it (see docs/adr/0001).
#
# With a Go + node toolchain this builds from source. WITHOUT one it falls back
# to downloading the matching release asset from GitHub, so `herdr plugin
# install` works on a machine that has neither.
#
#   ./scripts/build.sh                 source if the toolchain is there,
#                                      else a release asset -> bin/herdr-expose
#   ./scripts/build.sh --source        build from source; fail if go/npm missing
#   ./scripts/build.sh --download      always fetch the release asset
#   ./scripts/build.sh --release       build the full matrix   -> dist/
#   ./scripts/build.sh --skip-web      Go only (web/dist must already exist)
#   ./scripts/build.sh --no-link       build only; link nothing into $HOME
#
# ONE INSTALL. Once the binary exists this script finishes the job that
# "installed" actually means to a user, and PRINTS every path it touched:
#
#   ~/.local/bin/herdr-expose     -> this checkout's binary, so the CLI is on
#                                    PATH. A plugin install otherwise leaves it
#                                    inside Herdr's managed checkout while every
#                                    example says `herdr-expose <verb>`.
#   ~/.claude/skills/herdr-share  -> this checkout's skill/, so an agent can
#                                    drive it. Also ~/.agents/skills, but only
#                                    when that directory already exists.
#
# Both are SYMLINKS into the checkout, so a rebuild or a git pull updates them.
# Neither is silent, neither clobbers anything real, and neither can fail the
# build. `--no-link` (or HERDR_EXPOSE_NO_LINK=1) skips both; `herdr-expose
# skill uninstall` reverses the skill half.
#
# Environment:
#   VERSION     version string baked into the binary (default: git describe)
#   GO          go binary to use    (default: go)
#   NPM         npm binary to use   (default: npm)
#   REPO        OWNER/REPO to download release assets from
#               (default: muthuishere/herdr-expose)
#   REPO_API    GitHub API base for that repo (default: https://api.github.com)
#   HERDR_EXPOSE_NO_LINK=1  same as --no-link
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GO="${GO:-go}"
NPM="${NPM:-npm}"
REPO="${REPO:-muthuishere/herdr-expose}"
PKG="./cmd/herdr-expose"
BIN_NAME="herdr-expose"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}"

RELEASE=0
SKIP_WEB=0
MODE=auto
NO_LINK="${HERDR_EXPOSE_NO_LINK:-0}"
for arg in "$@"; do
  case "$arg" in
    --release)  RELEASE=1 ;;
    --skip-web) SKIP_WEB=1 ;;
    --source)   MODE=source ;;
    --download) MODE=download ;;
    --no-link)  NO_LINK=1 ;;
    -h|--help)  sed -n '2,42p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 not found on PATH. $2"; }
have() { command -v "$1" >/dev/null 2>&1; }

# -------------------------------------------------------- release asset -----
# No toolchain? Take the prebuilt binary for this platform from the latest
# GitHub release, and check its SHA-256 against the published SHA256SUMS. This
# is a download of a third-party binary: it is verified, but it is still only as
# trustworthy as the repo you pointed it at.
download_release() {
  need curl "Needed to download a release asset."
  need tar  "Needed to unpack a release asset."

  local os arch sums_url asset_url tmp file want got ext=""
  case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    # This script only ever runs under a POSIX shell. On Windows that means Git
    # Bash / MSYS2, which is what `herdr plugin install` uses there, and which
    # reports these.
    MINGW*|MSYS*|CYGWIN*) os=windows; ext=".exe" ;;
    *) die "no prebuilt binary for $(uname -s); install Go 1.22+ and node 20+ and rebuild from source." ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) die "no prebuilt binary for $(uname -m); install Go 1.22+ and node 20+ and rebuild from source." ;;
  esac

  log "fetching the latest ${REPO} release for ${os}/${arch}"
  local api="${REPO_API:-https://api.github.com}/repos/${REPO}/releases/latest"
  local json code
  json="$(curl -sSL -w '\n%{http_code}' -H 'Accept: application/vnd.github+json' "$api" 2>/dev/null || true)"
  code="$(printf '%s' "$json" | tail -n1)"
  json="$(printf '%s' "$json" | sed '$d')"
  case "$code" in
    200) ;;
    404) die "${REPO} has no published release yet, so there is no prebuilt binary to install. Install Go 1.22+ and node 20+ and run ./scripts/build.sh --source." ;;
    *)   die "cannot read $api (HTTP ${code:-none}). Install Go 1.22+ and node 20+ to build from source instead." ;;
  esac

  asset_url="$(printf '%s' "$json" \
    | grep -o '"browser_download_url": *"[^"]*"' | cut -d'"' -f4 \
    | grep "_${os}_${arch}\.tar\.gz$" | head -n1 || true)"
  sums_url="$(printf '%s' "$json" \
    | grep -o '"browser_download_url": *"[^"]*"' | cut -d'"' -f4 \
    | grep '_SHA256SUMS$' | head -n1 || true)"

  [ -n "$asset_url" ] || die "no ${os}/${arch} asset in the latest ${REPO} release. Install Go 1.22+ and node 20+ and build from source."
  [ -n "$sums_url" ]  || die "the latest ${REPO} release has no SHA256SUMS; refusing to install an unverified binary."

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  file="$(basename "$asset_url")"
  curl -fsSL -o "$tmp/$file" "$asset_url"
  curl -fsSL -o "$tmp/SHA256SUMS" "$sums_url"

  want="$(grep -E "(^|[ ./])${file}\$" "$tmp/SHA256SUMS" | awk '{print $1}' | head -n1 || true)"
  [ -n "$want" ] || die "$file is not listed in the release SHA256SUMS."
  if have shasum; then
    got="$(shasum -a 256 "$tmp/$file" | awk '{print $1}')"
  else
    need sha256sum "Needed to verify the download."
    got="$(sha256sum "$tmp/$file" | awk '{print $1}')"
  fi
  [ "$want" = "$got" ] || die "checksum mismatch for $file (expected $want, got $got)."
  log "checksum ok"

  mkdir -p bin
  tar -xzf "$tmp/$file" -C bin "${BIN_NAME}${ext}"
  chmod +x "bin/${BIN_NAME}${ext}" 2>/dev/null || true
  log "installed bin/${BIN_NAME}${ext} from $file"
  "./bin/${BIN_NAME}${ext}" version || true
}

# ------------------------------------------------------ the rest of it -----
# A binary in a directory nobody has on PATH, and a skill nobody knows is
# there, are both "installed" only in the sense that the bytes exist. This is
# the step that makes one command mean one command.
#
# Everything here is best-effort and LOUD: it prints what it did, it never
# clobbers a real file, and a failure warns instead of failing the build —
# `herdr plugin install` aborts on a non-zero exit, and a skill link is not a
# reason to lose the plugin.
link_into_home() {
  if [ "$NO_LINK" = "1" ]; then
    log "--no-link: nothing linked into \$HOME (do it later with: $BIN_NAME skill install)"
    return 0
  fi

  local bindir="$HOME/.local/bin"
  local link="$bindir/$BIN_NAME"
  local target="$REPO_ROOT/bin/$BIN_NAME"

  log "putting ${BIN_NAME} on PATH and installing the agent skill"

  mkdir -p "$bindir" 2>/dev/null || true
  if [ -L "$link" ]; then
    local points
    points="$(readlink "$link")"
    if [ "$points" = "$target" ]; then
      echo "  $link -> $target (already)"
    else
      ln -sfn "$target" "$link" && echo "  $link -> $target (repointed, was $points)"
    fi
  elif [ -e "$link" ]; then
    warn "$link exists and is not a symlink; leaving it alone. Call $target directly, or move that file."
  else
    ln -s "$target" "$link" && echo "  $link -> $target"
  fi

  case ":$PATH:" in
    *":$bindir:"*) ;;
    *) warn "$bindir is not on your PATH — add it, or call $target directly." ;;
  esac

  # The skill half lives in the binary (`skill install`), so there is exactly
  # one implementation of "where does skill/ belong": this script, the
  # hex:install-skill action and anyone typing the verb all run the same code.
  "$target" skill install || warn "the agent skill was not linked (see above). Fix that, then run: $BIN_NAME skill install"
}

# ---------------------------------------------------------------- web ------
# node is a BUILD-time dependency only. Nothing in the shipped binary needs it.
build_web() {
  need "$NPM" "Install node 20+ to build the web app, or run with --skip-web."
  log "building web app (npm ci && npm run build)"
  ( cd web && "$NPM" ci && "$NPM" run build )
  [ -f web/dist/index.html ] || die "web build produced no web/dist/index.html"
}

# ------------------------------------------------------------- dispatch ----
# Decide source vs download before touching anything.
if [ "$MODE" = download ]; then
  [ "$RELEASE" -eq 0 ] || die "--download and --release are mutually exclusive."
  download_release
  link_into_home
  exit 0
fi

if [ "$MODE" = auto ] && [ "$RELEASE" -eq 0 ]; then
  missing=""
  have "$GO" || missing="Go 1.22+"
  if [ "$SKIP_WEB" -eq 0 ] && ! have "$NPM" && [ ! -f web/dist/index.html ]; then
    missing="${missing:+$missing and }node 20+"
  fi
  if [ -n "$missing" ]; then
    warn "$missing not found on PATH; falling back to a published release binary."
    download_release
    link_into_home
    exit 0
  fi
fi

if [ "$SKIP_WEB" -eq 0 ]; then
  build_web
else
  log "skipping web build (--skip-web)"
  [ -f web/dist/index.html ] || die "web/dist/index.html missing; cannot --skip-web"
fi

# ----------------------------------------------------------------- go ------
need "$GO" "Install Go 1.22+ from https://go.dev/dl/, or run ./scripts/build.sh --download."

# CGO_ENABLED=0 gives a static binary and clean cross-compilation.
# -trimpath keeps absolute build paths out of the binary.
go_build() {
  local goos="$1" goarch="$2" out="$3"
  # Windows will not execute a file without the extension, and CreateProcess
  # only appends .exe for a command with no extension at all -- so the file on
  # disk has to be herdr-expose.exe even though the manifest says
  # ./bin/herdr-expose.
  case "$goos" in windows) out="${out}.exe" ;; esac
  mkdir -p "$(dirname "$out")"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    "$GO" build -trimpath -ldflags "$LDFLAGS" -o "$out" "$PKG"
}

if [ "$RELEASE" -eq 1 ]; then
  log "release matrix, version ${VERSION}"
  rm -rf dist
  # windows/arm64 is not an afterthought: Windows on ARM is the shape of the
  # test machine this port was verified on.
  for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
    goos="${target%/*}"
    goarch="${target#*/}"
    ext=""; case "$goos" in windows) ext=".exe" ;; esac
    out="dist/${BIN_NAME}_${VERSION}_${goos}_${goarch}/${BIN_NAME}"
    log "  ${goos}/${goarch}"
    go_build "$goos" "$goarch" "$out"
    ( cd "$(dirname "$out")" && tar -czf "../${BIN_NAME}_${VERSION}_${goos}_${goarch}.tar.gz" "${BIN_NAME}${ext}" )
  done
  # Bare filenames, so `shasum -a 256 -c <file>` works from inside dist/.
  # Linux CI has sha256sum but not always shasum; macOS is the other way round.
  if have shasum; then
    ( cd dist && shasum -a 256 ./*.tar.gz | sed 's|\./||' > "${BIN_NAME}_${VERSION}_SHA256SUMS" )
  else
    need sha256sum "Needed to checksum the release artifacts."
    ( cd dist && sha256sum ./*.tar.gz | sed 's|\./||' > "${BIN_NAME}_${VERSION}_SHA256SUMS" )
  fi
  log "release artifacts in dist/"
  ls -1 dist/*.tar.gz
else
  log "building bin/${BIN_NAME}, version ${VERSION}"
  host_os="$("$GO" env GOOS)"
  go_build "$host_os" "$("$GO" env GOARCH)" "bin/${BIN_NAME}"
  case "$host_os" in
    windows)
      # No symlinks and no ~/.local/bin convention on Windows; linking there
      # would create a broken link and print advice that does not apply.
      log "built bin/${BIN_NAME}.exe"
      log "add $(pwd)/bin to your PATH, or call bin\\${BIN_NAME}.exe directly"
      "./bin/${BIN_NAME}.exe" skill install || warn "the agent skill was not linked; run: ${BIN_NAME} skill install"
      ;;
    *)
      log "built bin/${BIN_NAME}"
      link_into_home
      ;;
  esac
fi
