package main

import "testing"

func TestNormalizeIRCRFC1459(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello", "hello"},
		{"ABCxyz", "abcxyz"},
		{"NiCk123", "nick123"},
		{"[Test]", "{test}"},
		{`Back\Slash`, "back|slash"},
		{"Tilde~", "tilde^"},
		{"Already lower", "already lower"},
		{"", ""},
		{"123!@#$%", "123!@#$%"},
		{"{}|^", "{}|^"},
	}
	for _, tt := range tests {
		got := normalizeIRC(tt.input, "rfc1459")
		if got != tt.want {
			t.Errorf("normalizeIRC(%q, rfc1459) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeIRCStrictRFC1459(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello", "hello"},
		{"[Test]", "{test}"},
		{`Back\Slash`, "back|slash"},
		{"Tilde~", "tilde~"},
		{"", ""},
	}
	for _, tt := range tests {
		got := normalizeIRC(tt.input, "strict-rfc1459")
		if got != tt.want {
			t.Errorf("normalizeIRC(%q, strict-rfc1459) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeIRCASCII(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello", "hello"},
		{"ABCxyz", "abcxyz"},
		{"[Test]", "[test]"},
		{`Back\Slash`, `back\slash`},
		{"Tilde~", "tilde~"},
		{"", ""},
	}
	for _, tt := range tests {
		got := normalizeIRC(tt.input, "ascii")
		if got != tt.want {
			t.Errorf("normalizeIRC(%q, ascii) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeIRCDefault(t *testing.T) {
	input := "Hello~[Test]"
	want := normalizeIRC(input, "rfc1459")

	got := normalizeIRC(input, "")
	if got != want {
		t.Errorf("normalizeIRC(%q, '') = %q, want %q (rfc1459 default)", input, got, want)
	}

	got = normalizeIRC(input, "unknown")
	if got != want {
		t.Errorf("normalizeIRC(%q, 'unknown') = %q, want %q (rfc1459 default)", input, got, want)
	}
}

func TestNormalizeIRCCaseInsensitive(t *testing.T) {
	casemapping := "rfc1459"
	a := normalizeIRC("TestNick", casemapping)
	b := normalizeIRC("testnick", casemapping)
	c := normalizeIRC("TESTNICK", casemapping)
	if a != b || b != c {
		t.Errorf("expected all normalized forms equal, got %q %q %q", a, b, c)
	}
}

func TestNormalizeIRCChannel(t *testing.T) {
	got := normalizeIRC("#TestChannel", "rfc1459")
	want := "#testchannel"
	if got != want {
		t.Errorf("normalizeIRC(%q, rfc1459) = %q, want %q", "#TestChannel", got, want)
	}
}
