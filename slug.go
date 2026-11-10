package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// Slugs are the public identifiers for the session-history website
// (docs/superpowers/specs/2026-11-10-session-website-design.md): 8 random
// base62 chars, ~2e14 space, minted once, never rotated. Uniqueness is
// enforced by a unique index; the space is large enough that collisions
// are a non-event (and callers error out rather than retry-loop).
const (
	slugLen  = 8
	slugChar = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// generateSlug draws slugLen unbiased random chars (math/big Int, not
// byte%len — that is modulo-biased). Ported from markdown-parsebin's
// util.GenerateSlug.
func generateSlug() (string, error) {
	max := big.NewInt(int64(len(slugChar)))
	b := make([]byte, slugLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate slug: %w", err)
		}
		b[i] = slugChar[n.Int64()]
	}
	return string(b), nil
}

// validSlug reports whether s is a well-formed slug. Used to reject
// malformed lookups before they touch the DB.
func validSlug(s string) bool {
	if len(s) != slugLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}
