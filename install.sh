#!/usr/bin/env sh
set -eu

# codeq install script
#
# Default behavior: download a pre-built binary from GitHub Releases.
# Fallback: clone the repository and `go build`.
#
# Usage (typical):
#   curl -fsSL https://raw.githubusercontent.com/osvaldoandrade/codeq/main/install.sh | sh
#
# Overrides:
#   CODEQ_INSTALL_MODE=auto|binary|source   # default: auto (binary, fall back to source)
#   CODEQ_INSTALL_REPO=osvaldoandrade/codeq # owner/repo on github.com
#   CODEQ_INSTALL_REF=vX.Y.Z|latest         # release tag (binary mode) or git ref (source mode)
#   CODEQ_INSTALL_DIR=/custom/bin           # destination directory
#   CODEQ_GITHUB_TOKEN=<pat>                # optional, raises GitHub API rate limit
#   CODEQ_INSTALL_NO_PROFILE=1              # skip shell-profile PATH updates

repo="${CODEQ_INSTALL_REPO:-osvaldoandrade/codeq}"
ref="${CODEQ_INSTALL_REF:-latest}"
mode="${CODEQ_INSTALL_MODE:-auto}"

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "[codeq] error: missing required command: $1" >&2
    exit 1
  fi
}

have_cmd() {
  command -v "$1" >/dev/null 2>&1
}

mktemp_dir() {
  if have_cmd mktemp; then
    mktemp -d 2>/dev/null && return 0
    mktemp -d -t codeq-install 2>/dev/null && return 0
    mktemp -d "${TMPDIR:-/tmp}/codeq-install.XXXXXX" 2>/dev/null && return 0
  fi
  dir="${TMPDIR:-/tmp}/codeq-install.$$"
  mkdir -p "$dir"
  echo "$dir"
}

detect_os() {
  case "$(uname -s 2>/dev/null || echo unknown)" in
    Linux*)   echo "linux" ;;
    Darwin*)  echo "darwin" ;;
    MINGW*|MSYS*|CYGWIN*) echo "windows" ;;
    *)        echo "" ;;
  esac
}

detect_arch() {
  case "$(uname -m 2>/dev/null || echo unknown)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) echo "" ;;
  esac
}

detect_exe_suffix() {
  if [ "$(detect_os)" = "windows" ]; then
    echo ".exe"
  else
    echo ""
  fi
}

detect_install_dir() {
  if [ -n "${CODEQ_INSTALL_DIR:-}" ]; then
    echo "$CODEQ_INSTALL_DIR"
    return 0
  fi

  # Prefer a writable directory already in PATH.
  old_ifs="$IFS"
  IFS=":"
  for d in $PATH; do
    if [ -n "$d" ] && [ -d "$d" ] && [ -w "$d" ]; then
      IFS="$old_ifs"
      echo "$d"
      return 0
    fi
  done
  IFS="$old_ifs"

  if [ -n "${HOME:-}" ]; then
    if [ -d "$HOME/.local/bin" ] || mkdir -p "$HOME/.local/bin" 2>/dev/null; then
      if [ -w "$HOME/.local/bin" ]; then
        echo "$HOME/.local/bin"
        return 0
      fi
    fi
    if [ -d "$HOME/bin" ] || mkdir -p "$HOME/bin" 2>/dev/null; then
      if [ -w "$HOME/bin" ]; then
        echo "$HOME/bin"
        return 0
      fi
    fi
  fi

  if [ -d "/usr/local/bin" ] && [ -w "/usr/local/bin" ]; then
    echo "/usr/local/bin"
    return 0
  fi

  echo "/usr/local/bin"
}

append_path_line() {
  file="$1"
  dir="$2"
  line="export PATH=\"$dir:\$PATH\""
  if have_cmd grep; then
    if [ -f "$file" ] && grep -F "$dir" "$file" >/dev/null 2>&1; then
      return 0
    fi
  fi
  mkdir -p "$(dirname "$file")" >/dev/null 2>&1 || true
  {
    echo ""
    echo "# Added by codeq installer"
    echo "$line"
  } >>"$file"
}

# Download a URL to a file. Uses curl if available, otherwise wget.
# Adds GitHub bearer auth when CODEQ_GITHUB_TOKEN or GITHUB_TOKEN is set.
http_download() {
  url="$1"
  dest="$2"
  accept="${3:-}"

  tok="${CODEQ_GITHUB_TOKEN:-${GITHUB_TOKEN:-}}"

  if have_cmd curl; then
    set -- curl -fsSL -o "$dest" \
      -H "User-Agent: codeq-install-sh"
    if [ -n "$accept" ]; then
      set -- "$@" -H "Accept: $accept"
    fi
    if [ -n "$tok" ]; then
      set -- "$@" -H "Authorization: Bearer $tok"
    fi
    set -- "$@" "$url"
    "$@"
    return $?
  fi

  if have_cmd wget; then
    set -- wget -q -O "$dest" \
      --header="User-Agent: codeq-install-sh"
    if [ -n "$accept" ]; then
      set -- "$@" --header="Accept: $accept"
    fi
    if [ -n "$tok" ]; then
      set -- "$@" --header="Authorization: Bearer $tok"
    fi
    set -- "$@" "$url"
    "$@"
    return $?
  fi

  echo "[codeq] error: neither curl nor wget is available" >&2
  return 1
}

# Resolve a release tag from "latest" (or pass through a pinned tag).
resolve_release_tag() {
  given="$1"
  if [ "$given" != "latest" ] && [ -n "$given" ]; then
    echo "$given"
    return 0
  fi

  tmpjson="$(mktemp_dir)/release.json"
  if ! http_download \
      "https://api.github.com/repos/${repo}/releases/latest" \
      "$tmpjson" \
      "application/vnd.github+json"; then
    return 1
  fi

  # Extract tag_name without requiring jq. Match the first occurrence.
  if have_cmd sed; then
    tag="$(sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "$tmpjson" | head -n 1)"
    if [ -n "$tag" ]; then
      echo "$tag"
      return 0
    fi
  fi
  return 1
}

install_from_binary() {
  os="$(detect_os)"
  arch="$(detect_arch)"
  exe="$(detect_exe_suffix)"

  if [ -z "$os" ] || [ -z "$arch" ]; then
    echo "[codeq] binary mode: unsupported platform (os='$os' arch='$arch')" >&2
    return 1
  fi

  echo "[codeq] resolving release: ${ref}"
  tag="$(resolve_release_tag "$ref")" || {
    echo "[codeq] could not resolve release tag '$ref'" >&2
    return 1
  }
  echo "[codeq] release: ${tag}"

  asset="codeq-${os}-${arch}${exe}"
  url="https://github.com/${repo}/releases/download/${tag}/${asset}"

  tmp="$(mktemp_dir)"
  tmpfile="$tmp/${asset}"
  echo "[codeq] downloading: ${url}"
  if ! http_download "$url" "$tmpfile" "application/octet-stream"; then
    rm -rf "$tmp" >/dev/null 2>&1 || true
    echo "[codeq] binary download failed" >&2
    return 1
  fi

  install_dir="$(detect_install_dir)"
  mkdir -p "$install_dir"
  dest="$install_dir/codeq${exe}"
  echo "[codeq] installing to: $dest"
  if have_cmd install; then
    install -m 0755 "$tmpfile" "$dest"
  else
    cp "$tmpfile" "$dest"
    chmod 0755 "$dest" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmp" >/dev/null 2>&1 || true
  CODEQ_INSTALL_FINAL_DIR="$install_dir"
  return 0
}

install_from_source() {
  need_cmd git
  need_cmd go

  exe="$(detect_exe_suffix)"
  tmp="$(mktemp_dir)"
  trap_cleanup_tmp="$tmp"

  clone_url="https://github.com/${repo}.git"
  echo "[codeq] cloning: $clone_url"
  git clone --quiet --depth 1 "$clone_url" "$tmp/repo"

  if [ -n "$ref" ] && [ "$ref" != "latest" ]; then
    echo "[codeq] checking out: $ref"
    (
      cd "$tmp/repo"
      git fetch --quiet --depth 1 origin "$ref" 2>/dev/null || true
      git checkout --quiet "$ref"
    )
  fi

  echo "[codeq] building"
  out="$tmp/codeq${exe}"
  (cd "$tmp/repo" && go build -o "$out" ./cmd/codeq)

  install_dir="$(detect_install_dir)"
  mkdir -p "$install_dir"
  dest="$install_dir/codeq${exe}"

  echo "[codeq] installing to: $dest"
  if have_cmd install; then
    install -m 0755 "$out" "$dest"
  else
    cp "$out" "$dest"
    chmod 0755 "$dest" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmp" >/dev/null 2>&1 || true
  CODEQ_INSTALL_FINAL_DIR="$install_dir"
  return 0
}

case "$mode" in
  auto)
    if install_from_binary; then
      :
    else
      echo "[codeq] falling back to source build"
      install_from_source
    fi
    ;;
  binary)
    install_from_binary || exit 1
    ;;
  source)
    install_from_source
    ;;
  *)
    echo "[codeq] error: CODEQ_INSTALL_MODE must be one of: auto, binary, source (got '$mode')" >&2
    exit 1
    ;;
esac

install_dir="${CODEQ_INSTALL_FINAL_DIR:-}"

case ":$PATH:" in
  *":$install_dir:"*) : ;;
  *)
    if [ -n "$install_dir" ]; then
      echo "[codeq] note: '$install_dir' is not in PATH for this shell."
      if [ "${CODEQ_INSTALL_NO_PROFILE:-}" != "1" ] && [ -n "${HOME:-}" ]; then
        shell_name="$(basename "${SHELL:-}")"
        append_path_line "$HOME/.profile" "$install_dir"
        if [ "$shell_name" = "zsh" ]; then
          append_path_line "$HOME/.zprofile" "$install_dir"
        fi
        if [ "$shell_name" = "bash" ]; then
          append_path_line "$HOME/.bashrc" "$install_dir"
          append_path_line "$HOME/.bash_profile" "$install_dir"
        fi
        echo "[codeq] updated your shell profile(s). Open a new terminal or run:"
        echo "  export PATH=\"$install_dir:\$PATH\""
      else
        echo "[codeq] add it (bash/zsh): export PATH=\"$install_dir:\$PATH\""
      fi
    fi
    ;;
esac

echo "[codeq] done: codeq installed"
