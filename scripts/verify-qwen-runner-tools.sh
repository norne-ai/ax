#!/usr/bin/env bash
# Smoke-test the developer and verifier toolchain baked into the Qwen runner.
set -euo pipefail

required=(
  bash buf curl dapr gh git go gosec golangci-lint govulncheck grpcurl jq
  ko kubeconform kubectl make nats nc node npm psql protoc rg shellcheck
  staticcheck yq
)

for tool in "${required[@]}"; do
  command -v "$tool" >/dev/null || {
    echo "missing required Qwen runner tool: $tool" >&2
    exit 1
  }
done

go version
node --version
npm --version
git --version
gh --version | head -n 1
kubectl version --client
ko version
buf --version
protoc --version
grpcurl -version
nats --version
dapr --version
psql --version
jq --version
yq --version
shellcheck --version | head -n 2
golangci-lint version
staticcheck -version
govulncheck -version
gosec -version
kubeconform -v
