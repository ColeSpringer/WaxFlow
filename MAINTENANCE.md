# MAINTENANCE

Operational procedures that gate releases. The policy behind them is
ADR-0001 (clean-room) and docs/quality-gates.md.

## Clean-room procedure

Reference tiers are defined in [ADR-0001](docs/adr/0001-clean-room-policy.md).
Operationally:

1. **Tier A work** (specs, papers, BSD/MIT/Apache/PD sources): study and
   port freely. Record every ported source in `THIRD-PARTY-NOTICES.md` in
   the same PR.
2. **Tier B work** (LGPL/GPL: LAME, Shine and its Go ports, ffmpeg, faad):
   never open while implementing the corresponding component. Behavioral
   analysis happens in separate, dedicated passes whose only outputs are
   black-box artifacts (behavioral notes and parameter tables under
   `docs/notes/`, test vectors under `testdata/`) which implementation
   sessions then consume. No line-by-line porting, ever.
3. Tier B *binaries* are permitted as test oracles (differential CI jobs).
4. Every PR affirms the checklist in `.github/pull_request_template.md`.

### Reference ledger

| Component | Tier A references used | Tier B behavioral artifacts |
|---|---|---|
| (none yet) | | |

## AAC patent-status review

**Open. Must be completed before the AAC-LC encoder is enabled in release
builds.** Record here a good-faith review of the base MPEG-2/4 AAC-LC
patent status at time of shipping (the base patents are widely believed
expired; verify then, not now).

## Listening-test protocol

**Open. To be written when the nightly encoder-quality harness stands
up.** Will define the ABX procedure behind the clips the nightly report
publishes.

## Release checklist (grows over time)

- [ ] `make check` green (fmt, vet, race tests, depcheck)
- [ ] `THIRD-PARTY-NOTICES.md` audited against the reference ledger
- [ ] Tag `vX.Y.Z` pushed -> `release.yml` publishes binaries + SHA256SUMS +
      multi-arch image to ghcr.io
- [ ] Container smoke: `docker run` + HEALTHCHECK healthy
