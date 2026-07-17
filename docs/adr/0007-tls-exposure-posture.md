# ADR-0007: TLS and network exposure posture

Status: Accepted (2026-07-02)

## Context

WaxFlow's purpose is streaming personal libraries **outside** the LAN, so
WAN exposure is the normal deployment, not an edge case. Signed URLs
(ADR-0003) are bearer tokens: anyone who observes one can fetch audio until
it expires. Shipping without an explicit transport-security stance would
make plaintext exposure the accidental default.

## Decision

- **TLS posture is explicit, never assumed.** Two supported deployments:
  native TLS via `tlsCert`/`tlsKey` config (cheap in Go), or a documented
  TLS-terminating reverse proxy in front.
- **`waxflow doctor` warns** whenever `addr` is non-loopback and neither
  native TLS nor an acknowledged proxy posture is configured.
- **Fail closed on authentication**: if `addr` is non-loopback and no
  `apiKeys` are configured, the server **refuses to start** unless
  `allowUnauthenticated: true` is explicit. Keyless operation is an opt-in
  posture, never a default on `0.0.0.0`.
- The default `addr` is `127.0.0.1:4418`; only deployment layers (compose
  files) widen it to `0.0.0.0`.
- Container hardening rides in `compose.yaml`: `cap_drop: [ALL]`,
  `no-new-privileges`, read-only rootfs with tmpfs scratch, non-root UID
  10001, distroless static runtime with no OS layer.

## Consequences

- The skeleton daemon already defaults to loopback; the fail-closed rule
  activates when auth lands, and its absence before then is harmless
  because the skeleton serves only `/ping`.
- Documentation (README, compose comments) always shows the TLS-or-proxy
  choice next to any instruction that widens `addr`.
- `doctor`'s warning is advisory, not blocking: homelab users on trusted
  LANs stay in control, but they opt out knowingly.
