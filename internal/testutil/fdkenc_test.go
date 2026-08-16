package testutil

import "testing"

// A missing libfdk_aac must stay a skip under WAXFLOW_REQUIRE_FFMPEG=1, which
// no CI ffmpeg can satisfy. HaveFDK fatals here if it is rewired back onto
// that policy. Vacuous on a build that has libfdk_aac.
func TestFDKAbsenceIsNotAnFFmpegFailure(t *testing.T) {
	t.Setenv("WAXFLOW_REQUIRE_FFMPEG", "1")
	t.Setenv("WAXFLOW_REQUIRE_FDK", "")
	HaveFDK(t)
}
