// Browser client-matrix check: start a real WaxFlow daemon over a
// generated library, open the committed /demo page in headless Chromium,
// and drive every playback cell the browser column of
// docs/client-matrix.md claims: HLS variants through hls.js and
// progressive streams (live transcodes plus direct play) through
// <audio>. Each cell must actually progress past 2 s of playback with a
// healthy player.
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
// to a comma-separated list like "hls:opus,progressive:mp3".

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
  { surface: "progressive", format: "opus" },
  { surface: "progressive", format: "mp3" },
  { surface: "progressive", format: "aac" },
  { surface: "progressive", format: "flac" },
  { surface: "progressive", format: "wav" },
  { surface: "progressive", format: "auto" }, // direct play, original bytes
];

const only = process.env.CLIENT_E2E_CELLS
  ? new Set(process.env.CLIENT_E2E_CELLS.split(","))
  : null;
const cells = CELLS.filter((c) => !only || only.has(`${c.surface}:${c.format}`));

// A 6 s 48 kHz stereo 16-bit sine WAV, written directly: no external
// tools needed to make a fixture.
function makeWAV(seconds = 6, rate = 48000, channels = 2) {
  const frames = seconds * rate;
  const dataLen = frames * channels * 2;
  const buf = Buffer.alloc(44 + dataLen);
  buf.write("RIFF", 0);
  buf.writeUInt32LE(36 + dataLen, 4);
  buf.write("WAVEfmt ", 8);
  buf.writeUInt32LE(16, 16);
  buf.writeUInt16LE(1, 20); // PCM
  buf.writeUInt16LE(channels, 22);
  buf.writeUInt32LE(rate, 24);
  buf.writeUInt32LE(rate * channels * 2, 28);
  buf.writeUInt16LE(channels * 2, 32);
  buf.writeUInt16LE(16, 34);
  buf.write("data", 36);
  buf.writeUInt32LE(dataLen, 40);
  for (let i = 0; i < frames; i++) {
    const v = Math.round(Math.sin((2 * Math.PI * 440 * i) / rate) * 12000);
    for (let c = 0; c < channels; c++) buf.writeInt16LE(v, 44 + (i * channels + c) * 2);
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
async function runCell(page, base, cell) {
  await page.goto(`${base}/demo`);
  await page.fill("#key", API_KEY);
  await page.fill("#src", "lib/test.wav");
  const playerID = cell.surface === "hls" ? "hlsPlayer" : "player";
  if (cell.surface === "hls") {
    await page.selectOption("#hlsFormat", cell.format);
    await page.click("#hlsPlay");
  } else {
    await page.selectOption("#format", cell.format);
    await page.click("#play");
  }
  await page.waitForFunction(
    (id) => document.getElementById(id).currentTime > 2,
    playerID,
    { timeout: 30000 },
  );
  const state = await page.evaluate((id) => {
    const p = document.getElementById(id);
    return {
      paused: p.paused,
      mediaError: p.error ? p.error.code : 0,
      out: document.getElementById("out").textContent,
    };
  }, playerID);
  if (state.paused) throw new Error("player paused itself");
  if (state.mediaError) throw new Error(`media element error code ${state.mediaError}`);
  if (state.out.includes('"fatal":true')) throw new Error(`hls.js fatal error: ${state.out}`);
}

// Hard watchdog: a hung browser launch or player must fail the run,
// not pin it (the worst cell budget is 30 s and there are few cells).
const watchdog = setTimeout(() => {
  console.error("client-e2e FAILED: 10-minute watchdog fired");
  process.exit(1);
}, 10 * 60 * 1000);
watchdog.unref();

const work = mkdtempSync(join(tmpdir(), "waxflow-client-e2e-"));
const root = join(work, "lib");
const cache = join(work, "cache");
const data = join(work, "data");
for (const d of [root, cache, data]) mkdirSync(d, { recursive: true });
writeFileSync(join(root, "test.wav"), makeWAV());
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
    const name = `${cell.surface}:${cell.format}`;
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
