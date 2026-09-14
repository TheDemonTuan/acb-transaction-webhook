#!/usr/bin/env bash
# One-time fresh production secret provisioning
# Fails closed if pre-existing key material or data state is detected.
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lib.sh
source "$SCRIPT_DIR/lib.sh"

CONFIRMED=0
for arg in "$@"; do
  if [[ "$arg" == "--confirm-fresh-provision" || "$arg" == "--confirm" ]]; then
    CONFIRMED=1
  fi
done

if [[ "$CONFIRMED" -ne 1 ]]; then
  if [[ -t 0 ]]; then
    printf 'DANGER: You are about to provision fresh production secrets in:\n'
    printf '  - Secrets Directory: %s\n' "$SECRETS_DIR"
    printf 'This must ONLY be done once on a completely new installation.\n'
    printf 'Type "CONFIRM-PROVISION-PRODUCTION-SECRETS" to proceed: '
    read -r user_input
    if [[ "$user_input" == "CONFIRM-PROVISION-PRODUCTION-SECRETS" ]]; then
      CONFIRMED=1
    else
      log_error "Confirmation mismatch. Aborting secret provisioning."
      exit 1
    fi
  else
    log_error "Fresh secret provisioning aborted: Missing explicit confirmation flag: --confirm-fresh-provision"
    exit 1
  fi
fi

assert_fresh_installation
provision_fresh_secrets

log_info "Production secrets provisioned successfully."
exit 0
