# Token accounting and aggregate queries

## Case 1: Standard usage, zero usage, and one row per operation

**Setup:** Start fresh mock and proxy processes, reset the mock, and use a new state directory. Configure `max_retries: 5` and a short delay. Use `alice`'s test key.

**Actions:** Create a normal JSON response using the mock's default standard usage. Submit a compact request. Queue and submit a terminal JSON response without a `usage` member and a nonterminal JSON Response with standard usage. Queue a definitive HTTP 500 response without usage and another error response containing standard usage. Queue six 429 responses to exhaust retries. Retrieve and cancel one successful response, list its input items, and call the input-token endpoint. Query accounting grouped by model and user.

**Expected:** The JSON create and compact operations each have one row with the standard token totals and details. Cached, cache-write, and reasoning details are recorded in their own counters, not added a second time to total tokens. A terminal Response without `usage` records zero token counts. Nonterminal JSON usage is counted from that single response with reason `nonterminal_response`; it is never followed up. Definitive HTTP failures, including exhausted 429, record failed outcomes with zero usage unless their own response contains standard usage. A successful compaction object can omit lifecycle status and still contribute its own usage. Retrieval, cancellation, input-item listing, input-token counting, and retry attempts create no extra rows. The aggregate request count equals finished create/compact operations whose insert succeeded, not network attempts. There are no rows while those operations are still in flight.

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
  (started_at_us, username, model, kind, outcome, http_status,
   response_status, incomplete_reason, input_tokens, output_tokens, total_tokens,
   cached_tokens, cache_write_tokens, reasoning_tokens)
VALUES
  (CAST(strftime('%s', '2026-10-03T13:59:59Z') AS INTEGER) * 1000000, 'alice', 'before', 'create', 'completed', 200, 'completed', '', 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-10-03T14:00:00Z') AS INTEGER) * 1000000, 'alice', 'at-start', 'create', 'completed', 200, 'completed', '', 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-10-04T12:59:59Z') AS INTEGER) * 1000000, 'bob', 'inside', 'create', 'completed', 200, 'completed', '', 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-10-04T13:00:00Z') AS INTEGER) * 1000000, 'bob', 'at-end', 'create', 'completed', 200, 'completed', '', 4, 6, 10, 1, 2, 1);
```

Insert the fall-back rows as well, using the same counter values and distinct model labels:

```sql
INSERT INTO accounting_requests
  (started_at_us, username, model, kind, outcome, http_status,
   response_status, incomplete_reason, input_tokens, output_tokens, total_tokens,
   cached_tokens, cache_write_tokens, reasoning_tokens)
VALUES
  (CAST(strftime('%s', '2026-04-04T12:59:59Z') AS INTEGER) * 1000000, 'alice', 'before-fall', 'create', 'completed', 200, 'completed', '', 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-04-04T13:00:00Z') AS INTEGER) * 1000000, 'alice', 'at-start-fall', 'create', 'completed', 200, 'completed', '', 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-04-05T13:59:59Z') AS INTEGER) * 1000000, 'bob', 'inside-fall', 'create', 'completed', 200, 'completed', '', 4, 6, 10, 1, 2, 1),
  (CAST(strftime('%s', '2026-04-05T14:00:00Z') AS INTEGER) * 1000000, 'bob', 'at-end-fall', 'create', 'completed', 200, 'completed', '', 4, 6, 10, 1, 2, 1);
```

Restart the proxy against the same database.

For the Sydney spring-forward date, use the local day `2026-10-04`: its UTC interval is `[2026-10-03T14:00:00Z, 2026-10-04T13:00:00Z)`, a 23-hour day. For the fall-back date `2026-04-05`, use `[2026-04-04T13:00:00Z, 2026-04-05T14:00:00Z)`, a 25-hour day. Seed fixtures for both intervals.

**Actions:** Query each date with `period=YYYY-MM-DD&timezone=Australia/Sydney` and group by model. Query each interval with equivalent RFC3339 `start` and `end` offsets. Query `period=2026`, `period=2026-10`, and `period=2026-10-04` to check year/month/date forms. Query a UTC calendar day once without `timezone` and once with `timezone=UTC`. Also issue a range with a row exactly at `start` and another exactly at `end`.

**Expected:** Each Sydney date selects exactly its two interior boundary rows (request count 2, total tokens 20), includes its start, and excludes its end, with correct 23-hour and 25-hour local-day boundaries. `period=2026` selects all eight fixture rows; `period=2026-10` selects the four October rows. Equivalent period and range queries select the same rows. Omitted timezone matches explicit UTC. The half-open range includes its start and excludes its end.

**Evidence:** Save the fixture timestamps (not user keys), sanitized query results, and SQLite row count before/after queries. Query operations must not change the ledger.

**Cleanup:** Stop the proxy and remove the fixture database and configs.

## Case 4: Strict accounting filters

**Setup:** Start fresh mock and proxy processes and create two synthetic completed operations.

**Actions:** Query with malformed `period`, invalid IANA timezone, only `start`, only `end`, a timestamp without an offset, `period` combined with a range, unknown parameter names, repeated parameters, and invalid groupings. Query a nonexistent exact model/user filter with and without grouping.

**Expected:** Invalid filters return HTTP 400. A range requires both bounds; timed bounds require RFC3339 offsets. Exact filters match no additional rows. Empty ungrouped results contain zero totals; grouped results contain no groups. Querying accounting never changes stored records. Summaries contain only request and token counts, without `pending_count` or `incomplete_count`.

**Evidence:** Save sanitized query/status pairs and aggregate results.

**Cleanup:** Stop processes and remove local files.

## Case 5: Python client accepts simplified summaries

**Setup:** Start a fresh mock and proxy with a valid test user key. Create two synthetic Responses operations so both ungrouped and grouped queries have results.

**Actions:** Set `RESPONSES_PROXY_API_KEY` to the test key and run `scripts/query_accounting.py` without grouping, then with `--group-by model,user`. Query an exact model/user combination with no matches, both grouped and ungrouped. Inspect the corresponding HTTP JSON summaries.

**Expected:** The client accepts ungrouped totals and grouped summaries containing request and token counts. It prints `No matching records.` for empty grouped results and zero totals for empty ungrouped results. HTTP summaries and client output contain no pending or incomplete counters.

**Evidence:** Save sanitized script output and query JSON, confirming the grouped counts sum to the ungrouped counts.

**Cleanup:** Stop processes and remove local files.

## Case 6: Fresh SQLite schema and enum enforcement

**Setup:** Start and stop the proxy with a fresh state directory. Do not reuse an older accounting database; no migration or backward compatibility is provided.

**Actions:** Inspect `PRAGMA table_info(accounting_requests)`, `PRAGMA index_list(accounting_requests)`, and the table's CREATE statement in `sqlite_master`. With the proxy stopped, insert otherwise-valid synthetic records using each allowed `incomplete_reason`, then attempt an insert using `arbitrary_reason`. Also try an invalid `kind` and a negative token counter.

**Expected:** The schema keeps `http_status`, `response_status`, `kind`, and `outcome`. It has no saved response ID, pending state, or usage-known column. SQLite accepts only the reasons `''`, `transport_error`, `response_read_error`, `client_disconnected`, `stream_ended`, `nonterminal_response`, and `invalid_response`. CHECK constraints reject the arbitrary reason, invalid kind, and negative counter. No response-ID or polling-state index exists. Startup never migrates or repairs an older schema; no compatibility outcome is promised for it.

**Evidence:** Save sanitized schema/index descriptions, numeric row counts, and constraint failure categories. Use synthetic data only.

**Cleanup:** Remove the local database and configs.
