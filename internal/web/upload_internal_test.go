package web

import "testing"

// TestUploadType: what a file is served back as is decided once, here, and the
// two fallbacks only happen for a client that said nothing -- which a browser
// never is, so a test through a request would never reach them.
func TestUploadType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		declared string
		target   string
		want     string
	}{
		{
			name: "what the browser said", declared: "image/svg+xml",
			target: "chart.svg", want: "image/svg+xml",
		},
		{
			// Wrong, but it is the only party that saw the file, and WebDAV
			// stores what a PUT declared too.
			name: "even when it is wrong", declared: "text/plain",
			target: "photo.jpg", want: "text/plain",
		},
		{name: "nothing said, but the name knows", target: "photo.jpg", want: "image/jpeg"},
		{name: "nothing said and nothing to go on", target: "data", want: "application/octet-stream"},
		{name: "an extension nobody registered", target: "notes.qqq", want: "application/octet-stream"},
	}
	for _, tt := range tests {
		if got := uploadType(tt.declared, tt.target); got != tt.want {
			t.Errorf("%s: uploadType(%q, %q) = %q, want %q", tt.name, tt.declared, tt.target, got, tt.want)
		}
	}
}

// TestUploaded is the one thing on the page that comes back from the query
// string, so it is the one thing on the page a visitor writes.
func TestUploaded(t *testing.T) {
	t.Parallel()

	tests := []struct{ raw, want string }{
		{raw: "1", want: "1 file uploaded."},
		{raw: "3", want: "3 files uploaded."},
		{raw: "0"},
		{raw: "-2"},
		{raw: ""},
		{raw: "lots"},
		{raw: "<script>alert(1)</script>"},
	}
	for _, tt := range tests {
		if got := uploaded(tt.raw); got != tt.want {
			t.Errorf("uploaded(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
