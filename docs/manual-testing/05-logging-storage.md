# Logging, key reload, and SQLite write failures

## Case 1: Default logs and expanded logging

**Setup:** Start a fresh mock and proxy with the default `logging.level: upstream_errors` and stdout destination. Capture stdout privately. Reset mock state.

**Actions:** Send an invalid downstream key. Then queue and submit one upstream 500 and one 429 followed by a successful response. Restart with `logging.level: debug` and a file destination relative to the config file. Send another invalid key. Inspect logs with a quiet search that reports only match/no-match; do not print matching lines if they contain credentials.

**Expected:** At the default level, the upstream 500 error and exhausted 429 are logged with status, method, sanitized route, and status category; the 429 retry warning is present; downstream authentication failure is omitted. HTTP error logs do not contain response bodies, query strings, response IDs, or API keys. At debug level, the authentication failure is visible. File logging uses the config-relative path and stdout is the default destination.

**Evidence:** Save status codes, mock counts, log byte counts, and boolean search results for event presence and key absence. Do not paste log lines or test key values into the report.

**Cleanup:** Stop processes and remove local logs, config, and database.

## Case 2: Atomic key reload, failed reload, and in-flight identity

**Setup:** Start a fresh mock and proxy. Use `alice` / `test-alice-old`, `bob` / `test-bob-key`, and a private `state_dir`. Confirm directory and socket permissions with `stat` after startup.

**Actions:** Replace Alice's key in the users file with `test-alice-new`, add a second key for her, then run `responses-proxy reload-keys --config ...`. Test old and new keys. Next write a malformed users file containing a duplicate key, run reload again, and test the last valid key map. Queue a delayed 200 response. Start a create request with Bob's key, wait until the mock records receipt, remove Bob from the users file, reload, and let the in-flight request finish. Test Bob's key on a new request and query accounting by user.

**Expected:** A valid reload exits successfully and atomically activates new keys; removed keys stop authenticating and new keys work. A malformed reload exits nonzero and retains the prior valid map. New requests after Bob's removal fail authentication, while the already-authenticated request completes and remains attributed to Bob. The state directory is mode `0700` and the Unix reload socket is mode `0600`.

**Evidence:** Save CLI exit statuses, authentication status codes, redacted mock counts, accounting grouped by user, and numeric filesystem modes. Never include key values.

**Cleanup:** Stop processes and delete local files.

## Case 3: SQLite write lock, admission control, and recovery

**Setup:** Use a fresh database and start the mock and proxy. The implementation currently uses a five-second SQLite busy timeout; check the configured value before running this case. Queue one successful response delayed by several seconds. An external SQLite CLI/client is needed to hold a lock.

**Actions:** Start the delayed create request. After the mock records it and while the request is accepted, open `manual-test-state/state/accounting.sqlite` (or the equivalent path under the configured state directory) in the external client and run `BEGIN IMMEDIATE;` without committing. Wait for the upstream response and for its final accounting write to exceed the busy timeout. While the external transaction remains open, submit a second create request and inspect mock state. Release the external lock with `ROLLBACK;`. Wait for the service's retained write to retry, query accounting, and submit another create request.

**Expected:** The already accepted operation is retained for write retry and is not sent upstream again. New billable admissions fail with HTTP 503 while storage cannot write, and the mock receives no request for that rejected admission. After the SQLite lock is released, the accepted row becomes complete with the correct usage, accounting admission reopens, and a new request reaches the mock.

**Evidence:** Save lock acquisition/release times, client status codes, mock counts before/while/after the lock, and final accounting totals. Do not include credentials or request content.

**Cleanup:** Stop processes, release any open SQLite transaction, confirm no accounting write remains pending, and remove local database/configuration.
