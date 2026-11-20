# Security policy

## Supported versions

Security fixes are applied to the latest release and to `main`. Older tags are not patched in place.

## Reporting a vulnerability

Responsible disclosure: report suspected vulnerabilities privately. Do not open a public GitHub issue for an unfixed security bug.

- GitHub private vulnerability reporting: https://github.com/MERKAT0R/go-RFLink/security/advisories/new
- Email: merkator@merkator.pro

Include the version (`go-rflink` health payload or `rflink.Version`), what an attacker can do, and how to reproduce it. We will acknowledge a report within 7 days and aim to ship a fix or a mitigation note within 30 days for issues that affect the default configuration.

## Scope

In scope: the serial command path, MQTT command bridge, HTTP API and Web GUI authentication, API tokens, runtime config overlay, and outbound webhooks (including SSRF against cloud metadata or loopback).

Out of scope: RFLink firmware itself, the MQTT broker, and a deployment that binds the HTTP API to a public interface without `HTTP_AUTH_TOKEN`.
