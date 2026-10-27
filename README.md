# OpenAI Responses Proxy

OpenAI Responses Proxy authenticates local users, forwards Responses API create requests to one configured upstream, retries upstream `429` responses, and records token usage in SQLite. It supports `POST /v1/responses` and `GET /accounting` over HTTP. It does **not** implement other OpenAI APIs, HTTPS on its listener, or WebSockets.

## Configure

Copy [the example proxy configuration](examples/proxy.example.yaml) and [the example users file](examples/users.example.yaml) to a private directory. Replace the upstream API key and example user keys, then restrict access to both files:

```sh
cp examples/proxy.example.yaml /path/to/proxy.yaml
cp examples/users.example.yaml /path/to/users.yaml
chmod 600 /path/to/proxy.yaml /path/to/users.yaml
```

The proxy configuration requires `upstream.base_url`, `upstream.api_key`, and `users_file`. Set `upstream.base_url` to the complete upstream Responses endpoint, such as `https://api.openai.com/v1/responses`. The proxy removes a trailing slash. It sends the configured credential upstream as a Bearer token and verifies the upstream TLS certificate and hostname. Set `upstream.ca_file` to trust a private CA when needed.

The users file maps each username to one or more `api_keys`. A client presents one of these keys as `Authorization: Bearer ...`. The proxy records the username and replaces that credential with the upstream key. Each key must be unique, and every user must have at least one key.

See the example for optional listener, retry, timeout, logging, maximum request body size, and state directory settings. Durations use Go syntax such as `30s`. Relative paths for `users_file`, `upstream.ca_file`, `state_dir`, and a file logging destination resolve from the directory containing `proxy.yaml`, even when the process starts from another directory. The YAML files use strict decoding, so unknown fields cause an error.

## Start the service

Start the service with the config path:

```sh
bin/responses-proxy serve /path/to/proxy.yaml
```

The default listener is `127.0.0.1:8080`. The listener uses plain HTTP. Keep remote clients behind a trusted network boundary. The service creates `state_dir` with mode `0700` and keeps the SQLite database and key-reload socket at mode `0600`. Keep that directory private and persistent.

Changes to the main configuration require a restart. To reload user keys without restarting, edit the users file and run:

```sh
bin/responses-proxy reload-keys /path/to/proxy.yaml
```

The command asks the running process to read and validate the complete users file. A valid file replaces the active key set atomically. If it is invalid or unreadable, the command fails and the current keys remain active. Requests already authenticated retain their original username.

## Send a first request

Send `POST /v1/responses` to the proxy and use a key from the users file. Replace the example key in this request with your configured key:

```sh
curl http://127.0.0.1:8080/v1/responses \
  -H 'Authorization: Bearer sk-user-example-alice' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-6-luna","input":"Say hello."}'
```

The proxy forwards only `POST /v1/responses` to the configured upstream endpoint. It preserves the incoming query and request body, and it forwards streaming responses. The upstream determines which models are available. The proxy does not support other API routes.

The proxy retries only upstream `429` responses. `retry.max_retries` counts retries after the first attempt. For each retry, it independently adds or subtracts up to 20 percent of the configured delay. The proxy returns other upstream HTTP errors as received. A downstream disconnect cancels the active request.

## Query accounting

Every valid user can query global aggregates at `GET /accounting`. The endpoint returns totals or grouped summaries without raw request rows. For example:

```sh
curl 'http://127.0.0.1:8080/accounting?group_by=model,user&period=2026-10&timezone=UTC' \
  -H 'Authorization: Bearer sk-user-example-alice'
```

Supported query parameters are:

| Parameter | Meaning |
| --- | --- |
| `group_by` | Use `model`, `user`, or `model,user` to group results. Omit this parameter for a single `totals` summary. |
| `model`, `user` | Exact-match filters. |
| `period` | A calendar year (`2026`), month (`2026-10`), or date (`2026-10-08`). |
| `start`, `end` | Provide both to select the half-open interval `[start, end)`. Do not combine them with `period`. |
| `timezone` | IANA time zone for calendar periods and date-only bounds. Defaults to `UTC`. RFC3339 date-time bounds must include an offset. |

Grouped queries return a `groups` array. Each summary reports `request_count`, `input_tokens`, `output_tokens`, `total_tokens`, `cached_tokens`, `cache_write_tokens`, and `reasoning_tokens`. Invalid or repeated parameters, unknown filters, and invalid time ranges return HTTP 400. Empty ungrouped results contain zero totals. Empty grouped results contain no groups.

The [Python query client](scripts/query_accounting.py) can print these summaries. Set `RESPONSES_PROXY_API_KEY` to a valid user key, then run `python3 scripts/query_accounting.py --group-by model --period 2026-10` or use `--start` and `--end` for an explicit range.

## Accounting and operations

After each Responses create operation ends, the service attempts one SQLite insert. The record includes the request start time in UTC, username, model, outcome, upstream HTTP status, Response status, incomplete reason, and standard Responses token usage. Missing usage counts as zero. All upstream `429` retries share that insert.

A changed accounting schema requires a fresh database because the project does not provide migrations.

Accounting is best effort. The service logs and discards failed inserts without affecting request forwarding. Streaming usage comes only from a complete terminal event, and later retrieval does not update the record. An abrupt exit or expired shutdown deadline can leave records missing. The service does not replay failed inserts or recover missing records.

`http_status` is the upstream status, or `0` when no upstream HTTP response was received. The `incomplete_reason` field accepts `transport_error`, `response_read_error`, `client_disconnected`, `stream_ended`, `nonterminal_response`, `invalid_response`, or an empty string. It cannot contain a free-form error message.

At the default `upstream_errors` level, logs include upstream HTTP and transport errors, retry events, and service/accounting errors. They omit downstream authentication failures, credentials, and request or response bodies. A file log destination resolves relative to the main config file.

During shutdown, the service waits up to `timeouts.shutdown` for handlers to finish. If the deadline expires, it stops active work.

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
