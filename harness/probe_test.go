package harness

import "testing"

func TestVersionToken(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.318 (Claude Code)\n": "2.1.318",
		"codex-cli 0.164.0\n":     "0.164.0",
		"tool v1.2.3-beta.1":      "1.2.3-beta.1",
		"no version here":         "",
		"":                        "",
	} {
		if got := VersionToken(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestProbeWithoutLookPathFindsNothing(t *testing.T) {
	for _, r := range ProbeAll(t.Context(), nil, nil) {
		if r.Found || r.Version != "" || r.SignedIn != nil {
			t.Fatalf("%+v", r)
		}
	}
}
