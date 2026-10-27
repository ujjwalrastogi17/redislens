package pattern

import "sort"

// Key is what the trie records about one key. The caller decides Big and Cold,
// since the thresholds are report options.
type Key struct {
	Bytes     int64
	HasTTL    bool
	TTLMS     int64 // remaining TTL at snapshot time; valid if HasTTL
	Elems     int64 // collection size when known (hashtable/skiplist encodings), else 0
	Big       bool  // over the big-key threshold
	IdleKnown bool  // the snapshot recorded LRU idle time for this key
	Cold      bool  // idle longer than the cold threshold
}

// Stats aggregates keys that share a pattern (or a prefix).
type Stats struct {
	Keys    int64
	Bytes   int64
	WithTTL int64
	// TTL remaining at snapshot time, over keys with a TTL. Valid when WithTTL > 0.
	TTLMinMS, TTLMaxMS, TTLSumMS int64
	TTLBytes                     int64 // bytes of keys that have a TTL

	MaxBytes          int64 // largest single key
	MaxElems          int64 // most elements in a single key (when known)
	BigKeys, BigBytes int64
	IdleKnown         int64 // keys with an LRU idle time
	ColdKeys          int64
	ColdBytes         int64
}

// add folds one key into s.
func (s *Stats) add(k Key) {
	s.Keys++
	s.Bytes += k.Bytes
	s.MaxBytes = max(s.MaxBytes, k.Bytes)
	s.MaxElems = max(s.MaxElems, k.Elems)
	if k.Big {
		s.BigKeys++
		s.BigBytes += k.Bytes
	}
	if k.IdleKnown {
		s.IdleKnown++
	}
	if k.Cold {
		s.ColdKeys++
		s.ColdBytes += k.Bytes
	}
	if !k.HasTTL {
		return
	}
	if s.WithTTL == 0 || k.TTLMS < s.TTLMinMS {
		s.TTLMinMS = k.TTLMS
	}
	s.TTLMaxMS = max(s.TTLMaxMS, k.TTLMS)
	s.WithTTL++
	s.TTLSumMS += k.TTLMS
	s.TTLBytes += k.Bytes
}

// Merge folds o into s.
func (s *Stats) Merge(o Stats) {
	if o.WithTTL > 0 {
		if s.WithTTL == 0 || o.TTLMinMS < s.TTLMinMS {
			s.TTLMinMS = o.TTLMinMS
		}
		s.TTLMaxMS = max(s.TTLMaxMS, o.TTLMaxMS)
	}
	s.Keys += o.Keys
	s.Bytes += o.Bytes
	s.WithTTL += o.WithTTL
	s.TTLSumMS += o.TTLSumMS
	s.TTLBytes += o.TTLBytes
	s.MaxBytes = max(s.MaxBytes, o.MaxBytes)
	s.MaxElems = max(s.MaxElems, o.MaxElems)
	s.BigKeys += o.BigKeys
	s.BigBytes += o.BigBytes
	s.IdleKnown += o.IdleKnown
	s.ColdKeys += o.ColdKeys
	s.ColdBytes += o.ColdBytes
}

// AvgTTLMS is the mean remaining TTL over keys that have one.
func (s Stats) AvgTTLMS() int64 {
	if s.WithTTL == 0 {
		return 0
	}
	return s.TTLSumMS / s.WithTTL
}

// Node is one segment in the trie, e.g. "cart:" -> "{num}:" -> "items".
type Node struct {
	children map[string]*Node
	// ends holds stats for keys whose pattern ends exactly here, split by
	// Redis type (the same template can be a hash in one key and a string in another).
	ends map[string]*Stats
	// collapsed: this node had too many literal children; they were merged
	// into "*" and every future literal segment goes there too.
	collapsed bool
}

// Wildcard replaces literal segments of a high-cardinality node.
const Wildcard = "*"

// DefaultMaxChildren is the literal-children cap before a node collapses.
const DefaultMaxChildren = 500

// Trie is a prefix tree of key templates.
type Trie struct {
	root        Node
	maxChildren int
}

// NewTrie returns an empty trie. A node collapses once it would get more than
// maxChildren literal (non-placeholder) children; <= 0 means DefaultMaxChildren.
// This bounds memory: unclassified IDs like usernames can't add a node per key.
func NewTrie(maxChildren int) *Trie {
	if maxChildren <= 0 {
		maxChildren = DefaultMaxChildren
	}
	return &Trie{maxChildren: maxChildren}
}

// isLiteral: a segment the classifier did not turn into a placeholder.
func isLiteral[T text](seg T) bool {
	return len(seg) > 0 && seg[0] != '{' && seg[0] != '*'
}

// wildcardFor keeps the segment's trailing separator: "alice:" -> "*:".
// Returns a constant so the hot path doesn't allocate.
func wildcardFor[T text](seg T) string {
	switch seg[len(seg)-1] {
	case ':':
		return "*:"
	case '/':
		return "*/"
	case '|':
		return "*|"
	case '#':
		return "*#"
	case ',':
		return "*,"
	}
	return Wildcard
}

func (n *Node) literalChildren() int {
	c := 0
	for seg := range n.children {
		if isLiteral(seg) {
			c++
		}
	}
	return c
}

// collapse merges every literal child into its wildcard sibling.
func (t *Trie) collapse(n *Node) {
	n.collapsed = true
	for seg, c := range n.children {
		if !isLiteral(seg) {
			continue
		}
		delete(n.children, seg) // deleting during range is allowed in Go
		w := wildcardFor(seg)
		if dst := n.children[w]; dst != nil {
			t.merge(dst, c)
		} else {
			n.children[w] = c // wildcards aren't literal, so this loop skips them
		}
	}
}

// Merge folds o into t; o must not be used afterwards (its nodes are reused).
// The parallel pipeline builds one trie per worker and merges them at the end.
func (t *Trie) Merge(o *Trie) { t.merge(&t.root, &o.root) }

// merge folds src's subtree into dst, collapsing dst if it grows too wide.
func (t *Trie) merge(dst, src *Node) {
	// A collapsed src sends its keys to "*", so dst's own literals must go there too.
	if src.collapsed && !dst.collapsed {
		t.collapse(dst)
	}
	for kind, s := range src.ends {
		if dst.ends == nil {
			dst.ends = make(map[string]*Stats)
		}
		if d := dst.ends[kind]; d != nil {
			d.Merge(*s)
		} else {
			dst.ends[kind] = s
		}
	}
	for seg, c := range src.children {
		if dst.collapsed && isLiteral(seg) {
			seg = wildcardFor(seg)
		}
		if dst.children == nil {
			dst.children = make(map[string]*Node)
		}
		if d := dst.children[seg]; d != nil {
			t.merge(d, c)
		} else {
			dst.children[seg] = c
		}
	}
	dst.collapsed = dst.collapsed || src.collapsed
	if !dst.collapsed && dst.literalChildren() > t.maxChildren {
		t.collapse(dst)
	}
}

// Insert adds one key with template tmpl (from AppendNormalized).
// It walks tmpl segment by segment without building a []string, and copies
// a segment into a new string only when it creates a node.
func (t *Trie) Insert(tmpl []byte, kind string, k Key) {
	n := &t.root
	for len(tmpl) > 0 {
		end := 1 + indexSep(tmpl) // separator stays attached to its segment
		if end == 0 {
			end = len(tmpl)
		}
		n = t.child(n, tmpl[:end])
		tmpl = tmpl[end:]
	}
	if n.ends == nil {
		n.ends = make(map[string]*Stats)
	}
	s := n.ends[kind]
	if s == nil {
		s = &Stats{}
		n.ends[kind] = s
	}
	s.add(k)
}

func indexSep(b []byte) int {
	for i, c := range b {
		if isSep(c) {
			return i
		}
	}
	return -1
}

// child returns n's child for seg, creating it (and collapsing n) as needed.
// n.children[string(seg)] doesn't allocate when used as a lookup; the
// compiler only copies seg when the result is stored as a new map key.
func (t *Trie) child(n *Node, seg []byte) *Node {
	if n.children == nil {
		n.children = make(map[string]*Node)
	}
	if isLiteral(seg) {
		if !n.collapsed {
			if c := n.children[string(seg)]; c != nil {
				return c
			}
			if n.literalChildren() < t.maxChildren {
				c := &Node{}
				n.children[string(seg)] = c
				return c
			}
			t.collapse(n)
		}
		w := wildcardFor(seg)
		c := n.children[w]
		if c == nil {
			c = &Node{}
			n.children[w] = c
		}
		return c
	}
	c := n.children[string(seg)]
	if c == nil {
		c = &Node{}
		n.children[string(seg)] = c
	}
	return c
}

// foldRatio: a variant is folded into a sibling with at least this many times its keys.
const foldRatio = 20

// Consolidate folds rare variants of an ID segment into their dominant sibling.
// The classifier sees one key at a time, so some IDs of a single kind land in
// other placeholders: an 8-digit ID that happens to be a valid YYYYMMDD becomes
// {date}, and a random hex ID with no letters becomes {num}. Looking at siblings
// fixes that: user:{date}:profile with 2K keys next to user:{num}:profile with 4M
// keys is the same pattern. Call it once, after all keys are inserted (and
// tries merged), so the result doesn't depend on key order.
func (t *Trie) Consolidate() { t.consolidate(&t.root) }

func (t *Trie) consolidate(n *Node) {
	if len(n.children) > 1 {
		keys := make(map[string]int64, len(n.children))
		segs := make([]string, 0, len(n.children))
		for seg, c := range n.children {
			keys[seg] = c.total().Keys
			segs = append(segs, seg)
		}
		// Biggest first, so a variant folds into the most common sibling.
		sort.Slice(segs, func(i, j int) bool {
			if keys[segs[i]] != keys[segs[j]] {
				return keys[segs[i]] > keys[segs[j]]
			}
			return segs[i] < segs[j]
		})
		for _, dom := range segs {
			dn := n.children[dom]
			if dn == nil { // already folded into something bigger
				continue
			}
			for _, minor := range segs {
				mn := n.children[minor]
				if minor == dom || mn == nil || keys[minor]*foldRatio > keys[dom] || !foldsInto(minor, dom) {
					continue
				}
				t.merge(dn, mn)
				delete(n.children, minor)
				keys[dom] += keys[minor]
			}
		}
	}
	for _, c := range n.children {
		t.consolidate(c)
	}
}

// foldsInto reports whether segment minor may be folded into segment dom.
// Both must end in the same separator (or none).
func foldsInto(minor, dom string) bool {
	mBody, mSep := splitSep(minor)
	dBody, dSep := splitSep(dom)
	if mSep != dSep {
		return false
	}
	digits := mBody == Num || mBody == Date || mBody == TS
	switch dBody {
	case Num, Date, TS:
		return digits
	case Hex: // hex IDs without letters were classified as digits, without digits as words
		return digits || isHexWord(mBody)
	}
	return false
}

// splitSep separates a segment's trailing separator: "{num}:" -> "{num}", ":".
func splitSep(seg string) (body, sep string) {
	if n := len(seg); n > 0 && isSep(seg[n-1]) {
		return seg[:n-1], seg[n-1:]
	}
	return seg, ""
}

// isHexWord: 8+ hex letters and digits, e.g. "bafeecafbcba", which isHex rejects
// for having no digit (so that words like "deadbeef" survive on their own).
func isHexWord(s string) bool {
	if len(s) < 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isHexChar(s[i]) {
			return false
		}
	}
	return true
}

// Row is one line of a report.
type Row struct {
	Pattern string
	Kind    string // empty for prefix rollups
	Stats   Stats
}

// Patterns returns every full pattern, sorted by bytes descending.
func (t *Trie) Patterns() []Row {
	var rows []Row
	var walk func(n *Node, prefix string)
	walk = func(n *Node, prefix string) {
		for kind, s := range n.ends {
			rows = append(rows, Row{prefix, kind, *s})
		}
		for seg, c := range n.children {
			walk(c, prefix+seg)
		}
	}
	walk(&t.root, "")
	sortRows(rows)
	return rows
}

// Prefixes rolls patterns up to their first `depth` segments
// (depth 1: "cart:", "user:", ...), sorted by bytes descending.
func (t *Trie) Prefixes(depth int) []Row {
	var rows []Row
	var walk func(n *Node, prefix string, d int)
	walk = func(n *Node, prefix string, d int) {
		if d == depth || len(n.children) == 0 {
			rows = append(rows, Row{Pattern: prefix, Stats: n.total()})
			return
		}
		// Keys ending above the cut-off depth get their own row.
		if len(n.ends) > 0 && prefix != "" {
			var s Stats
			for _, e := range n.ends {
				s.Merge(*e)
			}
			rows = append(rows, Row{Pattern: prefix, Stats: s})
		}
		for seg, c := range n.children {
			walk(c, prefix+seg, d+1)
		}
	}
	walk(&t.root, "", 0)
	sortRows(rows)
	return rows
}

// Total is the sum over every key in the trie.
func (t *Trie) Total() Stats { return t.root.total() }

func (n *Node) total() Stats {
	var s Stats
	for _, e := range n.ends {
		s.Merge(*e)
	}
	for _, c := range n.children {
		s.Merge(c.total())
	}
	return s
}

// sortRows orders by bytes desc, then pattern for deterministic output.
func sortRows(rows []Row) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Stats.Bytes != rows[j].Stats.Bytes {
			return rows[i].Stats.Bytes > rows[j].Stats.Bytes
		}
		if rows[i].Pattern != rows[j].Pattern {
			return rows[i].Pattern < rows[j].Pattern
		}
		return rows[i].Kind < rows[j].Kind
	})
}
