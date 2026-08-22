package provision

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
)

// safeIdent matches a plain lowercase Postgres identifier: a letter or
// underscore followed by letters, digits or underscores, up to Postgres's
// 63-byte NAMEDATALEN limit. Role and database names flow into DDL that cannot
// be parameterised (CREATE ROLE / CREATE DATABASE), so we constrain names to
// this shape and reject anything else rather than trying to escape it (SPEC-09).
var safeIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// quoteIdent validates a Postgres identifier against safeIdent and returns it
// double-quoted for inclusion in DDL. It fails closed: an identifier that is not
// a plain lowercase name is rejected, not escaped, so no crafted name can break
// out of the DDL statement.
func quoteIdent(name string) (string, error) {
	if !safeIdent.MatchString(name) {
		return "", fmt.Errorf("unsafe identifier %q", name)
	}
	return `"` + name + `"`, nil
}

// slugSanitize lowercases a tenant slug and replaces every run of characters
// that are not [a-z0-9] with a single underscore, trimming leading/trailing
// underscores, so an arbitrary display-ish slug becomes a safe identifier stem.
var nonIdentRun = regexp.MustCompile(`[^a-z0-9]+`)

func slugSanitize(slug string) string {
	s := strings.ToLower(slug)
	s = nonIdentRun.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// deriveNames builds the tenant's database and role names from its slug and a
// short unique suffix (typically the first 8 hex chars of its UUID). The suffix
// keeps names unique even if two tenants share a sanitised slug.
func deriveNames(slug, suffix string) (dbName, role string) {
	stem := slugSanitize(slug)
	if stem == "" {
		stem = "tenant"
	}
	dbName = "tenant_" + stem + "_" + suffix
	role = "role_" + stem + "_" + suffix
	return dbName, role
}

// passwordAlphabet is the URL/DSN-safe printable set used for generated tenant
// passwords: no quote, double-quote or backslash, so the value is safe both in a
// CREATE ROLE ... PASSWORD literal (escaped by the driver) and a URL-escaped DSN.
const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"

// passwordLen is the generated password length; 32 chars over a 65-symbol
// alphabet is ~190 bits of entropy.
const passwordLen = 32

// generatePassword returns a cryptographically random password drawn from
// passwordAlphabet. It reads from crypto/rand and fails closed on a rand error.
func generatePassword() (string, error) {
	buf := make([]byte, passwordLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	out := make([]byte, passwordLen)
	for i, b := range buf {
		out[i] = passwordAlphabet[int(b)%len(passwordAlphabet)]
	}
	return string(out), nil
}
