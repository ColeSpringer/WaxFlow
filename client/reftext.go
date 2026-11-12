package client

import (
	"encoding/json"
	"maps"
	"net/url"

	"github.com/colespringer/waxflow/internal/reftext"
)

// A source reference in this package's types is the reference's own bytes,
// the string a file name is. JSON holds only UTF-8, so the types that carry
// one write it in the text form the daemon reads ("pct:" and %XX for a name
// that is not UTF-8, the reference itself otherwise) and read it back from
// that form. Callers never spell the form themselves.

// queryParams returns params as the daemon reads them back: a query string
// holds a reference's bytes, but the daemon decodes one that starts with
// the marker, so that one is escaped. The caller's values are not changed.
func queryParams(params url.Values) url.Values {
	src := params.Get("src")
	sent := reftext.EncodeQuery(src)
	if sent == src {
		return params
	}
	out := maps.Clone(params)
	out.Set("src", sent)
	return out
}

// MarshalJSON writes the request with its references in text form.
func (r JobRequest) MarshalJSON() ([]byte, error) {
	type wire JobRequest // the fields and tags, without this method
	w := wire(r)
	w.Src, w.Cue = reftext.Encode(r.Src), reftext.Encode(r.Cue)
	if r.Srcs != nil {
		w.Srcs = make([]string, len(r.Srcs))
		for i, ref := range r.Srcs {
			w.Srcs[i] = reftext.Encode(ref)
		}
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads a request, as a job document echoes it.
func (r *JobRequest) UnmarshalJSON(b []byte) error {
	type wire JobRequest
	var w wire
	err := json.Unmarshal(b, &w)
	if err == nil {
		w.Src, err = reftext.Decode(w.Src)
	}
	if err == nil {
		w.Cue, err = reftext.Decode(w.Cue)
	}
	for i := 0; err == nil && i < len(w.Srcs); i++ {
		w.Srcs[i], err = reftext.Decode(w.Srcs[i])
	}
	if err != nil {
		return err
	}
	*r = JobRequest(w)
	return nil
}

// MarshalJSON writes the member with its reference in text form.
func (m TimelineSrc) MarshalJSON() ([]byte, error) {
	type wire TimelineSrc
	w := wire(m)
	w.Src = reftext.Encode(m.Src)
	return json.Marshal(w)
}

// UnmarshalJSON reads a member written by MarshalJSON.
func (m *TimelineSrc) UnmarshalJSON(b []byte) error {
	type wire TimelineSrc
	var w wire
	err := json.Unmarshal(b, &w)
	if err == nil {
		w.Src, err = reftext.Decode(w.Src)
	}
	if err != nil {
		return err
	}
	*m = TimelineSrc(w)
	return nil
}

// MarshalJSON writes the request with Params' src in text form.
func (r SignRequest) MarshalJSON() ([]byte, error) {
	type wire SignRequest
	w := wire(r)
	if src, ok := r.Params["src"]; ok {
		w.Params = maps.Clone(r.Params)
		w.Params["src"] = reftext.Encode(src)
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads a request written by MarshalJSON.
func (r *SignRequest) UnmarshalJSON(b []byte) error {
	type wire SignRequest
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	if src, ok := w.Params["src"]; ok {
		ref, err := reftext.Decode(src)
		if err != nil {
			return err
		}
		w.Params["src"] = ref
	}
	*r = SignRequest(w)
	return nil
}

// MarshalJSON writes the root with its path in text form.
func (r RootStatus) MarshalJSON() ([]byte, error) {
	type wire RootStatus
	w := wire(r)
	w.Path = reftext.Encode(r.Path)
	return json.Marshal(w)
}

// UnmarshalJSON reads a root as GET /roots writes it: Path is the folder's
// own bytes afterwards.
func (r *RootStatus) UnmarshalJSON(b []byte) error {
	type wire RootStatus
	var w wire
	err := json.Unmarshal(b, &w)
	if err == nil {
		w.Path, err = reftext.Decode(w.Path)
	}
	if err != nil {
		return err
	}
	*r = RootStatus(w)
	return nil
}
