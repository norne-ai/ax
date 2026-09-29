#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$script_dir/.." && pwd)"

default_image="localhost:5001/ax-qwen-task-runner@sha256:50d5d3ea4a3a244f02b547197fc4d0e8b1223f1f7e8cd616006eb387d5534170"
image="${AX_QWEN_IMAGE:-$default_image}"

# Provider profiles baked into the runner image. Model Studio models are declared
# in /etc/qwen-code/settings-modelstudio.json and take their key from a container
# env var fed by spec.secretEnv; the local NInfer model keeps the image default
# profile and its non-secret placeholder key.
ninfer_endpoint="http://ninfer.hermes-agent.svc.cluster.local:8000/v1"
modelstudio_endpoint="https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"
modelstudio_settings="/etc/qwen-code/settings-modelstudio.json"
modelstudio_key_env="MODELSTUDIO_API_KEY"
modelstudio_models="qwen3.8-max qwen3.8-max-preview qwen3.8-flash qwen3.7-plus qwen3.6-plus qwen3.7-max qwen3.6-flash"

endpoint_override="${AX_QWEN_ENDPOINT:-}"
model="${AX_QWEN_MODEL:-qwen3.8-27b}"
provider="${AX_QWEN_PROVIDER:-}"
api_key="${AX_QWEN_API_KEY:-}"
model_secret="${AX_MODELSTUDIO_SECRET:-modelstudio-api}"
model_secret_key="${AX_MODELSTUDIO_SECRET_KEY:-api-key}"
reasoning_effort="${AX_QWEN_REASONING_EFFORT:-}"
atespace="${AX_ATESPACE:-default}"
task_name=""
experiment=""
git_secret="${AX_EXPERIMENTS_GIT_SECRET:-experiments-git}"
git_secret_key="${AX_EXPERIMENTS_GIT_SECRET_KEY:-token}"
prompt=""
prompt_file=""
watch=false
dry_run=false
serve=true
preview_port=""
preview_domain="${AX_PREVIEW_DOMAIN:-norne}"
# Must match AX_PREVIEW_HOST_PREFIX's default in the task's preview mux, or the
# hostname this script advertises is not the one the mux routes.
preview_host_prefix="preview-"

usage() {
  cat <<'EOF'
Launch a Qwen Code task on AX. Qwen Serve mode is the default.

Usage:
  scripts/run-qwen-task.sh [options] "PROMPT"
  scripts/run-qwen-task.sh --experiment NAME --prompt-file PATH [options]
  printf '%s\n' "PROMPT" | scripts/run-qwen-task.sh [options]

Options:
  -p, --prompt TEXT     Prompt to send to Qwen Code
  -f, --prompt-file PATH
                        Read the prompt from PATH; use - for stdin
  -n, --name NAME      Task name (default: qwen-<unix timestamp>)
  -e, --experiment NAME
                        Experiment directory under runs/ (required)
  -m, --model ID       Model to run the task on (default: qwen3.8-27b)
      --provider NAME  ninfer or modelstudio; inferred from --model when omitted
      --api-key-secret NAME
                        Kubernetes Secret in the task atespace holding the Model
                        Studio API key (default: modelstudio-api)
      --api-key-secret-key KEY
                        Key within --api-key-secret (default: api-key)
      --list-models    Print the known model ids per provider and exit
      --git-secret NAME Kubernetes Secret containing the scoped Git token
                        (default: experiments-git)
      --git-secret-key KEY
                        Key within --git-secret (default: token)
      --reasoning-effort EFFORT
                        Model reasoning effort: none, low, medium, or xhigh
                        (default: the model's own default)
      --preview [PORT]  Give the app the agent runs a hostname of its own:
                        http://preview-<task>.<preview-domain>. Sets
                        AX_PREVIEW_TARGET for the runner's preview mux, and PORT
                        is the port the app listens on inside the task (default
                        3000). The Web Shell derives the preview hostname from
                        its own origin, then opens the panel beside the chat on
                        load, hot reload included. Needs a runner image carrying
                        qwen-preview-mux.mjs and the forked shell; an older
                        --image digest silently serves the Web Shell instead of
                        the app.
      --preview-domain NAME
                        Base domain the preview hostname is reported under
                        (default $AX_PREVIEW_DOMAIN or norne, the LAN zone; use
                        norne.app for the tunnel, where the scheme is https).
  -a, --atespace NAME  AX atespace (default: $AX_ATESPACE or default)
      --watch          Watch task status after applying it
      --serve          Run Qwen Serve (default; retained for explicit scripts)
      --headless       Run a one-shot stream-JSON session without the Web UI
      --dry-run        Print the generated manifest without applying it
  -h, --help           Show this help

Environment overrides:
  AX_BIN, AX_QWEN_IMAGE, AX_QWEN_ENDPOINT, AX_QWEN_MODEL, AX_QWEN_PROVIDER,
  AX_QWEN_API_KEY, AX_QWEN_REASONING_EFFORT, AX_ATESPACE, AX_PREVIEW_DOMAIN,
  AX_MODELSTUDIO_SECRET, AX_MODELSTUDIO_SECRET_KEY,
  AX_EXPERIMENTS_GIT_SECRET, AX_EXPERIMENTS_GIT_SECRET_KEY

Model Studio tasks read their API key from a Kubernetes Secret instead of the
manifest. Create it once in the atespace that runs the task:

  kubectl -n default create secret generic modelstudio-api \
    --from-literal=api-key=sk-...

The task writes Qwen's stream-JSON output to /workspace/qwen-output.jsonl.
EOF
  echo
  echo "Models:"
  echo "  ninfer       qwen3.8-27b (default; local endpoint, no credential)"
  printf '  modelstudio  %s\n' "$modelstudio_models"
}

list_models() {
  echo "ninfer (local, key from AX_QWEN_API_KEY or the image placeholder):"
  echo "  qwen3.8-27b"
  echo
  echo "modelstudio (Alibaba Model Studio Token Plan, key from a Kubernetes Secret):"
  for known in $modelstudio_models; do
    echo "  $known"
  done
}

positionals=()
while (($#)); do
  case "$1" in
    -p|--prompt)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      prompt="$2"
      shift 2
      ;;
    --prompt=*)
      prompt="${1#*=}"
      shift
      ;;
    -f|--prompt-file)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      prompt_file="$2"
      shift 2
      ;;
    --prompt-file=*)
      prompt_file="${1#*=}"
      shift
      ;;
    -n|--name)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      task_name="$2"
      shift 2
      ;;
    --name=*)
      task_name="${1#*=}"
      shift
      ;;
    -e|--experiment)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      experiment="$2"
      shift 2
      ;;
    --experiment=*)
      experiment="${1#*=}"
      shift
      ;;
    -m|--model)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      model="$2"
      shift 2
      ;;
    --model=*)
      model="${1#*=}"
      shift
      ;;
    --provider)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      provider="$2"
      shift 2
      ;;
    --provider=*)
      provider="${1#*=}"
      shift
      ;;
    --api-key-secret)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      model_secret="$2"
      shift 2
      ;;
    --api-key-secret=*)
      model_secret="${1#*=}"
      shift
      ;;
    --api-key-secret-key)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      model_secret_key="$2"
      shift 2
      ;;
    --api-key-secret-key=*)
      model_secret_key="${1#*=}"
      shift
      ;;
    --preview)
      # The port is optional, so only consume the next word when it is a number;
      # otherwise a following flag like --dry-run would be swallowed as a port.
      if [[ $# -ge 2 && "$2" =~ ^[0-9]+$ ]]; then
        preview_port="$2"
        shift 2
      else
        preview_port="3000"
        shift
      fi
      ;;
    --preview=*)
      preview_port="${1#*=}"
      shift
      ;;
    --preview-domain)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      preview_domain="$2"
      shift 2
      ;;
    --preview-domain=*)
      preview_domain="${1#*=}"
      shift
      ;;
    --list-models)
      list_models
      exit 0
      ;;
    --git-secret)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      git_secret="$2"
      shift 2
      ;;
    --git-secret=*)
      git_secret="${1#*=}"
      shift
      ;;
    --git-secret-key)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      git_secret_key="$2"
      shift 2
      ;;
    --git-secret-key=*)
      git_secret_key="${1#*=}"
      shift
      ;;
    --reasoning-effort)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      reasoning_effort="$2"
      shift 2
      ;;
    --reasoning-effort=*)
      reasoning_effort="${1#*=}"
      shift
      ;;
    -a|--atespace)
      [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 2; }
      atespace="$2"
      shift 2
      ;;
    --atespace=*)
      atespace="${1#*=}"
      shift
      ;;
    --watch)
      watch=true
      shift
      ;;
    --serve)
      serve=true
      shift
      ;;
    --headless)
      serve=false
      shift
      ;;
    --dry-run)
      dry_run=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      positionals+=("$@")
      break
      ;;
    -*)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
    *)
      positionals+=("$1")
      shift
      ;;
  esac
done

prompt_sources=0
[[ -n "$prompt" ]] && ((prompt_sources += 1))
[[ -n "$prompt_file" ]] && ((prompt_sources += 1))
[[ ${#positionals[@]} -gt 0 ]] && ((prompt_sources += 1))
if ((prompt_sources > 1)); then
  echo "Provide the prompt using only one of --prompt, --prompt-file, positional arguments, or stdin." >&2
  exit 2
fi

if [[ -n "$prompt_file" ]]; then
  if [[ "$prompt_file" == "-" ]]; then
    prompt="$(cat)"
  elif [[ ! -r "$prompt_file" ]]; then
    echo "Prompt file is not readable: $prompt_file" >&2
    exit 2
  else
    prompt="$(<"$prompt_file")"
  fi
elif [[ -z "$prompt" && ${#positionals[@]} -gt 0 ]]; then
  prompt="${positionals[*]}"
elif [[ -z "$prompt" && ! -t 0 ]]; then
  prompt="$(cat)"
fi
if [[ -z "$prompt" ]]; then
  echo "A non-empty prompt is required." >&2
  usage >&2
  exit 2
fi

if [[ -z "$experiment" ]]; then
  echo "--experiment is required; it becomes runs/<experiment> in norne-ai/experiments." >&2
  usage >&2
  exit 2
fi
if [[ ${#experiment} -gt 80 || ! "$experiment" =~ ^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$ ]]; then
  echo "Invalid experiment '$experiment': use 1-80 lowercase letters, digits, dots, underscores, or hyphens." >&2
  exit 2
fi
if [[ -z "$git_secret" || -z "$git_secret_key" ]]; then
  echo "--git-secret and --git-secret-key must be non-empty." >&2
  exit 2
fi
if [[ -z "$model" ]]; then
  echo "A non-empty model id is required." >&2
  exit 2
fi
case "$provider" in
  "")
    provider="ninfer"
    for known in $modelstudio_models; do
      if [[ "$model" == "$known" ]]; then
        provider="modelstudio"
        break
      fi
    done
    ;;
  ninfer|modelstudio) ;;
  *)
    echo "Invalid provider '$provider': expected ninfer or modelstudio." >&2
    exit 2
    ;;
esac

if [[ "$provider" == modelstudio ]]; then
  endpoint="${endpoint_override:-$modelstudio_endpoint}"
  settings_path="$modelstudio_settings"
  key_env="$modelstudio_key_env"
  if [[ -n "$api_key" ]]; then
    key_source="AX_QWEN_API_KEY (plaintext)"
  else
    if [[ -z "$model_secret" || -z "$model_secret_key" ]]; then
      echo "--api-key-secret and --api-key-secret-key must be non-empty." >&2
      exit 2
    fi
    key_source="Secret $model_secret/$model_secret_key"
  fi
  catalogued=false
  for known in $modelstudio_models; do
    if [[ "$model" == "$known" ]]; then
      catalogued=true
      break
    fi
  done
  if [[ "$catalogued" != true ]]; then
    echo "Note: '$model' is not in the runner image Model Studio catalog; it will be sent to $endpoint as-is." >&2
  fi
else
  endpoint="${endpoint_override:-$ninfer_endpoint}"
  settings_path=""
  key_env="OPENAI_API_KEY"
  api_key="${api_key:-ollama}"
  key_source="AX_QWEN_API_KEY (plaintext)"
fi
case "$reasoning_effort" in
  ""|none|low|medium|xhigh) ;;
  *)
    echo "Invalid reasoning effort '$reasoning_effort': expected none, low, medium, or xhigh." >&2
    exit 2
    ;;
esac

started_at="$(date +%s)"
if [[ -z "$task_name" ]]; then
  task_experiment="${experiment//[._]/-}"
  task_name="qwen-${task_experiment:0:40}-$started_at"
fi
if [[ ${#task_name} -gt 63 || ! "$task_name" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
  echo "Invalid task name '$task_name': use a lowercase DNS label of at most 63 characters." >&2
  exit 2
fi

preview_target=""
preview_url=""
preview_note=""
if [[ -n "$preview_port" ]]; then
  if [[ ! "$preview_port" =~ ^[0-9]+$ ]] || ((preview_port < 1 || preview_port > 65535)); then
    echo "Invalid --preview port '$preview_port': expected 1-65535." >&2
    exit 2
  fi
  if [[ ! "$preview_domain" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]; then
    echo "Invalid --preview-domain '$preview_domain': use a lowercase DNS name." >&2
    exit 2
  fi
  if (( ${#task_name} + ${#preview_host_prefix} > 63 )); then
    echo "Task name '$task_name' leaves no room for the ${preview_host_prefix} hostname prefix (max $((63 - ${#preview_host_prefix})) characters)." >&2
    exit 2
  fi
  # The LAN zone is served over plain HTTP by ingress-nginx; every other base
  # domain reaches the cluster through the Cloudflare tunnel, which is HTTPS and
  # behind Access. Reporting the wrong scheme would hand out a dead link.
  if [[ "$preview_domain" == norne ]]; then
    preview_scheme="http"
  else
    preview_scheme="https"
  fi
  preview_target="http://127.0.0.1:$preview_port"
  preview_url="$preview_scheme://$preview_host_prefix$task_name.$preview_domain/"
  preview_note="- The app must listen on 0.0.0.0:$preview_port inside the task and keep running after you finish (for a Next.js dev server: NEXT_TELEMETRY_DISABLED=1 npx next dev -H 0.0.0.0 -p $preview_port, started in the background, with no basePath). The preview panel at $preview_url opens itself beside the chat, so confirm the app answers there before calling the work done."
fi

git_branch="qwen/$experiment-$started_at"
prompt="$(cat <<EOF
You are working in the norne-ai/experiments Git repository on branch $git_branch.

Mandatory delivery rules:
- Put every new or modified project file under runs/$experiment/.
- Do not modify files outside runs/$experiment/.
- Complete and verify the requested work.
- Before declaring the task complete, commit all changes on $git_branch with a meaningful commit message and push that branch to origin.
- In your final response, report the branch name, commit SHA, verification performed, and any remaining issues.
$preview_note

User task:
$prompt
EOF
)"

command -v python3 >/dev/null 2>&1 || {
  echo "python3 is required to encode the prompt safely." >&2
  exit 1
}

manifest="$(
  AX_TASK_NAME="$task_name" \
  AX_TASK_ATESPACE="$atespace" \
  AX_TASK_IMAGE="$image" \
  AX_TASK_PROMPT="$prompt" \
  AX_TASK_ENDPOINT="$endpoint" \
  AX_TASK_MODEL="$model" \
  AX_TASK_API_KEY="$api_key" \
  AX_TASK_KEY_ENV="$key_env" \
  AX_TASK_SETTINGS_PATH="$settings_path" \
  AX_TASK_MODEL_SECRET="$model_secret" \
  AX_TASK_MODEL_SECRET_KEY="$model_secret_key" \
  AX_TASK_REASONING_EFFORT="$reasoning_effort" \
  AX_TASK_SERVE="$serve" \
  AX_TASK_PREVIEW_TARGET="$preview_target" \
  AX_EXPERIMENT_NAME="$experiment" \
  AX_GIT_BRANCH="$git_branch" \
  AX_GIT_SECRET="$git_secret" \
  AX_GIT_SECRET_KEY="$git_secret_key" \
  python3 - <<'PY'
import json
import os

serve = os.environ["AX_TASK_SERVE"] == "true"
prepare = r'''set -euo pipefail
git config --global user.name "${AX_GIT_AUTHOR_NAME:-Norne Qwen}"
git config --global user.email "${AX_GIT_AUTHOR_EMAIL:-qwen@norne.local}"
cd /workspace
if git show-ref --verify --quiet "refs/heads/$AX_GIT_BRANCH"; then
  git switch "$AX_GIT_BRANCH"
else
  git switch -c "$AX_GIT_BRANCH"
fi
mkdir -p "runs/$AX_EXPERIMENT_NAME"
'''
if serve:
    command = ["bash", "-lc", prepare + "exec /usr/local/bin/ax-qwen-serve"]
else:
    command = [
        "bash",
        "-lc",
        prepare + 'qwen --prompt "$AX_QWEN_PROMPT" '
        "--approval-mode yolo "
        "--output-format stream-json --include-partial-messages "
        "| tee /workspace/qwen-output.jsonl",
    ]

workspace = {
    "apiVersion": "ax.io/v1alpha1",
    "kind": "Workspace",
    "metadata": {
        "name": "qwen-experiments",
        "atespace": os.environ["AX_TASK_ATESPACE"],
    },
    "spec": {
        "git": [{
            "name": "origin",
            "repo": "https://github.com/norne-ai/experiments.git",
            "branch": "main",
            "dir": ".",
        }],
    },
}

env = [
    {"name": "AX_QWEN_PROMPT", "value": os.environ["AX_TASK_PROMPT"]},
    {"name": "OPENAI_BASE_URL", "value": os.environ["AX_TASK_ENDPOINT"]},
    {"name": "OPENAI_MODEL", "value": os.environ["AX_TASK_MODEL"]},
    {"name": "AX_EXPERIMENT_NAME", "value": os.environ["AX_EXPERIMENT_NAME"]},
    {"name": "AX_GIT_BRANCH", "value": os.environ["AX_GIT_BRANCH"]},
]

# The task's preview mux only fronts the daemon when AX_PREVIEW_TARGET is set, so
# an unset port here means the hostname routes to the Web Shell instead.
# Do not pin AX_PREVIEW_URL here. The same task can be opened through the plain
# HTTP LAN zone or the HTTPS tunnel, and an absolute URL for either one becomes
# mixed content or an invalid TLS navigation in the other. The mux publishes a
# bare preview hostname derived from the incoming Host so the Web Shell can keep
# the page's scheme and port. preview_url remains only in the prompt and launch
# summary as a convenient link for the zone selected with --preview-domain.
preview_target = os.environ.get("AX_TASK_PREVIEW_TARGET", "")
if preview_target:
    env.append({"name": "AX_PREVIEW_TARGET", "value": preview_target})
secret_env = [{
    "name": "GITHUB_TOKEN",
    "secretKeyRef": {
        "name": os.environ["AX_GIT_SECRET"],
        "key": os.environ["AX_GIT_SECRET_KEY"],
    },
}]

# The provider profile decides which Qwen settings file the task loads and where
# its model API key comes from. The key is either a plaintext value (the local
# NInfer placeholder) or a Secret reference, never both: AX rejects a name that
# appears in both spec.env and spec.secretEnv.
settings_path = os.environ["AX_TASK_SETTINGS_PATH"]
if settings_path:
    env.append({"name": "QWEN_CODE_SYSTEM_SETTINGS_PATH", "value": settings_path})
key_env = os.environ["AX_TASK_KEY_ENV"]
api_key = os.environ["AX_TASK_API_KEY"]
if api_key:
    env.append({"name": key_env, "value": api_key})
else:
    secret_env.append({
        "name": key_env,
        "secretKeyRef": {
            "name": os.environ["AX_TASK_MODEL_SECRET"],
            "key": os.environ["AX_TASK_MODEL_SECRET_KEY"],
        },
    })

task = {
    "apiVersion": "ax.io/v1alpha1",
    "kind": "Task",
    "metadata": {
        "name": os.environ["AX_TASK_NAME"],
        "atespace": os.environ["AX_TASK_ATESPACE"],
    },
    "spec": {
        "image": os.environ["AX_TASK_IMAGE"],
        "command": command,
        "env": env,
        "secretEnv": secret_env,
        "resources": {
            "requests": {"cpu": "500m", "memory": "1Gi"},
            "limits": {"cpu": "4", "memory": "8Gi"},
        },
        "workspaces": [{"name": "qwen-experiments", "path": "/workspace"}],
        "debug": True,
    },
}

reasoning_effort = os.environ["AX_TASK_REASONING_EFFORT"]
if reasoning_effort:
    task["spec"]["env"].append({
        "name": "AX_QWEN_REASONING_EFFORT",
        "value": reasoning_effort,
    })

print(json.dumps(workspace, indent=2))
print("---")
print(json.dumps(task, indent=2))
PY
)"

if [[ "$dry_run" == true ]]; then
  printf '%s\n' "$manifest"
  exit 0
fi

if [[ -n "${AX_BIN:-}" ]]; then
  ax_bin="$AX_BIN"
elif [[ -x "$repo_dir/bin/ax" ]]; then
  ax_bin="$repo_dir/bin/ax"
elif command -v ax >/dev/null 2>&1; then
  ax_bin="$(command -v ax)"
else
  echo "AX CLI not found. Run 'make build' or set AX_BIN." >&2
  exit 1
fi

printf '%s\n' "$manifest" | "$ax_bin" apply -f -

cat <<EOF

Started AX task: $task_name
Provider: $provider
Model: $model
Inference endpoint: $endpoint
Model API key: $key_source
Reasoning effort: ${reasoning_effort:-default}
Experiment path: runs/$experiment
Git branch: $git_branch
Preview app: ${preview_url:-not requested (pass --preview)}

Watch status:
  $ax_bin -a $atespace watch task $task_name
EOF

if [[ "$serve" == true ]]; then
  cat <<EOF

Open the Qwen Web Shell after the workspace is ready:
  $ax_bin -a $atespace qwen-ui $task_name --host 0.0.0.0

Daemon logs:
  $ax_bin -a $atespace ssh $task_name -- tail -n 100 /workspace/.ax/qwen-serve.log
EOF
else
  cat <<EOF

Follow Qwen output after the workspace is ready:
  $ax_bin -a $atespace ssh $task_name -- tail -f /workspace/qwen-output.jsonl
EOF
fi

if [[ "$watch" == true ]]; then
  exec "$ax_bin" -a "$atespace" watch task "$task_name"
fi
