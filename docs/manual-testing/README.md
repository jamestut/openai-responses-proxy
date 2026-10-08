# Manual testing with an AI agent

These are hands-on test specifications, not a unit-test suite or an automated test runner. The operator or AI agent performs each action, records the listed evidence, compares it with the acceptance criteria, and cleans up its local processes and temporary files. Do not use a production key with the mock. For any live integration check, use only the authorized `gpt-6-luna` model and never copy credentials or sensitive request content into evidence.

## Build the binaries

Run from the repository root:

```sh
mkdir -p bin manual-test-state
go build -o bin/responses-proxy ./cmd/responses-proxy
go build -o bin/mock-upstream ./cmd/mock-upstream
```

The mock has an upstream listener and a separate loopback-only control listener. Start it in a terminal:

```sh
bin/mock-upstream --listen 127.0.0.1:19090 --admin-listen 127.0.0.1:19091 --expected-bearer mock-upstream-key
```

The control API can queue scripted upstream responses, inspect request evidence, and reset its state. It never returns or stores the `Authorization` value; it reports whether the header was present, its scheme, and `authorization_redacted: true`. When `--expected-bearer` is set, the mock also reports whether the received Bearer value matches that synthetic key without retaining the value. Request bodies appear in the local control state to support forwarding checks, so use only synthetic test data.

Queue scripted responses for one method and path:

```sh
curl -sS -X POST http://127.0.0.1:19091/__control/queue \
  -H 'Content-Type: application/json' \
  -d '{"method":"POST","path":"/v1/responses","responses":[{"status":429,"headers":{"Retry-After":"99"},"body":{"error":{"message":"try later"}}}]}'
```

Use `delay_ms` for a delayed response. Use an `events` array for SSE; each event has optional `delay_ms` and a JSON `data` value. The mock flushes every event and terminates the stream with `[DONE]`. The control API also supports `GET /__control/state`, `POST /__control/reset`, and `POST /__control/background` with `{"complete_after_polls":2}`. Default Responses routes return synthetic standard usage, while scripted responses let an agent exercise errors, missing usage, redirects, and alternate terminal events.

Read current mock evidence with:

```sh
curl -sS http://127.0.0.1:19091/__control/state
```

The response includes per-method/path call counts and a timestamped record for each received request: method, path, authorization presence and scheme, redaction/match status, synthetic body, final status, cancellation status, and completion time.

## Create a local proxy configuration

Use a test-only key and isolated state directory. `retry.delay` should be short for retry scenarios, for example `1s`; leave `max_retries: 5` unless a scenario says otherwise. Start the proxy with:

```sh
bin/responses-proxy serve manual-test-state/proxy.local.yaml
```

The sample files under `examples/` provide the current YAML structure. For this fixture, set `upstream.base_url` to `http://127.0.0.1:19090/v1`, set `upstream.api_key: mock-upstream-key`, set `users_file: ./users.yaml` with `alice` / `test-alice-key` and `bob` / `test-bob-key`, and set `state_dir: ./state`. Use `listen: 127.0.0.1:8080`. These relative paths resolve from `manual-test-state/proxy.local.yaml`. Local configuration and database paths are ignored by Git.

Send client requests to `http://127.0.0.1:8080/v1` with `Authorization: Bearer test-alice-key`. Query accounting at `http://127.0.0.1:8080/accounting` with the same user key. Run each scenario with a fresh mock control state and fresh state directory unless that scenario explicitly checks persistence or restart behavior.

## Specifications

- [Service, routes, and authentication](01-service-and-auth.md)
- [429 retries, cancellation, concurrency, and streaming](02-retry-streaming.md)
- [TLS, background responses, and restart recovery](03-tls-background.md)
- [Token accounting and aggregate queries](04-accounting.md)
- [Logging, reload safety, and storage failures](05-logging-storage.md)

For every case, keep evidence to status codes, sanitized JSON fields, call counts/timestamps, accounting aggregates, and redacted log excerpts. Never include API keys, Authorization values, live prompts, or upstream response content from live checks. A case passes only when all stated client, upstream, accounting, and log expectations hold; otherwise record the exact failed expectation and the smallest sanitized evidence needed to reproduce it.
