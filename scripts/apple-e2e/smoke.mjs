// Driver for the AVPlayer smoke probe (smoke.swift): start a real daemon
// over a one-file library, mint one signed HLS URL, and play it twice, with
// and without the ATS exception.
//
// This is deliberately not the harness the client matrix's Apple cells will
// run on. It is the spike that decides whether that harness can exist and
// what it has to declare, so it keeps its own fixture writer and daemon spawn
// rather than sharing client-e2e.mjs's: the sharing is worth doing once the
// answers are in and the shape is known, and doing it now would mean editing
// a working harness for a program that may be thrown away.
//
// Both probe builds run whatever the first one does, because the interesting
// result is the pair. A plain build that plays says no exception is needed; a
// plain build that fails on -1022 (ATS) beside a plist build that plays says
// the exception is the whole story; both failing the same way says the problem
// is not ATS and the error text says what it is.
//
// Usage: node scripts/apple-e2e/smoke.mjs
// Environment: WAXFLOW_BIN overrides the daemon command (the Makefile target
// passes ./bin/waxflow).

import { mkdtempSync, writeFileSync, rmSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn, spawnSync } from "node:child_process";

const API_KEY = "apple-smoke-key";
const PORT = 20000 + Math.floor(Math.random() * 20000);
const HERE = import.meta.dirname;

// A 6 s 48 kHz stereo sine WAV, written directly so the spike needs no
// external tool on the runner.
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
      if ((await fetch(`${base}/ping`)).ok) return;
    } catch {}
    if (Date.now() > deadline) throw new Error("daemon never answered /ping");
    await new Promise((r) => setTimeout(r, 200));
  }
}

// build compiles the probe, optionally with the Info.plist linked into
// __TEXT,__info_plist, which is how a command-line tool carries one.
function build(out, withPlist) {
  const args = ["-O", join(HERE, "smoke.swift"), "-o", out];
  if (withPlist) {
    args.push(
      "-Xlinker", "-sectcreate",
      "-Xlinker", "__TEXT",
      "-Xlinker", "__info_plist",
      "-Xlinker", join(HERE, "Info.plist"),
    );
  }
  const res = spawnSync("swiftc", args, { stdio: ["ignore", "inherit", "inherit"] });
  if (res.error) throw res.error;
  if (res.status !== 0) throw new Error(`swiftc exited ${res.status}`);
}

const work = mkdtempSync(join(tmpdir(), "waxflow-apple-smoke-"));
const root = join(work, "lib");
for (const d of [root, join(work, "cache"), join(work, "data")]) mkdirSync(d, { recursive: true });
writeFileSync(join(root, "test.wav"), makeWAV());
const configPath = join(work, "config.json");
writeFileSync(
  configPath,
  JSON.stringify({
    addr: `127.0.0.1:${PORT}`,
    roots: [{ name: "lib", path: root }],
    apiKeys: [API_KEY],
    cacheDir: join(work, "cache"),
    dataDir: join(work, "data"),
  }),
);

const bin = (process.env.WAXFLOW_BIN || "go run -C cli ./cmd/waxflow").split(" ");
const daemon = spawn(bin[0], [...bin.slice(1), "server", "--config", configPath], {
  stdio: ["ignore", "inherit", "inherit"],
});

// A hung swiftc, a hung probe, or a daemon that never answers must fail the
// run rather than pin the job until the runner's own timeout. Two probes at
// their 30 s budget plus two compiles is well under a minute, so five is a
// wide margin.
const watchdog = setTimeout(() => {
  console.error("apple-smoke FAILED: 5-minute watchdog fired");
  process.exit(2);
}, 5 * 60 * 1000);
watchdog.unref();

// stopDaemon waits for the exit that has not happened yet, and returns at once
// for the one that already has: `once("exit")` on a process that died while
// the probes ran never fires again, and awaiting it hangs the script for good.
async function stopDaemon() {
  if (daemon.exitCode !== null || daemon.signalCode !== null) return;
  daemon.kill("SIGTERM");
  await new Promise((r) => daemon.once("exit", r));
}

let setupErr = null;
let ran = 0;
let played = 0;
try {
  const base = `http://127.0.0.1:${PORT}`;
  await waitForPing(base, 60000);

  // The client-matrix recipe: POST /sign returns a URL carrying its own
  // signature, so the probe needs no API key and no header support.
  const resp = await fetch(`${base}/sign`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-API-Key": API_KEY },
    body: JSON.stringify({
      path: "/hls/master.m3u8",
      params: { src: "lib/test.wav", format: "aac" },
    }),
  });
  const body = await resp.json();
  if (!resp.ok || !body.url) throw new Error(`/sign: ${resp.status} ${JSON.stringify(body)}`);
  const url = new URL(body.url, base).toString();

  for (const cell of [
    { name: "plain", plist: false },
    { name: "ats-exception", plist: true },
  ]) {
    // Each cell is its own attempt: the pair is the result, so one build
    // failing must not cost the other's answer.
    try {
      const out = join(work, `smoke-${cell.name}`);
      build(out, cell.plist);
      const res = spawnSync(out, [url, "2", "30"], { encoding: "utf8" });
      ran++;
      const text = `${res.stdout || ""}${res.stderr || ""}`.trim();
      for (const line of text.split("\n")) console.log(`apple-smoke ${cell.name}: ${line}`);
      if (res.status === 0) {
        played++;
        console.log(`apple-smoke OK   ${cell.name}`);
      } else {
        console.error(`apple-smoke FAIL ${cell.name} (exit ${res.status})`);
      }
    } catch (err) {
      console.error(`apple-smoke ERROR ${cell.name}: ${err}`);
    }
  }
} catch (err) {
  setupErr = err;
  console.error("apple-smoke FAILED before it could probe:", err);
} finally {
  await stopDaemon();
  clearTimeout(watchdog);
  rmSync(work, { recursive: true, force: true });
}

// Exit codes carry the finding, since a CI job's status is the first thing
// read. Either probe playing is the answer the harness needs, so that is
// tested FIRST and goes green whatever the other probe did: a plain build that
// will not compile must not throw away an ATS build that played. Both probes
// failing IS an answer, and a discouraging one, so it fails and the printed
// AVFoundation errors say why. Neither probe running is not an answer at all,
// and says this job is wrong rather than anything about AVPlayer.
if (played > 0) {
  console.log(`apple-smoke: ${played} of ${ran} probes that ran played`);
  process.exit(0);
}
if (setupErr || ran === 0) {
  console.error("apple-smoke: no probe ran");
  process.exit(2);
}
console.error(`apple-smoke: none of the ${ran} probes that ran played; read the errors above`);
process.exit(1);
