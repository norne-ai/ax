#!/bin/sh
set -eu

# Git invokes this helper for HTTPS credentials. The token is supplied through
# Task secretEnv and is never written to the remote URL or Git configuration.
case "${1:-}" in
  *Username*) printf '%s\n' "${AX_GIT_USERNAME:-x-access-token}" ;;
  *Password*) printf '%s\n' "${AX_GIT_TOKEN:-}" ;;
  *) exit 1 ;;
esac
