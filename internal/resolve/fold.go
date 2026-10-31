package resolve

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var foldReplacer = strings.NewReplacer("đ", "d", "Đ", "d", "ø", "o", "Ø", "o", "ł", "l", "Ł", "l", "ß", "ss", "æ", "ae", "œ", "oe")

// Fold lowercases s and strips accents, for case- and accent-insensitive
// matching ("Mẹ" -> "me", "Đà Lạt" -> "da lat").
func Fold(s string) string {
	s = foldReplacer.Replace(s)
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		out = s
	}
	return strings.ToLower(out)
}

// MatchAll reports whether every whitespace-separated term of folded query q
// appears in folded text.
func MatchAll(foldedText string, terms []string) bool {
	for _, t := range terms {
		if !strings.Contains(foldedText, t) {
			return false
		}
	}
	return len(terms) > 0
}

// Terms splits and folds a query.
func Terms(q string) []string { return strings.Fields(Fold(q)) }
