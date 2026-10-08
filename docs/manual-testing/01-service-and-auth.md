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
