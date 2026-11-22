//go:build !amd64.v3

package waxflow

// FloatBuildVersion is this build's term in a transcode plan's Versions,
// naming how it rounds float arithmetic. It is empty below GOAMD64=v3, where
// the compiler fuses no multiply into an add.
const FloatBuildVersion = ""
