package webcrawl

import (
	"bufio"
	"bytes"
	"strings"
)

// robotsRules is the parsed, agent-specific view of a robots.txt file: the Allow
// and Disallow paths that apply to one crawler user-agent (SPEC-04 §2 "robots.txt
// fetched per host and honoured"). It is intentionally a small, well-scoped parser
// rather than a new dependency (see ADR): the crawler needs group selection, path
// prefix matching and longest-match Allow/Disallow precedence — nothing more.
type robotsRules struct {
	rules []robotsRule // ordered by parse; precedence is by match length, not order
}

type robotsRule struct {
	path  string // the path prefix; "" matches everything
	allow bool   // true = Allow, false = Disallow
}

// parseRobots parses robots.txt bytes and returns the rules for the given
// user-agent. Group selection follows the standard: the most specific matching
// User-agent group wins; a group is matched if our UA string starts with (case-
// insensitively) the group's agent token. Absent a specific match, the "*" group
// applies. An empty/absent file allows everything.
func parseRobots(data []byte, userAgent string) *robotsRules {
	ua := strings.ToLower(userAgent)

	// Collect rules per agent token. A blank line ends the current group's agent
	// header run, but agent tokens accumulate until a rule line appears (multiple
	// User-agent lines may share one rule block).
	groups := map[string][]robotsRule{}
	var currentAgents []string
	inRules := false

	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		key, val, ok := splitField(line)
		if !ok {
			continue
		}
		switch key {
		case "user-agent":
			if inRules {
				// A new group is starting; reset the agent accumulator.
				currentAgents = nil
				inRules = false
			}
			currentAgents = append(currentAgents, strings.ToLower(val))
		case "allow", "disallow":
			inRules = true
			if val == "" && key == "disallow" {
				continue // "Disallow:" with empty value means allow all — no rule
			}
			rule := robotsRule{path: val, allow: key == "allow"}
			agents := currentAgents
			if len(agents) == 0 {
				agents = []string{"*"}
			}
			for _, a := range agents {
				groups[a] = append(groups[a], rule)
			}
		}
	}

	// Select the group: the longest agent token that is a prefix of our UA, else "*".
	best := ""
	bestLen := -1
	for agent := range groups {
		if agent == "*" {
			continue
		}
		if strings.HasPrefix(ua, agent) && len(agent) > bestLen {
			best, bestLen = agent, len(agent)
		}
	}
	if bestLen < 0 {
		best = "*"
	}
	return &robotsRules{rules: groups[best]}
}

// allowed reports whether a path may be fetched. The longest matching rule wins;
// on an exact-length tie an Allow beats a Disallow (the permissive convention).
// With no matching rule, the path is allowed.
func (r *robotsRules) allowed(path string) bool {
	if path == "" {
		path = "/"
	}
	matchLen := -1
	allow := true
	for _, rule := range r.rules {
		if !strings.HasPrefix(path, rule.path) {
			continue
		}
		if len(rule.path) > matchLen || (len(rule.path) == matchLen && rule.allow) {
			matchLen = len(rule.path)
			allow = rule.allow
		}
	}
	return allow
}

// splitField splits a "Key: value" robots.txt line, lowercasing the key.
func splitField(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(line[:i])), strings.TrimSpace(line[i+1:]), true
}
