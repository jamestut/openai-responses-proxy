# 429 retries, cancellation, concurrency, and streaming

## Case 1: Retry count, jitter, and final 429

**Setup:** Start fresh mock and proxy processes with `retry.delay: 1s` and `retry.max_retries: 5`. Reset mock state. Send all requests with `alice`'s test key and synthetic JSON only.

**Actions:** Queue five `429` replies followed by one `200` completed response for `POST /v1/responses`, then submit one create request. Reset the mock, create a response to obtain its ID, queue one `429` for `GET /v1/responses/{id}`, and retrieve it. Reset the mock. Queue six `429` replies with distinct JSON error messages and `Retry-After: 99`, then submit another create request. For each run, read `/__control/state` and compare timestamps for consecutive upstream calls.

**Expected:** The first create succeeds on attempt six. Its five retry intervals are each approximately 0.8 to 1.2 seconds, allowing up to 300 ms additional scheduling/transport overhead. The retrieval retries once after 429 and then succeeds with two GET attempts; retrieval neither adds nor changes accounting. The exhausted create is attempted exactly six times and returns the sixth `429` status and body to the client. The `Retry-After` value does not replace the configured delay. The mock receives the identical method, path, query, and body on every retry. Each create has one accounting insert when it ends, not one per attempt.

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

**Expected:** In the first run, the mock sees only the initial 429 call and no later retry. In the second run, the mock records the delayed request as canceled. Both operations attempt one zero-token accounting insert on exit, with `incomplete_reason: client_disconnected`. The prior 429 remains available as `http_status` in the retry-wait case. The proxy does not continue upstream work after the downstream context is canceled, and neither operation can later recover usage.

**Evidence:** Save client termination status, timestamped upstream counts, and the mock's canceled flag. Confirm one record per finished operation when SQLite is writable; inspect only synthetic outcome/status/reason fields and aggregates.

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

**Expected:** The proxy returns `text/event-stream`, forwards the delta before the delayed terminal event, and does not buffer the entire stream. The first terminal usage is recorded once with input 12, cached 2, cache-write 1, output 4, reasoning 1, total 16. Cached, cache-write, and reasoning detail are not added again to total tokens. For the second stream, provisional usage is ignored and the terminal event without usage records zero tokens. No row is inserted or updated for a nonterminal event.

**Evidence:** Save event order and arrival times, sanitized mock call count, and the accounting summary. Keep only synthetic event text.

**Cleanup:** Stop processes and remove local files.

## Case 6: Terminal SSE frame at EOF without the blank-line delimiter

**Setup:** Start a proxy and a purpose-built local upstream. Configure the upstream to answer `POST /v1/responses` with `Content-Type: text/event-stream`, a `response.completed` data line ending in one newline but no following blank line, and then close the connection. Give the event a synthetic response ID and usage. Configure `GET /v1/responses/{id}` to return the same terminal Response object as JSON with known usage. Use a fresh accounting database.

**Actions:** Submit one create request with `stream: true`. Record the client stream and query accounting immediately. Wait several seconds without making another request and query accounting again. Then retrieve the saved ID with a client `GET /v1/responses/{id}` through the proxy and query accounting again. Inspect the local upstream request counts.

**Expected:** The proxy forwards the received terminal data line but does not count usage from an event lacking the SSE blank-line delimiter. The finished create records zero tokens and `incomplete_reason: stream_ended`; no response ID is saved. The proxy sends no automatic GET. A client GET forwards the terminal JSON Response or 404 unchanged and does not insert or update accounting, even when its body contains usage. The original zero-token record remains unchanged across restart.

**Evidence:** Save the sanitized client stream, aggregate summaries before and after the client GET, local POST/GET counts, and final row count. Use only synthetic IDs and input.

**Cleanup:** Stop processes and remove local files.

## Case 7: SSE delimiters and terminal outcomes

**Setup:** Use a purpose-built local upstream and fresh database so raw SSE framing can be controlled. Use only synthetic payloads and credentials.

**Actions:** Send a stream with a heartbeat comment, multiline `data:` fields, and CRLF delimiters, ending in a complete terminal Response with usage. Repeat with `response.failed` and `response.incomplete` terminal payloads. Then send a terminal payload followed only by a whitespace-only line and EOF, without an empty-line delimiter. Finally, send a complete terminal event followed by a nonterminal event with different usage/status metadata.

**Expected:** Raw event bytes are forwarded, standalone heartbeat comments flush promptly, and multiline data is parsed as one event. Complete terminal failed/incomplete events contribute their own usage just as completed events do. A whitespace-only line is not an SSE event delimiter: that operation records zero usage with reason `stream_ended`. Once terminal accounting data is captured, later nonterminal events cannot overwrite its usage or metadata. Each operation still attempts only one insert at handler exit.

**Evidence:** Save synthetic framing, arrival order, aggregates, and sanitized enum/status values. Do not include credentials.

**Cleanup:** Stop processes and remove local files.
