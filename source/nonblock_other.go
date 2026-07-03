//go:build !unix

package source

// Platforms without FIFO open-hang semantics need no open flag; the
// regular-file check still applies.
const openNonblock = 0
