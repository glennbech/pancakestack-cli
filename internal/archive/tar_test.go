package archive

import "testing"

func TestIsJunkFile(t *testing.T) {
	cases := []struct {
		name string
		junk bool
	}{
		// macOS AppleDouble junk — always drop. Extension doesn't matter:
		// the backend strips the same names, so letting them through the
		// CLI just moves the silent drop to the server side.
		{".DS_Store", true},
		{"._Icon", true},
		{"._preferences", true},
		{"._readme.txt", true},
		{"._lights.fit", true},
		{"._M31.fits", true},
		{"._stack.fz", true},

		// Real user data — never filter.
		{"lights.fit", false},
		{"M31.fits", false},
		{"final.fz", false},
		{"random.txt", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isJunkFile(tc.name)
			if got != tc.junk {
				t.Fatalf("isJunkFile(%q) = %v, want %v", tc.name, got, tc.junk)
			}
		})
	}
}

func TestIsJunkDir(t *testing.T) {
	cases := []struct {
		name string
		junk bool
	}{
		{"__MACOSX", true},
		{"lights", false},
		{"", false},
		{".DS_Store", false}, // that's a file, not a directory
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isJunkDir(tc.name)
			if got != tc.junk {
				t.Fatalf("isJunkDir(%q) = %v, want %v", tc.name, got, tc.junk)
			}
		})
	}
}
