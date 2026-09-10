// AVPlayer smoke probe: play one URL and say whether playback advanced.
//
// This is a spike, not the harness. It exists to settle the two things about
// a hosted macOS runner that decide how an AVPlayer harness has to be built,
// and it answers both in one run of the job that drives it:
//
//  1. Does AVPlayer advance with no audio output device? A GitHub Actions
//     macOS runner has no speakers and no logged-in audio session. If
//     currentTime never moves, an AVPlayer harness cannot assert playback at
//     all and the whole approach needs rethinking.
//  2. Does App Transport Security block plain-http loopback from a
//     command-line Swift process? A binary with no Info.plist has no ATS
//     exception to declare. The driver builds this file twice, once plain and
//     once with an embedded plist carrying NSAllowsLocalNetworking, and the
//     two outcomes say whether an exception is needed.
//
// Output is one line per run, "OK ..." or "FAIL ...", so the driver can read
// it without parsing AVFoundation's error descriptions.
//
// Usage: smoke <url> [seconds] [timeout]

import AVFoundation
import Foundation

let args = CommandLine.arguments
guard args.count > 1, let url = URL(string: args[1]) else {
    print("FAIL usage: smoke <url> [seconds] [timeout]")
    exit(2)
}
let want = args.count > 2 ? (Double(args[2]) ?? 2) : 2
let timeout = args.count > 3 ? (Double(args[3]) ?? 30) : 30

func describe(_ err: Error?) -> String {
    guard let err else { return "none" }
    // The localized description alone routinely reads "The operation could not
    // be completed", which names nothing; the underlying error is where the
    // ATS refusal and the parser refusal actually appear.
    let ns = err as NSError
    var parts = ["\(ns.domain)/\(ns.code)", ns.localizedDescription]
    if let under = ns.userInfo[NSUnderlyingErrorKey] as? NSError {
        parts.append("underlying=\(under.domain)/\(under.code) \(under.localizedDescription)")
    }
    return parts.joined(separator: " | ")
}

print("INFO macOS=\(ProcessInfo.processInfo.operatingSystemVersionString) url=\(url.absoluteString)")

let item = AVPlayerItem(url: url)
let player = AVPlayer(playerItem: item)
player.play()

var sawReady = false
let deadline = Date().addingTimeInterval(timeout)
while Date() < deadline {
    // Properties are polled rather than observed: KVO delivery would make the
    // probe depend on a run loop being serviced in a particular way, and this
    // has to be the simplest thing that can answer the question. The run loop
    // is spun anyway, since that is also the sleep.
    switch item.status {
    case .failed:
        print("FAIL status=failed error=\(describe(item.error))")
        exit(1)
    case .readyToPlay:
        if !sawReady {
            print("INFO status=readyToPlay duration=\(item.duration.seconds)")
            sawReady = true
        }
        let t = player.currentTime().seconds
        if t.isFinite && t >= want {
            print("OK played=\(t) duration=\(item.duration.seconds)")
            exit(0)
        }
    default:
        break
    }
    RunLoop.main.run(until: Date().addingTimeInterval(0.2))
}

let played = player.currentTime().seconds
print("FAIL timeout status=\(item.status.rawValue) ready=\(sawReady) played=\(played) error=\(describe(item.error))")
exit(1)
