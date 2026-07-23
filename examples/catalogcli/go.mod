// The module path is outside github.com/colespringer/waxflow/ on
// purpose: it puts this module on the far side of Go's internal rule,
// where every consumer of the cli.Flavor seam lives. See main.go.
module waxflow.test/catalogcli

go 1.26

require (
	github.com/colespringer/waxflow v0.0.0-00010101000000-000000000000
	github.com/colespringer/waxflow/cli v0.0.0-00010101000000-000000000000
)

require (
	github.com/colespringer/waxlabel v1.2.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/cobra v1.10.2 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
)

replace github.com/colespringer/waxflow => ../..

replace github.com/colespringer/waxflow/cli => ../../cli
