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
- `internal/proxy` handles supported routes, upstream HTTP, retries, streaming, background polling, and the accounting query endpoint.
- `internal/accounting` owns the SQLite schema, durable request state, token parsing, and aggregate/time filtering; `internal/logging` owns log filtering and output.
- `cmd/mock-upstream` is the synthetic upstream used by the manual scenarios. Human- and AI-agent-readable test specifications are indexed in `docs/manual-testing/README.md`.

## Contracts to preserve

- This service implements the OpenAI Responses API only. Keep the public listener plain HTTP, do not add WebSocket support, and keep upstream HTTP or HTTPS certificate and hostname verification enabled.
- The configured upstream base URL must end in `/v1`; preserve its path prefix, incoming query, and request body when forwarding. Replace downstream Authorization with the configured upstream `Authorization: Bearer` credential and continue filtering hop-by-hop headers.
- Keep the main configuration and user-key YAML separate. YAML decoding is strict, durations use Go duration syntax, and relative users, CA, state, and log paths resolve from the main config file.
- A key reload must validate the entire users file before atomically replacing the active key snapshot. A failed reload leaves the old snapshot active, and requests already authenticated retain their original username. Main-config changes require restart.
- Only HTTP 429 responses are retried. `max_retries` counts retries after the first attempt; use the configured delay with independent plus-or-minus 20 percent jitter, ignore upstream `Retry-After`, and stop waiting or working when the downstream request is canceled.
- Preserve standard Responses SSE framing: only a complete event ending in a blank line can update or finalize usage. An unterminated event at EOF may provide a response ID for recovery but must not finalize accounting by itself.
- One accounting row is admitted for each response create or compact operation, before upstream work. Store UTC time, original username, model, outcome, and standard Responses token usage only. Missing usage is zero; do not add cost estimates or provider-specific usage fields. Retrieval, polling, and retries must not create duplicate rows.
- Background response IDs are durable recovery keys. Preserve idempotent updates, per-response poll de-duplication, and service-context cancellation. Do not bind recovery polling to the client request context.
- Every valid user can query global accounting, but the endpoint returns aggregates rather than raw request rows. Preserve model/user grouping, exact filters, half-open time ranges, and the UTC default timezone.
- Keep `state_dir` private: the directory is mode 0700 and the reload socket and SQLite database are mode 0600. The configured shutdown deadline bounds handler, poller, and accounting-write draining; pending durable rows can resume after restart, but unflushed updates may be lost when the deadline expires.
- The default `upstream_errors` log level includes upstream HTTP/transport errors and retry events while omitting downstream authentication failures. Never log credentials or request/response bodies. Keep HTTP status logs to sanitized metadata, and sanitize transport and polling errors before logging because raw URL errors can include query strings or response ID paths.

## Manual validation and documentation

- The project uses manual AI-agent test specifications and the synthetic mock, not a traditional unit-test suite. When the user authorizes validation, start with `docs/manual-testing/README.md`, use only synthetic data with the mock, and clean up local processes and state afterward.
- Update the relevant manual specification when a behavior or invariant changes. Keep `README.md`, the example YAML files, and the manual specifications consistent with `internal/config` and the implemented API.
- Live upstream checks require separate authorization. Use only `gpt-6-luna`, keep credentials supplied at runtime in a private local config, and exclude credentials, prompts, and response text from source files, logs, and reports. Do not encode internal service addresses or credentials in repository docs.
