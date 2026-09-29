#!/usr/bin/env bash
# Build the forked Qwen Code Web Shell into the task runner's build context.
#
# The runner installs @qwen-code/qwen-code from npm, which ships the Web Shell
# as static files, and the daemon serves exactly that directory off disk. So the
# fork is a dist swap, not a fork of the CLI: the daemon, the tools, and the
# wire protocol stay the pinned npm release, and only the browser code comes
# from the fork. That keeps the two halves of a session in step, which is the
# part a full fork would make easy to get wrong.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/build-qwen-web-shell.sh [--force]

  --force, -f    rebuild even when the staged dist already matches the pin

Env:
  AX_WEB_SHELL_SRC     checkout to build from   (default ~/.cache/ax-qwen-web-shell/src)
  AX_WEB_SHELL_CTX     where to stage the dist  (default <repo>/.build/qwen-web-shell)
  AX_WEB_SHELL_IMAGE   node image to build in   (default node:22-bookworm-slim)
  AX_WEB_SHELL_STORE   pnpm store to reuse      (default ~/.cache/pnpm-store)
USAGE
}

ax_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pin_file="${ax_root}/cmd/ax-task-runner/qwen-web-shell.pin"
src_dir="${AX_WEB_SHELL_SRC:-${HOME}/.cache/ax-qwen-web-shell/src}"
out_dir="${AX_WEB_SHELL_CTX:-${ax_root}/.build/qwen-web-shell}"
node_image="${AX_WEB_SHELL_IMAGE:-node:22-bookworm-slim}"
store_dir="${AX_WEB_SHELL_STORE:-${HOME}/.cache/pnpm-store}"
container_cli="${CONTAINER_CLI:-$(command -v podman 2>/dev/null || command -v docker)}"

force=0
while [ $# -gt 0 ]; do
  case "$1" in
    --force | -f) force=1 ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "build-qwen-web-shell: unknown argument \"$1\"" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

[ -n "${container_cli}" ] || {
  echo "build-qwen-web-shell: no podman or docker to build the shell in" >&2
  exit 1
}

read_pin() { sed -n "s/^$1=//p" "${pin_file}" | head -1; }

[ -f "${pin_file}" ] || {
  echo "build-qwen-web-shell: missing ${pin_file}" >&2
  exit 1
}
repo="$(read_pin repo)"
commit="$(read_pin commit)"
branch="$(read_pin branch)"
if [ -z "${repo}" ] || [ -z "${commit}" ]; then
  echo "build-qwen-web-shell: ${pin_file} must set repo= and commit=" >&2
  exit 1
fi

if [ "${force}" -ne 1 ] && [ -f "${out_dir}/index.html" ] &&
  [ -f "${out_dir}/.pin" ] && [ "$(cat "${out_dir}/.pin")" = "${commit}" ]; then
  echo "Web Shell staged at ${out_dir#"${ax_root}/"} is already ${commit:0:12} (${branch}); --force to rebuild"
  exit 0
fi

# A shallow single-commit fetch is all the build needs, and keeping the
# checkout outside the repo avoids nesting a git tree inside a git tree.
mkdir -p "$(dirname "${src_dir}")" "${store_dir}"
[ -d "${src_dir}/.git" ] || git init -q "${src_dir}"
echo "==> Fetching ${branch} ${commit:0:12} from ${repo}"
if ! git -C "${src_dir}" fetch --depth 1 "${repo}" "${commit}" 2>/dev/null; then
  # Not every host will serve an arbitrary commit, so fall back to the branch
  # tip and insist it is the pinned one. A silent drift here would put a shell
  # in the image that no review ever saw.
  echo "    ${commit:0:12} is not fetchable by id, checking ${branch} instead"
  git -C "${src_dir}" fetch --depth 1 "${repo}" "${branch}"
  tip="$(git -C "${src_dir}" rev-parse FETCH_HEAD^{commit})"
  if [ "${tip}" != "${commit}" ]; then
    echo "build-qwen-web-shell: ${branch} is at ${tip} but the pin says ${commit}" >&2
    echo "    Update cmd/ax-task-runner/qwen-web-shell.pin to what you mean to ship." >&2
    exit 1
  fi
fi
# node_modules is untracked, so a hard reset keeps the install from one build
# to the next instead of paying for pnpm again every time.
git -C "${src_dir}" reset --hard --quiet FETCH_HEAD

echo "==> Building the Web Shell in ${node_image}"
"${container_cli}" run --rm \
  -v "${src_dir}:/repo" \
  -v "${store_dir}:/root/.local/share/pnpm/store" \
  -w /repo \
  "${node_image}" \
  bash -eu -c '
    corepack enable
    # The whole workspace, not just the shell package: the repo root runs
    # scripts/prepare.js on install, and a filtered install leaves that build
    # without the other packages it compiles.
    pnpm install --frozen-lockfile --store-dir /root/.local/share/pnpm/store
    pnpm --filter @qwen-code/web-shell exec vite build
  '

dist="${src_dir}/packages/web-shell/dist"
if [ ! -f "${dist}/index.html" ] || [ ! -d "${dist}/assets" ]; then
  # The daemon only serves a directory that has both, so staging anything else
  # would produce an image whose tasks open on a blank shell.
  echo "build-qwen-web-shell: build left no web shell at ${dist}" >&2
  exit 1
fi

rm -rf "${out_dir}"
mkdir -p "${out_dir}"
cp -a "${dist}/." "${out_dir}/"
# Stays inside the staged tree, so a running task can be asked which shell it is
# serving: cat <package>/web-shell/.pin
printf '%s\n' "${commit}" > "${out_dir}/.pin"

entry="$(sed -n 's/.*src="\/\(assets\/index-[^"]*\.js\)".*/\1/p' "${out_dir}/index.html" | head -1)"
echo "==> Staged $(find "${out_dir}" -type f | wc -l | tr -d ' ') files at ${out_dir#"${ax_root}/"}"
echo "    shell ${commit:0:12} (upstream $(read_pin upstream)) entry ${entry:-unknown}"
