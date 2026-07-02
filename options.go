package waxflow

import "log/slog"

// Option configures an Engine.
type Option func(*Engine)

// WithLogger sets the Engine's logger. Nil (and the default) discards.
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.log = l
		}
	}
}

// TranscodeOptions selects the Transcode output. It grows as encoders
// land; through M1 the only knob is the container, and output is always
// PCM in the source's sample format.
type TranscodeOptions struct {
	// Format is the output container: "wav" or "aiff".
	Format string
}

// ProbeOptions configures Engine.Probe.
type ProbeOptions struct {
	// Strict turns tolerated input damage into errors.
	Strict bool
}
