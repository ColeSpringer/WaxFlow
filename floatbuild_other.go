//go:build !amd64

package waxflow

import "runtime"

// FloatBuildVersion is this build's term in a transcode plan's Versions,
// naming how it rounds float arithmetic, which differs by architecture: arm64
// fuses multiply-adds, so a lossy encode can differ in its last bits.
const FloatBuildVersion = "fp-" + runtime.GOARCH
