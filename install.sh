#!/usr/bin/env sh
set -eu

# codeq install script (builds from git and installs into a PATH directory)
#
# Usage (typical):
#   curl -fsSL https://<host>/install.sh | sh
#
# Overrides:
#   CODEQ_INSTALL_REPO=https://github.com/osvaldoandrade/codeq.git
#   CODEQ_INSTALL_REF=main|vX.Y.Z|<sha>
#   CODEQ_INSTALL_DIR=/custom/bin

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "[codeq] error: missing required command: $1" >&2
    exit 1
  fi
}

mktemp_dir() {
  if command -v mktemp >/dev/null 2>&1; then
    mktemp -d 2>/dev/null && return 0
    mktemp -d -t codeq-install 2>/dev/null && return 0
    mktemp -d "${TMPDIR:-/tmp}/codeq-install.XXXXXX" 2>/dev/null && return 0
  fi
  dir="${TMPDIR:-/tmp}/codeq-install.$$"
  mkdir -p "$dir"
  echo "$dir"
}

detect_exe_suffix() {
  os="$(uname -s 2>/dev/null || echo unknown)"
  case "$os" in
    MINGW*|MSYS*|CYGWIN*) echo ".exe" ;;
    *) echo "" ;;
  esac
}

detect_install_dir() {
  if [ -n "${CODEQ_INSTALL_DIR:-}" ]; then
    echo "$CODEQ_INSTALL_DIR"
    return 0
  fi

  # Prefer a writable directory that is already in PATH.
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

  # Fallback to standard user-local bin dirs.
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

  # Fallback for macOS/Linux when user-local dirs aren't writable.
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

  # If grep isn't available, just append.
  if command -v grep >/dev/null 2>&1; then
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

need_cmd git
need_cmd go

repo="${CODEQ_INSTALL_REPO:-https://github.com/osvaldoandrade/codeq.git}"
ref="${CODEQ_INSTALL_REF:-}"
exe_suffix="$(detect_exe_suffix)"

tmp="$(mktemp_dir)"
cleanup() {
  rm -rf "$tmp" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

echo "[codeq] cloning: $repo"
git clone --quiet --depth 1 "$repo" "$tmp/repo"

if [ -n "$ref" ]; then
  echo "[codeq] checking out: $ref"
  (
    cd "$tmp/repo"
    # For tags/branches this works with a shallow clone; for SHAs it may require a fetch.
    git fetch --quiet --depth 1 origin "$ref" 2>/dev/null || true
    git checkout --quiet "$ref"
  )
fi

echo "[codeq] building"
out="$tmp/codeq${exe_suffix}"
(cd "$tmp/repo" && go build -o "$out" ./cmd/codeq)

install_dir="$(detect_install_dir)"
mkdir -p "$install_dir"
dest="$install_dir/codeq${exe_suffix}"

echo "[codeq] installing to: $dest"
if command -v install >/dev/null 2>&1; then
  install -m 0755 "$out" "$dest"
else
  cp "$out" "$dest"
  chmod 0755 "$dest" >/dev/null 2>&1 || true
fi

case ":$PATH:" in
  *":$install_dir:"*) : ;;
  *)
    echo "[codeq] note: '$install_dir' is not in PATH for this shell."
    if [ "${CODEQ_INSTALL_NO_PROFILE:-}" != "1" ] && [ -n "${HOME:-}" ]; then
      shell_name="$(basename "${SHELL:-}")"
      # Ensure new shells pick it up.
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
    ;;
esac

echo "[codeq] done: codeq installed"
