# Sanitized validation report

Date: 2026-10-08

This report records local manual checks performed with synthetic users, requests, responses, and upstream keys, plus one authorized live smoke request. No credential or response text is included. The repository contains manual test specifications rather than a traditional automated unit-test suite.

## Results

- Build: both documented Go binaries built successfully from the final source as `bin/responses-proxy` and `bin/mock-upstream`.
- Service, routes, authentication, and upstream forwarding: the earlier local manual scenarios passed, including supported Responses routes, rejected unsupported routes, and Bearer forwarding with the mock confirming its synthetic upstream key without recording the value.
- Retry, cancellation, and concurrency: the earlier local scenarios passed with five configured retries producing six attempts, jitter in the configured range, final 429 forwarding, cancellation stopping retries and active work, and concurrent requests overlapping.
- Token accounting and aggregation: standard usage details, global user visibility, grouping, and filters passed earlier. A terminal response without usage produced one complete zero-token row. An accepted request retained its original accounting row through an SQLite write lock; new admissions returned 503 without another upstream call, and releasing the lock recovered the original row and reopened admission.
- Streaming: usage from an in-progress SSE event was retained when the terminal event omitted usage. A terminal data line at EOF without the required blank-line delimiter was forwarded but not treated as dispatched; recovery by response ID completed the same row with 3 input, 2 output, and 5 total tokens.
- Background persistence: a queued response created by Bob remained attributed to Bob after proxy restarts, completed with 12 input, 4 output, and 16 total tokens, and was retrievable by Alice without a duplicate accounting row.
- TLS and transport outcomes: an untrusted CA and a trusted certificate with a hostname mismatch each returned 502 and recorded a complete failed zero-token row before any HTTP request reached the mock. Trusting the generated CA for `localhost` succeeded with 12 input, 4 output, and 16 total tokens. A disconnect after the upstream received a request returned 502 and recorded an incomplete row.
- Shutdown: interrupting the proxy while an accepted request waited on a 1.5-second upstream response drained that request, returned 200, and persisted its complete 4 input, 3 output, and 7 total token usage.
- Date and timezone filters: Sydney spring-forward and fall-back periods each selected the expected two rows and matched their equivalent offset-bearing ranges. The 2026 year query selected 8 fixture rows and 80 tokens; October selected 4 rows and 40 tokens. Omitted timezone matched explicit UTC.
- Logging: at the default level, invalid-key authentication returned 401 with no log entry, 429 retries were logged, and HTTP 500 responses logged only status, method, sanitized path, and category. Response-body and query markers were absent. Debug logging recorded authentication failures to a config-relative file with mode 0600 and omitted the test key.
- Live smoke: the supplied internal upstream returned HTTP 200 and a completed `gpt-6-luna` response. Accounting stored one complete row with 10 input, 5 output, and 15 total tokens. The prompt and response text were excluded from this report.

## Cleanup and limits

All temporary local configs, databases, generated certificates, response files, mock servers, and proxy processes used for these checks were removed or stopped. The live credential config and response file under `/tmp` were removed.

The manual specifications are not an automated regression suite. If shutdown exceeds its configured deadline, accepted database writes that could not flush may be lost; durable pending rows are reconciled on restart, as described in the user documentation. Cost accounting was excluded from the final implementation scope; accounting records token usage only.
