// Package gainspec parses a gain setting: the gain= request parameter and
// the defaultGain configuration key, which share one grammar. It lives apart
// from the server so the configuration can refuse a spelling the daemon
// would refuse at start.
package gainspec

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/colespringer/waxflow/waxerr"
)

// Mode is where a gain comes from.
type Mode int

const (
	// Off applies no gain.
	Off Mode = iota
	// Track and Album read the source's ReplayGain tags.
	Track
	Album
	// Fixed is a dB number the caller names.
	Fixed
)

// Spec is a parsed gain setting. DB is read only under Fixed.
type Spec struct {
	Mode Mode
	DB   float64
}

// Spellings lists the named modes Parse accepts. A dB number is accepted
// too and has no spelling to list.
func Spellings() []string {
	return []string{"off", "track", "album"}
}

// Parse reads off, track, album, or a finite dB number with an optional dB
// suffix, in any case. The empty string is refused: it means a default,
// which the caller supplies. A positive number is not clamped here, since
// the ceiling depends on the dynamics preset.
func Parse(v string) (Spec, error) {
	lower := strings.ToLower(v)
	switch lower {
	case "off":
		return Spec{Mode: Off}, nil
	case "track":
		return Spec{Mode: Track}, nil
	case "album":
		return Spec{Mode: Album}, nil
	}
	db, err := strconv.ParseFloat(strings.TrimSuffix(lower, "db"), 64)
	if err != nil || math.IsNaN(db) || math.IsInf(db, 0) {
		return Spec{}, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("gain %q: want off, track, album, or a dB number", v))
	}
	return Spec{Mode: Fixed, DB: db}, nil
}
