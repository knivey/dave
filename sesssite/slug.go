package main

// Mirror of dave's validSlug (root slug.go, same repo). sesssite only
// ever VALIDATES slugs — dave is the single writer and mints them — so
// the minting half (generateSlug) deliberately does not come along.
// Keep the two bodies in sync when the format changes.

const slugLen = 8

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
