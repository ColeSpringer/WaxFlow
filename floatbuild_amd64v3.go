//go:build amd64.v3 && !amd64.v4

package waxflow

// FloatBuildVersion is this build's term in a transcode plan's Versions.
// At GOAMD64=v3 the compiler fuses multiply-adds, so a lossy
// encode can differ from the default level's in its last bits.
const FloatBuildVersion = "fp-amd64v3"
