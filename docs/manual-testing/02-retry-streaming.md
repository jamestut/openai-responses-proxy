# 429 retries, cancellation, concurrency, and streaming

## Case 1: Retry count, jitter, and final 429

**Setup:** Start fresh mock and proxy processes with `retry.delay: 1s` and `retry.max_retries: 5`. Reset mock state. Send all requests with `alice`'s test key and synthetic JSON only.

**Actions:** Queue five `429` replies followed by one `200` completed response for `POST /v1/responses`, then submit one create request. Reset the mock, create a response to obtain its ID, queue one `429` for `GET /v1/responses/{id}`, and retrieve it. Reset the mock. Queue six `429` replies with distinct JSON error messages and `Retry-After: 99`, then submit another create request. For each run, read `/__control/state` and compare timestamps for consecutive upstream calls.

**Expected:** The first create succeeds on attempt six. Its five retry intervals are each approximately 0.8 to 1.2 seconds, allowing up to 300 ms additional scheduling/transport overhead. The retrieval retries once after 429 and then succeeds with two GET attempts; retrieval adds no accounting row. The exhausted create is attempted exactly six times and returns the sixth `429` status and body to the client. The `Retry-After` value does not replace the configured delay. The mock receives the identical method, path, query, and body on every retry. Each admitted create appears once in accounting, not once per attempt.

**Evidence:** Save client status/body, attempt count, sanitized mock records, retry timing deltas, and accounting request count. Do not include Authorization values.

**Cleanup:** Stop processes and remove the local state and test config.

## Case 2: Other statuses and redirects

**Setup:** Start a fresh mock and proxy with a short retry delay. Reset the mock.

**Actions:** Queue one each of HTTP 400 and HTTP 500 with synthetic JSON error bodies; submit a create request for each. Queue a 302 with `Location: /v1/responses` and a synthetic body; submit another create request. Read mock state.

**Expected:** Each status and body is passed through immediately, with one upstream call per request. The proxy does not follow the redirect and does not retry statuses other than 429.

**Evidence:** Save client status and sanitized body for each call, and mock counts showing exactly one call per scripted response.

**Cleanup:** Stop processes and remove local files.

## Case 3: Client disconnect during retry wait and active upstream request

**Setup:** Start a fresh mock and proxy with `retry.delay: 2s`, `max_retries: 5`. Reset mock state.

**Actions:** Queue one `429`, start a create request, then terminate the client connection within 300 ms while the proxy is waiting to retry. Wait two seconds and inspect mock state. Reset. Queue a `200` response with `delay_ms: 5000`, start another create request, then terminate the client within 300 ms. Inspect mock state again.

**Expected:** In the first run, the mock sees only the initial 429 call and no later retry; accounting records a complete failed row with zero usage because the upstream returned a definitive 429. In the second run, the mock records the delayed request as canceled. Since the request was written upstream but its result was lost, accounting marks the row incomplete. The proxy does not continue upstream work after the downstream context is canceled.

**Evidence:** Save client termination status, timestamped upstream counts, and the mock's canceled flag. Query accounting and confirm each accepted create has at most one row; report its final outcome/usage state as implemented.

**Cleanup:** Stop processes and remove local state/config.

## Case 4: Concurrent requests

**Setup:** Start a fresh mock and proxy. Reset the mock. Queue three `200` responses for `POST /v1/responses`, each delayed by 1000 ms.

**Actions:** Launch three independent create requests at once using separate client processes or an equivalent parallel HTTP client. Record start/end times and read mock state.

**Expected:** All requests succeed. Their mock receipt timestamps are close together and total wall time is below three seconds, showing requests overlap. Accounting contains three rows with the right user and model.

**Evidence:** Save the three request timestamps, client completion times/statuses, and grouped accounting totals.

**Cleanup:** Stop processes and remove local files.

## Case 5: SSE forwarding and terminal usage

**Setup:** Start a fresh mock and proxy. Reset mock state. Prepare a stream with a `response.output_text.delta` event containing synthetic text and a `response.completed` event containing a standard Response object with `usage` (`input_tokens: 12`, `input_tokens_details.cached_tokens: 2`, `input_tokens_details.cache_write_tokens: 1`, `output_tokens: 4`, `output_tokens_details.reasoning_tokens: 1`, and `total_tokens: 16`). Delay the terminal event by 700 ms.

**Actions:** Queue those two events for `POST /v1/responses`. Send a create request with `stream: true`, reading with `curl -N` or an equivalent client that records when each event arrives. Query accounting when the stream ends. Then queue a second stream whose `response.in_progress` event contains a standard Response `usage` object with the same counters, while `response.completed` for the same response ID has no `usage`; send another streamed request and query again.

**Expected:** The proxy returns `text/event-stream`, forwards the delta before the delayed terminal event, and does not buffer the entire stream. The first terminal usage is recorded once with input 12, cached 2, cache-write 1, output 4, reasoning 1, total 16. Cached, cache-write, and reasoning detail are not added again to total tokens. For the second stream, the earlier standard usage observation remains recorded when a later terminal event omits the usage object; it is not reset to zero or counted twice.

**Evidence:** Save event order and arrival times, sanitized mock call count, and the accounting summary. Keep only synthetic event text.

**Cleanup:** Stop processes and remove local files.

## Case 6: Terminal SSE frame at EOF without the blank-line delimiter

**Setup:** Start a proxy with a short background poll interval and a purpose-built local upstream. Configure the upstream to answer `POST /v1/responses` with `Content-Type: text/event-stream`, a `response.completed` data line ending in one newline but no following blank line, and then close the connection. Give the event a synthetic response ID and usage. Configure `GET /v1/responses/{id}` to return the same terminal Response object as JSON with known usage. Use a fresh accounting database.

**Actions:** Submit one create request with `stream: true`. Record the client stream, query accounting immediately, then wait for the recovery poll and query accounting again. Inspect the local upstream request counts.

**Expected:** The proxy forwards the received terminal data line, but does not treat an event lacking the SSE blank-line delimiter as dispatched or finalize accounting from that frame. It saves the response ID for recovery. The poller retrieves the terminal JSON Response, completes the original accounting row with its usage, and does not add a second row. If the fixture instead returns 404 for the saved ID, the same row becomes incomplete.

**Evidence:** Save the sanitized client stream, pre-recovery and post-recovery aggregate summaries, local POST/GET counts, and final row count. Use only synthetic IDs and input.

**Cleanup:** Stop processes and remove local files.
