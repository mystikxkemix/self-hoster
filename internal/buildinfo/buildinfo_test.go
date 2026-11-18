package buildinfo

import "testing"

func TestFormat(t *testing.T) {
	tests := map[string]string{
		"": "dev",
		"ef98d2d0a1b2c3d4e5f60718293a4b5c6d7e8f90": "ef98d2d",
		"ef98d2d-dirty": "ef98d2d-dirty",
		"v1.2.0":        "v1.2.0",
	}
	for in, want := range tests {
		if got := format(in); got != want {
			t.Errorf("format(%q) = %q, want %q", in, got, want)
		}
	}
}
