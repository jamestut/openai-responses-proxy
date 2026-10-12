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

The SQLite ledger records one row for each admitted response creation or compaction request, including its start time in UTC, username, model, outcome, and standard Responses token usage. It stores input, output, total, cached, cache-write, and reasoning token counts. Missing usage is recorded as zero. Retrieval, cancellation, input-token counting, and retries do not create additional accounting rows. A create that returns a queued or running Response, or a stream interrupted after yielding a usable response ID, remains pending until a client retrieves `GET /v1/responses/{id}` and observes a terminal Response. That retrieval updates the original row with its original username and model. There is no automatic retrieval, and pending rows remain pending across restart until a client retrieves them. A plain client GET 404 without a valid Response or usage leaves the row pending. An unterminated SSE event may provide an ID for a later client retrieval, but cannot finalize usage by itself. A nonterminal create that provides no usable response ID can remain incomplete.

Every valid user can query the full ledger through `GET /accounting`:

```sh
curl 'http://127.0.0.1:8080/accounting?group_by=model,user&timezone=UTC' \
  -H 'Authorization: Bearer sk-user-example-alice'
```

Without `group_by`, the response contains one summary under `totals`; with grouping, it contains a `groups` array. Each summary includes request count, input/output/total tokens, cached/cache-write/reasoning tokens, and pending/incomplete counts. `group_by` accepts `model`, `user`, or `model,user`. Add exact `model` and `user` filters as needed. `period` accepts a year (`2026`), month (`2026-10`), or date (`2026-10-08`). Alternatively, provide both `start` and `end`; the range includes the start and excludes the end. Date-only boundaries are interpreted at local midnight in `timezone`; RFC3339 date-time boundaries must include an offset. The timezone is an IANA name and defaults to UTC.

For example, this query selects all records from October 2026 in Sydney local time and groups them by model:

```sh
curl 'http://127.0.0.1:8080/accounting?period=2026-10&timezone=Australia%2FSydney&group_by=model' \
  -H 'Authorization: Bearer sk-user-example-alice'
```

The [Python accounting query client](scripts/query_accounting.py) prints the same aggregate as nested bullet lists and needs only Python 3. Set a proxy user key in `RESPONSES_PROXY_API_KEY`; the key is read from the environment and is not accepted as a command-line argument:

```sh
export RESPONSES_PROXY_API_KEY='your-proxy-user-key'
python3 scripts/query_accounting.py
python3 scripts/query_accounting.py --group-by user
python3 scripts/query_accounting.py --group-by model
python3 scripts/query_accounting.py --group-by model,user --model gpt-6-luna --user alice
python3 scripts/query_accounting.py --period 2026-10 --timezone Australia/Sydney
python3 scripts/query_accounting.py --start 2026-10-01 --end 2026-11-01 --timezone Australia/Sydney
```

`--base-url` defaults to the proxy root `http://127.0.0.1:8080`; it can point to another proxy root when needed. Use `--model` and `--user` independently or together to apply exact filters. Calendar periods and date-only range bounds use the selected IANA timezone, which defaults to UTC.

An ungrouped empty result prints zero totals. An empty grouped result prints `No matching records.`

Malformed filters, invalid time zones, a range missing either boundary, or combining `period` with `start`/`end` returns HTTP 400. An ungrouped empty result contains zero totals; grouped empty results contain no groups. Results include pending and incomplete counts.

## Retry, timeout, and logging settings

See the example configuration for all settings. The default `upstream_errors` log level records upstream errors and retry events. It does not log downstream authentication failures. Set a broader level such as `debug` when diagnosing those failures. The default destination is stdout; a file path is resolved relative to the main config file. Credentials are redacted from logs.

The default retry delay is 30 seconds with five retries. Connection and TLS handshake timeouts default to 10 seconds. Response-header and total-request timeouts are disabled by default to allow long streaming responses to finish. A request-body limit defaults to 64 MiB.

## Manual testing

This repository provides AI-agent-readable manual test specifications and a local mock upstream; it does not include a traditional unit-test suite. Start with [docs/manual-testing/README.md](docs/manual-testing/README.md). The mock helper can queue HTTP responses, delays, and SSE events; inspect received requests with authorization values redacted; and generate a local CA for HTTPS checks.

## Operational notes

SQLite state, including the reload socket, lives under `state_dir`. Keep that directory private and persistent. If the service loses the final upstream response before usage can be recorded, a row with a saved response ID remains pending for client retrieval; a nonterminal create with no usable ID can remain incomplete. On shutdown, the service drains handlers and queued accounting writes within `timeouts.shutdown`; if that deadline expires, it exits rather than wait indefinitely. Pending rows remain pending after restart until a client GET observes a terminal Response. Accounting updates that were not flushed before the deadline may be lost. No schema migration runs: fresh databases omit the legacy `background` column and polling state index, while existing databases retain their schema. Retain database backups according to your own operational needs.
