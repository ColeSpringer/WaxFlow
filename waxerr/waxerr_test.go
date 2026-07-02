package waxerr_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

func TestErrorString(t *testing.T) {
	inner := errors.New("open failed")
	tests := []struct {
		name string
		err  *waxerr.Error
		want string
	}{
		{"msg only", waxerr.New(waxerr.CodeNotFound, "no such source"), "no such source"},
		{"msg and wrapped", waxerr.Wrap(waxerr.CodeSourceUnreadable, "reading source", inner), "reading source: open failed"},
		{"wrapped only", waxerr.Wrap(waxerr.CodeInternal, "", inner), "open failed"},
		{"bare sentinel", waxerr.ErrOverloaded, "overloaded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSentinelIs(t *testing.T) {
	err := fmt.Errorf("handler: %w", waxerr.New(waxerr.CodeNotFound, "no such source"))
	if !errors.Is(err, waxerr.ErrNotFound) {
		t.Error("errors.Is should match ErrNotFound through wrapping")
	}
	if errors.Is(err, waxerr.ErrOverloaded) {
		t.Error("errors.Is must not match a different code")
	}
}

func TestAsTypeExtraction(t *testing.T) {
	inner := waxerr.Wrap(waxerr.CodeSourceUnreadable, "reading source", fs.ErrPermission)
	err := fmt.Errorf("stream: %w", inner)

	e, ok := errors.AsType[*waxerr.Error](err)
	if !ok {
		t.Fatal("errors.AsType should find *waxerr.Error in the chain")
	}
	if e.Code != waxerr.CodeSourceUnreadable {
		t.Errorf("Code = %q, want %q", e.Code, waxerr.CodeSourceUnreadable)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Error("wrapped cause must stay visible to errors.Is")
	}
}

func TestCodeOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want waxerr.Code
	}{
		{"nil", nil, ""},
		{"boundary error", waxerr.New(waxerr.CodeOverloaded, "busy"), waxerr.CodeOverloaded},
		{"wrapped boundary error", fmt.Errorf("x: %w", waxerr.New(waxerr.CodeNotFound, "")), waxerr.CodeNotFound},
		{"context canceled", context.Canceled, waxerr.CodeCanceled},
		{"context deadline", context.DeadlineExceeded, waxerr.CodeCanceled},
		{"plain error", errors.New("boom"), waxerr.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := waxerr.CodeOf(tt.err); got != tt.want {
				t.Errorf("CodeOf() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"internal", waxerr.New(waxerr.CodeInternal, ""), 1},
		{"invalid-request", waxerr.New(waxerr.CodeInvalidRequest, ""), 2},
		{"payload-too-large", waxerr.New(waxerr.CodePayloadTooLarge, ""), 2},
		{"not-found", waxerr.New(waxerr.CodeNotFound, ""), 3},
		{"source-unreadable", waxerr.New(waxerr.CodeSourceUnreadable, ""), 4},
		{"source-changed", waxerr.New(waxerr.CodeSourceChanged, ""), 4},
		{"catalog-unavailable", waxerr.New(waxerr.CodeCatalogUnavailable, ""), 4},
		{"unsupported-format", waxerr.New(waxerr.CodeUnsupportedFormat, ""), 5},
		{"unsupported-source", waxerr.New(waxerr.CodeUnsupportedSource, ""), 5},
		{"canceled", waxerr.New(waxerr.CodeCanceled, ""), 6},
		{"context cancellation", context.Canceled, 6},
		{"unauthorized", waxerr.New(waxerr.CodeUnauthorized, ""), 7},
		{"signature-invalid", waxerr.New(waxerr.CodeSignatureInvalid, ""), 7},
		{"signature-expired", waxerr.New(waxerr.CodeSignatureExpired, ""), 7},
		{"overloaded", waxerr.New(waxerr.CodeOverloaded, ""), 8},
		{"unclassified", errors.New("boom"), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := waxerr.ExitCode(tt.err); got != tt.want {
				t.Errorf("ExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestExitContractComplete pins the invariant that every defined Code
// belongs to exactly one exit class. Adding a Code without classifying it
// fails here.
func TestExitContractComplete(t *testing.T) {
	seen := make(map[waxerr.Code]int)
	for _, class := range waxerr.ExitContract() {
		for _, c := range class.Codes {
			seen[c]++
		}
	}
	for _, c := range waxerr.Codes() {
		if seen[c] != 1 {
			t.Errorf("code %q appears in %d exit classes, want exactly 1", c, seen[c])
		}
	}
	if len(seen) != len(waxerr.Codes()) {
		t.Errorf("exit contract classifies %d codes, %d are defined", len(seen), len(waxerr.Codes()))
	}
}
