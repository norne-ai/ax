#!/bin/sh
# Supply local inference defaults without replacing values declared by a Task.
: "${OPENAI_API_KEY:=ollama}"
: "${OPENAI_BASE_URL:=http://ninfer.hermes-agent.svc.cluster.local:8000/v1}"
: "${OPENAI_MODEL:=qwen3.8-27b}"
: "${QWEN_HOME:=/workspace/.ax/qwen}"
: "${QWEN_RUNTIME_DIR:=/workspace/.ax/qwen-runtime}"
: "${QWEN_CODE_SYSTEM_SETTINGS_PATH:=/etc/qwen-code/settings.json}"
: "${AX_HTTP_PROXY_TARGET:=http://127.0.0.1:4170}"
if [ -z "${QWEN_SERVER_TOKEN:-}" ]; then
  QWEN_SERVER_TOKEN="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
fi
: "${AX_HTTP_PROXY_BEARER:=$QWEN_SERVER_TOKEN}"
export OPENAI_API_KEY OPENAI_BASE_URL OPENAI_MODEL
export QWEN_HOME QWEN_RUNTIME_DIR QWEN_SERVER_TOKEN
export AX_HTTP_PROXY_TARGET AX_HTTP_PROXY_BEARER

# The same scoped credential authenticates the runner's initial private clone
# and Git commands issued later by Qwen. Keep GITHUB_TOKEN available to tools
# that understand it, while adapting it to the askpass helper's stable name.
if [ -z "${AX_GIT_TOKEN:-}" ] && [ -n "${GITHUB_TOKEN:-}" ]; then
  AX_GIT_TOKEN="$GITHUB_TOKEN"
  export AX_GIT_TOKEN
fi
if [ -n "${AX_GIT_TOKEN:-}" ]; then
  GIT_ASKPASS=/usr/local/bin/ax-git-askpass
  GIT_TERMINAL_PROMPT=0
  export GIT_ASKPASS GIT_TERMINAL_PROMPT
fi

mkdir -p "$QWEN_HOME" "$QWEN_RUNTIME_DIR"
chmod 0700 "$QWEN_HOME" "$QWEN_RUNTIME_DIR"

case "${AX_QWEN_REASONING_EFFORT:-}" in
  ""|none|low|medium|xhigh) ;;
  *)
    echo "Invalid AX_QWEN_REASONING_EFFORT '$AX_QWEN_REASONING_EFFORT'; expected none, low, medium, or xhigh." >&2
    exit 2
    ;;
esac

# Resolve the Qwen profile for this task. OPENAI_MODEL selects the entry under
# modelProviders.openai, so a per-task reasoning effort belongs in that entry
# rather than in the top-level model.generationConfig, which a matched entry
# shadows field by field. Prints the settings path to use; the task-local copy
# is written only when something actually changed.
runtime_settings="$QWEN_RUNTIME_DIR/settings.json"
if ! settings_path="$(node -e '
  const fs = require("fs");
  const [source, runtime] = process.argv.slice(1);
  const raw = fs.readFileSync(source, "utf8");
  const settings = JSON.parse(raw);
  const fail = (message) => {
    process.stderr.write(`qwen settings: ${message}\n`);
    process.exit(2);
  };
  const model = (process.env.OPENAI_MODEL || "").trim() || settings.model?.name || "";
  const effort = (process.env.AX_QWEN_REASONING_EFFORT || "").trim();
  const entries = settings.modelProviders?.openai;
  const entry = Array.isArray(entries) ? entries.find((item) => item?.id === model) : undefined;

  // Qwen falls back to OPENAI_API_KEY when an entry envKey variable is empty,
  // which would hand the local placeholder credential to a paid endpoint.
  if (entry?.envKey && !(process.env[entry.envKey] || settings.env?.[entry.envKey] || "").trim()) {
    fail(`model "${model}" requires ${entry.envKey}, which is empty; supply it from a Kubernetes Secret with Task spec.secretEnv.`);
  }

  if (effort) {
    const caps = entry?.capabilities?.reasoning;
    if (!entry) {
      settings.model ??= {};
      settings.model.generationConfig ??= {};
      settings.model.generationConfig.extra_body ??= {};
      settings.model.generationConfig.extra_body.reasoning = { effort };
    } else {
      entry.generationConfig ??= {};
      const generation = entry.generationConfig;
      if (effort === "none") {
        if (caps && (caps.canDisable === false || generation.thinkingMandatory === true)) {
          fail(`model "${model}" cannot disable thinking; use low, medium, or xhigh.`);
        }
        if (caps) {
          generation.extra_body = { ...generation.extra_body, enable_thinking: false };
          delete generation.reasoning;
        } else {
          generation.extra_body = { ...generation.extra_body, reasoning: { effort } };
        }
      } else if (caps?.toggleOnly) {
        fail(`model "${model}" only toggles thinking; use none to disable it or omit the effort.`);
      } else if (caps) {
        if (Array.isArray(caps.efforts) && caps.efforts.length && !caps.efforts.includes(effort)) {
          fail(`model "${model}" accepts reasoning efforts ${caps.efforts.join(", ")}; got "${effort}".`);
        }
        generation.reasoning = { ...generation.reasoning, effort };
      } else {
        generation.extra_body = { ...generation.extra_body, reasoning: { effort } };
      }
    }
  }

  if (JSON.stringify(settings) === JSON.stringify(JSON.parse(raw))) {
    process.stdout.write(`${source}\n`);
  } else {
    fs.writeFileSync(runtime, `${JSON.stringify(settings, null, 2)}\n`, { mode: 0o600 });
    process.stdout.write(`${runtime}\n`);
  }
' "$QWEN_CODE_SYSTEM_SETTINGS_PATH" "$runtime_settings")"; then
  exit 2
fi
if [ -n "$settings_path" ] && [ "$settings_path" != "$QWEN_CODE_SYSTEM_SETTINGS_PATH" ]; then
  QWEN_CODE_SYSTEM_SETTINGS_PATH="$settings_path"
  export QWEN_CODE_SYSTEM_SETTINGS_PATH
fi

# Keep the Go runner as PID 1 so its process-group supervision and signal
# forwarding behavior is unchanged.
exec /usr/local/libexec/ax-task-runner "$@"
