package testutil

import "runtime"

// NonUTF8Name returns a file name this platform's file systems hold that is
// not valid UTF-8, beside its %XX escaping, or false where there is none. A
// Linux name is any bytes. A Windows name can hold a lone surrogate, which
// Go spells as three bytes. macOS refuses both.
func NonUTF8Name() (name, escaped string, ok bool) {
	switch runtime.GOOS {
	case "linux":
		return "caf\xe9.wav", "caf%E9.wav", true
	case "windows":
		return "caf\xed\xa0\x80.wav", "caf%ED%A0%80.wav", true
	}
	return "", "", false
}
