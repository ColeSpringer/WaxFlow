# ADR-0002: Public API surface, module layout, and stability policy

Status: Accepted (2026-07-02)

## Context

The codecs are meant to be imported by anyone (WaxBin and WaxTap included),
which demands a stable, dependency-free public tree. But codec APIs will
churn heavily through v0.x, and multi-module repos tax every change with
tagging ceremony.

## Decision

- **Public packages**: root facade, `waxerr`, `audio`, `dsp/...`,
  `codec/...`, `container/...`, `format`, `source`, `server`, `client`.
  Everything else lives under `internal/`.
- **The public tree is stdlib-only.** `make depcheck` runs `go list -deps`
  over the public packages and fails CI on any non-stdlib import. Runtime
  third-party dependencies are confined to `cmd/` and `internal/`
  (`spf13/cobra`; later `colespringer/waxlabel`).
- **Single module through v1.0.** External importers of, say,
  `waxflow/codec/flac` inherit cobra in their module *graph* (never their
  binaries); that is acceptable during v0.x churn.
- **Structural extraction at v1.0**: when codec APIs freeze, the stdlib-only
  tree graduates to a nested module with an empty `require` block, so the
  guarantee becomes structural instead of script-enforced.
- **No compatibility promise before v1.0.0.** Tagged v0.x releases are
  usable snapshots, not contracts. At v1.0.0 every exported identifier in
  the public tree is semver-locked, preceded by a strict surface audit.
- Unfinished codecs compile and test but stay **unregistered** in `format`
  and `/caps`; the service never advertises what it cannot do.

## Consequences

- `depcheck`'s package list grows as public packages land; forgetting to add a
  new public package there is the main failure mode, so the Makefile derives
  the list from a single variable reviewed in PRs.
- Nested modules are how dependencies stay out of the main module graph;
  `resolver/` was the one planned before v1.0 (see the 2026-07-17
  amendment, which removed it).

## Amendment (2026-07-11): the extraction as implemented

The v1.0 structural extraction landed inverted from the original
wording, with the same guarantee: instead of the stdlib-only tree
moving into a nested module (which would have changed every published
import path), the tree stayed at the module root and everything
carrying dependencies moved out. The root module's `require` block is
now empty, so importing any public package pulls in nothing, by
construction rather than by CI script (`depcheck` stays as the fast
pre-push guard).

- `cli/` (nested module): the cobra command tree, the waxlabel-backed
  metadata mapper (`cli/label`), and the stock binary
  (`cli/cmd/waxflow`). Its packages exist to build the binary; they
  carry no independent stability promise, except for the extension
  surface carved out in the 2026-07-17 amendment below.
- `resolver/` (nested module, pre-existing): the WaxBin flavor,
  requiring `waxflow/cli` for the shared command tree. Removed
  2026-07-17; see the amendment below.
- `oracletest/` (nested module, test-only): the tests whose oracles are
  third-party modules (waxlabel metadata round trips, the go-mp3
  differential, the m4b chapter golden, the jobs e2e). The resolver's
  precedent (nested modules keep gated integration suites out of `go
  test ./...`) applied to test dependencies; `make test-oracle` and a
  dedicated CI step run it.
- The v1.0 surface audit also added `context.Context` to
  `source.Resolver.Resolve` (the recorded resolver limitation): catalog
  lookups now observe request cancellation, bounded by the resolver's
  own query timeout and still aborted by Close.

Internal packages (`internal/...`) remain in the root module and are
reachable by the nested modules through Go's path-prefix internal rule;
they add no dependencies (the root go.mod stays empty), and their
instability is contained by the replace-pinned, same-repo builds.

## Amendment (2026-07-17): `resolver/` removed, `cli.Flavor` promoted

`resolver/` is deleted. It was the only module here requiring WaxBin, so
every WaxBin pseudo-version bump moved this repo's HEAD for a release
containing no WaxFlow work, and its `replace waxflow => ../` had begun
redirecting *waxbin's own* waxflow dependency to the local tree once
WaxBin started requiring WaxFlow. The catalog lookup moves to WaxBin;
building a CLI that resolves `pid:` becomes a consumer's job.

That makes third-party integrators the intended consumers of `cli/`, so
the disclaimer above is carved back:

- **Supported extension surface**: `cli.Flavor`, `cli.ResolverOptions`,
  `cli.PIDSourceReporter`, `cli.ExecuteFlavor`, and `cli.Execute`. These
  are what a build outside this repo needs to add source schemes, and
  they are treated as public API.
- The rest of `cli/` keeps the "no independent stability promise"
  disclaimer.

`PIDSourceReporter` is there for the same reason the seam was narrowed.
`/caps` advertises `delivery.pid` from what the CLI knows, and what it
knew was `catalogDB != ""` -- exact while the only resolver was in-tree
and keyed on catalogDB, an inference about a stranger's build once it is
not. A resolver that implements it answers for itself; one that does not
keeps the inference. Optional, so it costs implementers nothing.

Freezing the pre-amendment signature would have been dishonest:
`Flavor.OpenResolver` named `internal/config.Config`, so no module
outside the `github.com/colespringer/waxflow/` import path could
implement it. `resolver/` only ever satisfied it by living inside the
prefix. It is narrowed to exported and stdlib types (`ResolverOptions`),
which is what makes a stability promise meaningful here.

This also retires the containment claim above. "Contained by the
replace-pinned, same-repo builds" held only while every implementer was
in this repo, and the deletion moves the implementer across that line.
What contains it now is `examples/catalogcli/`: a module whose path sits
outside the waxflow prefix on purpose, so Go's internal rule applies to
it exactly as to a consumer. Nothing else in the tree can fail that way
-- no test inside the prefix can, and `depcheck` does not look (it gates
third-party deps, filtering `waxflow/...` imports out by construction).
`make test-example` and a CI step run it.

Its reach is worth stating precisely, because it is narrower than "any
internal type in `cli/`". Go's internal rule is enforced against the
imports a package actually writes, so the canary fails only on internal
types a consumer is *forced to name*: the parameter and result types of
`OpenResolver`, which cannot be implemented without naming them, and any
field an implementer must set. An exported field of an internal type
that a consumer can leave zero slips through -- `server.Config.Meta`
(typed `internal/meta.Mapper`) is exactly that, and the canary
constructs a `server.Config` and compiles today. That gap is known and
accepted: promoting the metadata mapper is a much larger permanent
commitment, worth making only when an embedder asks. The canary guards
the seam it was built for, not the whole surface.
