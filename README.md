# OpenAI Responses Proxy

OpenAI Responses Proxy authenticates local users, forwards OpenAI Responses API requests to one configured upstream, retries upstream `429` responses, and records token usage in SQLite. It supports the Responses API over HTTP. It does **not** implement other OpenAI APIs, HTTPS on its listener, or WebSockets.

## Configure

Copy [the example proxy configuration](examples/proxy.example.yaml) and [the example users file](examples/users.example.yaml) to a private directory. Replace the upstream API key and example user keys, then restrict access to both files:

```sh
cp examples/proxy.example.yaml /path/to/proxy.yaml
cp examples/users.example.yaml /path/to/users.yaml
chmod 600 /path/to/proxy.yaml /path/to/users.yaml
```

The proxy configuration requires `upstream.base_url`, `upstream.api_key`, and `users_file`. The upstream URL must be an absolute HTTP(S) URL ending in `/v1`; the configured credential is sent upstream as a Bearer token. Upstream TLS certificate and hostname checks stay enabled. Set `upstream.ca_file` to trust a private CA when needed.

The users file maps each username to one or more `api_keys`. A client presents one of these keys as `Authorization: Bearer ...`; the proxy records the username and replaces that credential with the upstream key. Each key must be unique, and every user must have at least one key.

See the example for optional listener, retry, timeout, logging, request-size, and state-directory settings. Durations use Go syntax such as `30s`. Relative paths for `users_file`, `upstream.ca_file`, `state_dir`, and a file logging destination are resolved from the directory containing `proxy.yaml`, regardless of the process working directory. The YAML files are decoded strictly, so unknown fields are errors.

## Start the service

Start the service with the config path:

```sh
bin/responses-proxy serve /path/to/proxy.yaml
```

The default listener is `127.0.0.1:8080`. The listener uses plain HTTP; keep remote clients behind a trusted network boundary. The service creates `state_dir` with mode `0700` and keeps the SQLite database and key-reload socket at mode `0600`. Keep that directory private and persistent.

Main-config changes require a restart. To reload user keys without restarting, edit the users file and run:

```sh
bin/responses-proxy reload-keys /path/to/proxy.yaml
```

The command asks the running process to read and validate the complete users file. A valid file replaces the active key set atomically. If it is invalid or unreadable, the command fails and the current keys remain active. Requests already authenticated retain their original username.

## Send a first request

Point a Responses API client at `http://127.0.0.1:8080/v1` and use a key from the users file. Replace the example key in this request with your configured key:

```sh
curl http://127.0.0.1:8080/v1/responses \
  -H 'Authorization: Bearer sk-user-example-alice' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-6-luna","input":"Say hello."}'
```

The proxy forwards Responses API create, retrieve, delete, cancel, input-item listing, compact, and input-token counting routes. It forwards request fields and streaming responses; model availability is determined by the upstream.

Only upstream `429` responses are retried. `retry.max_retries` counts attempts after the first, and the configured delay gets independent plus-or-minus 20 percent jitter. Other upstream HTTP errors are returned as received. A downstream disconnect cancels work in progress.

## Query accounting

Every valid user can query global aggregates at `GET /accounting`; results are grouped or totaled, never raw request rows. For example:

```sh
curl 'http://127.0.0.1:8080/accounting?group_by=model,user&period=2026-10&timezone=UTC' \
  -H 'Authorization: Bearer sk-user-example-alice'
```

Supported query parameters are:

| Parameter | Meaning |
| --- | --- |
| `group_by` | `model`, `user`, or `model,user`; omit for a single `totals` summary. |
| `model`, `user` | Exact-match filters. |
| `period` | A calendar year (`2026`), month (`2026-10`), or date (`2026-10-08`). |
| `start`, `end` | Both required together; selects the half-open interval `[start, end)`. Cannot be combined with `period`. |
| `timezone` | IANA time zone for calendar periods and date-only bounds; defaults to `UTC`. RFC3339 date-time bounds must include an offset. |

Grouped queries return a `groups` array. Each summary reports `request_count`, `input_tokens`, `output_tokens`, `total_tokens`, `cached_tokens`, `cache_write_tokens`, and `reasoning_tokens`. Invalid or repeated parameters, unknown filters, and invalid time ranges return HTTP 400. Empty ungrouped results contain zero totals; empty grouped results contain no groups.

The [Python query client](scripts/query_accounting.py) can print these summaries. Set `RESPONSES_PROXY_API_KEY` to a valid user key, then run `python3 scripts/query_accounting.py --group-by model --period 2026-10` or use `--start` and `--end` for an explicit range.

## Accounting and operations

The proxy attempts one accounting insert when a response create or compact operation ends. It records request start time in UTC, username, model, kind, outcome, upstream HTTP status, Response status, incomplete reason, and standard Responses token usage. Missing usage counts as zero. Retrieval and other utility routes do not create or update records; a `429` retry sequence still results in one accounting attempt.

Accounting is best effort. Failed inserts are logged and discarded without retrying writes or preventing future requests. An in-flight request has no row yet. Streaming usage is taken only from a complete terminal event; missing usage counts as zero. Background responses without usage are recorded with zero tokens, and later retrieval does not update the record. Abrupt exit, a failed insert, or expiry of the configured shutdown deadline can leave a record missing. There is no replay or recovery.

`http_status` is the upstream status, or `0` when no upstream HTTP response was received. The `incomplete_reason` field is limited to `transport_error`, `response_read_error`, `client_disconnected`, `stream_ended`, `nonterminal_response`, `invalid_response`, or the empty string. It is diagnostic metadata, not a free-form error message.

The default `upstream_errors` log level includes upstream HTTP and transport errors, retry events, and service/accounting errors, while omitting downstream authentication failures. Logs exclude credentials and request or response bodies. A file log destination is resolved relative to the main config file.

The service drains handlers up to `timeouts.shutdown` during shutdown. If the deadline expires, active work is stopped. Use a fresh database for this version; older schemas are unsupported and are not migrated.

## Build from source

With Go 1.24 or newer, run `./scripts/build.sh` from the repository root to write `bin/responses-proxy`. Run `bin/responses-proxy version` to display its build information.

## Run tests

With Go 1.24 or newer, run the Go test suite from the repository root:

```sh
go test ./...
```

To also check for data races, run:

```sh
go test -race ./...
```

For interactive development, `cmd/mock-upstream` provides a local synthetic upstream.
