# Configuration

Settings come from, highest precedence first: command-line flags,
`WAXFLOW_*` environment variables, a JSON config file (`--config` or
`WAXFLOW_CONFIG`), and the defaults below. A key's environment variable is
`WAXFLOW_` plus the key in upper snake case (`logLevel` is
`WAXFLOW_LOG_LEVEL`).

`waxflow doctor` checks a configuration before you start a daemon on it.

## Keys

| Key | Env | Default | Purpose |
|---|---|---|---|
| `addr` | `WAXFLOW_ADDR` | `127.0.0.1:4418` | listen address |
| `roots` | `WAXFLOW_ROOTS` | none | named library folders (see [Library roots](#library-roots)) |
| `apiKeys` | `WAXFLOW_API_KEYS` | none | control-API keys, comma-separated in the env form (see [API keys](#api-keys)) |
| `allowUnauthenticated` | `WAXFLOW_ALLOW_UNAUTHENTICATED` | `false` | run keyless on a non-loopback `addr` anyway |
| `signingSecret` | `WAXFLOW_SIGNING_SECRET` | generated into `dataDir` (0600) | HMAC key for signed URLs: a literal secret, or a `kid:hex,kid2:hex` rotation list |
| `tlsCert`, `tlsKey` | `WAXFLOW_TLS_CERT`, `WAXFLOW_TLS_KEY` | none | serve TLS natively; without them, put a TLS-terminating proxy in front ([ADR-0007](adr/0007-tls-exposure-posture.md)) |
| `allowedOrigins` | `WAXFLOW_ALLOWED_ORIGINS` | none | CORS allowlist for the playback endpoints |
| `metricsKey` | `WAXFLOW_METRICS_KEY` | none | a key that unlocks `GET /metrics` and nothing else |
| `dataDir` | `WAXFLOW_DATA_DIR` | platform config dir + `/waxflow` | daemon state: signing secret, job store |
| `cacheDir` | `WAXFLOW_CACHE_DIR` | platform cache dir + `/waxflow` | transcode cache |
| `cacheMaxBytes` | `WAXFLOW_CACHE_MAX_BYTES` | 10 GiB | cache size, evicted least recently used first |
| `cacheMaxAge` | `WAXFLOW_CACHE_MAX_AGE` | off | also evict entries not read for this long (Go duration) |
| `scratchDir` | `WAXFLOW_SCRATCH_DIR` | temp dir + `/waxflow` | upload spool (the container mounts a tmpfs there) |
| `scratchMaxBytes` | `WAXFLOW_SCRATCH_MAX_BYTES` | 8 GiB | total size of the spool |
| `uploadMaxBytes` | `WAXFLOW_UPLOAD_MAX_BYTES` | 2 GiB | size of one upload |
| `uploadTTL` | `WAXFLOW_UPLOAD_TTL` | `1h` | evict an upload this long after it was made (Go duration, `0` never) |
| `sourceMaxBytes` | `WAXFLOW_SOURCE_MAX_BYTES` | 4 GiB | largest source file the daemon opens |
| `liveSlots` | `WAXFLOW_LIVE_SLOTS` | CPUs minus 1 | concurrent live transcodes; past it, requests get `503` with `Retry-After: 2` |
| `jobSlots` | `WAXFLOW_JOB_SLOTS` | 2 | concurrent job workers; jobs queue, and pause while the live pool is full |
| `defaultGain` | `WAXFLOW_DEFAULT_GAIN` | `track` | gain when a request has no `gain=`: `off`, `track`, `album`, or a dB number |
| `resampleProfile` | `WAXFLOW_RESAMPLE_PROFILE` | `hq` | `hq`, or `fast` for constrained hosts |
| `paceBurstSeconds` | `WAXFLOW_PACE_BURST_SECONDS` | 30 | seconds of audio a live stream sends at full speed before pacing starts |
| `paceFactor` | `WAXFLOW_PACE_FACTOR` | 2.0 | after the burst, cap a live stream at this multiple of realtime (`0` turns pacing off) |
| `liveIdleGrace` | `WAXFLOW_LIVE_IDLE_GRACE` | `5s` | stop a `/stream` encode once every client reading it has been gone this long (`0` never; [ADR-0012](adr/0012-live-pipelines-end-with-their-audience.md)) |
| `hlsIdleGrace` | `WAXFLOW_HLS_IDLE_GRACE` | `60s` | stop an HLS variant worker nobody has fetched from for this long (`0` never) |
| `pidLookupURL` | `WAXFLOW_PID_LOOKUP_URL` | none | host that resolves `pid:<id>` references (see [`pid:` sources](#pid-sources)) |
| `pidLookupKey` | `WAXFLOW_PID_LOOKUP_KEY` | none | bearer token sent with each pid lookup |
| `catalogDB` | `WAXFLOW_CATALOG_DB` | none | catalog path for a build with a catalog resolver (see [`pid:` sources](#pid-sources)) |
| `logLevel` | `WAXFLOW_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `debugAddr` | `WAXFLOW_DEBUG_ADDR` | off | pprof listener; must be loopback |
| `demo` | `WAXFLOW_DEMO` | `false` | serve the browser test page at `/demo` (development only) |

## The config file

- JSON, UTF-8. A file that is not UTF-8, or that escapes half a surrogate
  pair, is refused rather than read with those bytes replaced. Unknown keys
  are refused.
- A path that is not UTF-8 (a root's, `dataDir`, `cacheDir`, `scratchDir`,
  `tlsCert`, `tlsKey`, `catalogDB`) is written in the text form a source
  reference takes ([api.md](api.md#source-references)), in the file or in
  its environment variable: `pct:/mnt/caf%E9`.
- Every command that loads the configuration refuses a malformed value:
  `server`, `doctor`, `sign`, `transcode`, `split`, `ping`, `cache`, and a
  `probe` of a `pid:` or `upload:` reference (a probe of a plain path loads
  none).
- Two rules belong to starting a daemon, so `server` and `doctor` apply them
  and a local `transcode` does not: no keys on a non-loopback `addr`, and a
  `debugAddr` that is not loopback.

## API keys

Requests present a key as `X-API-Key: <key>` or `Authorization: Bearer
<key>`. A key no request could present (an empty one, or one with white
space at either end) is refused at load.

The daemon fails closed: on a non-loopback `addr` with no `apiKeys`, it
refuses to start unless `allowUnauthenticated` is `true`.

## Library roots

A root is a named folder. In the file:

```json
{"roots": [{"name": "lib", "path": "/music"}, {"name": "podcasts", "path": "/srv/podcasts"}]}
```

In the environment: `WAXFLOW_ROOTS=lib=/music,podcasts=/srv/podcasts`. A
source reference is the root's name plus a path under it:
`lib/Artist/Album/01.flac`. Each root is opened with `os.Root`, so a
reference cannot escape it and symlinks stay inside it; a source must be a
regular file no larger than `sourceMaxBytes`.

### Adding roots at runtime

A daemon whose roots come from the JSON file (not `WAXFLOW_ROOTS`) serves
`POST /roots/reload`: it re-reads the file and opens, closes or re-points
roots to match, so a new root streams without a restart. Only `roots` and
`sourceMaxBytes` reload; every other setting needs a restart. Write the
file atomically (a temp file, then a rename) so a reload never reads half
of it. Where the endpoint cannot do anything (no config file, or roots
pinned by `WAXFLOW_ROOTS`) it is not served and `/caps` reports
`delivery.rootsReload: false`. The full contract is in
[api.md](api.md#post-rootsreload).

### Unmounted roots

A root whose folder does not open, or is empty (an unplugged drive, a mount
that has not arrived or failed), does not stop the daemon or fail a reload.
The other roots serve; requests on that one answer `503 root-unavailable`
with `Retry-After: 2`, and jobs on it wait instead of failing. It mounts on
the first request after its folder holds something, or within 10 s by the
daemon's sweep, which also notices a root whose folder goes away later. A
folder that stops answering before it mounts (a dead network mount) holds
no request, reload or sweep for more than 5 s; one that hangs while mounted
holds the requests reading it, as any blocked read does.

The log names each change once (Warn for a missing or empty folder, Error
for anything else, which needs an operator). `GET /roots` lists every root
with its state, a reload's `unmounted` field names the roots it left down,
and `waxflow doctor` fails each unmounted root by name (an empty one is a
warning).

## `pid:` sources

A `pid:<id>` reference names an item by the id its host hands out rather
than by path. Set `pidLookupURL` (and `pidLookupKey` if the host wants a
token) and the daemon asks that host for the item's path; the contract the
host implements is in [api.md](api.md#pid-lookup-host). Without it, `pid:`
answers `501 unsupported-source`. `pidLookupURL` is read at start; changing
it needs a restart.

`catalogDB` is for a build that resolves pids some other way, such as
against a catalog directly, through the `cli.Flavor` seam (`cli/root.go`);
`examples/catalogcli/` is a working CLI built that way. It is exclusive with
`pidLookupURL`. The stock `waxflow` binary does not serve it, and refuses
it wherever it opens a resolver: `server`, `sign`, `doctor`'s pid check,
and any one-shot command given a `pid:` or `upload:` reference. A one-shot
on a plain path opens no resolver, so it neither honors nor refuses it. A Flavor that is present
but unconfigured hands `pid:` to the stock lookup and adds its own key to
the refusal.
