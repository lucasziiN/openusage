#!/usr/bin/env bash
# Rebuild OMP with the OpenUsage below-footer patch and install it over the
# current binary. Run this after `omp update`, which replaces the patched build
# with a stock release that lacks `belowFooter` and `getNativeFooter`.
#
# Usage: rebuild-omp-patched.sh [version]     e.g. 18.3.1 (default: latest release)
#
# Environment overrides:
#   OMP_SRC          clone of can1357/oh-my-pi used for worktrees (default: ~/dev/omp-layout)
#   OMP_INSTALL_DIR  directory holding omp.exe (default: directory of `omp` on PATH)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
patch="$here/omp-below-footer.patch"
src="${OMP_SRC:-$HOME/dev/omp-layout}"

die() { echo "error: $*" >&2; exit 1; }

[ -f "$patch" ] || die "patch not found: $patch"
git -C "$src" rev-parse --git-dir >/dev/null 2>&1 || die "OMP_SRC is not a git checkout: $src"

if [ -n "${OMP_INSTALL_DIR:-}" ]; then
  install_dir="$OMP_INSTALL_DIR"
else
  omp_path="$(command -v omp || true)"
  [ -n "$omp_path" ] || die "omp is not on PATH; set OMP_INSTALL_DIR"
  install_dir="$(dirname "$omp_path")"
fi
[ -f "$install_dir/omp.exe" ] || die "omp.exe not found in $install_dir"

version="${1:-}"
if [ -z "$version" ]; then
  if command -v gh >/dev/null; then
    version="$(gh release view -R can1357/oh-my-pi --json tagName -q .tagName)"
  else
    version="$(git -C "$src" ls-remote --tags --refs origin 'v*' | sed 's#.*refs/tags/##' |
      grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1)"
  fi
fi
version="${version#v}"
tag="v$version"
worktree="$(dirname "$src")/omp-$version-patched"
echo "==> Building patched OMP $version in $worktree"

git -C "$src" fetch --no-tags origin "refs/tags/$tag:refs/tags/$tag"
if [ -d "$worktree" ]; then
  # Reuse a previous attempt only if it is exactly the tag plus this patch.
  [ "$(git -C "$worktree" rev-parse HEAD)" = "$(git -C "$src" rev-parse "$tag^{commit}")" ] ||
    die "$worktree exists but is not at $tag; remove it with: git -C $src worktree remove --force $worktree"
  git -C "$worktree" apply --reverse --check "$patch" 2>/dev/null ||
    die "$worktree has other changes; remove it with: git -C $src worktree remove --force $worktree"
else
  git -C "$src" worktree add --detach "$worktree" "$tag"
  if ! git -C "$worktree" apply --3way "$patch"; then
    die "patch does not apply cleanly to $tag. Resolve the conflicts in $worktree, then refresh the patch with:
  git -C $worktree diff HEAD > $patch
and rerun this script."
  fi
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Bun: use one on PATH, else download the release OMP itself embeds (omp.exe
# reports it in BUN_BE_BUN mode). omp.exe cannot stand in for bun: it installs
# and tests fine but cannot compile a standalone executable.
if ! command -v bun >/dev/null; then
  bun_version="$(BUN_BE_BUN=1 "$install_dir/omp.exe" --version)"
  bun_dir="${XDG_CACHE_HOME:-$HOME/.cache}/omp-patched/bun-$bun_version"
  if [ ! -x "$bun_dir/bun.exe" ]; then
    echo "==> Downloading bun $bun_version"
    curl -fsSL -o "$tmp/bun.zip" \
      "https://github.com/oven-sh/bun/releases/download/bun-v$bun_version/bun-windows-x64-baseline.zip"
    unzip -q -o "$tmp/bun.zip" -d "$tmp/bun"
    mkdir -p "$bun_dir"
    cp "$tmp"/bun/*/bun.exe "$bun_dir/"
  fi
  export PATH="$bun_dir:$PATH"
fi
echo "==> Using bun $(bun --version)"

echo "==> Fetching native addon @oh-my-pi/pi-natives-win32-x64@$version"
curl -fsSL -o "$tmp/natives.tgz" \
  "https://registry.npmjs.org/@oh-my-pi/pi-natives-win32-x64/-/pi-natives-win32-x64-$version.tgz"
mkdir -p "$tmp/natives" "$worktree/packages/natives/native"
tar -xzf "$tmp/natives.tgz" -C "$tmp/natives"
cp "$tmp"/natives/package/*.node "$worktree/packages/natives/native/"

echo "==> Installing dependencies"
(cd "$worktree" && bun install --frozen-lockfile)

echo "==> Running patch tests"
(cd "$worktree/packages/coding-agent" &&
  bun test test/modes/controllers/extension-ui-controller.test.ts test/status-line-segment-padding.test.ts)

echo "==> Compiling"
rm -f "$worktree/packages/coding-agent/dist/omp.exe"
(cd "$worktree/packages/coding-agent" && bun run build)

built="$worktree/packages/coding-agent/dist/omp.exe"
built_version="$("$built" --version)"
[ "$built_version" = "omp/$version" ] || die "built binary reports '$built_version', expected omp/$version"
grep -q -a getNativeFooter "$built" || die "built binary is missing getNativeFooter; patch not compiled in"

current_version="$("$install_dir/omp.exe" --version | sed 's#^omp/##')"
backup="$install_dir/omp-$current_version-$(date +%Y%m%d%H%M%S).exe.bak"
echo "==> Installing (previous binary kept at $backup)"
# Renaming works even while omp.exe is running on Windows; copying over it does not.
mv "$install_dir/omp.exe" "$backup"
cp "$built" "$install_dir/omp.exe"

echo "==> Done: $("$install_dir/omp.exe" --version) with below-footer patch. Restart running OMP sessions."
