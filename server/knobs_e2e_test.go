// The knob net. TestGoldenKnobAnswers prints what each surface answers when an
// encoder knob rides beside each output format: the status and, for a refusal,
// its error code. No message is in the file: the wording has its own tests,
// and a reworded sentence should not move a line here.
//
// The rule it buys: an answer moves only in a change that says so, and this
// file's diff is that change's behaviour, whole.
package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/server"
)

// knobAnswer is a response as a line states it: the status, and the error
// code when the body is an envelope. It returns the body too.
func knobAnswer(t *testing.T, resp *http.Response) (answer string, body []byte) {
	t.Helper()
	body = readBody(t, resp)
	if resp.StatusCode < 400 {
		return fmt.Sprint(resp.StatusCode), body
	}
	var env server.ErrorBody
	if err := json.Unmarshal(body, &env); err != nil || env.Code == "" {
		t.Fatalf("a %d that is not an envelope: %s", resp.StatusCode, body)
	}
	return fmt.Sprintf("%d %s", resp.StatusCode, env.Code), body
}

func TestGoldenKnobAnswers(t *testing.T) {
	env := newTestEnv(t, func(cfg *server.Config) {
		cfg.JobsDir = filepath.Join(t.TempDir(), "jobs")
	})
	formats := waxflow.OutputFormats()
	var out strings.Builder
	line := func(surface, format, knob, answer string) {
		if knob == "" {
			knob = "bare"
		}
		fmt.Fprintf(&out, "%s | %s | %s | %s\n", surface, format, knob, answer)
	}

	// /stream by HEAD, which answers from the plan and starts no encode. A
	// HEAD carries no envelope, so a refusal is asked again by GET for its
	// code. That GET asks for one byte past the first, which a live transcode
	// answers 416 without starting, so a request that is not refused after all
	// shows as a status that differs and encodes nothing.
	for _, format := range append(formats, "auto") {
		for _, knob := range []string{"", "bitrate=128", "q=high", "hev2=1"} {
			url := "/stream?src=lib/sine.wav&format=" + format
			if knob != "" {
				url += "&" + knob
			}
			resp := env.req(t, http.MethodHead, url, nil)
			if head := resp.StatusCode; head >= 400 {
				resp.Body.Close()
				resp = env.get(t, url, map[string]string{"Range": "bytes=1-1"})
				if resp.StatusCode != head {
					t.Errorf("HEAD %s = %d and a GET of it = %d", url, head, resp.StatusCode)
				}
			}
			answer, _ := knobAnswer(t, resp)
			line("stream", format, knob, answer)
		}
	}

	// The HLS mint, in its keyed master form: every rung has to plan.
	for _, format := range formats {
		for _, knob := range []string{"", "bitrate=128", "bitrates=64,96", "hev2=1"} {
			url := "/hls/master.m3u8?src=lib/sine.wav&format=" + format
			if knob != "" {
				url += "&" + knob
			}
			answer, _ := knobAnswer(t, env.get(t, url, nil))
			line("hls", format, knob, answer)
		}
	}

	// A transcode job. The answer is the creation's, which plans. The job is
	// deleted as soon as it is accepted: its encode is no part of the answer.
	for _, format := range formats {
		for _, knob := range []string{"", "bitrate=128", "hev2=true", "flacLevel=5", "wavpackLevel=3", "apeLevel=3000"} {
			body := fmt.Sprintf(`{"type":"transcode","src":"lib/sine.wav","format":%q`, format)
			if name, value, ok := strings.Cut(knob, "="); ok {
				body += fmt.Sprintf(`,%q:%s`, name, value)
			}
			resp := env.postJSON(t, "/jobs", body+"}")
			answer, created := knobAnswer(t, resp)
			line("jobs", format, knob, answer)
			if resp.StatusCode != http.StatusCreated {
				continue
			}
			var job struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(created, &job); err != nil || job.ID == "" {
				t.Fatalf("a 201 that names no job: %s", created)
			}
			del := env.req(t, http.MethodDelete, "/jobs/"+job.ID, nil)
			if gone := readBody(t, del); del.StatusCode != http.StatusNoContent {
				t.Errorf("DELETE /jobs/%s = %d: %s", job.ID, del.StatusCode, gone)
			}
		}
	}

	testutil.GoldenText(t, filepath.Join("testdata", "golden", "knob-answers.txt"), []byte(out.String()), *update,
		"what a surface answers to an encoder knob moved.\n"+
			"If the change says it moves these answers, run `make goldens` and review the diff line by line.")
}
