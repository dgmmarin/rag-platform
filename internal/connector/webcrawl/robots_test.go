package webcrawl

import "testing"

const sampleRobots = `
User-agent: *
Disallow: /private
Disallow: /search
Allow: /private/public

User-agent: RAGPlatformCrawler
Disallow: /nobot
`

func TestRobotsHonoursWildcardGroup(t *testing.T) {
	// A UA with no specific group falls back to "*".
	r := parseRobots([]byte(sampleRobots), "SomeOtherBot")
	cases := map[string]bool{
		"/":               true,
		"/private":        false,
		"/private/x":      false,
		"/private/public": true, // longer Allow wins over shorter Disallow
		"/search?q=1":     false,
		"/nobot":          true, // /nobot only applies to the RAG-specific group
	}
	for path, want := range cases {
		if got := r.allowed(path); got != want {
			t.Fatalf("wildcard allowed(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestRobotsHonoursSpecificGroup(t *testing.T) {
	// Our own UA gets its specific group, which does NOT inherit the "*" rules.
	r := parseRobots([]byte(sampleRobots), "RAGPlatformCrawler")
	if r.allowed("/nobot") {
		t.Fatal("RAGPlatformCrawler group must disallow /nobot")
	}
	if !r.allowed("/private") {
		t.Fatal("specific group has no /private rule; must be allowed")
	}
}

func TestRobotsEmptyAllowsAll(t *testing.T) {
	r := parseRobots(nil, "RAGPlatformCrawler")
	if !r.allowed("/anything") {
		t.Fatal("empty robots must allow all")
	}
}

func TestRobotsCaseInsensitiveAgentMatch(t *testing.T) {
	r := parseRobots([]byte("user-agent: ragplatformcrawler\ndisallow: /x\n"), "RAGPlatformCrawler/1.0")
	if r.allowed("/x") {
		t.Fatal("agent token match must be case-insensitive and prefix-based")
	}
}
