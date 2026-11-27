module github.com/colespringer/waxflow/oracletest

go 1.26

require (
	github.com/colespringer/waxflow v0.0.0-00010101000000-000000000000
	github.com/colespringer/waxflow/cli v0.0.0-00010101000000-000000000000
	github.com/colespringer/waxlabel v1.9.0
	github.com/hajimehoshi/go-mp3 v0.3.4
	github.com/jfreymuth/oggvorbis v1.0.5
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jfreymuth/vorbis v1.0.2 // indirect
	github.com/spf13/cobra v1.10.2 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
)

replace github.com/colespringer/waxflow => ../

replace github.com/colespringer/waxflow/cli => ../cli
