// Browser client-matrix check: start a real WaxFlow daemon over a
// generated library, open the committed /demo page in headless Chromium,
// and drive every playback cell the browser column of
// docs/client-matrix.md claims: HLS variants through hls.js, progressive
// streams (live transcodes plus direct play) through <audio>, and
// multi-source timelines seeked across a track boundary. Each cell must
// actually progress past 2 s of playback with a healthy player.
//
// This run is the "automated" basis behind the hls-js profile in GET
// /caps; if a cell here changes, the profile table in server/types.go
// and docs/client-matrix.md must follow.
//
// Gated tooling (not part of `make test`): needs Node 18+ and Playwright
// with Chromium installed:
//
//   npm install playwright && npx playwright install chromium
//   make client-e2e
//
// Environment: WAXFLOW_BIN overrides the daemon command (the Makefile
// target passes ./bin/waxflow; the `go run -C cli` default is a convenience
// whose wrapper process does not reliably forward SIGTERM to the
// daemon, so prefer a built binary); CLIENT_E2E_CELLS narrows the run
// to a comma-separated list of cell names, surface:format unless the
// cell carries its own name, like "hls:opus,progressive:mp3,hls:flac20".

import { mkdtempSync, writeFileSync, rmSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn } from "node:child_process";
import { chromium } from "playwright";

const API_KEY = "client-e2e-key";
const PORT = 20000 + Math.floor(Math.random() * 20000);

// The browser cells. ALAC is deliberately absent: Chromium ships no ALAC
// decoder (Apple clients cover it; see docs/client-matrix.md).
const CELLS = [
  { surface: "hls", format: "opus" },
  { surface: "hls", format: "aac" },
  { surface: "hls", format: "flac" },
  // The same rung from a 24-bit source. FLAC keeps a lossless source's
  // depth, and the init header's sample entry has to say so: Chromium's
  // MP4 parser refuses an fLaC entry whose samplesize disagrees with
  // STREAMINFO at the first append, so a 16-bit-only cell would pass a
  // header that lies about every hi-res library.
  { surface: "hls", format: "flac", src: "test24.wav", name: "flac24" },
  // And from a 20-bit source, which Chromium's MP4 parser cannot play as
  // coded: it takes only 8, 16, 24, and 32 as a sample size, and the FLAC
  // mapping requires the field to equal STREAMINFO's, so there is no header
  // that works. The mint widens the source to 24 bits instead (lossless: a
  // left shift of zero-padded LSBs), which is what this cell proves reached
  // a real player. It fails on a daemon built before that rule.
  { surface: "hls", format: "flac", src: "test20.wav", name: "flac20" },
  { surface: "progressive", format: "opus" },
  { surface: "progressive", format: "mp3" },
  { surface: "progressive", format: "aac" },
  { surface: "progressive", format: "flac" },
  { surface: "progressive", format: "wav" },
  { surface: "progressive", format: "auto" }, // direct play, original bytes
  // A three-file queue delivered as one continuous stream, seeked across a
  // track boundary. The engine tests prove the samples; only a real player
  // proves that what it receives is one stream it can seek inside.
  { surface: "timeline", format: "opus" },
  { surface: "timeline", format: "flac" },
  // A virtual track: one offset range of one file, served as its own HLS
  // presentation. The engine tests prove the samples are the right ones
  // (TestSpanDeliversItsOwnSamples) and that a resampled span's first
  // samples are primed rather than a transient
  // (TestSpanPrerollMatchesContinuous). What only a real player proves is
  // that the presentation it receives describes the span and not the file:
  // opus resamples 44.1k to 48k, which is the case a CUE rip actually hits.
  { surface: "span", format: "opus" },
];

// The span cell's window over span.wav, in source samples. Both ends are
// CD frame boundaries (588 samples each at 44100) and neither is a round
// number of seconds, which is the case a seconds-based span gets wrong and
// this surface exists not to be. 137 and 438 frames: 1.827 s to 5.840 s.
const SPAN_RATE = 44100;
const SPAN_FROM = 137 * 588; // 80556
const SPAN_TO = 438 * 588; // 257544, inside the 6 s fixture
const SPAN_SECONDS = (SPAN_TO - SPAN_FROM) / SPAN_RATE; // 4.013...

// The timeline fixture: three tracks whose boundaries are not on segment
// boundaries, so a seam that survived the sample math still has somewhere
// to show up. TIMELINE_SEAM is the first boundary, inside track 2.
const TIMELINE_TRACKS = [5, 4, 5]; // seconds
const TIMELINE_SEAM = TIMELINE_TRACKS[0];

const only = process.env.CLIENT_E2E_CELLS
  ? new Set(process.env.CLIENT_E2E_CELLS.split(","))
  : null;
const cellName = (c) => `${c.surface}:${c.name || c.format}`;
const cells = CELLS.filter((c) => !only || only.has(cellName(c)));

// A 6 s 48 kHz stereo sine WAV at 16, 20, or 24 bits, written directly: no
// external tools needed to make a fixture.
//
// 20 bits is the odd one and is the shape a real 20-bit rip has: the samples
// sit left-justified in 24-bit words, wBitsPerSample says 20, and the reader
// reports a 20-bit track (24-bit container, 20 valid bits).
function makeWAV(seconds = 6, rate = 48000, channels = 2, bits = 16) {
  if (bits !== 16 && bits !== 20 && bits !== 24) throw new Error(`makeWAV: ${bits}-bit fixtures are not written here`);
  const frames = seconds * rate;
  const width = bits === 20 ? 3 : bits / 8;
  const dataLen = frames * channels * width;
  const buf = Buffer.alloc(44 + dataLen);
  buf.write("RIFF", 0);
  buf.writeUInt32LE(36 + dataLen, 4);
  buf.write("WAVEfmt ", 8);
  buf.writeUInt32LE(16, 16);
  buf.writeUInt16LE(1, 20); // PCM
  buf.writeUInt16LE(channels, 22);
  buf.writeUInt32LE(rate, 24);
  buf.writeUInt32LE(rate * channels * width, 28);
  buf.writeUInt16LE(channels * width, 32);
  buf.writeUInt16LE(bits, 34);
  buf.write("data", 36);
  buf.writeUInt32LE(dataLen, 40);
  const amp = 12000 * (1 << (bits - 16));
  // The left-justification for 20-in-24; every other depth fills its word.
  const justify = bits === 20 ? 16 : 1;
  for (let i = 0; i < frames; i++) {
    const v = Math.round(Math.sin((2 * Math.PI * 440 * i) / rate) * amp) * justify;
    for (let c = 0; c < channels; c++) buf.writeIntLE(v, 44 + (i * channels + c) * width, width);
  }
  return buf;
}

async function waitForPing(base, deadlineMS) {
  const deadline = Date.now() + deadlineMS;
  for (;;) {
    try {
      const resp = await fetch(`${base}/ping`);
      if (resp.ok) return;
    } catch {}
    if (Date.now() > deadline) throw new Error("daemon never answered /ping");
    await new Promise((r) => setTimeout(r, 200));
  }
}

// Drive one cell on the demo page and require playback progress: the
// player's currentTime past 2 s within 30 s, not paused, no fatal
// hls.js error and no <audio> element error.
const PLAYER_ID = { hls: "hlsPlayer", progressive: "player", timeline: "tlPlayer", span: "hlsPlayer" };

// fatalRE matches an hls.js fatal in the demo page's log. The page renders it
// with JSON.stringify(v, null, 2), so the colon is followed by a space and a
// needle of '"fatal":true' matches nothing: the browser cells' only report of
// an hls.js fatal was silently dead until this became a regexp.
//
// It is not the arm that fires most: both proofs of the fatal race reported
// through the media element's own error instead, which carries Chromium's
// parser text and is the better message anyway. Two reasons the log can be
// empty of a fatal that happened, and both are the page's: show() replaces
// #out rather than appending, so a later event overwrites an earlier fatal,
// and the media element sets error before hls.js has finished reporting. So
// this arm is the backstop for a fatal that sets no media error, a segment
// fetch failing being the common one.
const fatalRE = /"fatal":\s*true/;

// health reads the player's state and throws on anything a working cell
// cannot show: a self-pause, a media element error, an hls.js fatal.
async function health(page, playerID, what) {
  const state = await page.evaluate((id) => {
    const p = document.getElementById(id);
    return {
      paused: p.paused,
      currentTime: p.currentTime,
      duration: p.duration,
      mediaError: p.error ? p.error.code : 0,
      out: document.getElementById("out").textContent,
    };
  }, playerID);
  if (state.paused) throw new Error(`player paused itself ${what}`);
  if (state.mediaError) throw new Error(`media element error code ${state.mediaError} ${what}`);
  if (fatalRE.test(state.out)) throw new Error(`hls.js fatal error ${what}: ${state.out}`);
  return state;
}

// waitProgress waits for a progress predicate, racing it against a watch for
// the signs that this stream is not going to play at all.
//
// Without the race an init segment the browser refuses outright fails as
// "waitForFunction: Timeout 30000ms exceeded" after half a minute, naming
// nothing, when hls.js had already said exactly what it refused within a
// second. The cell failed either way; the diagnosis was the loss.
//
// The two waits are separate rather than one combined predicate because
// their outcomes differ in kind: the progress one resolving is the cell
// passing, and the fatal one resolving is the cell failing with a message to
// print. The fatal watch is given a longer budget and its own rejection is
// swallowed, so a clean 30 s with nothing refused still fails as the progress
// timeout it is rather than as a race the watchdog happened to win.
async function waitProgress(page, playerID, predicate, arg, what) {
  const progress = page.waitForFunction(predicate, arg, { timeout: 30000 });
  const fatal = page
    .waitForFunction(
      (id) => {
        const out = document.getElementById("out").textContent;
        // Kept in step with fatalRE above; this half runs in the page, where
        // that binding does not exist.
        const at = out.search(/"fatal":\s*true/);
        if (at >= 0) return `hls.js fatal: ${out.slice(Math.max(0, at - 500), at + 200)}`;
        const p = document.getElementById(id);
        if (p.error) return `media element error code ${p.error.code}: ${p.error.message}`;
        return null;
      },
      playerID,
      { timeout: 35000 },
    )
    .then((h) => h.jsonValue())
    .catch(() => null);
  // The loser is abandoned, so its later rejection must not surface as an
  // unhandled one when the next cell navigates the page.
  progress.catch(() => {});
  const reason = await Promise.race([progress.then(() => null), fatal]);
  if (reason) throw new Error(`${reason} (${what})`);
  await progress;
}

async function runCell(page, base, cell) {
  await page.goto(`${base}/demo`);
  await page.fill("#key", API_KEY);
  const playerID = PLAYER_ID[cell.surface];
  if (cell.surface === "timeline") {
    await page.fill("#tlSrcs", TIMELINE_TRACKS.map((_, i) => `lib/tl-${i}.wav`).join("\n"));
    await page.selectOption("#tlFormat", cell.format);
    await page.click("#tlPlay");
  } else if (cell.surface === "span") {
    await page.fill("#src", "lib/span.wav");
    await page.selectOption("#hlsFormat", cell.format);
    await page.fill("#hlsFrom", String(SPAN_FROM));
    await page.fill("#hlsTo", String(SPAN_TO));
    await page.click("#hlsPlay");
  } else if (cell.surface === "hls") {
    await page.fill("#src", `lib/${cell.src || "test.wav"}`);
    await page.selectOption("#hlsFormat", cell.format);
    await page.click("#hlsPlay");
  } else {
    await page.fill("#src", "lib/test.wav");
    await page.selectOption("#format", cell.format);
    await page.click("#play");
  }
  await waitProgress(
    page,
    playerID,
    (id) => document.getElementById(id).currentTime > 2,
    playerID,
    "waiting for 2 s of playback",
  );
  const state = await health(page, playerID, "during playback");

  if (cell.surface === "span") {
    // The whole claim: the presentation describes the span, not the file.
    // A playlist built from the file's track would run the fixture's full
    // 6 s, and would play perfectly while being the wrong stream, so the
    // duration is the assertion rather than a proxy for one.
    if (Math.abs(state.duration - SPAN_SECONDS) > 0.2) {
      throw new Error(
        `the player sees a ${state.duration.toFixed(3)}s stream, want the span's ` +
          `${SPAN_SECONDS.toFixed(3)}s: the playlist describes the file, not the virtual track`,
      );
    }
    return;
  }
  if (cell.surface !== "timeline") return;

  // The player must see the whole queue as one stream. Asserted before the
  // seek because it is the same failure with a better name: a timeline that
  // silently delivered only its first member would otherwise fail below as a
  // bare 30-second timeout waiting to reach a boundary it never had.
  const want = TIMELINE_TRACKS.reduce((a, b) => a + b, 0);
  if (Math.abs(state.duration - want) > 0.5) {
    throw new Error(
      `the player sees a ${state.duration.toFixed(3)}s stream, want the queue's ${want}s: ` +
        `the timeline is not being delivered whole`,
    );
  }

  // The timeline's own claim: a queue is one stream, so seeking across a
  // track boundary is an ordinary seek. Land just before the first seam and
  // require playback to carry on through it, which is where a delivery that
  // only pretends to be continuous (a second init, a discontinuity, a
  // mis-numbered segment) would stall or error instead.
  const seekTo = TIMELINE_SEAM - 1;
  await page.evaluate(
    ([id, t]) => {
      const p = document.getElementById(id);
      p.currentTime = t;
      p.play();
    },
    [playerID, seekTo],
  );
  await waitProgress(
    page,
    playerID,
    ([id, past]) => document.getElementById(id).currentTime > past,
    [playerID, TIMELINE_SEAM + 1],
    `playing through the ${TIMELINE_SEAM}s track boundary`,
  );
  await health(page, playerID, `after seeking across the ${TIMELINE_SEAM}s track boundary`);
}

// Hard watchdog: a hung browser launch or player must fail the run,
// not pin it. Fourteen cells at their 30 s budget (two waits for the
// timeline cells) come to about 8 minutes, so 15 keeps a margin.
const watchdog = setTimeout(() => {
  console.error("client-e2e FAILED: 15-minute watchdog fired");
  process.exit(1);
}, 15 * 60 * 1000);
watchdog.unref();

const work = mkdtempSync(join(tmpdir(), "waxflow-client-e2e-"));
const root = join(work, "lib");
const cache = join(work, "cache");
const data = join(work, "data");
for (const d of [root, cache, data]) mkdirSync(d, { recursive: true });
writeFileSync(join(root, "test.wav"), makeWAV());
writeFileSync(join(root, "test24.wav"), makeWAV(6, 48000, 2, 24));
writeFileSync(join(root, "test20.wav"), makeWAV(6, 48000, 2, 20));
// The span fixture is 44.1 kHz on purpose: HLS opus is always 48 kHz, so a
// span of it is resampled by construction, which is the CUE-rip case and
// the one where a span's first samples come out of a filter window that
// has to be primed rather than empty.
writeFileSync(join(root, "span.wav"), makeWAV(6, SPAN_RATE));
// The timeline queue: three tracks of different lengths, so a bug that
// assumed uniform members shows up as a wrong boundary rather than passing
// by symmetry.
TIMELINE_TRACKS.forEach((seconds, i) => {
  writeFileSync(join(root, `tl-${i}.wav`), makeWAV(seconds));
});
const configPath = join(work, "config.json");
writeFileSync(
  configPath,
  JSON.stringify({
    addr: `127.0.0.1:${PORT}`,
    roots: [{ name: "lib", path: root }],
    apiKeys: [API_KEY],
    cacheDir: cache,
    dataDir: data,
  }),
);

const bin = (process.env.WAXFLOW_BIN || "go run -C cli ./cmd/waxflow").split(" ");
const daemon = spawn(bin[0], [...bin.slice(1), "server", "--demo", "--config", configPath], {
  stdio: ["ignore", "inherit", "inherit"],
});

let browser;
let failures = 0;
try {
  const base = `http://127.0.0.1:${PORT}`;
  await waitForPing(base, 30000);

  // Headless Chromium blocks audible autoplay without a gesture; the
  // test clicks a button, but the policy still wants the flag.
  browser = await chromium.launch({ args: ["--autoplay-policy=no-user-gesture-required"] });
  const page = await browser.newPage();
  let pageErr = null;
  page.on("pageerror", (err) => {
    pageErr = err;
  });

  for (const cell of cells) {
    const name = cellName(cell);
    pageErr = null;
    try {
      await runCell(page, base, cell);
      if (pageErr) throw pageErr;
      console.log(`client-e2e OK   ${name}`);
    } catch (err) {
      console.error(`client-e2e FAIL ${name}: ${err}`);
      failures++;
    }
  }
} catch (err) {
  console.error("client-e2e FAILED:", err);
  failures++;
} finally {
  if (browser) await browser.close();
  daemon.kill("SIGTERM");
  await new Promise((r) => daemon.once("exit", r));
  rmSync(work, { recursive: true, force: true });
}
if (failures) console.error(`client-e2e: ${failures} of ${cells.length} cells failed`);
else console.log(`client-e2e: all ${cells.length} cells passed`);
process.exit(failures ? 1 : 0);
