package dataset

import (
	"sort"
	"strings"
	"unicode"
)

// Cross-source merge (M9, §13).
//
// §15 calls this "the hardest quality problem in this document", and the reason
// is that every other stage has a right answer available to it. A schema either
// validates or does not. A row either quotes its source or does not. Whether two
// rows are the same company is a judgement, and no amount of care in the code
// turns a judgement into a fact.
//
// So the response is not to be clever, it is to be measurable. merge_test.go
// builds datasets whose duplicate structure is KNOWN — the same entity written
// several ways, plus entities that genuinely differ — and reports pairwise
// precision and recall. Those numbers are in `mole eval`. They are the only
// quality figure in M9 that needs no model and no labelling, which is exactly
// why the thresholds below are calibrated against them rather than chosen.
//
// # Three deliberate choices
//
// Blocking, because comparing every pair is quadratic and a result set reaches
// thousands of rows. Multi-pass blocking, because a single block key loses true
// matches at its edges.
//
// Complete linkage, not transitive closure. If A matches B and B matches C, a
// union-find would put all three together even when A and C are plainly
// different — "Acme Ltd", "Acme", "Acme Foods" chains a real company into an
// unrelated one. A row joins a cluster only if it matches EVERY member.
//
// Conflicts preserved, never resolved. §11 renders contradiction edges
// explicitly "rather than silently resolved by whichever claim the model liked",
// and two sources giving one company two revenues is that problem with a column
// header. The merge records both.

// Options tune the merge.
type Options struct {
	// Threshold is the similarity a pair of key values must reach.
	//
	// Calibrated, not chosen: see TestMergeQualityOnKnownDuplicates, which
	// reports precision and recall across a range and fails if the default
	// drops below what is recorded there.
	Threshold float64
	// MaxRows bounds the input. A merge is at worst quadratic within a block, so
	// an unbounded row set is an unbounded run.
	MaxRows int
}

// Defaults for the merge.
const (
	// DefaultThreshold sits in the gap the token measure opens up: on the
	// ground-truth set every true pair scores 0.67 or above and every false one
	// 0.50 or below, so anything in between separates them. The test prints the
	// curve and fails if the recorded precision and recall regress.
	//
	// It is a gap rather than a knife edge because the measure counts words. Two
	// names sharing one word out of three cannot reach it; two names differing
	// only by a typo or a reordering cannot fall below it.
	DefaultThreshold = 0.60
	DefaultMaxRows   = 20000
)

func (o Options) withDefaults() Options {
	if o.Threshold <= 0 {
		o.Threshold = DefaultThreshold
	}
	if o.MaxRows <= 0 {
		o.MaxRows = DefaultMaxRows
	}
	return o
}

// Merge folds rows describing the same entity together.
//
// Deterministic: the input is sorted before clustering, so the same rows in a
// different order produce the same dataset. §14.1 keys cassettes on request
// bodies and a report is generated from this, so an order-dependent merge would
// make a replay a coin toss.
func Merge(schema Schema, rows []Row, opts Options) Dataset {
	opts = opts.withDefaults()
	out := Dataset{Schema: schema, Extracted: len(rows)}

	if len(rows) > opts.MaxRows {
		out.Notes = append(out.Notes, describeTruncation(len(rows), opts.MaxRows))
		rows = rows[:opts.MaxRows]
	}

	keyed := make([]keyedRow, 0, len(rows))
	for _, r := range rows {
		k := schema.keyOf(r)
		if k.normalised == "" {
			// No usable key. Extraction already refuses these, so reaching here
			// means a row arrived from somewhere else — a stored dataset from an
			// older schema, say. Counted rather than dropped in silence.
			continue
		}
		keyed = append(keyed, keyedRow{row: r, key: k})
	}
	if n := len(rows) - len(keyed); n > 0 {
		out.Notes = append(out.Notes, describeUnkeyed(n))
	}

	// Sorted by key then source, so clustering order does not depend on
	// extraction order.
	sort.SliceStable(keyed, func(i, j int) bool {
		if keyed[i].key.normalised != keyed[j].key.normalised {
			return keyed[i].key.normalised < keyed[j].key.normalised
		}
		return keyed[i].row.Source < keyed[j].row.Source
	})

	for _, cluster := range cluster(keyed, opts.Threshold) {
		out.Rows = append(out.Rows, schema.fold(cluster))
	}
	return out
}

func describeTruncation(got, limit int) string {
	return "the merge was given more rows than it will compare (" +
		itoa(got) + " against a limit of " + itoa(limit) + "); the excess was dropped"
}

func describeUnkeyed(n int) string {
	return itoa(n) + " row(s) had no usable key value and could not be merged"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// -----------------------------------------------------------------------------
// Keys
// -----------------------------------------------------------------------------

type key struct {
	// raw is the key fields joined, as the sources wrote them.
	raw string
	// normalised is what similarity is computed on.
	normalised string
	// blocks are the cheap signatures used to decide which pairs to compare.
	blocks []string
}

type keyedRow struct {
	row Row
	key key
}

func (s Schema) keyOf(r Row) key {
	var parts []string
	for _, name := range s.Keys() {
		if v, ok := r.Get(name); ok {
			parts = append(parts, v)
		}
	}
	raw := strings.Join(parts, " ")
	norm := Normalise(raw)
	return key{raw: raw, normalised: norm, blocks: blocksFor(norm)}
}

// corporateSuffixes are dropped before comparing.
//
// "Acme Ltd" and "Acme Limited" are one company, and a similarity score computed
// on the full strings puts them further apart than "Acme Ltd" and "Acme Foods" —
// which is the wrong way round and the single biggest source of both false
// positives and false negatives on company names.
var corporateSuffixes = map[string]bool{
	"ltd": true, "limited": true, "plc": true, "llp": true, "lp": true,
	"inc": true, "incorporated": true, "llc": true, "corp": true, "corporation": true,
	"co": true, "company": true, "group": true, "holdings": true, "holding": true,
	"gmbh": true, "ag": true, "sa": true, "sas": true, "srl": true, "spa": true,
	"bv": true, "nv": true, "ab": true, "as": true, "oy": true, "aps": true,
	"pty": true, "pte": true, "kk": true, "kg": true, "ug": true, "se": true,
}

// Normalise reduces a key value to what should be compared.
//
// Exported because the eval harness computes the same normalisation when scoring,
// and a scorer using a different rule from the merge would be measuring something
// else.
func Normalise(s string) string {
	// Case and accents first: "Nestlé" and "Nestle" are the same company, and a
	// source that lost the accent should not become a second entity.
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(fold(r))
		case unicode.IsSpace(r), r == '-', r == '_', r == '/', r == '&':
			b.WriteByte(' ')
		default:
			// Punctuation is removed rather than spaced: "St.Gallen" is one
			// token, and "acme, inc." must not become "acme  inc".
		}
	}

	var kept []string
	for _, tok := range strings.Fields(b.String()) {
		if corporateSuffixes[tok] {
			continue
		}
		kept = append(kept, tok)
	}
	if len(kept) == 0 {
		// Everything was a suffix. Better a comparable string than an empty one:
		// "Ltd" as a whole key value is not a company, but dropping it silently
		// would lose the row rather than report it.
		return strings.Join(strings.Fields(b.String()), " ")
	}
	return strings.Join(kept, " ")
}

// foldTable maps the accented Latin letters that appear in entity names onto
// their base form.
//
// A map, and it started as two parallel strings — which silently misaligned. The
// "from" side had 75 runes and the "to" side 79, so every letter past the drift
// mapped to the wrong base: "Łódź" normalised to "dodz" rather than "lodz". Two
// parallel sequences whose correspondence nothing checks is the construct that
// caused it, so the correspondence is now written out and cannot drift.
//
// A table rather than golang.org/x/text/unicode/norm: this binary has no
// third-party dependency it does not need, and the set that matters for entity
// names on the web is small and stable.
var foldTable = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a', 'ā': 'a', 'ă': 'a', 'ą': 'a',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e', 'ē': 'e', 'ĕ': 'e', 'ė': 'e', 'ę': 'e', 'ě': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i', 'ĩ': 'i', 'ī': 'i', 'ĭ': 'i', 'į': 'i',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o', 'ø': 'o', 'ō': 'o', 'ŏ': 'o', 'ő': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u', 'ũ': 'u', 'ū': 'u', 'ŭ': 'u', 'ů': 'u', 'ű': 'u', 'ų': 'u',
	'ç': 'c', 'ć': 'c', 'ĉ': 'c', 'ċ': 'c', 'č': 'c',
	'ñ': 'n', 'ń': 'n', 'ņ': 'n', 'ň': 'n',
	'ý': 'y', 'ÿ': 'y', 'ŷ': 'y',
	'ž': 'z', 'ź': 'z', 'ż': 'z',
	'š': 's', 'ś': 's', 'ŝ': 's', 'ş': 's', 'ß': 's',
	'ğ': 'g', 'ĝ': 'g',
	'ď': 'd', 'đ': 'd', 'ð': 'd',
	'ł': 'l', 'ļ': 'l', 'ľ': 'l',
	'ŕ': 'r', 'ř': 'r',
	'ť': 't', 'ţ': 't', 'þ': 't',
	'æ': 'a', 'œ': 'o',
}

// fold maps one rune onto its base form, or returns it unchanged.
func fold(r rune) rune {
	if base, ok := foldTable[r]; ok {
		return base
	}
	return r
}

// blockPrefix is how many characters of a token form a block signature.
const blockPrefix = 4

// blocksFor produces the cheap signatures that decide which pairs are compared.
//
// Multi-pass: the whole key's prefix AND each token's prefix, so a pair is
// compared if they agree anywhere. A single block key would lose "British
// Airways" against "Airways, British" and every case where the distinguishing
// word is not first.
func blocksFor(norm string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	flat := strings.ReplaceAll(norm, " ", "")
	add(prefix(flat, blockPrefix))
	for _, tok := range strings.Fields(norm) {
		add(prefix(tok, blockPrefix))
	}
	return out
}

func prefix(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n])
}

// -----------------------------------------------------------------------------
// Clustering
// -----------------------------------------------------------------------------

// cluster groups rows by key similarity, under complete linkage.
func cluster(rows []keyedRow, threshold float64) [][]keyedRow {
	// Which rows share a block with which. Only these pairs are ever scored.
	byBlock := map[string][]int{}
	for i, r := range rows {
		for _, b := range r.key.blocks {
			byBlock[b] = append(byBlock[b], i)
		}
	}

	assigned := make([]int, len(rows))
	for i := range assigned {
		assigned[i] = -1
	}
	var clusters [][]keyedRow
	var members [][]int

	for i := range rows {
		if assigned[i] >= 0 {
			continue
		}
		// A new cluster, then everything that matches ALL of it.
		clusters = append(clusters, []keyedRow{rows[i]})
		members = append(members, []int{i})
		ci := len(clusters) - 1
		assigned[i] = ci

		for _, j := range candidates(byBlock, rows[i].key.blocks) {
			if j <= i || assigned[j] >= 0 {
				continue
			}
			if matchesAll(rows, members[ci], j, threshold) {
				clusters[ci] = append(clusters[ci], rows[j])
				members[ci] = append(members[ci], j)
				assigned[j] = ci
			}
		}
	}
	return clusters
}

func candidates(byBlock map[string][]int, blocks []string) []int {
	seen := map[int]bool{}
	var out []int
	for _, b := range blocks {
		for _, j := range byBlock[b] {
			if !seen[j] {
				seen[j] = true
				out = append(out, j)
			}
		}
	}
	sort.Ints(out)
	return out
}

// matchesAll is the complete-linkage test.
//
// Every existing member, not just the one that pulled the candidate in. With
// transitive closure "Acme", "Acme Ltd" and "Acme Foods" collapse into one row
// because the middle matches both ends — and the result is a dataset that has
// quietly merged two companies.
func matchesAll(rows []keyedRow, members []int, j int, threshold float64) bool {
	for _, m := range members {
		if Similarity(rows[m].key.normalised, rows[j].key.normalised) < threshold {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// Similarity
// -----------------------------------------------------------------------------

// Similarity scores two normalised key values in [0, 1].
//
// Token-level, and that is the third design after two measured failures. The
// numbers below are from the ground-truth set in merge_test.go.
//
// Attempt one was Jaro-Winkler on the strings plus token overlap divided by the
// SMALLER token set. Precision 0.583: "Acme" against "Acme Bakery Holdings"
// scored 1.0 because the whole of the shorter side was covered, and a shared
// first word is the commonest thing two different companies have.
//
// Attempt two divided by the union instead, which fixed that measure and left
// precision unchanged — because Jaro-Winkler was scoring the same pairs at 0.88
// on its own. Its prefix boost is designed for short personal names, and on
// company names the distinguishing word comes second: "deutschebank" against
// "deutschetelekom" scores 0.89.
//
// So string-level similarity is not the right primitive here. What separates
// these cases is which TOKENS the two sides share, with Jaro-Winkler used only
// inside a token to absorb a typo:
//
//	acme            / acme bakery          1/2 = 0.50   not a match
//	deutsche bank   / deutsche telekom     1/3 = 0.33   not a match
//	deutsche bank   / deutsche bnak        2/2 = 1.00   a match, typo absorbed
//	british airways / airways british      2/2 = 1.00   a match, order absorbed
//
// One special case survives: a source that closes up a name the other spaces
// out. "JP Morgan" and "JPMorgan" share no token at all, and are the same
// company — so the space-stripped forms are compared for equality first.
func Similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	if strings.ReplaceAll(a, " ", "") == strings.ReplaceAll(b, " ", "") {
		return 1
	}
	return fuzzyJaccard(a, b)
}

// tokenMatch is how similar two tokens must be to count as the same word.
//
// High, because this absorbs a typo and nothing else. At 0.90 "bank" and "bnak"
// are one word (0.925) while "bank" and "bond" are not.
const tokenMatch = 0.90

// fuzzyJaccard is the intersection over the union of the token sets, where two
// tokens intersect if they are nearly the same word.
//
// Greedy pairing, best match first. Exact for the sizes involved — a key value
// with more than a handful of tokens is not an entity name — and an optimal
// assignment would be more code for no measurable difference.
func fuzzyJaccard(a, b string) float64 {
	ta, tb := strings.Fields(a), strings.Fields(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}

	used := make([]bool, len(tb))
	var shared int
	for _, x := range ta {
		bestScore, bestIdx := 0.0, -1
		for j, y := range tb {
			if used[j] {
				continue
			}
			s := 1.0
			if x != y {
				s = jaroWinkler(x, y)
			}
			if s > bestScore {
				bestScore, bestIdx = s, j
			}
		}
		if bestIdx >= 0 && bestScore >= tokenMatch {
			used[bestIdx] = true
			shared++
		}
	}
	if shared == 0 {
		return 0
	}
	// The union counts each side's unmatched tokens once plus the shared ones.
	union := shared + (len(ta) - shared) + (len(tb) - shared)
	return float64(shared) / float64(union)
}

// jaroWinkler is the standard measure, with the standard prefix boost.
func jaroWinkler(a, b string) float64 {
	j := jaro(a, b)
	if j <= 0.7 {
		return j
	}
	ra, rb := []rune(a), []rune(b)
	var l int
	for l < len(ra) && l < len(rb) && l < 4 && ra[l] == rb[l] {
		l++
	}
	return j + float64(l)*0.1*(1-j)
}

func jaro(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 || lb == 0 {
		return 0
	}

	window := la
	if lb > window {
		window = lb
	}
	window = window/2 - 1
	if window < 0 {
		window = 0
	}

	matchedA := make([]bool, la)
	matchedB := make([]bool, lb)
	var matches int
	for i := 0; i < la; i++ {
		lo := i - window
		if lo < 0 {
			lo = 0
		}
		hi := i + window + 1
		if hi > lb {
			hi = lb
		}
		for k := lo; k < hi; k++ {
			if matchedB[k] || ra[i] != rb[k] {
				continue
			}
			matchedA[i], matchedB[k] = true, true
			matches++
			break
		}
	}
	if matches == 0 {
		return 0
	}

	var transpositions int
	k := 0
	for i := 0; i < la; i++ {
		if !matchedA[i] {
			continue
		}
		for !matchedB[k] {
			k++
		}
		if ra[i] != rb[k] {
			transpositions++
		}
		k++
	}
	m := float64(matches)
	return (m/float64(la) + m/float64(lb) + (m-float64(transpositions)/2)/m) / 3
}

// -----------------------------------------------------------------------------
// Folding a cluster into one row
// -----------------------------------------------------------------------------

// fold assembles one Merged from the rows that matched.
func (s Schema) fold(cluster []keyedRow) Merged {
	out := Merged{Cells: map[string]Cell{}, Members: len(cluster), Quotes: map[string]string{}}

	sources := map[string]bool{}
	for _, kr := range cluster {
		sources[kr.row.Source] = true
		if _, seen := out.Quotes[kr.row.Source]; !seen && kr.row.Quote != "" {
			out.Quotes[kr.row.Source] = kr.row.Quote
		}
	}
	for src := range sources {
		out.Sources = append(out.Sources, src)
	}
	sort.Strings(out.Sources)

	for _, f := range s.Fields {
		// Values in the order the sources are sorted, so a tie resolves the same
		// way every run.
		counts := map[string]int{}
		srcOf := map[string][]string{}
		var order []string
		for _, kr := range cluster {
			v, ok := kr.row.Get(f.Name)
			if !ok {
				continue
			}
			if counts[v] == 0 {
				order = append(order, v)
			}
			counts[v]++
			srcOf[v] = append(srcOf[v], kr.row.Source)
		}
		if len(order) == 0 {
			continue
		}
		// Most corroborated first, then the value from the alphabetically first
		// source: a deterministic rule, and one that prefers agreement over
		// arrival order.
		sort.SliceStable(order, func(i, j int) bool {
			if counts[order[i]] != counts[order[j]] {
				return counts[order[i]] > counts[order[j]]
			}
			return order[i] < order[j]
		})

		cell := Cell{Text: order[0], Sources: dedupe(srcOf[order[0]])}
		if len(order) > 1 {
			// A key field's alternatives are SPELLINGS, not disagreements: the
			// merge grouped these rows because it judged the key values to name
			// one entity, so calling them a conflict would contradict its own
			// decision. Everything else is a genuine disagreement.
			if f.Key {
				cell.Variants = order[1:]
			} else {
				cell.Others = order[1:]
			}
		}
		out.Cells[f.Name] = cell
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
