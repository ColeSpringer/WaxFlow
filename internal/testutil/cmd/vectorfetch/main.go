// Command vectorfetch downloads the SHA-256-pinned conformance vectors
// into testdata/vectors. It backs `make verify-vectors`; CI caches the
// directory keyed on the pinned digests.
package main

import (
	"fmt"
	"os"

	"github.com/colespringer/waxflow/internal/testutil"
)

func main() {
	if err := testutil.Fetch(os.Stdout, testutil.VectorsDir(), testutil.Vectors); err != nil {
		fmt.Fprintf(os.Stderr, "vectorfetch: %v\n", err)
		os.Exit(1)
	}
}
