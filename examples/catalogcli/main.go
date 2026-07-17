// Command catalogcli is a worked example: a waxflow CLI that resolves
// pid:<ULID> source references against a catalog.
//
// No build in the WaxFlow repo resolves pid: references. A build that
// wants them supplies cli.Flavor.OpenResolver, as this command does
// against a stub catalog: catalogDB names a directory, and the file
// called <ULID> inside it holds the path of the track that PID names.
// That is the whole lookup a real catalog does, minus the database.
// Replace openCatalog and catalog.Resolve with real queries and the
// surrounding shape stands.
//
// The module path is deliberately outside github.com/colespringer/waxflow/.
// Go's internal rule keys on the import path, so this module cannot
// reach waxflow/internal/..., exactly like any third-party consumer. If
// the cli.Flavor seam ever widens back to an internal type, this module
// stops compiling, and it is the only thing in the tree that does:
// every other test lives inside the prefix and names internal types
// freely.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxflow/cli"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// version is stamped by the build: -ldflags "-X main.version=v0.0.1".
var version = "dev"

func main() {
	os.Exit(cli.ExecuteFlavor(version, os.Args[1:], os.Stdout, os.Stderr, cli.Flavor{
		Name:         "catalog",
		OpenResolver: openResolver,
	}))
}

// openResolver is cli.Flavor.OpenResolver: it chains the catalog ahead
// of the library roots when catalogDB is configured.
func openResolver(ctx context.Context, opts cli.ResolverOptions) (source.Resolver, io.Closer, error) {
	if opts.CatalogDB == "" {
		// Present but unconfigured. This build owns the refusal for its
		// own scheme, so a pid: reference reports the missing catalog
		// instead of falling through to the stock unsupported-source
		// error, which would tell the operator to go find a build that
		// serves pid: -- this one.
		return noCatalog{next: opts.Next}, nil, nil
	}
	cat, err := openCatalog(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	return cat, cat, nil
}

// catalog resolves pid:<ULID> against the stub catalog and delegates
// every other reference to the library roots.
type catalog struct {
	dir      string
	next     source.Resolver
	maxBytes int64
	log      *slog.Logger
}

// openCatalog does the open-time I/O a real catalog does -- confirm the
// catalog is there and usable before any command runs, where a database
// would check its schema or ping its pool. ctx bounds that work, which
// is why OpenResolver takes one.
func openCatalog(ctx context.Context, opts cli.ResolverOptions) (*catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fi, err := os.Stat(opts.CatalogDB)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeCatalogUnavailable,
			"catalogcli: opening catalog "+opts.CatalogDB, err)
	}
	if !fi.IsDir() {
		return nil, waxerr.New(waxerr.CodeCatalogUnavailable,
			"catalogcli: catalog "+opts.CatalogDB+" is not a directory")
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = source.DefaultMaxBytes
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// A real catalog starts its invalidation poll here, and only when
	// opts.Daemon is true: under a one-shot command nothing outlives the
	// resolution, so a background goroutine has no one to serve. This
	// stub caches nothing and has nothing to start.
	log.Debug("catalog opened", "dir", opts.CatalogDB, "daemon", opts.Daemon)
	return &catalog{dir: opts.CatalogDB, next: opts.Next, maxBytes: maxBytes, log: log}, nil
}

func (c *catalog) Resolve(ctx context.Context, ref string) (*source.File, error) {
	pid, ok := strings.CutPrefix(ref, "pid:")
	if !ok {
		return c.next.Resolve(ctx, ref)
	}
	if pid == "" || strings.ContainsAny(pid, `/\`) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"catalogcli: malformed pid reference "+ref)
	}
	// The lookup: PID to the path of the file it names. A real catalog
	// queries its database and caches the answer.
	path, err := os.ReadFile(filepath.Join(c.dir, pid))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeNotFound,
			"catalogcli: no catalog entry for "+ref, err)
	}
	// The resolved path is the extension hint too: the catalog knows the
	// real filename, and the CLI sniffs the format anyway.
	f, err := source.OpenLocal(ref, strings.TrimSpace(string(path)), strings.TrimSpace(string(path)))
	if err != nil {
		return nil, err
	}
	if f.ID.Size > c.maxBytes {
		f.Close()
		return nil, waxerr.New(waxerr.CodePayloadTooLarge,
			fmt.Sprintf("catalogcli: %d bytes exceeds the %d-byte source cap", f.ID.Size, c.maxBytes))
	}
	return f, nil
}

// Close releases what openCatalog opened. The CLI closes it after the
// resolver's last use; a real catalog closes its database here.
func (c *catalog) Close() error { return nil }

// PIDSources implements cli.PIDSourceReporter: a catalog is open, so
// this daemon serves pid: references and /caps says so. Keying that on
// the resolver rather than on a config field is what lets a build
// resolve pid: from something other than catalogDB.
func (c *catalog) PIDSources() bool { return true }

// noCatalog answers pid references precisely when no catalog is
// configured, and delegates everything else.
type noCatalog struct{ next source.Resolver }

func (n noCatalog) Resolve(ctx context.Context, ref string) (*source.File, error) {
	if strings.HasPrefix(ref, "pid:") {
		return nil, waxerr.New(waxerr.CodeUnsupportedSource,
			"catalogcli: pid references need catalogDB configured")
	}
	return n.next.Resolve(ctx, ref)
}

// PIDSources implements cli.PIDSourceReporter: this build could serve
// pid: references but has no catalog, so /caps must not advertise them.
func (noCatalog) PIDSources() bool { return false }
