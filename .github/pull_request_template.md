## What

<!-- one paragraph: what this PR delivers -->

## Checklist

- [ ] `make check` passes locally (gofmt, vet, functional + `test-race`, depcheck)
- [ ] **Clean-room affirmation**: I did not open any Tier B (LGPL/GPL)
      source (LAME, Shine or its ports, ffmpeg, faad) while implementing
      code in this PR (ADR-0001)
- [ ] Any Tier A ports are attributed in `THIRD-PARTY-NOTICES.md` and the
      MAINTENANCE.md reference ledger
- [ ] Sample-affecting changes bump the node's `Version()` (ADR-0004)
- [ ] New parsers ship with fuzz targets and obey the hostile-input
      invariants (ADR-0005)
- [ ] Unfinished codecs remain unregistered in `format`/`/caps`
- [ ] ADRs updated (superseded, not edited) if an invariant changed
