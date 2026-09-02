// Command vectorfetch downloads the SHA-256-pinned conformance vectors
// into testdata/vectors. It backs `make verify-vectors`; CI caches the
// directory keyed on the pinned digests. With arguments it fetches only
// the vectors whose names match an argument exactly or by prefix, which
// lets targets like `make opus-tools` pull one pinned file without the
// full corpus.
//
// With -extract DIR the matched vectors are also unpacked into DIR after the
// digest check: tar.gz and tar.bz2 archives, with Go's own decompressors, so
// a build recipe does not depend on a bzip2 binary being installed.
package main

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxflow/internal/testutil"
)

func main() {
	extract := flag.String("extract", "", "unpack the fetched archives into this directory")
	flag.Parse()
	vectors := testutil.Vectors
	if args := flag.Args(); len(args) > 0 {
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
	if *extract == "" {
		return
	}
	for _, v := range vectors {
		path := filepath.Join(testutil.VectorsDir(), filepath.FromSlash(v.Name))
		if err := extractArchive(path, *extract); err != nil {
			fmt.Fprintf(os.Stderr, "vectorfetch: extracting %s: %v\n", v.Name, err)
			os.Exit(1)
		}
		fmt.Printf("unpacked %s into %s\n", v.Name, *extract)
	}
}

// extractArchive unpacks a .tar.gz or .tar.bz2 into dir. Entries are confined
// to dir: an absolute or parent-relative name is refused, since the archive is
// pinned but the check costs nothing.
func extractArchive(path, dir string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader
	switch {
	case strings.HasSuffix(path, ".tar.gz") || strings.HasSuffix(path, ".tgz"):
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	case strings.HasSuffix(path, ".tar.bz2"):
		r = bzip2.NewReader(f)
	default:
		return fmt.Errorf("%s is not a tar.gz or tar.bz2 archive", filepath.Base(path))
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(h.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") || strings.Contains(name, string(filepath.Separator)+"..") {
			return fmt.Errorf("archive entry %q escapes the target directory", h.Name)
		}
		target := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			// Symlinks and specials have no place in a source tarball this
			// tree builds from; skipping them keeps the unpack confined.
		}
	}
}
