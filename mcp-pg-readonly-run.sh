#!/usr/bin/env bash
# mcp-pg-readonly-run — secret wrapper for mcp-pg-readonly.
#
# Sources the canonical secrets file (chmod 600) so the password never has to
# live in ~/.zcode/cli/config.json alongside the provider keys, then execs the
# binary. Used as the MCP `command` from ZCode (design §6.2 variant A).
#
# Override the secrets path with MCP_PG_SECRETS if you keep it elsewhere.
set -euo pipefail

SECRETS="${MCP_PG_SECRETS:-$HOME/.config/mcp-pg-readonly/env}"

if [[ -f "$SECRETS" ]]; then
  # shellcheck disable=SC1090
  set -a; source "$SECRETS"; set +a
else
  echo "mcp-pg-readonly-run: warning: secrets file not found at $SECRETS" \
       "(set PG_RO_PWD another way or create it)" >&2
fi

exec "$HOME/bin/mcp-pg-readonly" "$@"
