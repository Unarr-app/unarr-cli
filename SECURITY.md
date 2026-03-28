# Security Policy

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| latest  | :white_check_mark: |
| < latest | :x:               |

Only the latest release receives security updates.

## Reporting a Vulnerability

**Please do NOT report security vulnerabilities through public GitHub issues.**

Instead, report them via **GitHub Security Advisories**:

1. Go to [Security Advisories](https://github.com/torrentclaw/torrentclaw-cli/security/advisories)
2. Click **"Report a vulnerability"**
3. Fill in the details

Alternatively, email **security@torrentclaw.com** with:

- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

## Response Timeline

- **Acknowledgment**: within 48 hours
- **Initial assessment**: within 5 business days
- **Fix and disclosure**: coordinated with reporter, typically within 30 days

## Scope

The following are in scope:

- Command injection or arbitrary code execution
- Path traversal or file access outside intended directories
- Authentication bypass or credential exposure
- Denial of service in the daemon
- Dependency vulnerabilities with exploitable impact

The following are out of scope:

- Vulnerabilities in torrent protocol itself (BitTorrent DHT, peer exchange)
- Issues requiring physical access to the machine
- Social engineering attacks

## Security Practices

This project follows these security practices:

- **No hardcoded credentials** — API keys stored in config files with 0600 permissions
- **Path traversal protection** — All file operations validated through `safePath()`
- **HTTPS by default** — All API communication uses TLS
- **Response size limits** — API responses capped at 1MB
- **Non-root Docker** — Container runs as unprivileged user (UID 1000)
- **Dependency scanning** — Automated via Dependabot

## Disclosure Policy

We follow coordinated disclosure. We will credit reporters in the release notes unless they prefer to remain anonymous.
