package jobs

import (
	"fmt"
	"strings"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/internal/gainspec"
	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/internal/timeline"
	"github.com/colespringer/waxflow/internal/tofile"
	"github.com/colespringer/waxflow/waxerr"
)

// The policy bounds on an analyze job's silence fields. They are taste, so
// they live here rather than in the engine, which holds only what the
// detector needs (finite, negative, positive).
const (
	minSilenceThresholdDB = -90.0
	maxSilenceMinDuration = 60 * time.Second
)

// Check is CheckBody for a stored job: a cue reached it as Cuts and Skip.
func (req Request) Check() error { return req.CheckBody(nil) }

// CheckBody refuses what the request alone rules out for its type, in
// creation's words and order. Creation runs it before any source resolves,
// and a stored job runs it again before it runs. wire, when not nil, runs
// between the cuts rule and the skip rule: the rules for what a body carries
// and a request does not. What needs the daemon (a default gain) stays with
// the server.
func (req Request) CheckBody(wire func() error) error {
	bad := func(format string, args ...any) error {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	unshaped := req.Params == (outshape.Params{}) && req.Gain == "" && req.Loudness == ""
	silent := !req.Silence && req.SilenceThresholdDB == 0 && req.SilenceMinSeconds == 0
	switch req.Type {
	case TypeAnalyze, TypeTranscode, TypeMerge, TypeSplit:
	case TypeTimeline:
		if !unshaped || !silent || req.Src != "" || len(req.MemberTitles) > 0 || len(req.Cuts) > 0 || len(req.Skip) > 0 ||
			len(req.Pieces) > 0 || req.TrackTotal != 0 {
			return bad("type timeline takes srcs, spans and crossfadeSeconds")
		}
		return nil
	default:
		return nil // the runner refuses a type it does not know
	}
	// A merge names its members in srcs and every other type one source in
	// src; neither spelling is tolerated on the other.
	if req.Type == TypeMerge {
		if req.Src != "" {
			return bad("type merge takes srcs, not src")
		}
		if len(req.Srcs) == 0 {
			return bad("type merge needs srcs: at least one member")
		}
		if len(req.Srcs) > timeline.MaxMembers {
			return bad("%d members is past the %d-member bound; a merge is a play queue, not a library",
				len(req.Srcs), timeline.MaxMembers)
		}
		for i, m := range req.Srcs {
			if m == "" {
				return bad("member %d has no src", i)
			}
		}
	} else {
		if req.Src == "" {
			return bad("src is required")
		}
		if len(req.Srcs) > 0 {
			return bad("srcs applies to merge jobs")
		}
	}
	if req.Type == TypeSplit && len(req.MemberTitles) > 0 {
		return bad("a split's titles name its pieces, so a split job carries them as pieces")
	}
	if req.Type != TypeMerge && req.Type != TypeSplit && len(req.MemberTitles) > 0 {
		return bad("titles applies to merge and split jobs")
	}
	if req.Type != TypeSplit && len(req.Cuts) > 0 {
		return bad("cuts applies to split jobs")
	}
	if req.Type != TypeSplit && (len(req.Pieces) > 0 || req.TrackTotal != 0) {
		return bad("pieces and trackTotal apply to split jobs")
	}
	if wire != nil {
		if err := wire(); err != nil {
			return err
		}
	}
	if req.Type != TypeSplit && len(req.Skip) > 0 {
		return bad("skip applies to split jobs")
	}
	if req.Type != TypeAnalyze && !silent {
		return bad("the silence fields apply to analyze jobs")
	}
	// Gain and loudness answer how loud one track should be: a merge has N
	// tracks in and a split N out, so neither has one number to apply.
	if (req.Type == TypeMerge || req.Type == TypeSplit) && (req.Gain != "" || req.Loudness != "") {
		return bad("gain and loudness apply to transcode jobs; %s writes the samples it is given", req.Type)
	}
	if req.Type == TypeAnalyze {
		if !unshaped {
			return bad("type analyze takes src and the silence fields")
		}
		// A threshold with no silence:true would be silently ignored.
		if !req.Silence && (req.SilenceThresholdDB != 0 || req.SilenceMinSeconds != 0) {
			return bad("silenceThresholdDb and silenceMinSeconds need silence:true")
		}
		if sil := req.SilenceOptions(); sil != nil {
			return checkSilenceBounds(sil)
		}
		return nil
	}
	if req.Format == "" || req.Format == "auto" {
		return bad("%s jobs need an explicit format (%s)", req.Type, strings.Join(waxflow.OutputFormats(), ", "))
	}
	// The wire policy answers ahead of the loudness rules; Params.Check
	// repeats it.
	if err := req.Params.CheckWire(); err != nil {
		return err
	}
	if req.Loudness != "" && req.Loudness != "analyze" {
		return bad("loudness %q: want analyze (or omit)", req.Loudness)
	}
	if req.Loudness == "analyze" && req.Gain != "" {
		return bad("loudness analyze replaces gain; drop the gain field")
	}
	if req.Gain != "" {
		if _, err := gainspec.Parse(req.Gain); err != nil {
			return err
		}
	}
	if err := req.Params.Check(outshape.Job); err != nil {
		return err
	}
	// Titles are index-aligned to the members, so the only shapes that mean
	// anything are none and one per member. A blank entry is legal: it falls
	// through to the tag or the fallback.
	if n := len(req.MemberTitles); req.Type == TypeMerge && n != 0 && n != len(req.Srcs) {
		return bad("titles has %d entries but the merge has %d members; a title per member, or none", n, len(req.Srcs))
	}
	if req.Type == TypeSplit {
		// A body names a split's pieces in titles, a job in pieces.
		field := "pieces"
		if wire != nil {
			field = "titles"
		}
		return req.checkPieces(field)
	}
	return nil
}

// checkPieces holds a split's Pieces to one per piece or none, and the own
// tags of each piece it writes to what a tag block embeds. field names the
// list in a refusal. A split with no cuts is refused by its cut rule.
func (req Request) checkPieces(field string) error {
	bad := func(format string, args ...any) error {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	n, pieces := len(req.Pieces), len(req.Cuts)+1
	if len(req.Cuts) > 0 && n != 0 && n != pieces {
		return bad("%s has %d entries but the split has %d pieces; a title per piece, or none", field, n, pieces)
	}
	if req.TrackTotal < 0 {
		return bad("trackTotal %d: want a positive count, or 0 for the number of pieces", req.TrackTotal)
	}
	total := req.TrackTotal
	if total == 0 {
		total = pieces
	}
	for i, p := range req.Pieces {
		if p.Number < 0 {
			return bad("piece %d has track number %d; want 0 or more", i, p.Number)
		}
		if req.Skipped(i) {
			continue
		}
		if err := tofile.CheckPieceTags(req.Format, req.Container, p.Number, total, p.Title); err != nil {
			return waxerr.Annotate(fmt.Sprintf("%s[%d]", field, i), err)
		}
	}
	return nil
}

// checkSilenceBounds applies the policy bounds, failing a NaN as well as a
// value out of range.
func checkSilenceBounds(o *waxflow.SilenceOptions) error {
	if o.ThresholdDB != 0 && !(o.ThresholdDB >= minSilenceThresholdDB && o.ThresholdDB < 0) {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("silence threshold %g dBFS outside %g..0", o.ThresholdDB, minSilenceThresholdDB))
	}
	// 0 is legal and means the engine default, like the threshold's 0.
	if o.MinDuration < 0 || o.MinDuration > maxSilenceMinDuration {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("silence minimum duration %v outside 0..%v", o.MinDuration, maxSilenceMinDuration))
	}
	return nil
}
