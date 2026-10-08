# TLS, background responses, and restart recovery

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

## Case 2: Background completion and persistence across proxy restart

**Setup:** Start the HTTP mock. Set its completion threshold using `POST /__control/background` with `{"complete_after_polls":4}`. Start a proxy with `background.poll_interval: 2s`, a fresh persistent state directory, and `alice`'s test key.

**Actions:** Submit a create request with `background: true` and `model: gpt-6-luna`. Verify its response ID and query accounting immediately. Stop the proxy process abruptly after it has returned the queued response and while accounting still reports pending work. Keep the mock and database directory in place. Restart the proxy with the same config and wait until the accounting result becomes terminal. Retrieve the response as either `alice` or `bob` if both are configured.

**Expected:** The first accepted operation creates one pending accounting row. After restart the poller resumes from the saved response ID, obtains the completed Response, and updates that same row with the original username/model and standard usage. Retrieval by another valid user does not create a second row or change the original attribution. The mock reports poll calls both before/after restart and only one create call.

**Evidence:** Save pre-restart and post-restart accounting aggregates, the synthetic response ID, mock call counts, and sanitized process restart notes. Do not inspect or capture credentials.

**Cleanup:** Stop both processes and remove the test database/configuration.

## Case 3: Unrecoverable background response

**Setup:** Start a fresh mock and proxy with a short poll interval. Prepare a scripted successful background-create response containing a synthetic response ID that the mock has no stored state for.

**Actions:** Queue the create response, submit the background request, and wait through several poll attempts. Query accounting and inspect the mock's retrieval statuses.

**Expected:** The response starts pending, then becomes incomplete when the final response cannot be recovered. The record retains the original username/model and is not duplicated by polling. An ordinary terminal HTTP error with no usage is a complete zero-usage outcome; a missing final response is the incomplete case.

**Evidence:** Save the synthetic ID, poll call count/statuses, and final pending/incomplete accounting counts.

**Cleanup:** Stop processes and remove local files.
