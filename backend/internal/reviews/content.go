// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Deterministic content rules. Every finding carries the exact original span it
// came from, so masking, quoting and highlighting never drift from what the
// shopper wrote. Context matters: harsh words about a product, about yourself,
// or reported from someone else ("the courier called me an idiot") are allowed.

package reviews

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"asksauby/internal/model"
)

// Content is everything the content rules found in one review.
type Content struct {
	T          Text
	Signals    []model.Signal
	Language   string // en | hinglish | other
	Injection  bool
	Unclear    bool
	UnclearWhy string
	Target     string   // self | other | product (deterministic estimate)
	Reported   [][2]int // original spans of quoted or reported speech
}

// Placeholder lexicons. The slur stand-ins are made-up words that mean nothing.
var (
	slurWords    = []string{"zorkoid", "blorfin", "snargle"}
	personInsult = []string{"idiot", "moron", "imbecile", "loser", "jerk", "fool", "dimwit", "nitwit", "bewakoof", "ullu", "gadha", "nalayak"}
	anyInsult    = []string{"stupid", "dumb"} // also used about products ("stupid design"): only a violation aimed at a person
	personRefs   = toSet("anyone", "everyone", "whoever", "somebody", "someone", "seller", "sellers", "courier", "couriers", "delivery", "guy", "boy", "man", "woman", "staff", "agent", "agents", "support",
		"owner", "he", "she", "they", "you", "u", "ur", "your", "him", "her", "them", "reviewer", "reviewers", "people", "team", "executive",
		"rider", "driver", "employee", "employees", "salesman", "bhai", "banda", "wala", "log")
	productRefs = toSet("product", "item", "it", "this", "thing", "design", "battery", "charger", "quality", "build", "app", "phone", "headphones",
		"earbuds", "laptop", "watch", "shoes", "shoe", "bag", "backpack", "monitor", "machine", "device", "cable", "strap", "screen", "sound",
		"software", "firmware", "manual", "box", "packaging", "price", "maker", "pump", "lid", "button", "buttons", "fit", "sizing")
	selfRefs    = toSet("i", "im", "me", "myself", "mujhe", "main")
	nameNeutral = toSet("company", "business", "brand", "store", "shop", "person", "people", "man", "guy", "lady", "woman", "team", "professional",
		"gem", "star", "legend", "pro", "hero", "champ", "saint", "angel", "seller", "reseller", "retailer", "vendor", "partner", "legit",
		"real", "authorized", "authorised", "official", "lifesaver", "delight", "pleasure", "bit", "little", "few", "lot", "student", "kid",
		"customer", "fan", "regular", "joke", "mess", "disaster", "nightmare", "letdown", "disappointment", "pain", "while", "day", "week", "month", "year", "minute", "hour", "dealer", "distributor", "member", "friend")
	hinglishStrong = toSet("bakwas", "bekaar", "bekar", "bahut", "accha", "achha", "acha", "nahi", "nahin", "ekdum", "bilkul", "paisa", "paise",
		"kharab", "mast", "zabardast", "yaar", "kya", "hai", "hain", "tha", "thi", "karo", "kiya", "diya", "liya", "bewakoof", "ullu", "gadha",
		"nalayak", "wala", "wali", "sahi", "galat", "jaldi", "bhai", "bilkul", "lekin", "aur", "mein", "bhi", "raha", "rahi", "gaya", "gayi")
	// common function words of languages we don't check yet (Spanish, French, German, Portuguese, Italian)
	foreignCommon = toSet("el", "la", "los", "las", "muy", "es", "y", "que", "con", "para", "por", "una", "del", "pero", "nada", "bien",
		"malo", "bueno", "producto", "les", "et", "est", "très", "tres", "des", "une", "pas", "du", "avec", "pour", "produit", "der", "das",
		"und", "ist", "nicht", "sehr", "ein", "eine", "mit", "não", "nao", "muito", "com", "che", "molto", "non", "funciona", "todo", "rapide",
		"livraison", "gut", "schnell", "você", "obrigado", "também", "molto", "buono", "prodotto", "товар", "всем", "очень")
	englishCommon = toSet("the", "a", "an", "is", "are", "was", "were", "it", "this", "that", "and", "but", "of", "to", "for", "in", "with",
		"my", "i", "very", "not", "have", "has", "after", "use", "works", "good", "great", "bad", "would", "will", "be", "at", "on", "so",
		"too", "really", "love", "quality", "battery", "product", "price", "they", "you", "seller", "delivery", "arrived", "just", "only")
)

var (
	rePhone    = regexp.MustCompile(`\+?\d(?:[\s\-.()]{0,2}\d){9,14}`)
	reEmail    = regexp.MustCompile(`[a-z0-9._%+\-]+@[a-z0-9\-]+(?:\.[a-z0-9\-]+)*\.[a-z]{2,}`)
	reEmailOb  = regexp.MustCompile(`[a-z0-9._%+\-]+\s*(?:\(at\)|\[at\]|\{at\}| at )\s*[a-z0-9\-]+\s*(?:\(dot\)|\[dot\]|\{dot\}| dot |\.)\s*(?:com|in|net|org|co|io)\b`)
	reURL      = regexp.MustCompile(`(?:https?://|www\.)[^\s<>"']+`)
	reDomain   = regexp.MustCompile(`\b[a-z0-9][a-z0-9\-]*(?:\.[a-z0-9\-]+)*\.(?:com|net|org|in|io|co|ly|xyz|shop|store|test|info|biz|link|site|online|me|app|gg|tv|to|cc|club|live)(?:/[^\s<>"']*)?`)
	reThreat   = regexp.MustCompile(`\b(i know (where|which \w+|when) (you|u|ur|your|he|she|they|his|her|their)\b[^.!?]{0,40}\b(live|lives|work|works|leave|leaves|is|are)|i know where (you|u|he|she|they) li?ve?s?|watch (your|ur) back|(better )?watch out,? (seller|courier|you|u)|(seller|courier|you|u),? (better|should) watch out|(i'?ll|i ll|i will|gonna|going to|i'?m going to|i m going to) (find|hurt|beat|get|kill|break|burn|stab|shoot|smash|come and \w+)( (you|u|him|her|them|his|their|it|the \w+))?|(you|u)('?ll| will) regret (this|it)|(waiting|wait) (outside|for (him|her|you))[^.!?]{0,40}(make|pay)|make (him|her|you|them) pay|dekh lunga|dekh lenge|haddi(yan)? tod)\b`)
	reInject   = regexp.MustCompile(`(ignore|disregard|forget|override) (all |any |the |your |these |those )*(previous|prior|above|earlier|preceding|system|moderation)? ?(instructions|rules|prompts?|guidelines|policy)|system prompt|^\s*(system|assistant|developer)\s*:|\n\s*(system|assistant|developer)\s*:|you are now|developer mode|jailbreak|(mark|label|classify|rate|tag) (this|the|my) (review )?(as )?(safe|clean|approved|ok|allowed|5 ?stars?|genuine)|(approve|publish) (all|this|every|my) (5 ?star )?reviews?|new instructions|</?(system|assistant|instructions?|review)>|review>>>|<<<review|pichle instructions|instructions ignore karo|as an ai`)
	reReported = regexp.MustCompile(`\b(called|calling|call|named|abused|labelled|labeled) (me|us|my \w+|our \w+)\b|\b(said|told|tells|says|saying|shouted|yelled|messaged|texted|wrote) (to )?(me|us)?\s*(that )?(i|we|i'm|im) (was|were|am|are|'m|is)?\b|\b(said|told|tells|says|saying|shouted|yelled)\s*:?\s*$`)
	reQuoted   = regexp.MustCompile(`"[^"]{1,200}"|“[^”]{1,200}”|‘[^’]{1,200}’|'[^']{2,200}'`)
	reNaming   = regexp.MustCompile(`\b(the )?(seller|sellers|courier|delivery (guy|boy|man|person|agent)|staff|support( agent)?|agent|owner|you|u|he|she|they|this seller|this guy|anyone who [^.!?]{0,40})\s*(is|are|was|were|'s|'re|r|is such|was such)\s+(a|an|such a|such an|total|complete|absolute|real|utter)(\s+\w+){1,3}`)
	reDehuman  = regexp.MustCompile(`\b(subhumans?|vermin|cockroaches|parasites)\b|\b(all|those|these) (\w+ )?(people|\w+s) (are|should be) (all )?(thieves|animals|trash|filth|scum|dirty|criminals|thrown out|kicked out|banned)|thrown out of (this|the|our) country|go back to (your|their) (own )?country|keep (them|those people) away from (our|my)`)
	reControl  = regexp.MustCompile(`"(action|labels|counted|status|rule_id|decision|verdict)"\s*:|\bdecode (and|&|then) (follow|run|obey|execute)|ignora (todas )?(las )?instrucciones|instrucciones anteriores|###\s*(instruction|system)|पिछले निर्देश`)
	reLongUse  = regexp.MustCompile(`\b(using|used|had|owned|wearing|worn|tested|been using|have had|i've had|after|for|since)\b[^.!?]{0,20}\b(\d+|a few|few|several|many|two|three|four|six)\s+(weeks|months|years)\b|\blong[- ]term\b|\b(months|years) of (use|daily use|wear)\b|\b(mahine|saal) se\b`)
)

// AnalyzeContent runs every deterministic content rule over one review.
func AnalyzeContent(text, photoText string, p *Policy) Content {
	c := Content{T: Normalize(text)}
	norm := c.T.Norm
	add := func(name, strength, detail string, start, end int) {
		r := p.Rules[0]
		for _, x := range p.Rules {
			if contains(x.Triggers, name) {
				r = x
			}
		}
		if !contains(r.Triggers, name) {
			r = Rule{}
		}
		for _, g := range c.Signals {
			if g.Name == name && g.Start == start && g.End == end {
				return // same finding from two patterns
			}
		}
		c.Signals = append(c.Signals, model.Signal{Name: name, Rule: r.ID, Kind: "content", Strength: strength, Detail: detail, Start: start, End: end})
	}
	// spans found by a regex on the normalized text, mapped back to the original
	normSpans := func(re *regexp.Regexp) [][2]int {
		var out [][2]int
		for _, m := range re.FindAllStringIndex(norm, -1) {
			a, b := utf8.RuneCountInString(norm[:m[0]]), utf8.RuneCountInString(norm[:m[1]])
			if s, e := c.T.NormSpan(a, b); s >= 0 {
				out = append(out, [2]int{s, e})
			}
		}
		return out
	}
	// quoted / reported speech: words inside are someone else's
	for _, sp := range normSpans(reQuoted) {
		c.Reported = append(c.Reported, sp)
	}
	for _, sp := range normSpans(reReported) {
		end := sp[1]
		for k, n := end, 0; k < len(c.T.Orig) && n < 5; k++ { // the next few words are what was said
			if c.T.Orig[k] == ' ' {
				n++
			}
			if strings.ContainsRune(".!?;\n", c.T.Orig[k]) {
				break
			}
			end = k + 1
		}
		c.Reported = append(c.Reported, [2]int{sp[0], end})
	}

	for _, sp := range normSpans(rePhone) {
		if codeLike(c.T, sp) {
			continue // "Order OD4021938475", "Part number 7741-0098-22": identifiers, not phone numbers
		}
		add("contact_phone", "strong", "phone number", sp[0], sp[1])
	}
	for _, sp := range spokenNumbers(c.T) {
		add("contact_phone", "strong", "phone number spelled out in words", sp[0], sp[1])
	}
	emails := append(normSpans(reEmail), normSpans(reEmailOb)...)
	for _, sp := range emails {
		add("contact_email", "strong", "email address", sp[0], sp[1])
	}
	for _, sp := range append(normSpans(reURL), normSpans(reDomain)...) {
		s := strings.ToLower(c.T.Span(sp[0], sp[1]))
		if strings.Contains(s, "myflipkart.com") || overlapsAny(sp, emails) {
			continue
		}
		add("link", "strong", "link to another site", sp[0], sp[1])
	}
	threat := false
	threatSpans := normSpans(reThreat)
	if len(threatSpans) == 0 { // l33t spellings: k1ll, w@tch, l1ve
		var ws []string
		for _, w := range c.T.Words {
			ws = append(ws, strings.ToLower(w.Text))
		}
		if reThreat.MatchString(strings.Join(ws, " ")) && len(c.T.Words) > 0 {
			threatSpans = [][2]int{{c.T.Words[0].Start, c.T.Words[len(c.T.Words)-1].End}}
		}
	}
	for _, sp := range threatSpans {
		if !inside(sp, c.Reported) {
			threat = true
			add("threat", "strong", "threat against a person", sp[0], sp[1])
		}
	}
	inj := append(normSpans(reInject), normSpans(reControl)...)
	if len(inj) == 0 { // spaced or dotted letters: "I.G.N.O.R.E previous instructions"
		var ws []string
		for _, w := range c.T.Squeeze {
			ws = append(ws, w.Text)
		}
		if reInject.MatchString(strings.Join(ws, " ")) {
			inj = append(inj, [2]int{0, len(c.T.Orig)})
		}
	}
	if photoText != "" && reInject.MatchString(Normalize(photoText).Norm) {
		inj = append(inj, [2]int{-1, -1})
	}
	for _, sp := range inj {
		c.Injection = true
		d := "tries to instruct the moderation system"
		if sp[0] < 0 {
			d = "photo text tries to instruct the moderation system"
		}
		add("injection", "strong", d, sp[0], sp[1])
	}
	if photoText != "" {
		pt := Normalize(photoText).Norm
		if rePhone.MatchString(pt) || reEmail.MatchString(pt) || reEmailOb.MatchString(pt) {
			add("contact_phone", "strong", "contact details on the attached photo", -1, -1)
		}
	}

	// lexicon matches on every view: exact words, l33t and squeezed spellings
	selfOrReported, person := false, false
	seen := map[[2]int]bool{}
	for _, view := range [][]Token{c.T.Words, c.T.Squeeze} {
		for _, tok := range view {
			sp := [2]int{tok.Start, tok.End}
			word := strings.ToLower(tok.Text)
			kind := ""
			switch {
			case matchWord(word, slurWords):
				kind = "slur"
			case matchWord(word, personInsult):
				kind = "insult"
			case matchWord(word, anyInsult):
				kind = "insult_any"
			}
			if kind == "" || seen[sp] {
				continue
			}
			if compound(c.T, sp) { // "idiot-proof", "stupid-fast": part of another word
				continue
			}
			seen[sp] = true
			obf := c.T.Span(sp[0], sp[1])
			disguised := !strings.EqualFold(obf, tok.Text) && !strings.EqualFold(strings.TrimRight(obf, "s"), strings.TrimRight(tok.Text, "s"))
			if inside(sp, c.Reported) {
				selfOrReported = true
				c.Signals = append(c.Signals, model.Signal{Name: "reported_speech", Kind: "context", Strength: "info",
					Detail: "insult quoted or reported, not the author's own words", Start: sp[0], End: sp[1]})
				continue
			}
			tgt := targetOf(c.T, sp)
			if kind == "slur" {
				d := "placeholder slur"
				if disguised {
					d += " (disguised spelling)"
				}
				add("slur", "strong", d, sp[0], sp[1])
				person = true
				continue
			}
			if kind == "insult" && (tgt == "" || (tgt == "product" && genericRef(c.T, sp))) {
				tgt = "person" // words like idiot or moron describe people, whatever "this" or "it" is nearby
			}
			switch tgt {
			case "self":
				selfOrReported = true
			case "person":
				d := "insult aimed at a person"
				if disguised {
					d += " (disguised spelling)"
				}
				add("insult", "strong", d, sp[0], sp[1])
				person = true
			case "product":
			default:
				if kind == "insult" { // a person-only insult with no clear target
					c.Unclear, c.UnclearWhy = true, "insult with no clear target"
				}
			}
		}
	}
	// "the seller is a total clown": name-calling with a word we don't know
	if !person {
		for _, sp := range normSpans(reNaming) {
			words := strings.Fields(strings.ToLower(c.T.Span(sp[0], sp[1])))
			for k, w := range words { // judge only what comes after "is a" / "was a total"
				if w == "is" || w == "are" || w == "was" || w == "were" || w == "r" || strings.HasSuffix(w, "'s") {
					words = words[k+1:]
					break
				}
			}
			neutral := false
			for _, w := range words {
				neutral = neutral || nameNeutral[strings.Trim(w, ",.!?")]
			}
			if !neutral && !inside(sp, c.Reported) {
				c.Unclear, c.UnclearWhy = true, "name-calling the rules can't judge"
				c.Signals = append(c.Signals, model.Signal{Name: "name_calling", Kind: "context", Strength: "info",
					Detail: "describes a person with a word the rules don't know", Start: sp[0], End: sp[1]})
			}
		}
	}
	dh := normSpans(reDehuman)
	if len(dh) == 0 { // spaced letters: "all v e x m o r r s are trash"
		var ws []string
		for _, w := range c.T.Squeeze {
			ws = append(ws, w.Text)
		}
		if reDehuman.MatchString(strings.Join(ws, " ")) && len(c.T.Orig) > 0 {
			dh = [][2]int{{0, len(c.T.Orig)}}
		}
	}
	for _, sp := range dh {
		if !inside(sp, c.Reported) {
			c.Unclear, c.UnclearWhy = true, "dehumanizing language the rules can't judge"
			c.Signals = append(c.Signals, model.Signal{Name: "dehumanizing", Kind: "context", Strength: "info",
				Detail: "dehumanizing or exclusionary language about a group", Start: sp[0], End: sp[1]})
		}
	}
	c.Language = language(c.T)
	if c.Language == "other" {
		add("language", "medium", "language our checks don't cover yet", -1, -1)
	}
	if c.T.Invis > 0 {
		c.Signals = append(c.Signals, model.Signal{Name: "invisible_chars", Kind: "context", Strength: "info",
			Detail: "zero-width or formatting characters removed before checking", Start: -1, End: -1})
	}
	switch {
	case person || threat:
		c.Target = "other"
	case selfOrReported:
		c.Target = "self"
	default:
		c.Target = "product"
	}
	return c
}

// ClaimsLongUse reports whether a review describes weeks/months/years of use.
func ClaimsLongUse(text string) bool { return reLongUse.MatchString(Normalize(text).Norm) }

// codeLike: a digit run that is part of an identifier (letters glued to it) or
// follows "part/order/serial/model/sku number".
func codeLike(t Text, sp [2]int) bool {
	if sp[0] > 0 && unicode.IsLetter(t.Orig[sp[0]-1]) {
		return true
	}
	before := strings.ToLower(t.Span(max(0, sp[0]-24), sp[0]))
	for _, k := range []string{"part number", "part no", "order", "serial", "model", "sku", "invoice", "tracking", "awb", "imei"} {
		if strings.Contains(before, k) {
			return true
		}
	}
	return false
}

// numberWords spell digits in English and Hindi; 8+ in a row is a phone number written out.
var numberWords = toSet("zero", "oh", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
	"shunya", "sunya", "ek", "do", "teen", "char", "chaar", "paanch", "panch", "chhe", "che", "chheh", "saat", "aath", "nau")

func spokenNumbers(t Text) [][2]int {
	var out [][2]int
	run, start := 0, 0
	flush := func(end int) {
		if run >= 8 {
			out = append(out, [2]int{t.Words[start].Start, t.Words[end].End})
		}
		run = 0
	}
	for i, w := range t.Words {
		lw := strings.ToLower(w.Text)
		if numberWords[lw] || (len(lw) == 1 && lw[0] >= '0' && lw[0] <= '9') {
			if run == 0 {
				start = i
			}
			run++
			continue
		}
		if run > 0 {
			flush(i - 1)
		}
	}
	if run > 0 {
		flush(len(t.Words) - 1)
	}
	return out
}

// genericRef: the nearest product reference before the insult is a pronoun ("this", "it").
func genericRef(t Text, sp [2]int) bool {
	for k := len(t.Words) - 1; k >= 0; k-- {
		if t.Words[k].End <= sp[0] {
			for j := k; j >= 0 && j >= k-7; j-- {
				if w := strings.ToLower(t.Words[j].Text); productRefs[w] {
					return w == "this" || w == "it"
				}
			}
			return false
		}
	}
	return false
}

// compound reports whether a word is joined by a hyphen to another word.
func compound(t Text, sp [2]int) bool {
	if sp[1]-sp[0] < 2 {
		return false
	}
	isL := func(i int) bool { return i >= 0 && i < len(t.Orig) && unicode.IsLetter(t.Orig[i]) }
	return (sp[1] < len(t.Orig) && t.Orig[sp[1]] == '-' && isL(sp[1]+1)) || (sp[0] > 0 && t.Orig[sp[0]-1] == '-' && isL(sp[0]-2))
}

func matchWord(w string, lex []string) bool {
	if len(w) > 4 && (w[0] == 'a' || w[0] == 'i') && matchWord1(w[1:], lex) { // "a z o r k o i d" joined with its article
		return true
	}
	return matchWord1(w, lex)
}

func matchWord1(w string, lex []string) bool {
	for _, x := range lex {
		cx := collapse(x)
		if w == x || w == cx || w == x+"s" || w == cx+"s" || w == x+"es" {
			return true
		}
	}
	return false
}

// targetOf decides who an insult at original span sp is about: the nearest
// reference word before it in the same clause ("the seller is an idiot", "what a
// stupid design"), else the word right after it ("idiot seller").
func targetOf(t Text, sp [2]int) string {
	ws := t.Words // exact spellings, so "seller" is still "seller"
	i := len(ws)
	for k, w := range ws {
		if w.Start >= sp[0] {
			i = k
			break
		}
	}
	for k := i - 1; k >= 0 && k >= i-7; k-- {
		w := strings.ToLower(ws[k].Text)
		next := sp[0]
		if k+1 < len(ws) && ws[k+1].Start < sp[0] {
			next = ws[k+1].Start
		}
		if gap := t.Span(ws[k].End, next); strings.ContainsAny(gap, ".!?;\n") || w == "but" || w == "lekin" {
			break // the reference must be in the same clause
		}
		switch {
		case selfRefs[w]:
			return "self"
		case personRefs[w]:
			return "person"
		case productRefs[w]:
			return "product"
		}
	}
	for _, w := range ws {
		if w.Start >= sp[1] {
			lw := strings.ToLower(w.Text)
			if personRefs[lw] {
				return "person"
			} else if productRefs[lw] {
				return "product"
			}
			break
		}
	}
	return ""
}

func language(t Text) string {
	latin, other := 0, 0
	for _, r := range t.Orig {
		// judge the script after compatibility folding (fullwidth, math letters, small
		// caps are Latin) but before look-alike folding, which would hide real Cyrillic/Greek
		if f := []rune(fold(r)); len(f) > 0 && !(r >= 0x370 && r <= 0x52F) {
			r = f[0]
		}
		if unicode.IsLetter(r) {
			if r < 0x250 {
				latin++
			} else {
				other++
			}
		}
	}
	if latin+other > 0 && float64(other)/float64(latin+other) > 0.3 {
		return "other"
	}
	strong, eng, foreign := 0, 0, 0
	for _, w := range t.Words {
		lw := strings.ToLower(w.Text)
		if hinglishStrong[lw] {
			strong++
		}
		if englishCommon[lw] {
			eng++
		}
		if foreignCommon[lw] {
			foreign++
		}
	}
	if strong >= 2 || (strong == 1 && len(t.Words) <= 5) {
		return "hinglish"
	}
	if (foreign >= 2 && foreign > eng) || (len(t.Words) >= 4 && foreign >= 1 && eng == 0 && strong == 0) {
		return "other"
	}
	return "en"
}

func inside(sp [2]int, spans [][2]int) bool {
	for _, s := range spans {
		if sp[0] >= s[0] && sp[1] <= s[1] {
			return true
		}
	}
	return false
}

func overlapsAny(sp [2]int, spans [][2]int) bool {
	for _, s := range spans {
		if sp[0] < s[1] && s[0] < sp[1] {
			return true
		}
	}
	return false
}

func toSet(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// Mask replaces contact details and links in the original with a fixed marker (by code, never by a model).
func Mask(t Text, sigs []model.Signal) string {
	type sp struct {
		a, b int
		s    string
	}
	var spans []sp
	for _, g := range sigs {
		if g.Start < 0 {
			continue
		}
		switch g.Name {
		case "contact_phone", "contact_email":
			spans = append(spans, sp{g.Start, g.End, "[contact removed]"})
		case "link":
			spans = append(spans, sp{g.Start, g.End, "[link removed]"})
		}
	}
	if len(spans) == 0 {
		return string(t.Orig)
	}
	var b strings.Builder
	pos := 0
	for pos < len(t.Orig) {
		hit := -1
		for i, s := range spans {
			if s.a == pos && (hit < 0 || s.b > spans[hit].b) {
				hit = i
			}
		}
		if hit >= 0 {
			b.WriteString(spans[hit].s)
			end := spans[hit].b
			for _, s := range spans { // merge overlaps
				if s.a < end && s.b > end {
					end = s.b
				}
			}
			pos = end
			continue
		}
		b.WriteRune(t.Orig[pos])
		pos++
	}
	return b.String()
}

// Shingles are 3-word sequences of the normalized words (near-duplicate checks).
func Shingles(t Text) map[string]bool {
	var ws []string
	for _, w := range t.Words {
		ws = append(ws, strings.ToLower(w.Text))
	}
	out := map[string]bool{}
	for i := 0; i+3 <= len(ws); i++ {
		out[strings.Join(ws[i:i+3], " ")] = true
	}
	return out
}

// Jaccard similarity of two shingle sets.
func Jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	in := 0
	for k := range a {
		if b[k] {
			in++
		}
	}
	return float64(in) / float64(len(a)+len(b)-in)
}
