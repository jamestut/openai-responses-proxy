# OpenAI Responses Proxy

OpenAI Responses Proxy is a small HTTP proxy for the OpenAI Responses API. It authenticates local users with keys from a YAML file, sends requests to one configured upstream, retries upstream `429` responses, and records token usage in SQLite. It does not implement the Chat Completions API, other OpenAI APIs, HTTPS on its listener, or WebSockets.

## Build and run

Build the service with Go 1.24 or newer by running the script from the repository root:

```sh
./scripts/build.sh
```

The script writes `bin/responses-proxy` and embeds the exact tag at `HEAD`, or the abbreviated commit ID when `HEAD` has no tag. Without usable Git metadata, it uses version `dev`. It appends `-dev` when the worktree has staged, unstaged, or nonignored untracked changes. The reported build time is captured in UTC when the script starts. Run `bin/responses-proxy version` to display the version, full commit ID, build time, and Go version. Direct `go build` remains available; builds that do not set linker metadata report version `dev` and build time `unknown`.

The build script can also be run from another working directory by invoking it through its path, for example `/path/to/openai-responses-proxy/scripts/build.sh`.

Copy the example files to a private directory as `proxy.yaml` and `users.yaml`. Put the upstream API key and local user keys in those private files, then restrict their permissions:

```sh
cp examples/proxy.example.yaml /path/to/proxy.yaml
cp examples/users.example.yaml /path/to/users.yaml
chmod 600 /path/to/proxy.yaml /path/to/users.yaml
```

Start the service with `bin/responses-proxy serve /path/to/proxy.yaml`. The default listener is `127.0.0.1:8080`; set `listen` to a different address in the configuration if needed. The listener uses plain HTTP, so protect it with a trusted network boundary when clients connect remotely.

The proxy forwards requests to the configured upstream base URL. The example points to `https://api.openai.com/v1`; upstream HTTPS certificates and hostnames are verified. A private CA can be trusted with `upstream.ca_file`.

## Users and key reloads

The users YAML file maps each username to one or more Bearer keys. A client sends one of those keys as `Authorization: Bearer ...`. The proxy checks the key, records the associated username, and replaces the client credential with the upstream credential. Every valid user can access Responses resources and query the full accounting data, including activity from other users.

To reload keys without restarting the service, update the users YAML file and run:

```sh
bin/responses-proxy reload-keys /path/to/proxy.yaml
```

The running service rereads and validates the complete file through a private Unix socket under `state_dir`. A valid file replaces the active key map atomically. A malformed or unreadable file leaves the active keys in place and makes the command fail. Requests already authenticated keep their original username; new requests use the reloaded keys. Changes to the main configuration require a restart.

## Responses API requests

Set the client base URL to `http://127.0.0.1:8080/v1` and use a key from the users file. For example:

```sh
curl http://127.0.0.1:8080/v1/responses \
  -H 'Authorization: Bearer sk-user-example-alice' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-6-luna","input":"Say hello."}'
```

The proxy supports these Responses API routes: create a response, retrieve or delete a response, cancel a response, list response input items, compact input, and count input tokens. It forwards JSON fields and streaming responses. It does not validate model names; model availability is determined by the upstream.

Upstream `429` responses are retried using the configured delay with independent 20 percent jitter. `max_retries` is the number of retries after the first attempt, so the default of five permits at most six upstream attempts. The final `429` is returned after retries are exhausted. Other upstream HTTP errors are returned to the client. A client disconnect cancels work still in progress.

## Accounting

The proxy attempts one SQLite accounting insert when each response creation or compaction operation ends. It records the request start time in UTC, original username, model, operation kind, outcome, HTTP status, Response status, and standard token counts: input, output, total, cached, cache-write, and reasoning tokens. Detail counters are not added again to total tokens.

`kind` is `create` or `compact`. `http_status` is the upstream HTTP status, or `0` when no upstream HTTP response was received, including local request-body rejection. `response_status` is the lowercased Response lifecycle status, an SSE-inferred status, or `failed` inferred for an HTTP error; it stays empty when unavailable. `outcome` describes the operation's result, such as `completed`, `failed`, or `incomplete`.

Missing usage is zero. Nonterminal events and unterminated EOF frames do not contribute usage. Retrieval, deletion, cancellation, input-item listing, and input-token counting never create or update accounting rows. HTTP 429 retries produce only one accounting attempt for the overall operation.

There are no saved response IDs, pending accounting rows, provisional usage writes, or later-retrieval updates. Background responses still forward normally, but a queued response without usage records zero tokens; fetching it later does not change that record. Accounting inserts are best effort: a failed insert is logged and discarded, without write retries, recovery queues, or blocking future requests on storage availability. An operation still in flight has no row yet.

The `incomplete_reason` column uses a SQLite-enforced enum:

| Value | Meaning |
| --- | --- |
| `""` | No proxy-detected incomplete condition. Missing usage alone is allowed. |
| `transport_error` | The upstream request failed at the transport layer. |
| `response_read_error` | Reading a JSON body or unfinished SSE stream failed. |
| `client_disconnected` | The client disconnected before the operation finished. |
| `stream_ended` | The stream ended before a complete terminal Response event. |
| `nonterminal_response` | The JSON Response remained queued, running, or otherwise nonterminal. |
| `invalid_response` | A successful JSON response was not a usable Responses object. |

Every valid user can query global accounting aggregates through `GET /accounting`:

```sh
curl 'http://127.0.0.1:8080/accounting?group_by=model,user&timezone=UTC' \
  -H 'Authorization: Bearer sk-user-example-alice'
```

Without `group_by`, the response contains one summary under `totals`; with grouping, it contains a `groups` array. Each summary includes request count and input/output/total/cached/cache-write/reasoning token counts. Pending/incomplete aggregate counters have been removed. `group_by` accepts `model`, `user`, or `model,user`. Add exact `model` and `user` filters as needed. `period` accepts a year (`2026`), month (`2026-10`), or date (`2026-10-08`). Alternatively, provide both `start` and `end`; the range includes the start and excludes the end. Date-only boundaries are interpreted at local midnight in `timezone`; RFC3339 date-time boundaries must include an offset. The timezone is an IANA name and defaults to UTC.

For example, this query selects all records from October 2026 in Sydney local time and groups them by model:

```sh
curl 'http://127.0.0.1:8080/accounting?period=2026-10&timezone=Australia%2FSydney&group_by=model' \
  -H 'Authorization: Bearer sk-user-example-alice'
```

Malformed filters, invalid time zones, a range missing either boundary, or combining `period` with `start`/`end` returns HTTP 400. An ungrouped empty result contains zero totals; grouped empty results contain no groups. Totals remain exact even beyond the signed 64-bit aggregate range.

The [Python accounting query client](scripts/query_accounting.py) prints request and token counts from ungrouped or grouped summaries. Set `RESPONSES_PROXY_API_KEY` to a valid user key, then run:

```sh
export RESPONSES_PROXY_API_KEY='your-user-key'
python3 scripts/query_accounting.py
python3 scripts/query_accounting.py --group-by model,user --period 2026-10 --timezone UTC --human
python3 scripts/query_accounting.py --user alice --today
```

Use `--start` and `--end` instead of `--period` for a half-open time range. Empty grouped results print `No matching records.`; empty ungrouped results print zero totals.

## Retry, timeout, and logging settings

See the example configuration for all settings. The default `upstream_errors` log level records upstream errors, retry events, and service/accounting errors. It does not log downstream authentication failures. Set a broader level such as `debug` when diagnosing those failures. The default destination is stdout; a file path is resolved relative to the main config file. Credentials are excluded from logs.

The default retry delay is 30 seconds with five retries. Connection and TLS handshake timeouts default to 10 seconds. Response-header and total-request timeouts are disabled by default to allow long streaming responses to finish. A request-body limit defaults to 64 MiB.

## Testing

Run the automated Go tests with local synthetic fixtures:

```sh
go test ./...
go test -race ./...
```

See [docs/testing.md](docs/testing.md) for prerequisites, test conventions, and the mapping from the former manual scenarios to automated coverage. The standalone `cmd/mock-upstream` remains available as an optional developer tool; the test suite starts its own isolated fixtures. Build/version metadata and the Python accounting client are outside the suite's coverage.

## Operational notes

SQLite state, including the reload socket, lives under `state_dir`. Keep that directory private and persistent. On shutdown, the service drains handlers within `timeouts.shutdown`; if that deadline expires, it stops active work and exits rather than wait indefinitely. A failed insert, abrupt exit, or expired shutdown deadline can lose an accounting record. There is no write replay or recovery after restart.

Use a fresh database for this version. Older schemas are unsupported; there is no migration, schema rebuild, or compatibility layer. Existing database files are not deleted automatically. Retain backups according to your own operational needs.
