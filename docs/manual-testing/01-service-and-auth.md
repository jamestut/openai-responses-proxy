# Service, routes, and authentication

## Case 1: Responses routes and request forwarding

**Setup:** Build both executables. Start a clean mock on `127.0.0.1:19090` and a proxy configured with `upstream.base_url: http://127.0.0.1:19090/v1`, a mock-only upstream key, listener `127.0.0.1:8080`, and user `alice` / `test-alice-key`. Start with an empty state directory and reset the mock using `POST /__control/reset`.

**Actions:** Send `POST /v1/responses` with `{"model":"gpt-6-luna","input":"synthetic hello","metadata":{"case":"forwarding"}}`. Use the returned response ID to call `GET /v1/responses/{id}`, `GET /v1/responses/{id}/input_items`, `POST /v1/responses/{id}/cancel`, and `DELETE /v1/responses/{id}`. Then retrieve the deleted ID again and query accounting. Also call `POST /v1/responses/compact` with a synthetic input and `POST /v1/responses/input_tokens` with a synthetic input. Query mock state. Send `POST /v1/chat/completions` as a negative protocol-scope check.

**Expected:** Supported routes return the matching mock response status and JSON shape. The Responses create body, including unfamiliar metadata, reaches the mock unchanged. For every forwarded request, the mock reports `authorization_checked: true` and `authorization_matches_expected: true` for its synthetic upstream key, while still redacting the received value; this proves the client key was replaced. After deletion, retrieval returns HTTP 404, but the original completed ledger row retains its token counters and `incomplete_count` remains zero. The unsupported Chat Completions path returns a local 404 and does not increment any mock route count. Each API request reaches the expected method and path exactly once.

**Evidence:** Save the sanitized client statuses and response IDs, mock call counts and method/path/body records, and the unsupported-route count. Do not save credential values.

**Cleanup:** Stop the proxy and mock, then remove this case's local state directory and configuration.

## Case 2: User key validation and identity

**Setup:** Start a fresh mock and proxy. Configure `alice` with two test keys and `bob` with one. Reset the mock and use a fresh database.

**Actions:** Create one response with each of Alice's keys and one with Bob's key. Attempt a request with an unknown key and with no Authorization header. Query `/accounting` using either valid user key.

**Expected:** All three valid keys authenticate and requests are attributed to the owning username. Both invalid requests receive HTTP 401 and cause no upstream call. Either valid user can read totals for all three admitted requests.

**Evidence:** Save the three mock call records, authentication status codes, and accounting result grouped by user. Verify there are no extra upstream calls from invalid authentication.

**Cleanup:** Stop processes and remove this case's local files.

## Case 3: Build metadata and command arguments

**Setup:** Use a disposable copy of the source checkout for the Git-state checks and refer to its path as `$checkout`. Build the proxy with `$checkout/scripts/build.sh` from a directory outside that copy. Keep the default `bin/` build output ignored.

**Actions:** Run `$checkout/bin/responses-proxy version` and record its output twice without rebuilding. In the disposable checkout, build once with `HEAD` at an exact tag and once with an untagged commit that has a tagged ancestor. Separately build with one staged change, one unstaged tracked change, and one nonignored untracked file; restore the clean state between checks. Build again with only ignored files under `bin/`. Run `$checkout/bin/responses-proxy version extra`, `$checkout/bin/responses-proxy serve`, and `$checkout/bin/responses-proxy reload-keys`.

**Expected:** The version command works without a config argument and reports `Version:`, `Commit:`, `Build time:`, and `Go version:`. Repeated runs of the same binary report the same build time. The version is the exact tag at `HEAD`, or the abbreviated current commit when `HEAD` is untagged; it never substitutes a nearby ancestor tag. If multiple exact tags point to `HEAD`, the selected tag follows Git's standard selection. Each staged, unstaged, or nonignored untracked change adds `-dev`, while ignored `bin/` outputs do not. The commit is the full revision when Git metadata is available. The build time is UTC in `YYYY-MM-DDTHH:MM:SSZ` form. Invalid or incomplete positional commands return status 2 and print usage.

**Evidence:** Save the version output for each clean/dirty state and the status and usage output for the invalid commands. Do not record unrelated source contents.

**Cleanup:** Remove the disposable checkout and its generated binaries.

## Case 4: Source archive and direct build defaults

**Setup:** Use a source-only archive with no Git metadata, refer to its extracted path as `$archive`, and use Go 1.24 or newer.

**Actions:** Run `$archive/scripts/build.sh` and then `$archive/bin/responses-proxy version`. Also build the proxy directly with `go build -o bin/responses-proxy-direct ./cmd/responses-proxy` from the source root and run `bin/responses-proxy-direct version`.

**Expected:** The scripted source-archive build reports `Version: dev`, an unknown full commit, a UTC build time, and the embedded Go version. The direct build remains usable and reports the default `Version: dev` and `Build time: unknown` values.

**Evidence:** Save the four reported metadata fields for both binaries and confirm the direct build exits successfully.

**Cleanup:** Remove the extracted archive and generated binaries.
