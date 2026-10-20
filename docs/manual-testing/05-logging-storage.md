# Logging, key reload, and SQLite write failures

## Case 1: Default logs and expanded logging

**Setup:** Start a fresh mock and proxy with the default `logging.level: upstream_errors` and stdout destination. Capture stdout privately. Reset mock state.

**Actions:** Send an invalid downstream key. Then queue and submit one upstream 500 and one 429 followed by a successful response. Restart with `logging.level: debug` and a file destination relative to the config file. Send another invalid key. Inspect logs with a quiet search that reports only match/no-match; do not print matching lines if they contain credentials.

**Expected:** At the default level, the upstream 500 error and exhausted 429 are logged with status, method, sanitized route, and status category; the 429 retry warning is present; downstream authentication failure is omitted. HTTP error logs do not contain response bodies, query strings, response IDs, or API keys. At debug level, the authentication failure is visible. File logging uses the config-relative path and stdout is the default destination.

**Evidence:** Save status codes, mock counts, log byte counts, and boolean search results for event presence and key absence. Do not paste log lines or test key values into the report.

**Cleanup:** Stop processes and remove local logs, config, and database.

## Case 2: Atomic key reload, failed reload, and in-flight identity

**Setup:** Start a fresh mock and proxy. Use `alice` / `test-alice-old`, `bob` / `test-bob-key`, and a private `state_dir`. Confirm directory and socket permissions with `stat` after startup.

**Actions:** Replace Alice's key in the users file with `test-alice-new`, add a second key for her, then run `bin/responses-proxy reload-keys manual-test-state/proxy.local.yaml`. Test old and new keys. Next write a malformed users file containing a duplicate key, run reload again, and test the last valid key map. Queue a delayed 200 response. Start a create request with Bob's key, wait until the mock records receipt, remove Bob from the users file, reload, and let the in-flight request finish. Test Bob's key on a new request and query accounting by user.

**Expected:** A valid reload exits successfully and atomically activates new keys; removed keys stop authenticating and new keys work. A malformed reload exits nonzero and retains the prior valid map. New requests after Bob's removal fail authentication, while the already-authenticated request completes and remains attributed to Bob. The state directory is mode `0700` and the Unix reload socket is mode `0600`.

**Evidence:** Save CLI exit statuses, authentication status codes, redacted mock counts, accounting grouped by user, and numeric filesystem modes. Never include key values.

**Cleanup:** Stop processes and delete local files.

## Case 3: SQLite write failure is logged and discarded

**Setup:** Use a fresh database and start the mock and proxy with default `upstream_errors` logging captured privately. The implementation uses a five-second SQLite busy timeout. An external SQLite client is needed to hold a lock.

**Actions:** Acquire an external write lock using `BEGIN IMMEDIATE;`. Submit two create requests whose mock responses finish while the lock is held. Keep the lock held until both accounting attempts have failed; observe sanitized error-log presence without printing keys or bodies. Release the lock with `ROLLBACK;`, wait several seconds, query accounting, and submit a third request. Also run a malformed accounting query while a normal create succeeds.

**Expected:** All creates reach the upstream and return its responses despite accounting failures. No local accounting admission 503 is produced. Each failed insert is logged even at the default level, without credential/body content. The failed rows never reappear after lock release: there is no queue, replay, or background recovery. The third request records normally. Query failures do not gate later creates.

**Evidence:** Save client statuses, mock call counts, log-presence booleans, and accounting counts before/after releasing the lock. Do not include credentials or bodies.

**Cleanup:** Release the external transaction, stop processes, and remove local files.

## Case 4: Bounded shutdown without a write-drain worker

**Setup:** Start fresh mock and proxy processes with a short `timeouts.shutdown` and private state. Prepare one delayed response that finishes within the deadline and another that exceeds it. Use separate runs.

**Actions:** Start a create and signal the proxy while the upstream request is active. Record exit timing, client status, mock cancellation, and the resulting database aggregates after restarting. Repeat while a completed operation's insert is blocked by an external SQLite write lock; release the lock during cleanup.

**Expected:** A handler that finishes within the deadline can record its own usage. At deadline expiry the proxy closes active HTTP work, cancels service persistence, and exits without waiting indefinitely for SQLite close. A record may be absent when its insert cannot finish. Restart never recovers lost operations, updates old rows, or queries the upstream. The reload socket and database remain private.

**Evidence:** Save sanitized exit timing, statuses, counts, and permission modes. Do not claim guaranteed recording after the shutdown deadline.

**Cleanup:** Release locks, stop processes, and remove local files.
