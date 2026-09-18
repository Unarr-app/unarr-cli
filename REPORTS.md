# Private technical reports

Run `unarr report` (alias `unarr reports`). Answer `yes` to prepare a local
report, review the displayed JSON, then choose `save`, `send`, or `cancel`.
Sending requires a separate `yes`. EOF and all other responses cancel.
There is no unattended upload flag. Preparation performs no network checks.

The report contains the agent UUID, bounded system metrics, service state,
five boolean settings and recognized technical log events. It never copies
raw logs, arbitrary error messages, task titles, paths, config text, IPs,
hostnames, account IDs, names, emails, URLs or credentials. Unknown text is
omitted and counted. This intentionally sacrifices detail where privacy cannot
be established. The stable agent UUID is pseudonymous and can be correlated
with an installation; do not describe reports as untraceable.

The exact reviewed JSON is sent over HTTPS to the dedicated diagnostic-report
endpoint without agent authentication, cookies, environment proxies, redirects
or mirror fallback. Collection and report errors do not initialize telemetry.
Private local files use exclusive creation and permissions 0600 on Unix,
or a protected current-user ACL on Windows. A failed
upload saves the reviewed report locally and exits with failure.

Upload is disabled by default in both the CLI build and backend. Enable the
CLI with `-X github.com/Unarr-app/unarr-cli/internal/diagnostics.privateDeliveryEnabled=true`
only after deploying and verifying the dedicated endpoint. This also prevents
capability requests from reaching an older proxy that might still log IPs.
The backend must be enabled only after checking that
all deployed proxies, CDN, application and delivery logs omit source IP and
personal data for this route. A network connection necessarily exposes a source
IP to the receiver; the payload schema alone cannot establish non-retention.
The server capability check occurs only after upload consent. No legacy support
or email fallback is used when private delivery is unavailable.

Regression checks:

```sh
go test -race ./internal/diagnostics ./internal/cmd
```

Test consent cancellation, malformed/oversized logs, secret-bearing log lines,
symlinks, missing sources, byte-identical preview/upload, endpoint disabled,
redirect refusal, no identity headers and private file creation. Network tests
use loopback HTTP servers and never deliver a real report.
