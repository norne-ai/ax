#!/usr/bin/env bash
set -euo pipefail

mkdir -p /workspace/.ax

# The bootstrap client waits for the daemon, creates the shared default
# session, and submits the launcher's initial prompt. It remains attached to
# the prompt request while the Web Shell independently follows the same
# session over SSE.
node /usr/local/libexec/qwen-serve-bootstrap.mjs \
  >>/workspace/.ax/qwen-bootstrap.log 2>&1 &

# Bind non-loopback so Qwen accepts the browser's port-translated Host header.
# The bearer remains inside the task: the AX runner injects it while proxying.
qwen serve \
  --hostname 0.0.0.0 \
  --port 4170 \
  --require-auth \
  --workspace /workspace \
  2>&1 | tee -a /workspace/.ax/qwen-serve.log
