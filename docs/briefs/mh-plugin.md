Repository: ~/projects/hermes-localrouter (NEW, separate git repo; your worktree root IS this repo). Read docs/SPEC.md from ~/projects/localrouter (path given below) section "Hermes plugin", plus the Hermes plugin docs:
 - /home/wporter/.hermes/hermes-agent/website/docs/user-guide/features/plugins.md (register(ctx), ctx.register_tool / register_hook / register_command, plugin.yaml, plugins are opt-in via plugins.enabled)
 - /home/wporter/.hermes/hermes-agent/website/docs/user-guide/features/hooks.md (pre_tool_call return {"action":"block","message":...}; callback signature `(tool_name, args, task_id, **kwargs)`)
 - Look at a real bundled plugin for conventions, e.g. /home/wporter/.hermes/hermes-agent/plugins/disk-cleanup/ and the PluginContext in /home/wporter/.hermes/hermes-agent/hermes_cli/plugins.py (register_tool signature at ~line 1720; check register_command's real signature there).
LocalRouter control API (running locally at http://127.0.0.1:8787, no auth on loopback; you MAY curl it read-only to see real shapes: GET /control/v1/status, GET /control/v1/usage?since=24h&group=model, POST /control/v1/admit {"class":"background","account":"ollama-cloud"} or {"class":"background","model":"glm-5.3-flash"} -> {"decision":"allow|deny","account_id","reason"}).

Build (Python stdlib only, py3.11 compatible):
- plugin.yaml, __init__.py (register), client.py (HTTP via urllib, timeout, optional bearer from key_file, never logs the key), tools, hook, slash command per SPEC.
- Config file ~/.config/localrouter/hermes-plugin.json (all optional, defaults per SPEC); env override LOCALROUTER_PLUGIN_CONFIG for tests only.
- delegate_task gate: decide admit target: if config has account_for_delegation use {account}, elif model_for_delegation use {model}, else try args.get("model") if present, else read nothing and allow (log at debug). Block message must be actionable and short. fail_open default true.
- Tool outputs: compact JSON strings, percentages rounded, include `stale` and reset times in local ISO; never dump raw provider payloads.
- README.md: install (copy/symlink dir into ~/.hermes/plugins/localrouter, `hermes plugins enable localrouter`), config, what the hook blocks, privacy (talks only to LocalRouter).
- tests/ with unittest: fake LocalRouter via http.server on 127.0.0.1:0 in a thread; cover status tool, usage tool, admit allow/deny/unreachable (fail_open true/false), non-delegate tools untouched, malformed responses, key file auth header. Run: `python3 -m unittest discover -s tests -v` (python3 is 3.11; no pip installs).
- Do NOT modify anything under ~/.hermes (no installing/enabling) — the architect installs after review.
- git init already done; commit your work there with conventional commits.
SPEC path: /home/wporter/projects/localrouter/docs/SPEC.md