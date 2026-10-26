// Package pattern turns raw Redis keys into templates like "cart:{num}:items".
package pattern

// Placeholder tokens. Kept as constants so the trie and report can match on them.
const (
	Num   = "{num}"
	UUID  = "{uuid}"
	Hex   = "{hex}"
	TS    = "{ts}"
	Date  = "{date}"
	Email = "{email}"
	IP    = "{ip}"
	Tag   = "{tag}"
)

// isSep reports whether c separates key segments. '-' and '_' are NOT
// separators here: they appear inside UUIDs and dates and are handled per segment.
func isSep(c byte) bool {
	return c == ':' || c == '/' || c == '|' || c == '#' || c == ','
}

// text is anything we can classify without converting: the raw key bytes on
// the hot path, or a string in tests and callers that already have one.
type text interface{ ~string | ~[]byte }

// Normalize converts a key to its template. Separators are preserved, so
// "a:1/b" becomes "a:{num}/b".
func Normalize(key []byte) string {
	return string(AppendNormalized(nil, key))
}

// AppendNormalized appends key's template to dst and returns the extended
// slice. Passing the previous result back as dst[:0] reuses its memory, so
// normalizing millions of keys doesn't allocate per key.
func AppendNormalized(dst, key []byte) []byte {
	start := 0
	for i := 0; i <= len(key); i++ {
		if i == len(key) || isSep(key[i]) {
			dst = appendSegment(dst, key[start:i])
			if i < len(key) {
				dst = append(dst, key[i])
			}
			start = i + 1
		}
	}
	return dst
}

// Segments splits a template on separators, keeping each separator attached
// to the end of the segment before it, so joining them gives the template back.
func Segments(tmpl string) []string {
	var out []string
	start := 0
	for i := 0; i < len(tmpl); i++ {
		if isSep(tmpl[i]) {
			out = append(out, tmpl[start:i+1])
			start = i + 1
		}
	}
	return append(out, tmpl[start:])
}

// appendSegment appends the template for one segment: the whole segment
// classified, or failing that, the part after the last '-' or '_'
// (e.g. "sku-42" -> "sku-{num}", "user_ab12cd34ef" -> "user_{hex}").
func appendSegment(dst, s []byte) []byte {
	if t, ok := classify(s); ok {
		return append(dst, t...)
	}
	if i := lastDashOrUnderscore(s); i > 0 && i < len(s)-1 {
		if t, ok := classify(s[i+1:]); ok {
			dst = appendSegment(dst, s[:i])
			return append(append(dst, s[i]), t...)
		}
	}
	return append(dst, s...)
}

// Classify returns the placeholder for s if s is a recognized variable token.
func Classify(s string) (string, bool) { return classify(s) }

// classify checks more specific shapes before more general ones.
func classify[T text](s T) (string, bool) {
	switch {
	case len(s) == 0:
		return "", false
	case isTag(s):
		return Tag, true
	case isUUID(s):
		return UUID, true
	case isEmail(s):
		return Email, true
	case isIPv4(s):
		return IP, true
	case isDate(s):
		return Date, true
	case isTimestamp(s):
		return TS, true
	case isNum(s):
		return Num, true
	case isHex(s):
		return Hex, true
	}
	return "", false
}

// Byte-search helpers: the strings/bytes versions take one concrete type.

func indexByte[T text](s T, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func lastIndexByte[T text](s T, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func lastDashOrUnderscore(s []byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '-' || s[i] == '_' {
			return i
		}
	}
	return -1
}

func containsAny[T text](s T, chars string) bool {
	for i := 0; i < len(s); i++ {
		if indexByte(chars, s[i]) >= 0 {
			return true
		}
	}
	return false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexChar(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func allDigits[T text](s T) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// atoi for short, already-validated digit strings.
func atoi[T text](s T) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// isNum: optional leading '-', then digits.
func isNum[T text](s T) bool {
	if s[0] == '-' {
		s = s[1:]
	}
	return allDigits(s)
}

// isTag: Redis Cluster hash tag "{...}" with non-empty content.
func isTag[T text](s T) bool {
	return len(s) > 2 && s[0] == '{' && s[len(s)-1] == '}' && !containsAny(s[1:len(s)-1], "{}")
}

// isUUID: 8-4-4-4-12 hex digits.
func isUUID[T text](s T) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHexChar(s[i]) {
				return false
			}
		}
	}
	return true
}

// isHex: >= 8 hex chars containing at least one digit and one letter
// (pure digits are {num}; pure letters are probably words like "deadbeef"... or "facade").
func isHex[T text](s T) bool {
	if len(s) < 8 {
		return false
	}
	var digit, letter bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isDigit(c):
			digit = true
		case isHexChar(c):
			letter = true
		default:
			return false
		}
	}
	return digit && letter
}

// isTimestamp: unix seconds (10 digits) or millis (13 digits) between 2001 and 2033.
func isTimestamp[T text](s T) bool {
	if !allDigits(s) {
		return false
	}
	switch len(s) {
	case 10:
		return s[0] == '1'
	case 13:
		return s[0] == '1'
	}
	return false
}

func validYMD(y, m, d int) bool {
	return y >= 1970 && y <= 2099 && m >= 1 && m <= 12 && d >= 1 && d <= 31
}

// isDate: YYYY-MM-DD, YYYY/MM/DD is impossible (slash is a separator), or YYYYMMDD.
func isDate[T text](s T) bool {
	switch len(s) {
	case 10:
		if s[4] != '-' || s[7] != '-' {
			return false
		}
		y, m, d := s[0:4], s[5:7], s[8:10]
		return allDigits(y) && allDigits(m) && allDigits(d) && validYMD(atoi(y), atoi(m), atoi(d))
	case 8:
		return allDigits(s) && validYMD(atoi(s[0:4]), atoi(s[4:6]), atoi(s[6:8]))
	}
	return false
}

// isEmail: something@something.something, no spaces. Deliberately loose.
func isEmail[T text](s T) bool {
	at := indexByte(s, '@')
	if at <= 0 || at != lastIndexByte(s, '@') {
		return false
	}
	dot := lastIndexByte(s, '.')
	return dot > at+1 && dot < len(s)-1 && !containsAny(s, " \t")
}

// isIPv4: four dot-separated numbers 0-255.
func isIPv4[T text](s T) bool {
	parts := 0
	for len(s) > 0 && parts < 4 {
		end := indexByte(s, '.')
		if end < 0 {
			end = len(s)
		}
		p := s[:end]
		if len(p) == 0 || len(p) > 3 || !allDigits(p) || atoi(p) > 255 {
			return false
		}
		parts++
		if end == len(s) {
			s = s[:0]
		} else {
			s = s[end+1:]
			if len(s) == 0 {
				return false // trailing dot
			}
		}
	}
	return parts == 4 && len(s) == 0
}
