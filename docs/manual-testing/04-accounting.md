# Token accounting and aggregate queries

## Case 1: Standard usage, zero usage, and one row per operation

**Setup:** Start fresh mock and proxy processes, reset the mock, and use a new state directory. Configure `max_retries: 5` and a short delay. Use `alice`'s test key.

**Actions:** Create a normal JSON response using the mock's default standard usage. Submit a compact request. Queue and submit a terminal JSON response without a `usage` member. Queue a definitive HTTP 500 response without usage. Queue six 429 responses to exhaust retries. Retrieve and cancel one successful response, list its input items, and call the input-token endpoint. Query accounting grouped by model and user.

**Expected:** The JSON create and compact operations each have one row with the standard token totals and details. Cached, cache-write, and reasoning details are recorded in their own counters, not added a second time to total tokens. A well-formed terminal Response with no `usage` is complete with zero token counts. Definitive HTTP failures, including exhausted 429, are complete failed rows with zero usage unless the error response contains standard usage. Retrieval, cancellation, input-item listing, input-token counting, and retry attempts create no extra rows. The aggregate request count equals admitted create/compact operations, not network attempts.

**Evidence:** Save sanitized query JSON, client statuses, mock counts, and the synthetic row identities/user/model if available. Keep the request input synthetic.

**Cleanup:** Stop processes and delete the test database/configuration.

## Case 2: User/model grouping and filters

**Setup:** Start a fresh mock and proxy with Alice and Bob test keys and an empty database. Reset mock state.

**Actions:** Create three Responses operations: Alice with model `gpt-6-luna`, Bob with model `gpt-6-luna`, and Alice with a different synthetic model name. Query with no grouping; then with `group_by=model`, `group_by=user`, and `group_by=model,user`. Repeat with exact `model` and `user` filters separately and combined. Query using either valid user key.

**Expected:** Every valid user sees the same global data. Ungrouped totals include all matching users. Group dimensions partition the same total request/token counts, and adding exact-name filters narrows the result without changing attribution. Unknown filter values produce empty zero totals (ungrouped) or no groups (grouped).

**Evidence:** Save sanitized summaries and confirm the grouped request/token sums equal the ungrouped totals.

**Cleanup:** Stop processes and delete local files.

## Case 3: UTC periods, exclusive range end, and daylight-saving boundaries

**Setup:** Start and stop a fresh proxy once so it creates an empty database. With the service stopped, use the SQLite CLI (or equivalent) to insert completed synthetic rows in `accounting_requests`. Use this fixture for each date, changing timestamps and model labels as indicated:

```sql
INSERT INTO accounting_requests
  (started_at_us, username, model, kind, request_outcome, http_status,
   response_status, usage_state, usage_known, input_tokens, output_tokens, total_tokens,
   cached_tokens, cache_write_tokens, reasoning_tokens)
VALUES
  (CAST(strftime('%s', '2026-10-03T13:59:59Z') AS INTEGER) * 1000000, 'alice', 'before', 'create', 'complete', 200, 'completed', 'complete', 1, 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-10-03T14:00:00Z') AS INTEGER) * 1000000, 'alice', 'at-start', 'create', 'complete', 200, 'completed', 'complete', 1, 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-10-04T12:59:59Z') AS INTEGER) * 1000000, 'bob', 'inside', 'create', 'complete', 200, 'completed', 'complete', 1, 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-10-04T13:00:00Z') AS INTEGER) * 1000000, 'bob', 'at-end', 'create', 'complete', 200, 'completed', 'complete', 1, 4, 6, 10, 1, 2, 1);
```

Insert the fall-back rows as well, using the same counter values and distinct model labels:

```sql
INSERT INTO accounting_requests
  (started_at_us, username, model, kind, request_outcome, http_status,
   response_status, usage_state, usage_known, input_tokens, output_tokens, total_tokens,
   cached_tokens, cache_write_tokens, reasoning_tokens)
VALUES
  (CAST(strftime('%s', '2026-04-04T12:59:59Z') AS INTEGER) * 1000000, 'alice', 'before-fall', 'create', 'complete', 200, 'completed', 'complete', 1, 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-04-04T13:00:00Z') AS INTEGER) * 1000000, 'alice', 'at-start-fall', 'create', 'complete', 200, 'completed', 'complete', 1, 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-04-05T13:59:59Z') AS INTEGER) * 1000000, 'bob', 'inside-fall', 'create', 'complete', 200, 'completed', 'complete', 1, 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-04-05T14:00:00Z') AS INTEGER) * 1000000, 'bob', 'at-end-fall', 'create', 'complete', 200, 'completed', 'complete', 1, 4, 6, 10, 1, 2, 1);
```

Restart the proxy against the same database.

For the Sydney spring-forward date, use the local day `2026-10-04`: its UTC interval is `[2026-10-03T14:00:00Z, 2026-10-04T13:00:00Z)`, a 23-hour day. For the fall-back date `2026-04-05`, use `[2026-04-04T13:00:00Z, 2026-04-05T14:00:00Z)`, a 25-hour day. Seed fixtures for both intervals.

**Actions:** Query each date with `period=YYYY-MM-DD&timezone=Australia/Sydney` and group by model. Query each interval with equivalent RFC3339 `start` and `end` offsets. Query `period=2026`, `period=2026-10`, and `period=2026-10-04` to check year/month/date forms. Query a UTC calendar day once without `timezone` and once with `timezone=UTC`. Also issue a range with a row exactly at `start` and another exactly at `end`.

**Expected:** Each Sydney date selects exactly its two interior boundary rows (request count 2, total tokens 20), includes its start, and excludes its end, with correct 23-hour and 25-hour local-day boundaries. `period=2026` selects all eight fixture rows; `period=2026-10` selects the four October rows. Equivalent period and range queries select the same rows. Omitted timezone matches explicit UTC. The half-open range includes its start and excludes its end.

**Evidence:** Save the fixture timestamps (not user keys), sanitized query results, and SQLite row count before/after queries. Query operations must not change the ledger.

**Cleanup:** Stop the proxy and remove the fixture database and configs.

## Case 4: Invalid time filters and pending/incomplete counts

**Setup:** Start fresh mock and proxy processes. Create one background response with a saved synthetic ID that is absent from mock state, leaving it pending. Submit a second create whose successful response has no usable Response object or ID, leaving its row incomplete.

**Actions:** Query ungrouped and grouped totals. Issue a client GET for the pending response ID and query again. Query with malformed `period`, invalid IANA timezone, only `start`, only `end`, a timestamp without an offset, and `period` combined with a range.

**Expected:** The first row remains pending after a client GET returns 404; it does not become incomplete, and there is no automatic GET. The second row is incomplete because its create response provided no usable ID. A later client GET that observes a terminal Response can finalize an existing pending row. Invalid filters return HTTP 400. A range requires both bounds; timed bounds require RFC3339 offsets. Accounting queries do not create or update ledger rows.

**Evidence:** Save sanitized query bodies/statuses, client GET counts, and pending/incomplete totals.

**Cleanup:** Stop processes and remove local state/config.

## Case 5: Python accounting query client

**Setup:** Use a fresh local mock and proxy as in Case 2, create synthetic rows for Alice and Bob with at least two model names, and set `RESPONSES_PROXY_API_KEY` to one local user key. Do not use a live API key.

**Actions:** Run `python3 scripts/query_accounting.py` with no grouping, then with `--group-by user`, `--group-by model`, and `--group-by model,user`. Run with `--human` and `--raw`, then together to check that the format options are mutually exclusive. Try exact `--model` and `--user` filters separately and together, including an unknown value. Query the current UTC date with both `--today` and `-t`, and verify that combining either with `--period` is rejected. Query a calendar period and an equivalent date-only range with `--timezone Australia/Sydney`; include a trailing slash on `--base-url` once. Unset `RESPONSES_PROXY_API_KEY` and run once to check missing-key handling, then try an invalid grouping, an incomplete range, an unauthorized local key, a stopped proxy, and a local HTTP fixture that returns malformed JSON, a non-success status, and a redirect. Capture exit status and sanitized stderr for each failure.

**Expected:** The client prints readable nested bullet summaries with all returned aggregate fields, including zero-valued token details and pending/incomplete counts. Token counts use thousand separators by default, no separators with `--raw`, and spaced `k`/`M`/`B`/`T` suffixes with at most one decimal using `--human`; values at or above 1,000 T use full integers with thousand separators. Combining the format options exits nonzero. Grouped results match `/accounting`; `model,user` grouping has users outermost and models nested inside each user. `--today` and `-t` query the same records as `--period YYYY-MM-DD` for the UTC date captured during the run, and combining either with `--period` exits nonzero. The calendar-period query and equivalent date-only range select the same records. An unknown exact filter prints zero totals or no groups as appropriate. Queries do not create ledger rows. Each failure exits nonzero without prompting. Error output contains no credential or response body; command-line validation and missing-key errors do not print a traceback. Query parameters are URL encoded, including the comma in `model,user` and timezone slashes.

**Evidence:** Save sanitized output, statuses, and fixture query strings. Verify the client output contains no key values and that the ledger row count is unchanged by queries.

**Cleanup:** Stop processes, unset the test environment variable, and remove the test database/configuration and temporary HTTP fixture.

## Case 6: Fresh and legacy SQLite schemas without migration

**Setup:** Use one fresh state directory and a separate copy of a synthetic database created by an earlier version, with its legacy `background` column and `accounting_state_idx` index. Do not migrate or alter the legacy database before starting the current proxy.

**Actions:** Start and stop the proxy with the fresh directory. Inspect `PRAGMA table_info(accounting_requests)` and `PRAGMA index_list(accounting_requests)`. Start the proxy directly against the legacy database and inspect those PRAGMAs before and after startup. Admit a new create request and query accounting against each database.

**Expected:** A fresh database has no `background` column or `accounting_state_idx` index. The legacy database remains usable without migration and retains its `background` column and `accounting_state_idx` index; startup does not drop or rebuild either. New admissions omit the legacy column and use its existing `0` default. Both databases accept new rows and return accounting aggregates.

**Evidence:** Save sanitized PRAGMA output, client statuses, and aggregate counts before and after each admission. Do not include raw request rows or credentials.

**Cleanup:** Stop processes and remove both local databases and configs.
