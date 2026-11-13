package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// TestUploadBoundsTheAnswer: the send of an upload has no time limit, and
// the answer does. A daemon, or a proxy in front of one, that sends its
// headers and then stalls must not hold a caller whose ctx has no deadline.
func TestUploadBoundsTheAnswer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.(http.Flusher).Flush()
		<-release // the body never comes
	}))
	defer srv.Close()
	defer close(release)

	c, err := New(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	c.bound = 50 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := c.Upload(context.Background(), "in.wav", strings.NewReader("RIFFdata"))
		done <- err
	}()
	select {
	case err := <-done:
		if waxerr.CodeOf(err) != waxerr.CodeCanceled || !strings.Contains(err.Error(), "deadline") {
			t.Errorf("an answer that stalls after its headers = %v, want a canceled deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Upload is still waiting on a body that never comes; nothing bounds the answer")
	}
}

// TestUploadSendIsNotBounded: the bound starts when the headers arrive, so
// a body that takes longer than it to send still uploads.
func TestUploadSendIsNotBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"schemaVersion":1,"id":"01J","ref":"upload:01J","bytes":`+strconv.FormatInt(n, 10)+`}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	c.bound = 50 * time.Millisecond
	up, err := c.Upload(context.Background(), "", &slowReader{chunks: 4, wait: 60 * time.Millisecond})
	if err != nil || up.Bytes != 4 {
		t.Fatalf("a send four times the bound long = %+v, %v; want it uploaded", up, err)
	}

	// A caller's own deadline replaces the bound, as for every JSON call.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Upload(ctx, "", strings.NewReader("x")); err != nil {
		t.Errorf("an upload under the caller's deadline: %v", err)
	}
}

// slowReader yields one byte per read, waiting before each.
type slowReader struct {
	chunks int
	wait   time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.chunks == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.wait)
	r.chunks--
	p[0] = 'x'
	return 1, nil
}
