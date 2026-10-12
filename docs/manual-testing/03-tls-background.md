# TLS, background responses, and client retrieval

## Case 1: HTTPS trust and hostname verification

**Setup:** Build `mock-upstream`, then generate a local CA and a localhost-only server certificate:

```sh
bin/mock-upstream --generate-test-pki manual-test-state/test-pki
bin/mock-upstream --listen 127.0.0.1:19090 --admin-listen 127.0.0.1:19091 --expected-bearer mock-upstream-key --tls-cert manual-test-state/test-pki/server.pem --tls-key manual-test-state/test-pki/server-key.pem
```

Start a proxy pointed at `https://localhost:19090/v1`, first without `upstream.ca_file`. Use a fresh state directory and test-only credentials.

**Actions:** Send a create request without the CA bundle. Stop the proxy, add `upstream.ca_file: ./test-pki/ca.pem` relative to `manual-test-state/proxy.local.yaml`, and restart. Send a create request to the same `localhost` URL. Stop it, configure the upstream URL as `https://127.0.0.1:19090/v1` while retaining the trusted CA, restart, and send a third request.

**Expected:** The default system trust rejects the test certificate chain. Trusting the generated CA allows the `localhost` request. The final request fails because the certificate does not contain an IP SAN for `127.0.0.1`, even though its chain is trusted. No failed handshake reaches the mock HTTP handler. The proxy must not disable certificate verification. The two TLS failures still produce downstream gateway errors and each accounting row is `request_outcome: failed`, `usage_state: complete`, with zero token usage, because no HTTP request was written upstream.

**Evidence:** Save client status/error category, sanitized mock counts, and proxy logs with credentials redacted. Confirm the successful case reaches the mock once.

**Cleanup:** Stop processes and remove the generated test CA, private keys, configs, and state. Never include the CA private key in evidence.

## Case 2: Background response stays pending across proxy restart until client retrieval

**Setup:** Start the HTTP mock. Set its completion threshold using `POST /__control/background` with `{"complete_after_polls":2}`. Start a proxy with a fresh persistent state directory and test keys for `alice` and `bob`. Do not configure a background polling section.

**Actions:** Submit a create request as `alice` with `background: true` and `model: gpt-6-luna`. Verify its response ID and query accounting immediately. Stop the proxy process after it has returned the queued response and while accounting still reports pending work. Keep the mock and database directory in place. Restart the proxy with the same config, wait several seconds without making a request, and query accounting and mock state. Retrieve the response once with a client GET as `alice`, then retrieve it again as `bob`.

**Expected:** The first accepted operation creates one pending accounting row. After restart the row remains pending and the mock has received no GET for its ID. The first client GET is still nonterminal; the second returns a terminal Response because `complete_after_polls` counts client GETs. That GET updates the same row with Alice's original username, model, and standard usage. Retrieval by Bob does not create a second row or change the original attribution. The mock reports one create call and only the two client GETs issued in this case.

**Evidence:** Save pre-restart, post-restart-before-GET, and post-terminal-GET accounting aggregates, the synthetic response ID, mock call counts, and sanitized process restart notes. Do not inspect or capture credentials.

**Cleanup:** Stop both processes and remove the test database/configuration.

## Case 3: A missing response and a create without a usable ID

**Setup:** Start a fresh mock and proxy. Prepare one scripted successful background-create response with a synthetic response ID that the mock has no stored state for, and another successful create response without a usable Response object or ID.

**Actions:** Queue the first create response and submit it with `background: true`. Query accounting, issue a client GET for its saved ID, and query accounting again. Restart the proxy with the same state directory, wait without issuing a request, and query accounting and mock state. Then queue and submit the response without a usable object or ID and query accounting again.

**Expected:** The row with a usable saved ID remains pending when its client GET returns 404 and remains pending after restart. The proxy issues no automatic GET, and the row is not marked incomplete by that 404. The create without a usable Response object or ID is incomplete under the existing behavior. An ordinary terminal HTTP error with no usage is a complete zero-usage outcome.

**Evidence:** Save synthetic IDs where available, client statuses, mock GET counts, and pending/incomplete accounting counts before and after restart.

**Cleanup:** Stop processes and remove local files.

## Case 4: Removed background polling configuration

**Setup:** Start with a config containing the former `background.poll_interval: 2s` setting and a fresh state directory.

**Actions:** Remove the top-level `background` section and start the proxy. Stop it, restore the former section, and try starting the proxy again.

**Expected:** Strict config decoding accepts the file after the old section is removed and rejects the file while the removed section remains. No polling settings are required for background response creation or client retrieval.

**Evidence:** Save sanitized startup status and error category for both config versions. Do not include credentials or private config contents.

**Cleanup:** Stop processes and remove local files.
