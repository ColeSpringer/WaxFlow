// Command waxflow (waxbin flavor) is the stock CLI with one addition:
// pid:<ULID> source references resolve against the WaxBin catalog named
// by catalogDB. Without catalogDB it behaves exactly like the stock
// binary.
package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/colespringer/waxflow/internal/cli"
	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/resolver"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// version is stamped by the build: -ldflags "-X main.version=v0.0.1".
var version = "dev"

func main() {
	os.Exit(cli.ExecuteFlavor(version, os.Args[1:], os.Stdout, os.Stderr, cli.Flavor{
		Name:         "waxbin",
		OpenResolver: openResolver,
	}))
}

// openResolver chains the catalog resolver ahead of the roots when
// catalogDB is configured.
func openResolver(cfg config.Config, next source.Resolver, logger *slog.Logger, daemon bool) (source.Resolver, io.Closer, error) {
	if cfg.CatalogDB == "" {
		// This build could serve pid refs; say what is actually missing
		// instead of the stock binary's "run the waxbin flavor".
		return noCatalog{next: next}, nil, nil
	}
	opts := resolver.Options{
		DBPath:   cfg.CatalogDB,
		Next:     next,
		MaxBytes: cfg.ResolvedSourceMaxBytes(),
		Logger:   logger,
	}
	if !daemon {
		// One-shot commands (probe, transcode, sign) resolve once and
		// tear down; the invalidation poll is daemon machinery.
		opts.PollInterval = -1
	}
	cat, err := resolver.Open(context.Background(), opts)
	if err != nil {
		return nil, nil, err
	}
	return cat, cat, nil
}

// noCatalog answers pid refs precisely when no catalog is configured.
type noCatalog struct{ next source.Resolver }

func (n noCatalog) Resolve(ref string) (*source.File, error) {
	if strings.HasPrefix(ref, "pid:") {
		return nil, waxerr.New(waxerr.CodeUnsupportedSource,
			"source: pid references need catalogDB configured")
	}
	return n.next.Resolve(ref)
}
