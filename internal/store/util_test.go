package store

import "testing"

func TestLastPathComponent(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"unix":               {"/home/u/proj", "proj"},
		"trailing slash":     {"/home/u/proj/", "proj"},
		"windows":            {`C:\Users\u\proj`, "proj"},
		"windows trailing":   {`C:\Users\u\proj\`, "proj"},
		"mixed separators":   {`/home/u\proj`, "proj"},
		"no separator":       {"proj", "proj"},
		"root":               {"/", ""},
		"empty":              {"", ""},
		"dot dir":            {"/home/u/.config", ".config"},
		"placeholder cwd":    {"(unknown)", "(unknown)"},
		"dashes not touched": {"/home/u/agent-session-butler", "agent-session-butler"},
	} {
		if got := lastPathComponent(tc.in); got != tc.want {
			t.Errorf("%s: lastPathComponent(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// Binary units, like a file manager — the boundary cases are what a size column
// gets wrong in practice.
func TestHumanSize(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024*1024 - 1, "1024.0 KiB"}, // rounds up in unit but stays in KiB
		{1024 * 1024 * 1024, "1.0 GiB"},
		{1610612736, "1.5 GiB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TiB"},
		{1 << 50, "1.0 PiB"},
		{1 << 60, "1.0 EiB"},
	} {
		if got := HumanSize(tc.in); got != tc.want {
			t.Errorf("HumanSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
