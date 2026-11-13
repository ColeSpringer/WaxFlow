//go:build !amd64.v3

package waxflow

// FloatBuildVersion is this build's term in a transcode plan's Versions:
// how the build rounds float arithmetic, named wherever that can
// differ from amd64 below GOAMD64=v3. It is empty there, where the compiler
// fuses no multiply into an add, so those keys carry no term.
const FloatBuildVersion = ""
