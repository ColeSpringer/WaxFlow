module github.com/colespringer/waxflow/resolver

go 1.26

require (
	github.com/colespringer/waxbin v0.0.0-20260702055125-8fd6f8d0a05e
	github.com/colespringer/waxflow v0.0.0-00010101000000-000000000000
	github.com/colespringer/waxflow/cli v0.0.0-00010101000000-000000000000
)

require (
	github.com/colespringer/waxlabel v1.0.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/gofrs/flock v0.13.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/oklog/ulid/v2 v2.1.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/spf13/cobra v1.10.2 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/image v0.43.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.53.0 // indirect
)

replace github.com/colespringer/waxflow => ../

replace github.com/colespringer/waxflow/cli => ../cli

// WaxBin's pinned commit predates waxlabel's Fields.Comment string ->
// []string change and does not compile against the parent's newer
// waxlabel, so this binary pins waxlabel at WaxBin's version. Verified
// against the parent's metadata suite run from this module (m4b golden
// byte-identical, jobs/tags/RG e2e green). Drop this replace when
// WaxBin rebases onto current waxlabel.
replace github.com/colespringer/waxlabel => github.com/colespringer/waxlabel v0.0.0-20260629094436-9b68ee971607
