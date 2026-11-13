//go:build !amd64

package waxflow

import "runtime"

// FloatBuildVersion is this build's term in a transcode plan's Versions:
// how the build rounds float arithmetic, which the compiler and
// the math package decide by architecture. arm64 fuses multiply-adds, for
// one, so a lossy encode can differ from amd64's in its last bits.
const FloatBuildVersion = "fp-" + runtime.GOARCH
