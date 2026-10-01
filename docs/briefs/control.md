Package: internal/control

Read/admin API + embedded status widget per SPEC "Control API".

- `New(deps Deps, opts Options) *Server` Deps{Accounts []core.Account (ordered),
  Quota core.QuotaSource, Policy core.Policy, Ledger core.Ledger,
  Routes []core.Route, Authenticate func(bearer string) (core.Client, bool),
  Clock core.Clock}; Options{RequireAuth bool, StaleAfter time.Duration}.
- `Handler() http.Handler` serving GET /healthz, GET /control/v1/status,
  GET /control/v1/usage, POST /control/v1/admit, GET / (widget).
- status JSON exactly per SPEC (schema_version 1). remaining_frac = 1-used (clamped);
  if window ResetAt in past show used 0 and flag `rolled: true`. reserve map from
  account. snapshot_age_s null when no snapshot. error from Snapshot.Err.
- usage: since accepts Go duration (24h, 168h) or "7d"/"30d" style day suffix; default
  24h; max 400d; group default account; invalid -> 400 JSON error.
- admit: {class, model} -> find route by model, candidates per class, Policy.DryRun.
  Unknown model -> 404. Bad class -> 400.
- RequireAuth: all /control/v1/* need a valid client bearer; /healthz and / remain
  open but the widget must then send the key — simplest: when RequireAuth, GET /
  returns a page that asks for a key stored in localStorage and sends it as bearer.
- Widget: single HTML file embedded via go:embed (internal/control/static/index.html),
  vanilla JS + inline CSS, no external requests. Dark/light via prefers-color-scheme.
  Per account card: provider badge, health/cooldown, horizontal bars for 5h and
  weekly showing REMAINING with a vertical marker at the reserve level, reset
  countdown (or "reset unknown"), stale/error indicator, background/interactive
  admissible pills, inflight count. Below: 24h and 7d totals (requests,
  input/cached/output tokens humanized, API-equivalent cost or "unpriced"), and a
  small per-model table for 24h. Poll every 15s; show last-updated time; tolerate
  API errors without breaking the page. Compact (~fits a 420px-wide side pane).
  Escape all text inserted into the DOM (textContent, never innerHTML with data).
- Tests: status JSON shape with fakes (rolled window, stale, error, no snapshot),
  usage since parsing + invalid group, admit dry-run uses DryRun not Acquire,
  RequireAuth enforcement, widget served with correct content-type and contains no
  http(s):// external asset references, no secrets in responses.
