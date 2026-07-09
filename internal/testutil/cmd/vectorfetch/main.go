// Command vectorfetch downloads the SHA-256-pinned conformance vectors
// into testdata/vectors. It backs `make verify-vectors`; CI caches the
// directory keyed on the pinned digests. With arguments it fetches only
// the vectors whose names match an argument exactly or by prefix, which
// lets targets like `make opus-tools` pull one pinned file without the
// full corpus.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/colespringer/waxflow/internal/testutil"
)

func main() {
	vectors := testutil.Vectors
	if args := os.Args[1:]; len(args) > 0 {
		vectors = nil
		for _, v := range testutil.Vectors {
			for _, a := range args {
				if v.Name == a || strings.HasPrefix(v.Name, a) {
					vectors = append(vectors, v)
					break
				}
			}
		}
		if len(vectors) == 0 {
			fmt.Fprintf(os.Stderr, "vectorfetch: no pinned vector matches %q\n", args)
			os.Exit(1)
		}
	}
	if err := testutil.Fetch(os.Stdout, testutil.VectorsDir(), vectors); err != nil {
		fmt.Fprintf(os.Stderr, "vectorfetch: %v\n", err)
		os.Exit(1)
	}
}
