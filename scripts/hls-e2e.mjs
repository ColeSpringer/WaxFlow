// hls.js end-to-end check: start a real WaxFlow daemon over a generated
// library, open the committed /demo page in headless Chromium, mint an
// HLS master through the page's own controls, and assert playback
// actually progresses without player errors.
//
// Gated tooling (not part of `make test`): needs Node 18+ and Playwright
// with Chromium installed:
//
//   npm install playwright && npx playwright install chromium
//   make hls-e2e
//
// Environment: WAXFLOW_BIN overrides the daemon command (default
// `go run ./cmd/waxflow`); HLS_E2E_FORMAT picks the variant format
// (default opus).

import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn } from "node:child_process";
import { chromium } from "playwright";

const API_KEY = "hls-e2e-key";
const PORT = 20000 + Math.floor(Math.random() * 20000);
const FORMAT = process.env.HLS_E2E_FORMAT || "opus";

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

const work = mkdtempSync(join(tmpdir(), "waxflow-hls-e2e-"));
const root = join(work, "lib");
const cache = join(work, "cache");
const data = join(work, "data");
for (const d of [root, cache, data]) {
  writeFileSync(join(work, ".keep"), ""); // ensure work exists on all platforms
  await import("node:fs").then((fs) => fs.mkdirSync(d, { recursive: true }));
}
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

const bin = (process.env.WAXFLOW_BIN || "go run ./cmd/waxflow").split(" ");
const daemon = spawn(bin[0], [...bin.slice(1), "server", "--demo", "--config", configPath], {
  stdio: ["ignore", "inherit", "inherit"],
});

let browser;
let failed = false;
try {
  const base = `http://127.0.0.1:${PORT}`;
  await waitForPing(base, 30000);

  // Headless Chromium blocks audible autoplay without a gesture; the
  // test clicks a button, but the policy still wants the flag.
  browser = await chromium.launch({ args: ["--autoplay-policy=no-user-gesture-required"] });
  const page = await browser.newPage();
  page.on("pageerror", (err) => {
    console.error("page error:", err);
    failed = true;
  });
  await page.goto(`${base}/demo`);
  await page.fill("#key", API_KEY);
  await page.fill("#src", "lib/test.wav");
  await page.selectOption("#hlsFormat", FORMAT);
  await page.click("#hlsPlay");

  // Playback must actually progress: currentTime past 2 s within 30 s.
  await page.waitForFunction(
    () => document.getElementById("hlsPlayer").currentTime > 2,
    null,
    { timeout: 30000 },
  );
  // And the player must be healthy: not paused, no surfaced hls.js error
  // object in the output panel.
  const state = await page.evaluate(() => ({
    paused: document.getElementById("hlsPlayer").paused,
    out: document.getElementById("out").textContent,
  }));
  if (state.paused) throw new Error("player paused itself");
  if (state.out.includes('"fatal":true')) throw new Error(`hls.js fatal error: ${state.out}`);

  console.log(`hls-e2e OK: ${FORMAT} played past 2s through hls.js`);
} catch (err) {
  console.error("hls-e2e FAILED:", err);
  failed = true;
} finally {
  if (browser) await browser.close();
  daemon.kill("SIGTERM");
  await new Promise((r) => daemon.once("exit", r));
  rmSync(work, { recursive: true, force: true });
}
process.exit(failed ? 1 : 0);
