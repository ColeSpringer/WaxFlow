//go:build !unix

package container

// Platforms without FIFO open-hang semantics need no open flag;
// CheckRegular still applies.
const OpenNonblock = 0
