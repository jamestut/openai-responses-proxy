# Repository guidance

## Working practices

- Read the relevant implementation and docs before changing behavior. Ask for clarification when a requirement materially affects the contract; otherwise state a small, reversible assumption and proceed.
- Keep the implementation simple. Prefer the standard library and existing package boundaries. Do not add abstractions, flags, dependencies, logging, error handling, tests, or compatibility layers unless the request needs them.
- Keep comments short and local, and use them only for non-obvious behavior or constraints. Do not paste full programs into chat; use minimal examples when they help.
- Write Markdown with ASCII punctuation and no fixed-width hard wrapping. In chat, link local files with absolute paths; repository docs may use relative links for navigation.
- Do not run Git commands, including read-only inspection, unless the user explicitly authorizes repository inspection for the current task. Do not modify Git state without an explicit request. Do not run builds, tests, migrations, or deployments unless the user explicitly requests them.
- The user prefers substantive work to be delegated to independent subagents coordinated by the primary agent. Assigned agents should implement their task directly and should not spawn further agents unless the coordinator asks. Follow applicable delegation skill and model instructions when selecting subagents.

## Project map

- `cmd/responses-proxy` contains the `serve` and `reload-keys` command entry points and service startup/shutdown.
- `internal/config` strictly decodes the main YAML file, applies defaults, validates settings, and resolves relative paths against the config directory.
- `internal/auth` holds the atomically replaceable user-key map; `internal/admin` implements key reload over the private Unix socket in `state_dir`.
- `internal/proxy` handles supported routes, upstream HTTP, retries, streaming, and the accounting query endpoint. Retrieval routes only forward upstream.
- `internal/accounting` owns the SQLite schema, one-shot request records, token parsing, and aggregate/time filtering; `internal/logging` owns log filtering and output.
- `cmd/mock-upstream` is an optional synthetic upstream for interactive development. Automated test conventions and scenario coverage are documented in `docs/testing.md`.

## Contracts to preserve

- This service implements the OpenAI Responses API only. Keep the public listener plain HTTP, do not add WebSocket support, and keep upstream HTTP or HTTPS certificate and hostname verification enabled.
- The configured upstream base URL must end in `/v1`; preserve its path prefix, incoming query, and request body when forwarding. Replace downstream Authorization with the configured upstream `Authorization: Bearer` credential and continue filtering hop-by-hop headers.
- Keep the main configuration and user-key YAML separate. YAML decoding is strict, durations use Go duration syntax, and relative users, CA, state, and log paths resolve from the main config file.
- A key reload must validate the entire users file before atomically replacing the active key snapshot. A failed reload leaves the old snapshot active, and requests already authenticated retain their original username. Main-config changes require restart.
- Only HTTP 429 responses are retried. `max_retries` counts retries after the first attempt; use the configured delay with independent plus-or-minus 20 percent jitter, ignore upstream `Retry-After`, and stop waiting or working when the downstream request is canceled.
- Preserve standard Responses SSE framing: only a complete terminal event ending in a blank line supplies accounting usage. Forward unterminated frames at EOF, but ignore their usage. Do not retain provisional usage or merge it into a later terminal event.
- After each response create or compact operation ends, attempt one SQLite insert with its start time in UTC, original username, model, kind, outcome, HTTP status, Response status, incomplete reason, and standard Responses token usage. Non-streaming JSON supplies its own usage; streaming supplies terminal-event usage. Missing usage is zero. Do not add cost estimates or provider-specific usage fields.
- `http_status` describes the upstream response, not a locally generated error. Keep it zero when no upstream HTTP response was received, including local body rejection or TLS failure. Preserve `kind` and `response_status` as diagnostic metadata.
- Accounting is best effort: log failed inserts at every log level, continue forwarding requests, and never queue or retry writes, gate requests on storage availability, or update rows from later retrievals. Keep no saved response IDs, pending state, provisional snapshots, or background recovery. Upstream HTTP 429 retries share one final accounting attempt.
- `incomplete_reason` is a string enum enforced by SQLite CHECK: empty string, `transport_error`, `response_read_error`, `client_disconnected`, `stream_ended`, `nonterminal_response`, or `invalid_response`. Add no free-form diagnostic text to this column.
- Every valid user can query global accounting, but the endpoint returns aggregates rather than raw request rows. Preserve model/user grouping, exact filters, half-open time ranges, and the UTC default timezone.
- Keep `state_dir` private: validate it before opening SQLite; the directory is mode 0700 and the reload socket and SQLite database are mode 0600. The configured shutdown deadline bounds handler draining. Accounting records may be lost on failed writes, abrupt exit, or deadline expiry; there is no replay or recovery.
- The schema and accounting response need not support older versions. Use a fresh database after this simplification; do not add migrations or legacy-schema compatibility. Aggregate summaries expose request and token counts without pending/incomplete counters.
- The default `upstream_errors` log level includes upstream HTTP/transport errors, retry events, and service/accounting errors while omitting downstream authentication failures. Never log credentials or request/response bodies. Keep HTTP status logs to sanitized metadata, and sanitize transport errors before logging because raw URL errors can include query strings or response ID paths.

## Automated validation and documentation

- The project uses standard Go package and integration tests. When the user authorizes validation, start with `docs/testing.md`, run `go test ./...` and `go test -race ./...` as authorized, use only local synthetic fixtures, and clean up local processes and state afterward.
- Update the relevant automated tests and coverage documentation when a behavior or invariant changes. Keep `README.md`, the example YAML files, and `docs/testing.md` consistent with `internal/config` and the implemented API. Keep expected results explicit and independent of production calculations. Build/version metadata and Python scripts are outside the automated suite; retain `cmd/mock-upstream` as an optional developer tool.
- Live upstream checks require separate authorization. Use only `gpt-6-luna`, keep credentials supplied at runtime in a private local config, and exclude credentials, prompts, and response text from source files, logs, and reports. Do not encode internal service addresses or credentials in repository docs.
