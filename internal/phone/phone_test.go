package phone

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"+1 (555) 123-4567", "+15551234567"},
		{"+15551234567", "+15551234567"},
		{"1-555-123-4567", "15551234567"},
		{"555.123.4567", "5551234567"},
		{"+44 20 7946 0958", "+442079460958"},
		{"+1 (555) 123-4567 ext. 9", "+155512345679"},
		{"  +15551234567  ", "+15551234567"},
		{"", ""},
		{"   ", ""},
		{"+", "+"},
		{"++1555", "+1555"},
		{"abc123", "123"},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
