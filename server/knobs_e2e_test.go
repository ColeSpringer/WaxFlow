// The knob net. TestGoldenKnobAnswers prints what each surface answers when an
// encoder knob rides beside each output format: the status and, for a refusal,
// its error code. No message is in the file: the wording has its own tests,
// and a reworded sentence should not move a line here.
//
// The rule it buys: an answer moves only in a change that says so, and this
// file's diff is that change's behaviour, whole.
package server_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/internal/hls"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/waxerr"
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
		for _, knob := range []string{"", "bitrate=128", "hev2=true", "flacLevel=5", "wavpackLevel=3", "apeLevel=3000", "vorbisQuality=4"} {
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

	testutil.GoldenText(t, testutil.PkgFile("testdata", "golden", "knob-answers.txt"), []byte(out.String()), *update,
		"what a surface answers to an encoder knob moved.\n"+
			"If the change says it moves these answers, run `make goldens` and review the diff line by line.")
}

// TestVorbisBitrateIsRefused: vorbis is quality-driven and takes no bit rate,
// so a bitrate on /stream points to q and one on a job to vorbisQuality.
func TestVorbisBitrateIsRefused(t *testing.T) {
	env := jobsEnv(t)
	wantRefusal(t, env.get(t, "/stream?src=lib/sine.wav&format=vorbis&bitrate=128", nil),
		http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat,
		"bitrate does not apply to vorbis, which is quality-driven; use q=low, med, or high")
	wantRefusal(t, env.postJSON(t, "/jobs", `{"type":"transcode","src":"lib/sine.wav","format":"vorbis","bitrate":128}`),
		http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat,
		"bitrate does not apply to vorbis, which is quality-driven; use vorbisQuality")
}

// TestStreamVorbisQuality: q selects a Vorbis quality on vorbis, so q=low and
// q=high both play, key apart and high is the larger body, and a job's
// vorbisQuality 6 writes a larger file than its 2. TestVorbisBitrateIsRefused
// holds the bit rate's refusal.
func TestStreamVorbisQuality(t *testing.T) {
	env := jobsEnv(t)
	log := &keyLog{}
	env.srv.OnKey(log.add)
	size := map[string]int{}
	canonical := map[string]string{}
	for _, q := range []string{"low", "high"} {
		log.take()
		resp := env.get(t, "/stream?src=lib/ramp.wav&format=vorbis&q="+q, nil)
		body := readBody(t, resp)
		keys := log.take()
		if resp.StatusCode != http.StatusOK || len(keys) != 1 {
			t.Errorf("q=%s = %d with %d keys, want 200 with 1: %s", q, resp.StatusCode, len(keys), body)
			continue
		}
		size[q], canonical[q] = len(body), keys[0].canonical
	}
	if len(size) == 2 {
		if canonical["low"] == canonical["high"] {
			t.Errorf("q=low and q=high share the canonical string %s", canonical["low"])
		}
		if size["high"] <= size["low"] {
			t.Errorf("q=high is %d bytes and q=low %d, want high the larger", size["high"], size["low"])
		}
	}
	written := map[float64]int{}
	for _, quality := range []float64{2, 6} {
		id := createJob(t, env, fmt.Sprintf(`{"type":"transcode","src":"lib/ramp.wav","format":"vorbis","vorbisQuality":%g}`, quality))
		awaitJob(t, env, id)
		written[quality] = len(readBody(t, env.get(t, "/jobs/"+id+"/result", nil)))
	}
	if written[6] <= written[2] {
		t.Errorf("vorbisQuality 6 wrote %d bytes and 2 wrote %d, want 6 the larger", written[6], written[2])
	}
}

// TestJobLevelOffItsFormatIsRefused: a compression level for another format is
// refused at a job's creation, in the job's own field name, rather than
// accepted and ignored.
func TestJobLevelOffItsFormatIsRefused(t *testing.T) {
	env := jobsEnv(t)
	for _, tc := range []struct{ field, want string }{
		{`"flacLevel":5`, "flacLevel applies to flac output; this request names opus"},
		{`"wavpackLevel":3`, "wavpackLevel applies to wavpack output; this request names opus"},
		{`"apeLevel":3000`, "apeLevel applies to ape output; this request names opus"},
	} {
		wantRefusal(t, env.postJSON(t, "/jobs", `{"type":"transcode","src":"lib/sine.wav","format":"opus",`+tc.field+`}`),
			http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat, tc.want)
	}
}

// TestJobsRefuseAMissingFormatBeforeTheKnobs: a job with no explicit format
// is told so, ahead of any knob or container the options carry.
func TestJobsRefuseAMissingFormatBeforeTheKnobs(t *testing.T) {
	env := jobsEnv(t)
	for _, format := range []string{"", `,"format":"auto"`} {
		wantRefusal(t, env.postJSON(t, "/jobs", `{"type":"transcode","src":"lib/sine.wav","hev2":true,"container":"webm"`+format+`}`),
			http.StatusBadRequest, waxerr.CodeInvalidRequest, "transcode jobs need an explicit format")
	}
}

// TestHLSBitrateRefusalNamesWhatHLSServes: an HLS mint words a bit rate on a
// lossless format in both of its spellings and names only the lossy formats
// HLS can serve.
func TestHLSBitrateRefusalNamesWhatHLSServes(t *testing.T) {
	env := newTestEnv(t, nil)
	wantRefusal(t, env.get(t, "/hls/master.m3u8?src=lib/sine.wav&format=flac&bitrates=64,96", nil),
		http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat,
		"bitrate/bitrates apply to lossy output (opus, aac, he-aac); flac is lossless")
}

// TestStreamSignRefusesWhatTheQueryDecides: signing a /stream URL answers what
// a HEAD of it would. What the query alone decides is refused before the
// source resolves, so a missing source cannot hide it; the rest is refused
// after, and a URL that signs plays.
func TestStreamSignRefusesWhatTheQueryDecides(t *testing.T) {
	env := newTestEnv(t, nil)
	sign := func(src, params string) *http.Response {
		return env.postJSON(t, "/sign", `{"path":"/stream","params":{"src":"`+src+`"`+params+`}}`)
	}
	for _, src := range []string{"lib/nosuch.wav", "lib/sine.wav", "lib/seed.webm"} {
		for _, c := range []struct{ params, want string }{
			{`,"format":"nosuch"`, `unsupported output format "nosuch"`},
			{`,"format":"flac","bitrate":"128"`, "bitrate applies to lossy output"},
			{`,"format":"aac","hev2":"1"`, "hev2 applies to he-aac output"},
			{`,"bitrate":"128"`, "bitrate applies to lossy output"},
			{`,"format":"auto","hev2":"1"`, "hev2 applies to he-aac output"},
			{`,"format":"mka","bitrate":"128"`, `unsupported output format "mka"`},
		} {
			wantRefusal(t, sign(src, c.params), http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat, c.want)
		}
		// A container no form takes is refused ahead of a q the format cannot use.
		wantRefusal(t, sign(src, `,"format":"flac","container":"nosuch","q":"high"`),
			http.StatusBadRequest, waxerr.CodeInvalidRequest, `flac container "nosuch": want mka or ogg`)
	}
	// A source container's own name waits for the source: on a WAV it cannot
	// direct-play and is refused, on a WebM it signs and plays.
	wantEnvelope(t, sign("lib/nosuch.wav", `,"format":"auto"`), http.StatusNotFound, waxerr.CodeNotFound)
	wantEnvelope(t, sign("lib/nosuch.wav", `,"format":"ogg"`), http.StatusNotFound, waxerr.CodeNotFound)
	wantRefusal(t, sign("lib/sine.wav", `,"format":"ogg"`),
		http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat, `unsupported output format "ogg"`)
	signAndPlay := func(what, src, params string) *url.URL {
		t.Helper()
		resp := sign(src, params)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d (%s), want it signed", what, resp.StatusCode, body)
			return nil
		}
		var signed struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(body, &signed); err != nil || signed.URL == "" {
			t.Fatalf("a signed answer with no url: %s", body)
		}
		play, err := http.Get(env.ts.URL + signed.URL)
		if err != nil {
			t.Fatal(err)
		}
		if got := readBody(t, play); play.StatusCode != http.StatusOK {
			t.Errorf("the signed URL of %s plays %d: %s", what, play.StatusCode, got)
		}
		u, err := url.Parse(signed.URL)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	signAndPlay("format=mka on a WebM", "lib/seed.webm", `,"format":"mka"`)
	// The sign pins the source's current id, so an id the query carries, a
	// stale one from a re-signed URL or a malformed one, is not checked.
	if other := signAndPlay("flac", "lib/album/track.flac", `,"format":"flac"`); other != nil {
		stale := other.Query().Get("id")
		signAndPlay("a stale id", "lib/sine.wav", `,"format":"wav","id":"`+stale+`"`)
	}
	signAndPlay("a malformed id", "lib/sine.wav", `,"format":"wav","id":"junk"`)
}

// TestStreamAndMintParseAlike: the shaping parameters /stream and the HLS mint
// both take are refused in one code and one sentence on each, whichever
// surface reads them.
func TestStreamAndMintParseAlike(t *testing.T) {
	env := newTestEnv(t, nil)
	answer := func(path string) string {
		t.Helper()
		resp := env.get(t, path, nil)
		body := readBody(t, resp)
		var e server.ErrorBody
		if err := json.Unmarshal(body, &e); err != nil || e.Code == "" {
			return fmt.Sprintf("%d %s", resp.StatusCode, body)
		}
		return fmt.Sprintf("%d %s %s", resp.StatusCode, e.Code, e.Error)
	}
	over := fmt.Sprint(waxflow.MaxBitrate/1000 + 1)
	for _, query := range []string{
		"format=nosuch",
		"rate=", "rate=abc", "rate=0", "rate=-1",
		"ch=", "ch=abc", "ch=0",
		"bits=", "bits=abc", "bits=0", "bits=20",
		"bitrate=", "bitrate=abc", "bitrate=0", "bitrate=" + over,
		"hev2=", "hev2=maybe",
		"rate=&ch=abc", "bits=20&hev2=", "bitrate=" + over + "&hev2=",
	} {
		stream := answer("/stream?src=lib/sine.wav&" + query)
		mint := answer("/hls/master.m3u8?src=lib/sine.wav&" + query)
		if stream != mint || !strings.HasPrefix(stream, "4") {
			t.Errorf("%s:\n/stream answers %s\nthe mint answers %s", query, stream, mint)
		}
	}
}

// TestMintRefusesWhatADescriptorCannotCarry: q, container and flacLevel have
// no place in an HLS descriptor, so the mint refuses each as an unknown
// parameter rather than minting a URL that drops it.
func TestMintRefusesWhatADescriptorCannotCarry(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, param := range []string{"q=high", "container=adts", "flacLevel=5"} {
		name, _, _ := strings.Cut(param, "=")
		wantRefusal(t, env.get(t, "/hls/master.m3u8?src=lib/sine.wav&format=aac&"+param, nil),
			http.StatusBadRequest, waxerr.CodeInvalidRequest, fmt.Sprintf("unknown parameter %q", name))
	}
}

// TestStreamContainerIsRefusedBeforeQ: a container the format has no form in
// is refused ahead of a q the format cannot use, as it is ahead of a bitrate.
func TestStreamContainerIsRefusedBeforeQ(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, c := range []struct{ query, want string }{
		{"format=flac&container=nosuch", `flac container "nosuch": want mka or ogg`},
		{"format=flac&container=webm", `flac container "webm": want mka or ogg`},
	} {
		for _, knob := range []string{"q=high", "bitrate=128"} {
			wantRefusal(t, env.get(t, "/stream?src=lib/sine.wav&"+c.query+"&"+knob, nil),
				http.StatusBadRequest, waxerr.CodeInvalidRequest, c.want)
		}
	}
}

// TestQOnADirectPlayableSourceIsNotIgnored: q on a source that would
// otherwise direct-play asks for an encode, so it is refused where the format
// takes neither a bit rate nor a quality and never answered with the original
// bytes.
func TestQOnADirectPlayableSourceIsNotIgnored(t *testing.T) {
	env := newTestEnv(t, nil)
	wantRefusal(t, env.get(t, "/stream?src=lib/album/track.flac&format=flac&q=high", nil),
		http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat,
		"q applies to lossy output (opus, vorbis, mp3, aac, he-aac); flac is lossless")
}

// TestKbpsIsBoundedBeforeItIsMultiplied: every kbit/s value a request names is
// refused above waxflow.MaxBitrate/1000, in its surface's words, before it is
// multiplied to bits per second. wraps is a value whose product wraps a 64-bit
// int to 128000, a rate the client never sent.
func TestKbpsIsBoundedBeforeItIsMultiplied(t *testing.T) {
	env := jobsEnv(t)
	over := fmt.Sprint(waxflow.MaxBitrate/1000 + 1)
	const wraps = "2305843009213694080"
	bound := "want at most 2147483 kbit/s"

	// A v= descriptor reaches the multiply without the mint's parse.
	v := hlsDescriptorFor(t, env, map[string]string{"src": "lib/sine.wav", "format": "opus", "bitrate": "128"})
	tampered := func(kbps string) string {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			t.Fatal(err)
		}
		swapped := bytes.Replace(raw, []byte(`"bitrate":128`), []byte(`"bitrate":`+kbps), 1)
		if bytes.Equal(swapped, raw) {
			t.Fatal("nothing swapped; fixture drifted")
		}
		return base64.RawURLEncoding.EncodeToString(swapped)
	}
	job := func(kbps string) *http.Response {
		return env.postJSON(t, "/jobs", `{"type":"transcode","src":"lib/sine.wav","format":"opus","bitrate":`+kbps+`}`)
	}
	for _, c := range []struct {
		name string
		do   func(kbps string) *http.Response
		want string
	}{
		{"stream bitrate", func(k string) *http.Response {
			return env.get(t, "/stream?src=lib/sine.wav&format=opus&bitrate="+k, nil)
		}, "bitrate " + over + ": " + bound},
		{"stream maxBitRate", func(k string) *http.Response {
			return env.get(t, "/stream?src=lib/sine.wav&format=opus&maxBitRate="+k, nil)
		}, "maxBitRate " + over + ": " + bound},
		{"mint bitrate", func(k string) *http.Response {
			return env.get(t, "/hls/master.m3u8?src=lib/sine.wav&format=opus&bitrate="+k, nil)
		}, "bitrate " + over + ": " + bound},
		{"mint bitrates", func(k string) *http.Response {
			return env.get(t, "/hls/master.m3u8?src=lib/sine.wav&format=opus&bitrates=64,"+k, nil)
		}, "ladder bitrate " + over + ": " + bound},
		{"descriptor", func(k string) *http.Response {
			return env.get(t, "/hls/media.m3u8?v="+tampered(k), nil)
		}, "bitrate " + over + ": " + bound},
		{"job bitrate", job, "bitrate " + over + ": " + bound},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantRefusal(t, c.do(over), http.StatusBadRequest, waxerr.CodeInvalidRequest, c.want)
			// A 32-bit build cannot parse the value at all, which is a 400 too.
			wantEnvelope(t, c.do(wraps), http.StatusBadRequest, waxerr.CodeInvalidRequest)
		})
	}
}

// TestHLSMaxKbpsIsTheEngines: the descriptor's kbit/s bound is the engine's
// MaxBitrate over 1000.
func TestHLSMaxKbpsIsTheEngines(t *testing.T) {
	if hls.MaxKbps != waxflow.MaxBitrate/1000 {
		t.Errorf("hls.MaxKbps = %d, want waxflow.MaxBitrate/1000 = %d", hls.MaxKbps, waxflow.MaxBitrate/1000)
	}
}

// TestHLSLadderRefusesANonPositiveRung: a bitrates entry that is not positive
// is refused in the ladder's words, ahead of the engine's options check.
func TestHLSLadderRefusesANonPositiveRung(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, rung := range []string{"-5", "0"} {
		wantRefusal(t, env.get(t, "/hls/master.m3u8?src=lib/sine.wav&format=opus&bitrates=64,"+rung, nil),
			http.StatusBadRequest, waxerr.CodeInvalidRequest, "ladder bitrate "+rung+" is not positive")
	}
}

// TestJobsRefuseTheRequestBeforeTheSource: what a job's request alone decides
// is refused before any source resolves or is measured, so a missing source or
// member cannot hide a knob the format does not take.
func TestJobsRefuseTheRequestBeforeTheSource(t *testing.T) {
	env := jobsEnv(t)
	const want = "flacLevel applies to flac output; this request names opus"
	for _, c := range []struct{ name, body string }{
		{"transcode", `{"type":"transcode","src":"lib/missing.wav","format":"opus","flacLevel":5}`},
		{"split", `{"type":"split","src":"lib/missing.wav","cuts":[4800],"format":"opus","flacLevel":5}`},
		{"merge", `{"type":"merge","srcs":["lib/sine.wav","lib/missing.wav"],"format":"opus","flacLevel":5}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantRefusal(t, env.postJSON(t, "/jobs", c.body), http.StatusUnsupportedMediaType, waxerr.CodeUnsupportedFormat, want)
		})
	}
	// A range is worded in the job's field names, on the engine's scale.
	for _, c := range []struct{ name, body, want string }{
		{"a flac level out of range", `{"type":"transcode","src":"lib/missing.wav","format":"flac","flacLevel":9}`,
			"flacLevel 9: want -1 to 8"},
		{"a wavpack level out of range", `{"type":"split","src":"lib/missing.wav","cuts":[4800],"format":"wavpack","wavpackLevel":7}`,
			"wavpackLevel 7: want 1 to 4"},
		{"an ape level not written", `{"type":"merge","srcs":["lib/sine.wav","lib/missing.wav"],"format":"ape","apeLevel":4000}`,
			"apeLevel 4000 decodes but is not written; want 1000, 2000, or 3000"},
		{"an ape level off its set", `{"type":"transcode","src":"lib/missing.wav","format":"ape","apeLevel":2500}`,
			"apeLevel 2500: want 1000, 2000, or 3000"},
		{"a vorbis quality out of range", `{"type":"transcode","src":"lib/missing.wav","format":"vorbis","vorbisQuality":50}`,
			"vorbisQuality 50: want -1 to 10"},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantRefusal(t, env.postJSON(t, "/jobs", c.body), http.StatusBadRequest, waxerr.CodeInvalidRequest, c.want)
		})
	}
	t.Run("analyze", func(t *testing.T) {
		wantRefusal(t, env.postJSON(t, "/jobs", `{"type":"analyze","src":"lib/missing.wav","format":"opus"}`),
			http.StatusBadRequest, waxerr.CodeInvalidRequest, "type analyze takes src and the silence fields")
	})
}
