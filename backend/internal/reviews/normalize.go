// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Normalization undoes the tricks people use to slip words past filters, while
// keeping a map from every normalized character back to the original text, so a
// match is always reported (and masked, and quoted) as an exact span of what the
// shopper wrote. The original is never changed.
//
//  1. drop zero-width and formatting characters and combining accents
//  2. NFKC folding for the compatibility ranges seen in reviews: fullwidth forms,
//     mathematical bold/italic letters, circled letters, ligatures, super/subscripts,
//     accented Latin letters (the standard library has no NFKC; this is a tested subset)
//  3. look-alike letters (Cyrillic, Greek, small capitals) -> Latin, then lower case
//  4. l33t inside words (z0rk0id -> zorkoid; "5 stars" is left alone)
//  5. spacing tricks (z o r k o i d, z.o.r.k.o.i.d) joined, repeated letters collapsed
package reviews

import (
	"strings"
	"unicode"
)

// Token is a word in one normalized view, with the original rune span it came from.
type Token struct {
	Text       string
	Start, End int // runes of the original text
}

// Text holds the original and its normalized views.
type Text struct {
	Orig    []rune
	Norm    string  // folded, look-alikes mapped, lower case
	normMap []int   // Norm rune -> original rune index
	Words   []Token // words of the l33t view (exact spelling)
	Squeeze []Token // words after joining spaced letters and collapsing repeats
	Invis   int     // zero-width / formatting characters removed
}

var zeroWidth = map[rune]bool{0x00AD: true, 0x180E: true, 0x200B: true, 0x200C: true, 0x200D: true, 0x200E: true,
	0x200F: true, 0x2060: true, 0x2061: true, 0x2062: true, 0x2063: true, 0x2064: true, 0xFEFF: true, 0x034F: true}

var lookalike = map[rune]rune{
	// Cyrillic
	'а': 'a', 'в': 'b', 'е': 'e', 'ё': 'e', 'к': 'k', 'м': 'm', 'н': 'h', 'о': 'o', 'р': 'p', 'с': 'c', 'т': 't', 'у': 'y',
	'х': 'x', 'і': 'i', 'ї': 'i', 'ј': 'j', 'ѕ': 's', 'ԁ': 'd', 'ӏ': 'l', 'ԛ': 'q', 'ԝ': 'w', 'ɡ': 'g',
	'А': 'a', 'В': 'b', 'Е': 'e', 'К': 'k', 'М': 'm', 'Н': 'h', 'О': 'o', 'Р': 'p', 'С': 'c', 'Т': 't', 'Х': 'x', 'І': 'i', 'Ј': 'j', 'Ѕ': 's',
	// Greek
	'α': 'a', 'β': 'b', 'ε': 'e', 'ι': 'i', 'κ': 'k', 'ν': 'v', 'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u', 'χ': 'x', 'ω': 'w',
	'Α': 'a', 'Β': 'b', 'Ε': 'e', 'Ζ': 'z', 'Η': 'h', 'Ι': 'i', 'Κ': 'k', 'Μ': 'm', 'Ν': 'n', 'Ο': 'o', 'Ρ': 'p', 'Τ': 't', 'Υ': 'y', 'Χ': 'x',
	// small capitals and other Latin look-alikes
	'ᴀ': 'a', 'ʙ': 'b', 'ᴄ': 'c', 'ᴅ': 'd', 'ᴇ': 'e', 'ꜰ': 'f', 'ɢ': 'g', 'ʜ': 'h', 'ɪ': 'i', 'ᴊ': 'j', 'ᴋ': 'k', 'ʟ': 'l',
	'ᴍ': 'm', 'ɴ': 'n', 'ᴏ': 'o', 'ᴘ': 'p', 'ʀ': 'r', 'ꜱ': 's', 'ᴛ': 't', 'ᴜ': 'u', 'ᴠ': 'v', 'ᴡ': 'w', 'ʏ': 'y', 'ᴢ': 'z',
	'ı': 'i', 'ƚ': 'l', 'ℓ': 'l',
}

// accented Latin -> base letter (the decomposition NFKC + accent stripping would give)
var accents = func() map[rune]rune {
	m := map[rune]rune{}
	for base, set := range map[rune]string{
		'a': "àáâãäåāăąǎ", 'c': "çćĉċč", 'd': "ďđ", 'e': "èéêëēĕėęě", 'g': "ĝğġģ", 'h': "ĥħ", 'i': "ìíîïĩīĭįǐ",
		'j': "ĵ", 'k': "ķ", 'l': "ĺļľŀł", 'n': "ñńņňŉ", 'o': "òóôõöøōŏőǒ", 'r': "ŕŗř", 's': "śŝşšș", 't': "ţťŧț",
		'u': "ùúûüũūŭůűųǔ", 'w': "ŵ", 'y': "ýÿŷ", 'z': "źżž",
	} {
		for _, r := range set {
			m[r] = base
			m[unicode.ToUpper(r)] = base
		}
	}
	return m
}()

var ligatures = map[rune]string{0xFB00: "ff", 0xFB01: "fi", 0xFB02: "fl", 0xFB03: "ffi", 0xFB04: "ffl", 0xFB05: "st", 0xFB06: "st",
	0x0132: "ij", 0x0133: "ij", 0x01C6: "dz", 0x00DF: "ss", 0x00E6: "ae", 0x00C6: "ae", 0x0153: "oe", 0x0152: "oe"}

var supSub = map[rune]rune{'⁰': '0', '¹': '1', '²': '2', '³': '3', '⁴': '4', '⁵': '5', '⁶': '6', '⁷': '7', '⁸': '8', '⁹': '9',
	'₀': '0', '₁': '1', '₂': '2', '₃': '3', '₄': '4', '₅': '5', '₆': '6', '₇': '7', '₈': '8', '₉': '9', 'ⁱ': 'i', 'ⁿ': 'n'}

// fold maps one rune to its compatibility form ("" drops it).
func fold(r rune) string {
	switch {
	case zeroWidth[r], r >= 0xFE00 && r <= 0xFE0F, unicode.Is(unicode.Mn, r): // invisible, variation selectors, accents
		return ""
	case r >= 0xFF01 && r <= 0xFF5E: // fullwidth ASCII
		return string(r - 0xFEE0)
	case r == 0x3000 || r == 0x00A0 || (r >= 0x2000 && r <= 0x200A) || r == 0x202F || r == 0x205F:
		return " "
	case r >= 0x1D400 && r <= 0x1D6A3: // mathematical alphanumeric letters, 52 per style
		i := (r - 0x1D400) % 52
		if i < 26 {
			return string('a' + i)
		}
		return string('a' + i - 26)
	case r >= 0x1D7CE && r <= 0x1D7FF: // mathematical digits, 10 per style
		return string('0' + (r-0x1D7CE)%10)
	case r >= 0x24B6 && r <= 0x24CF: // Ⓐ..Ⓩ
		return string('a' + r - 0x24B6)
	case r >= 0x24D0 && r <= 0x24E9: // ⓐ..ⓩ
		return string('a' + r - 0x24D0)
	case r >= 0x249C && r <= 0x24B5: // ⒜..⒵
		return string('a' + r - 0x249C)
	case r >= 0x2460 && r <= 0x2468: // ①..⑨
		return string('1' + r - 0x2460)
	case r >= 0x1F130 && r <= 0x1F149: // squared/regional letters
		return string('a' + r - 0x1F130)
	case r >= 0x1F150 && r <= 0x1F169:
		return string('a' + r - 0x1F150)
	case r >= 0x1F170 && r <= 0x1F189:
		return string('a' + r - 0x1F170)
	}
	if s, ok := ligatures[r]; ok {
		return s
	}
	if b, ok := supSub[r]; ok {
		return string(b)
	}
	if b, ok := accents[r]; ok {
		return string(b)
	}
	if b, ok := lookalike[r]; ok {
		return string(b)
	}
	return string(unicode.ToLower(r))
}

var leet = map[rune]rune{'0': 'o', '1': 'i', '3': 'e', '4': 'a', '5': 's', '7': 't', '8': 'b', '9': 'g', '@': 'a', '$': 's', '!': 'i', '|': 'l', '€': 'e', '£': 'l'}

// Normalize builds every view of s. It never fails and never edits the original.
func Normalize(s string) Text {
	t := Text{Orig: []rune(s)}
	var norm []rune
	for i, r := range t.Orig {
		f := fold(r)
		if f == "" && (zeroWidth[r] || (r >= 0xFE00 && r <= 0xFE0F)) {
			t.Invis++
		}
		for _, x := range f {
			norm = append(norm, x)
			t.normMap = append(t.normMap, i)
		}
	}
	t.Norm = string(norm)

	// words: runs of letters and digits, plus l33t symbols inside a word ("z@p", "id!ot").
	// l33t applies only inside a word that has a letter, so "5 stars" and "$50" stay put.
	type w struct {
		rs  []rune
		idx []int
	}
	var words []w
	cur := w{}
	isL := func(i int) bool { return i >= 0 && i < len(norm) && unicode.IsLetter(norm[i]) }
	flush := func() {
		if len(cur.rs) == 0 {
			return
		}
		hasLetter := false
		for _, r := range cur.rs {
			hasLetter = hasLetter || unicode.IsLetter(r)
		}
		if hasLetter {
			for k, r := range cur.rs {
				if l, ok := leet[r]; ok {
					cur.rs[k] = l
				}
			}
		}
		words = append(words, cur)
		cur = w{}
	}
	for i, r := range norm {
		in := unicode.IsLetter(r) || unicode.IsDigit(r)
		switch r {
		case '@', '$', '€', '£':
			in = isL(i-1) || isL(i+1)
		case '!', '|':
			in = isL(i-1) && isL(i+1)
		}
		if in {
			cur.rs = append(cur.rs, r)
			cur.idx = append(cur.idx, t.normMap[i])
			continue
		}
		flush()
	}
	flush()
	for _, x := range words {
		t.Words = append(t.Words, Token{string(x.rs), x.idx[0], x.idx[len(x.idx)-1] + 1})
	}

	// join runs of 3+ single-letter words split by short separators (z o r k o i d, z.o.r.k)
	for i := 0; i < len(t.Words); {
		j := i
		for j < len(t.Words) && len([]rune(t.Words[j].Text)) == 1 && (j == i || t.Words[j].Start-t.Words[j-1].End <= 3) {
			j++
		}
		if j-i >= 3 {
			var b strings.Builder
			for k := i; k < j; k++ {
				b.WriteString(t.Words[k].Text)
			}
			t.Squeeze = append(t.Squeeze, Token{collapse(b.String()), t.Words[i].Start, t.Words[j-1].End})
			i = j
			continue
		}
		t.Squeeze = append(t.Squeeze, Token{collapse(t.Words[i].Text), t.Words[i].Start, t.Words[i].End})
		i++
	}
	return t
}

// collapse squeezes repeated letters: "idiooot" -> "idiot", "zoorkkoid" -> "zorkoid".
func collapse(s string) string {
	var b strings.Builder
	var prev rune = -1
	for _, r := range s {
		if r != prev {
			b.WriteRune(r)
		}
		prev = r
	}
	return b.String()
}

// Span returns the original text of runes [start, end).
func (t Text) Span(start, end int) string {
	if start < 0 || end > len(t.Orig) || start >= end {
		return ""
	}
	return string(t.Orig[start:end])
}

// NormSpan maps a rune range of Norm back to the original text.
func (t Text) NormSpan(start, end int) (int, int) {
	if start < 0 || end > len(t.normMap) || start >= end {
		return -1, -1
	}
	return t.normMap[start], t.normMap[end-1] + 1
}
