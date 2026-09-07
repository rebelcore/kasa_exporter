# Security Policy

The Rebel Media security policy, which takes precedence over anything here, is
published at <https://docs.rebelcore.org/security>.

## Supported versions

| Version | Supported |
|---------|-----------|
| 1.x     | Yes       |

Fixes land on the latest minor release. There are no long-term support branches.

## Reporting a vulnerability

Report privately. Do not open a public issue, and do not disclose the problem
publicly until a fix is available.

Use [GitHub private vulnerability reporting](https://github.com/rebelcore/kasa_exporter/security/advisories/new),
or email <security@rebelcore.org> if you would rather not use GitHub.

A useful report says what the problem is, how to reproduce it, and what an
attacker gains. Device model, firmware version and exporter version help, since
much of this code is driven by what a particular firmware sends.

## What happens next

1. We acknowledge the report within 5 business days.
2. We confirm the issue and tell you whether we agree on the severity, normally
   within 10 business days.
3. We prepare a fix and a release. Coordinated disclosure is 90 days from the
   acknowledgement, or sooner once a release is out.
4. We publish a GitHub Security Advisory and credit you, unless you would
   prefer otherwise.

If a report is declined we will say why. If you disagree, say so — the
disagreement is usually more informative than the original report.

## Scope

In scope: this exporter's own code, its build and release workflows, and its
handling of TP-Link credentials and device responses.

Out of scope: vulnerabilities in TP-Link firmware or the device protocols
themselves. Report those to TP-Link. Note that the legacy protocol on TCP 9999
is obfuscated rather than encrypted, by design of the vendor and not of this
project — anyone on the same network can read that traffic, which is a property
of the hardware this exporter has to interoperate with.
