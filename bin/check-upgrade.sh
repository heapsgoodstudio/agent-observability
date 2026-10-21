#!/usr/bin/env bash
# Prevent a stale source checkout from replacing a newer installed dashboard.
set -euo pipefail

obs_install_dir="${1:-$HOME/.agent-observability}"
obs_installed_bin="$obs_install_dir/bin/claude-obs"
if [[ ! -x "$obs_installed_bin" ]]; then
  echo "No installed binary at $obs_installed_bin; use just install for a fresh setup." >&2
  exit 1
fi
obs_installed_version="$("$obs_installed_bin" version)"
obs_installed_ref="${obs_installed_version%-dirty}"
if ! git rev-parse --verify --quiet "${obs_installed_ref}^{commit}" >/dev/null; then
  echo "Cannot verify installed revision $obs_installed_version. Fetch its source history before upgrading." >&2
  exit 1
fi
if ! git merge-base --is-ancestor "$obs_installed_ref" HEAD; then
  echo "Upgrade refused: this checkout does not include installed revision $obs_installed_version." >&2
  echo "Move your changes onto that revision or a newer descendant before running just upgrade." >&2
  exit 1
fi
