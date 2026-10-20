# TLS, background responses, and client retrieval

## Case 1: HTTPS trust and hostname verification

**Setup:** Build `mock-upstream`, then generate a local CA and a localhost-only server certificate:

```sh
bin/mock-upstream --generate-test-pki manual-test-state/test-pki
bin/mock-upstream --listen 127.0.0.1:19090 --admin-listen 127.0.0.1:19091 --expected-bearer mock-upstream-key --tls-cert manual-test-state/test-pki/server.pem --tls-key manual-test-state/test-pki/server-key.pem
```

Start a proxy pointed at `https://localhost:19090/v1`, first without `upstream.ca_file`. Use a fresh state directory and test-only credentials.

**Actions:** Send a create request without the CA bundle. Stop the proxy, add `upstream.ca_file: ./test-pki/ca.pem` relative to `manual-test-state/proxy.local.yaml`, and restart. Send a create request to the same `localhost` URL. Stop it, configure the upstream URL as `https://127.0.0.1:19090/v1` while retaining the trusted CA, restart, and send a third request.

**Expected:** The default system trust rejects the test certificate chain. Trusting the generated CA allows the `localhost` request. The final request fails because the certificate does not contain an IP SAN for `127.0.0.1`, even though its chain is trusted. No failed handshake reaches the mock HTTP handler. The proxy must not disable certificate verification. The two TLS failures produce downstream gateway errors and zero-token records with `http_status: 0` and `incomplete_reason: transport_error`. No response ID or recovery state is stored.

**Evidence:** Save client status/error category, sanitized mock counts, and proxy logs with credentials redacted. Confirm the successful case reaches the mock once.

**Cleanup:** Stop processes and remove the generated test CA, private keys, configs, and state. Never include the CA private key in evidence.

## Case 2: Background responses forward without accounting recovery

**Setup:** Start the HTTP mock. Set its completion threshold using `POST /__control/background` with `{"complete_after_polls":2}`. Start a proxy with a fresh persistent state directory and test keys for `alice` and `bob`.

**Actions:** Submit a create request as `alice` with `background: true` and `model: gpt-6-luna`. Query accounting after the request ends. Restart the proxy with the same database, wait without making requests, and query accounting and mock state. Retrieve the response once as Alice and again as Bob.

**Expected:** The queued create records one row attributed to Alice with zero tokens, its upstream HTTP/Response statuses, and `incomplete_reason: nonterminal_response`. There is no saved response ID or pending state. The mock receives no automatic GET before or after restart. The two client GETs forward normally; the second returns terminal usage, but neither changes accounting. Attribution and the original zero-token totals remain unchanged.

**Evidence:** Save aggregate snapshots before/after restart and both GETs, sanitized mock counts, and synthetic status/reason fields from SQLite. Do not capture credentials.

**Cleanup:** Stop processes and remove local files.

## Case 3: Missing and malformed responses

**Setup:** Start a fresh mock and proxy. Prepare a successful queued create with a synthetic ID absent from mock state, plus a successful create returning an unrelated JSON object without usage.

**Actions:** Submit both creates. Retrieve the first ID, which returns 404. Query accounting before and after the GET, and again after restarting the proxy.

**Expected:** Both creates record zero tokens exactly once. The queued create has reason `nonterminal_response`; the unrelated JSON has reason `invalid_response`. The GET and restart do not update records or trigger follow-up requests. Ordinary HTTP errors without usage also record zero tokens and a failed outcome; no recovery state is created.

**Evidence:** Save client statuses, mock counts, aggregates, and the synthetic enum values. Do not save request text or credentials.

**Cleanup:** Stop processes and remove local files.

## Case 4: Removed background polling configuration

**Setup:** Start with a config containing the former `background.poll_interval: 2s` setting and a fresh state directory.

**Actions:** Remove the top-level `background` section and start the proxy. Stop it, restore the former section, and try starting the proxy again.

**Expected:** Strict decoding accepts the current file and rejects the removed configuration section. Background JSON fields continue to forward without any proxy polling or recovery settings.

**Evidence:** Save sanitized startup status and error category. Do not include private config contents.

**Cleanup:** Stop processes and remove local files.
