# Automated testing

The service is tested with Go's standard `testing` and `net/http/httptest` packages. Tests use local synthetic upstreams and fresh on-disk SQLite databases. They require no upstream account, live credentials, Python interpreter, Git commands, or SQLite CLI.

## Running the suite

Use Go 1.24 or newer on a Unix system with Unix domain sockets and signals. Install a C compiler and enable CGO for the race detector. An initial run may download the dependencies already listed in `go.mod`; scenario traffic stays on loopback listeners.

```sh
go test ./...
go test -race ./...
```

Both commands include package tests and service subprocess tests. Database contention cases deliberately exercise SQLite's configured busy timeout and take several seconds. Test subprocesses reuse the test executable; they do not invoke the build script or create a separate service executable.

To select a scenario, use its test name, for example:

```sh
go test ./internal/proxy -run TestSSEFramingUsageAndTerminalMetadata -v
```

A passing suite verifies the assertions described below. It does not guarantee behavior beyond those assertions. Build/version metadata and `scripts/query_accounting.py` are excluded from coverage by agreement. No live upstream checks are part of this suite.

## Writing understandable tests

Name tests after the behavior and expected result. Keep setup, the action, and assertions in reading order. Use table-driven cases for independent inputs and focused tests for reloads, cancellation, streaming, concurrency, and shutdown.

Keep expected values literal and independent of production parsing or aggregation. Assert exact statuses, attempt counts, forwarded bytes, token counters, and stored diagnostic fields. A fixture may create servers and temporary storage, but should not hide the scenario's response or its meaningful assertions.

Use channels to establish when an upstream request has arrived and when it may finish. Bounded polling is appropriate for externally observable completion, such as subprocess readiness. Use generous deadlines to detect hangs; avoid narrow elapsed-time thresholds or sleeps as evidence that an event occurred. Test jitter bounds separately from network scheduling.

Use `t.TempDir()` and `t.Cleanup()` for isolated files, databases, servers, cancellation, and subprocess cleanup. Release blocked handlers and SQLite locks on failure as well as success. Assert sanitized log fields and absence of synthetic secret markers without dumping credential-bearing output.

Tests may query their temporary database directly to verify one-shot inserts, attribution, schema constraints, and diagnostic fields that the public aggregate endpoint intentionally omits. Do not add a public inspection endpoint or substitute an in-memory database for the production on-disk storage contract.

## Coverage of former manual scenarios

The following mapping replaces the former manual specifications. Each entry names executable checks; the package columns identify where to find them. Stateful manual scenarios are split into independent assertions where that makes failures easier to understand.

| Former case | Package | Automated checks |
| --- | --- | --- |
| 01/1: Routes and forwarding | `internal/proxy` | `TestProxyForwardingAuthenticationAndLocalRejection`, `TestUtilityAndRetrievalRoutesForwardWithoutAccounting` |
| 01/2: Keys, identity, global access | `internal/auth`, `internal/proxy` | `TestAuthenticateAndReplace`, `TestAccountingHTTPGlobalGroupsAndExactFilters` |
| 01/3: Command arguments | `cmd/responses-proxy` | `TestCommandArgumentValidation`; build metadata and Git-state assertions excluded |
| 01/4: Archive and direct build metadata | - | Excluded by agreement |
| 02/1: Retry count, jitter, exhaustion | `internal/proxy` | `TestRetryPolicyReplaysRequestsAndRecordsOnce`, `TestRetryExhaustionOtherStatusesAndRedirect`, `TestRetrievalRetries429WithoutAccounting` |
| 02/2: Other statuses and redirects | `internal/proxy` | `TestRetryExhaustionOtherStatusesAndRedirect` |
| 02/3: Client cancellation | `internal/proxy` | `TestCancelDuringRetryWaitAndActiveUpstream` |
| 02/4: Concurrent requests | `internal/proxy` | `TestConcurrentCreatesOverlapAndEachRecordOnce` |
| 02/5: Streaming delivery and terminal usage | `internal/proxy` | `TestSSEFlushesBeforeTerminalEvent`, `TestSSEFramingUsageAndTerminalMetadata` |
| 02/6: Unterminated terminal frame | `internal/proxy` | `TestSSEFramingUsageAndTerminalMetadata`, `TestBackgroundRetrievalDoesNotRecoverAccounting` |
| 02/7: SSE delimiters and outcomes | `internal/proxy` | `TestSSEFramingUsageAndTerminalMetadata`, `TestSSEHeartbeatFlushesPromptly` |
| 03/1: CA trust and hostname checks | `internal/proxy` | `TestTLSVerificationAndCustomCA` |
| 03/2: Background/retrieval without recovery | `internal/proxy`, `cmd/responses-proxy` | `TestBackgroundRetrievalDoesNotRecoverAccounting`, `TestServeDeadlineCancelsActiveUpstreamAndDoesNotReplayOrRetrieve` |
| 03/3: Missing and malformed responses | `internal/proxy` | `TestJSONCreateCompactAndInvalidResponseOutcomes`, `TestBackgroundRetrievalDoesNotRecoverAccounting` |
| 03/4: Removed polling configuration | `internal/config` | `TestLoadRejectsUnknownRemovedAndMultipleDocumentFields` |
| 04/1: Standard usage, zero usage, one insert | `internal/accounting`, `internal/proxy` | `TestParseUsageObjectStandardAndMalformedCounters`, `TestJSONCreateCompactAndInvalidResponseOutcomes`, retry and utility-route tests above |
| 04/2: Grouping and exact filters | `internal/accounting`, `internal/proxy` | `TestQueryGroupingFiltersEmptyResultsAndExactLargeTotals`, `TestAccountingHTTPGlobalGroupsAndExactFilters` |
| 04/3: Calendar bounds and daylight saving | `internal/accounting` | `TestParseFiltersSydneyDSTAndHalfOpenBoundaries`, `TestParseFiltersCalendarPeriodsAndUTCDefault` |
| 04/4: Strict filters and summary fields | `internal/accounting`, `internal/proxy` | `TestParseFiltersStrictContract`, `TestAccountingHTTPRejectsInvalidQueriesAndMethods`, `TestAccountingHTTPGlobalGroupsAndExactFilters` |
| 04/5: Python query client | - | Excluded by agreement |
| 04/6: Schema and SQLite constraints | `internal/accounting` | `TestOpenUsesPrivateOnDiskDatabaseAndSchema`, `TestIncompleteReasonAndCounterConstraints` |
| 05/1: Filtering and sanitized logs | `internal/logging`, `internal/proxy`, `cmd/responses-proxy` | `TestUpstreamErrorsFilteringAndPrivateAppendFile`, `TestStdoutDestination`, `TestTransportErrorLogDoesNotExposeURLQuery`, `TestHTTPErrorAndRetryLogsContainOnlySanitizedMetadata`, service reload test |
| 05/2: Atomic reload and in-flight identity | `internal/admin`, `internal/auth`, `cmd/responses-proxy` | `TestReloadActivatesOnlyValidUsersAndUsesPrivateSocket`, `TestAuthenticateAndReplace`, `TestServeReloadPinsInFlightUsernameAndDrainsOnSignal` |
| 05/3: Discard failed storage writes | `internal/proxy` | `TestStorageFailureDoesNotBlockForwardingAndLogsAtEveryLevel`, `TestSQLiteBusyWritesAreDiscardedAndLaterCreatesStillRecord`, `TestUnavailableAccountingQueryDoesNotGateUpstreamRequests` |
| 05/4: Bounded shutdown and restart | `cmd/responses-proxy` | `TestServeReloadPinsInFlightUsernameAndDrainsOnSignal`, `TestServeDeadlineDropsLockedInsertAcrossRestart`, `TestServeDeadlineCancelsActiveUpstreamAndDoesNotReplayOrRetrieve` |

Additional configuration checks cover defaults, explicit zero durations, invalid settings, config-relative paths, example YAML, and whole-file users validation. `TestRecordValidatesInputsAndStoresUTCAttribution` checks recorded UTC instants, diagnostic metadata, model fallback, and compact attribution. Startup tests reject invalid configuration and users before service startup. `TestConcurrentAuthenticateDuringReplace` exercises atomic snapshots under concurrent reads and writes. `TestUpstreamTruncatedBodiesPreserveHTTPStatusAndRecordReadError` checks JSON and SSE transport read failures with actual truncated HTTP bodies.

When changing a contract, update its tests and this mapping together. A regression test should fail for a concrete violation of the contract, rather than mirror the implementation's intermediate calculations.

## Optional standalone mock

`cmd/mock-upstream` remains available for interactive development. Automated proxy tests use small local fixtures instead, so maintainers do not need to start this command before running the suite.

```sh
go run ./cmd/mock-upstream --listen 127.0.0.1:19090 --admin-listen 127.0.0.1:19091 --expected-bearer synthetic-upstream-key
```

The upstream listener serves synthetic Responses routes. The separate loopback control listener supports `POST /__control/queue`, `GET /__control/state`, `POST /__control/reset`, and `POST /__control/background`. A queue request specifies `method`, `path`, and `responses`; a response can contain `status`, `headers`, `body`, `delay_ms`, or SSE `events` with `data` and optional `delay_ms`. The background control accepts `complete_after_polls`.

Request evidence redacts Authorization and can report whether it matched the configured synthetic key. The control state includes request bodies, so use synthetic data only. The mock can generate a temporary localhost CA and certificate with `--generate-test-pki DIRECTORY`, and serve TLS with `--tls-cert` and `--tls-key`. Stop the process and remove any temporary state or certificates after use.
