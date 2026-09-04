package webcrawl

import "testing"

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase scheme and host, drop default port and fragment", "HTTP://Example.COM:80/Path?b=2&a=1#frag", "http://example.com/Path?a=1&b=2"},
		{"drop https default port", "https://Example.com:443/x", "https://example.com/x"},
		{"empty path becomes root", "https://example.com", "https://example.com/"},
		{"sort query params by key", "https://example.com/p?z=1&a=2&m=3", "https://example.com/p?a=2&m=3&z=1"},
		{"strip utm_* tracking params", "https://example.com/p?utm_source=g&utm_medium=cpc&id=5", "https://example.com/p?id=5"},
		{"strip known trackers, keep real params", "https://example.com/p?gclid=abc&fbclid=def&page=2", "https://example.com/p?page=2"},
		{"preserve path case, keep non-default port", "https://Example.com:8443/Deep/Path", "https://example.com:8443/Deep/Path"},
		{"no query stays clean", "https://example.com/a/b/", "https://example.com/a/b/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := parseAndNormalize(tc.in)
			if err != nil {
				t.Fatalf("parseAndNormalize(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseAndNormalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeURLStableForEquivalent(t *testing.T) {
	a, _, err := parseAndNormalize("https://Example.com/p?b=2&a=1&utm_source=x#top")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := parseAndNormalize("https://example.com/p?a=1&b=2")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("equivalent URLs normalised differently: %q vs %q", a, b)
	}
}

func TestNormalizeRejectsNonHTTP(t *testing.T) {
	for _, in := range []string{"mailto:a@b.com", "javascript:void(0)", "ftp://x.com/f", "://bad"} {
		if _, _, err := parseAndNormalize(in); err == nil {
			t.Fatalf("parseAndNormalize(%q) = nil error, want rejection", in)
		}
	}
}
