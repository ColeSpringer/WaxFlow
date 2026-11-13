//go:build amd64.v4

package waxflow

// FloatBuildVersion is this build's term in a transcode plan's Versions.
// At GOAMD64=v4 the compiler fuses multiply-adds, as at v3.
const FloatBuildVersion = "fp-amd64v4"
