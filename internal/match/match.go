// Package match turns loosely-spelled artist and title strings into a key that
// survives the ways the same song gets written down in different places.
package match

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Key identifies a song across sources: it folds case, punctuation, spacing and
// diacritics, and drops the parenthetical suffixes labels keep adding.
func Key(artist, title string) string {
	return Normalize(artist) + "\x00" + Normalize(StripSuffix(title))
}

// Normalize keeps only letters and digits, lowercased and without accents, so
// "Sigur Rós" and "sigur ros" are the same artist — which they are.
func Normalize(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		if unicode.Is(unicode.Mn, r) {
			continue // combining accent left over by the decomposition
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// StripSuffix drops "(Remastered 2011)", "- Live", "[Bonus Track]" and friends.
func StripSuffix(title string) string {
	for _, cut := range []string{" (", " [", " - "} {
		if i := strings.Index(title, cut); i > 0 {
			title = title[:i]
		}
	}
	return title
}
