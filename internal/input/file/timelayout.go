package file

import (
	"errors"
	"strings"
	"time"
)

// TimeLayout is a timestamp layout written with the tokens YYYY, MM, DD, hh,
// mm and ss and literal separators, translated to a Go reference layout.
type TimeLayout struct {
	goLayout string
}

// layoutTokens maps the tokens to the Go reference layout elements.
var layoutTokens = []struct{ token, goElem string }{
	{"YYYY", "2006"},
	{"MM", "01"},
	{"DD", "02"},
	{"hh", "15"},
	{"mm", "04"},
	{"ss", "05"},
}

var errLayout = errors.New("invalid time layout")

// ParseTimeLayout translates a token layout. A run of letters must be made of
// tokens only; the date tokens YYYY, MM and DD are required, each token may
// appear once, and the literal text between tokens holds no letter or digit.
func ParseTimeLayout(s string) (TimeLayout, error) {
	var out strings.Builder
	seen := make(map[string]bool)
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isLetter(c):
			tok, elem, ok := matchToken(s[i:])
			if !ok || seen[tok] {
				return TimeLayout{}, errLayout
			}
			seen[tok] = true
			out.WriteString(elem)
			i += len(tok)
		case c >= '0' && c <= '9', c >= 0x80:
			return TimeLayout{}, errLayout
		default:
			out.WriteByte(c)
			i++
		}
	}
	if !seen["YYYY"] || !seen["MM"] || !seen["DD"] {
		return TimeLayout{}, errLayout
	}
	return TimeLayout{goLayout: out.String()}, nil
}

func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func matchToken(s string) (token, goElem string, ok bool) {
	for _, t := range layoutTokens {
		if strings.HasPrefix(s, t.token) {
			return t.token, t.goElem, true
		}
	}
	return "", "", false
}

// Parse reads s, without surrounding spaces, in loc. A value that does not
// match the layout, or that Go normalised (31.02 read as 03.03), is not ok.
func (l TimeLayout) Parse(s string, loc *time.Location) (time.Time, bool) {
	if l.goLayout == "" {
		return time.Time{}, false
	}
	s = strings.TrimSpace(s)
	t, err := time.ParseInLocation(l.goLayout, s, loc)
	if err != nil || t.Format(l.goLayout) != s {
		return time.Time{}, false
	}
	return t.UTC(), true
}
