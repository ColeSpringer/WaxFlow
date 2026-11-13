package server_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/client"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/waxerr"
)

// TestClientUploadRoundTrip drives the uploads surface through the client:
// a spooled body is a source any src takes, under the reference the answer
// names, until it is deleted.
func TestClientUploadRoundTrip(t *testing.T) {
	env := newTestEnv(t, func(cfg *server.Config) {
		cfg.UploadDir = filepath.Join(t.TempDir(), "uploads")
	})
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	body, err := os.ReadFile(filepath.Join(env.root, "sine.wav"))
	if err != nil {
		t.Fatal(err)
	}

	// A name a query string has to escape, so the escaping is pinned too.
	const name = "in & out #1.wav"
	up, err := c.Upload(ctx, name, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if up.ID == "" || up.Ref != "upload:"+up.ID || up.Name != name || up.Bytes != int64(len(body)) {
		t.Fatalf("upload = %+v, want an id, its upload: reference, the name %q and %d bytes", up, name, len(body))
	}
	if up.SchemaVersion != 1 || up.ExpiresAt != 0 {
		t.Errorf("upload = %+v, want schema 1 and no expiry (this daemon sets no TTL)", up)
	}
	info, err := c.Probe(ctx, up.Ref)
	if err != nil {
		t.Fatalf("probing %s: %v", up.Ref, err)
	}
	if info.Container != "wav" {
		t.Errorf("the upload probes as %q, want wav", info.Container)
	}

	// A body with no name is still a source: the probe sniffs it.
	bare, err := c.Upload(ctx, "", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bare.Name != "" || bare.ID == up.ID {
		t.Errorf("unnamed upload = %+v, want no name and an id of its own", bare)
	}
	if _, err := c.Probe(ctx, bare.Ref); err != nil {
		t.Errorf("probing the unnamed upload: %v", err)
	}

	if err := c.DeleteUpload(ctx, up.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Probe(ctx, up.Ref); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Errorf("probing a deleted upload = %v, want not-found", err)
	}
	if err := c.DeleteUpload(ctx, up.ID); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Errorf("deleting it twice = %v, want not-found", err)
	}
	if _, err := c.Probe(ctx, bare.Ref); err != nil {
		t.Errorf("deleting one upload took another with it: %v", err)
	}

	// An empty id never reaches the daemon, where it would name no route.
	if err := c.DeleteUpload(ctx, ""); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Errorf("deleting the empty id = %v, want invalid-request", err)
	}
}

// TestClientUploadSurfacesTheDaemonsRefusal: a refused upload comes back as
// the daemon's code, not as a bare status.
func TestClientUploadSurfacesTheDaemonsRefusal(t *testing.T) {
	env := newTestEnv(t, func(cfg *server.Config) {
		cfg.UploadDir = filepath.Join(t.TempDir(), "uploads")
		cfg.UploadMaxBytes = 16
	})
	c, err := client.New(env.ts.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Upload(t.Context(), "big.wav", strings.NewReader(strings.Repeat("x", 64)))
	if waxerr.CodeOf(err) != waxerr.CodePayloadTooLarge {
		t.Errorf("an upload over the cap = %v, want payload-too-large", err)
	}
}
