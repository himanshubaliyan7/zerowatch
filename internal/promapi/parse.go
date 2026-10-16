package promapi

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The PromQL subset understood by the evaluator. Anything outside it is a
// parse error, which the HTTP layer reports as errorType bad_data.

// Expr is a parsed PromQL expression.
type Expr interface{ expr() }

// NumberLit is a number literal.
type NumberLit struct{ V float64 }

// StringLit is a string literal (only valid as a function argument or at
// the top of an instant query).
type StringLit struct{ S string }

// MatchType is a label matcher operator.
type MatchType string

// Matcher operators.
const (
	MatchEqual    MatchType = "="
	MatchNotEqual MatchType = "!="
	MatchRegexp   MatchType = "=~"
	MatchNotRegex MatchType = "!~"
)

// Matcher is one label matcher.
type Matcher struct {
	Name  string
	Type  MatchType
	Value string
	re    *regexp.Regexp
}

// Matches reports whether v satisfies the matcher.
func (m *Matcher) Matches(v string) bool {
	switch m.Type {
	case MatchEqual:
		return v == m.Value
	case MatchNotEqual:
		return v != m.Value
	case MatchRegexp:
		return m.re.MatchString(v)
	case MatchNotRegex:
		return !m.re.MatchString(v)
	}
	return false
}

func newMatcher(name string, t MatchType, v string) (*Matcher, error) {
	m := &Matcher{Name: name, Type: t, Value: v}
	if t == MatchRegexp || t == MatchNotRegex {
		re, err := regexp.Compile("^(?:" + v + ")$")
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression %q: %v", v, err)
		}
		m.re = re
	}
	return m, nil
}

// VectorSel is an instant vector selector.
type VectorSel struct {
	Matchers []*Matcher
	Offset   time.Duration
}

// MatrixSel is a range vector selector.
type MatrixSel struct {
	VS    *VectorSel
	Range time.Duration
}

// Call is a function call.
type Call struct {
	Func string
	Args []Expr
}

// Agg is an aggregation.
type Agg struct {
	Op       string
	Expr     Expr
	Param    Expr // topk/bottomk/quantile
	Grouping []string
	Without  bool
}

// Binary is a binary operation.
type Binary struct {
	Op       string
	LHS, RHS Expr
	Bool     bool
	On       bool // on(...) given (otherwise ignoring(...) or nothing)
	Matching []string
}

// Unary is unary minus or plus.
type Unary struct {
	Op   string
	Expr Expr
}

// Paren is a parenthesised expression.
type Paren struct{ Expr Expr }

func (NumberLit) expr()  {}
func (StringLit) expr()  {}
func (*VectorSel) expr() {}
func (*MatrixSel) expr() {}
func (*Call) expr()      {}
func (*Agg) expr()       {}
func (*Binary) expr()    {}
func (*Unary) expr()     {}
func (*Paren) expr()     {}

// ---- lexer ----

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tNumber
	tDuration
	tString
	tRange // raw text between [ and ]
	tOp    // punctuation and operators
)

type token struct {
	kind tokKind
	s    string
	num  float64
	dur  time.Duration
	pos  int
}

func lex(in string) ([]token, error) {
	var out []token
	i := 0
	for i < len(in) {
		c := in[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '#': // comment to end of line
			for i < len(in) && in[i] != '\n' {
				i++
			}
		case isIdentStart(c):
			j := i + 1
			for j < len(in) && isIdentChar(in[j]) {
				j++
			}
			out = append(out, token{kind: tIdent, s: in[i:j], pos: i})
			i = j
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(in) && in[i+1] >= '0' && in[i+1] <= '9':
			tk, n, err := lexNumber(in, i)
			if err != nil {
				return nil, err
			}
			out = append(out, tk)
			i = n
		case c == '"' || c == '\'' || c == '`':
			s, n, err := lexString(in, i)
			if err != nil {
				return nil, err
			}
			out = append(out, token{kind: tString, s: s, pos: i})
			i = n
		case c == '[':
			j := strings.IndexByte(in[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("unclosed '[' at position %d", i)
			}
			out = append(out, token{kind: tRange, s: strings.TrimSpace(in[i+1 : i+j]), pos: i})
			i += j + 1
		default:
			two := ""
			if i+1 < len(in) {
				two = in[i : i+2]
			}
			switch two {
			case "!=", "=~", "!~", "==", "<=", ">=":
				out = append(out, token{kind: tOp, s: two, pos: i})
				i += 2
				continue
			}
			if strings.IndexByte("(){},=<>+-*/%^@:", c) >= 0 {
				out = append(out, token{kind: tOp, s: string(c), pos: i})
				i++
				continue
			}
			return nil, fmt.Errorf("unexpected character %q at position %d", c, i)
		}
	}
	return append(out, token{kind: tEOF, pos: len(in)}), nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isIdentChar(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// lexNumber reads a number, or a duration such as 5m or 1h30m.
func lexNumber(in string, i int) (token, int, error) {
	j := i
	for j < len(in) && in[j] >= '0' && in[j] <= '9' {
		j++
	}
	if j < len(in) && isDurUnitStart(in, j) {
		k := i
		for k < len(in) && in[k] >= '0' && in[k] <= '9' {
			for k < len(in) && in[k] >= '0' && in[k] <= '9' {
				k++
			}
			if !isDurUnitStart(in, k) {
				return token{}, 0, fmt.Errorf("bad duration at position %d", i)
			}
			k += unitLen(in, k)
		}
		if k < len(in) && isIdentChar(in[k]) {
			return token{}, 0, fmt.Errorf("bad duration %q", in[i:k+1])
		}
		d, err := parseDuration(in[i:k])
		if err != nil {
			return token{}, 0, err
		}
		return token{kind: tDuration, s: in[i:k], dur: d, pos: i}, k, nil
	}
	j = i
	if strings.HasPrefix(in[i:], "0x") || strings.HasPrefix(in[i:], "0X") {
		j = i + 2
		for j < len(in) && strings.IndexByte("0123456789abcdefABCDEF", in[j]) >= 0 {
			j++
		}
		v, err := strconv.ParseInt(in[i+2:j], 16, 64)
		if err != nil {
			return token{}, 0, fmt.Errorf("bad number %q", in[i:j])
		}
		return token{kind: tNumber, s: in[i:j], num: float64(v), pos: i}, j, nil
	}
	for j < len(in) && (in[j] >= '0' && in[j] <= '9' || in[j] == '.') {
		j++
	}
	if j < len(in) && (in[j] == 'e' || in[j] == 'E') {
		k := j + 1
		if k < len(in) && (in[k] == '+' || in[k] == '-') {
			k++
		}
		if k < len(in) && in[k] >= '0' && in[k] <= '9' {
			for k < len(in) && in[k] >= '0' && in[k] <= '9' {
				k++
			}
			j = k
		}
	}
	v, err := strconv.ParseFloat(in[i:j], 64)
	if err != nil {
		return token{}, 0, fmt.Errorf("bad number %q", in[i:j])
	}
	if j < len(in) && isIdentChar(in[j]) {
		return token{}, 0, fmt.Errorf("bad number or duration %q", in[i:j+1])
	}
	return token{kind: tNumber, s: in[i:j], num: v, pos: i}, j, nil
}

func isDurUnitStart(in string, j int) bool {
	return unitLen(in, j) > 0
}

func unitLen(in string, j int) int {
	if j >= len(in) {
		return 0
	}
	if strings.HasPrefix(in[j:], "ms") {
		return 2
	}
	if strings.IndexByte("smhdwy", in[j]) >= 0 {
		return 1
	}
	return 0
}

var durUnits = map[string]time.Duration{
	"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour,
	"d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "y": 365 * 24 * time.Hour,
}

// parseDuration parses a Prometheus duration (1h30m, 5m, 250ms, 2d, 1w, 1y).
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total time.Duration
	i := 0
	for i < len(s) {
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		n := unitLen(s, j)
		if j == i || n == 0 {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		v, err := strconv.ParseInt(s[i:j], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		total += time.Duration(v) * durUnits[s[j:j+n]]
		i = j + n
	}
	return total, nil
}

func lexString(in string, i int) (string, int, error) {
	q := in[i]
	if q == '`' {
		j := strings.IndexByte(in[i+1:], '`')
		if j < 0 {
			return "", 0, fmt.Errorf("unterminated raw string at position %d", i)
		}
		return in[i+1 : i+1+j], i + j + 2, nil
	}
	var b strings.Builder
	j := i + 1
	for j < len(in) {
		c := in[j]
		if c == q {
			return b.String(), j + 1, nil
		}
		if c == '\\' && j+1 < len(in) {
			j++
			switch in[j] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\', '"', '\'':
				b.WriteByte(in[j])
			default:
				// Keep unknown escapes verbatim so regex escapes like \d survive.
				b.WriteByte('\\')
				b.WriteByte(in[j])
			}
			j++
			continue
		}
		b.WriteByte(c)
		j++
	}
	return "", 0, fmt.Errorf("unterminated string at position %d", i)
}

// ---- parser ----

type parser struct {
	toks []token
	i    int
}

// ParseExpr parses a PromQL expression of the supported subset.
func ParseExpr(in string) (Expr, error) {
	toks, err := lex(in)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	e, err := p.parseExpr(1)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, fmt.Errorf("unexpected %s at position %d", desc(t), t.pos)
	}
	if err := check(e); err != nil {
		return nil, err
	}
	return e, nil
}

func desc(t token) string {
	switch t.kind {
	case tEOF:
		return "end of input"
	case tRange:
		return "\"[" + t.s + "]\""
	}
	return strconv.Quote(t.s)
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token { t := p.toks[p.i]; p.i++; return t }

func (p *parser) isOp(s string) bool { t := p.peek(); return t.kind == tOp && t.s == s }

func (p *parser) isKw(s string) bool {
	t := p.peek()
	return t.kind == tIdent && strings.EqualFold(t.s, s)
}

func (p *parser) expect(s string) error {
	t := p.next()
	if t.kind != tOp || t.s != s {
		return fmt.Errorf("expected %q, got %s at position %d", s, desc(t), t.pos)
	}
	return nil
}

var binPrec = map[string]int{
	"or": 1, "and": 2, "unless": 2,
	"==": 3, "!=": 3, "<": 3, ">": 3, "<=": 3, ">=": 3,
	"+": 4, "-": 4,
	"*": 5, "/": 5, "%": 5, "atan2": 5,
	"^": 6,
}

func isComparison(op string) bool { return binPrec[op] == 3 }
func isSetOp(op string) bool      { return op == "and" || op == "or" || op == "unless" }

func (p *parser) binOp() (string, bool) {
	t := p.peek()
	switch t.kind {
	case tOp:
		if _, ok := binPrec[t.s]; ok {
			return t.s, true
		}
	case tIdent:
		l := strings.ToLower(t.s)
		if l == "and" || l == "or" || l == "unless" || l == "atan2" {
			return l, true
		}
	}
	return "", false
}

func (p *parser) parseExpr(minPrec int) (Expr, error) {
	lhs, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		op, ok := p.binOp()
		if !ok || binPrec[op] < minPrec {
			return lhs, nil
		}
		p.next()
		b := &Binary{Op: op, LHS: lhs}
		if p.isKw("bool") {
			if !isComparison(op) {
				return nil, fmt.Errorf("bool modifier can only be used on comparison operators")
			}
			p.next()
			b.Bool = true
		}
		if p.isKw("on") || p.isKw("ignoring") {
			b.On = strings.EqualFold(p.next().s, "on")
			labels, err := p.labelList()
			if err != nil {
				return nil, err
			}
			b.Matching = labels
			if b.On && b.Matching == nil {
				b.Matching = []string{}
			}
			if p.isKw("group_left") || p.isKw("group_right") {
				return nil, fmt.Errorf("unsupported: %s (many-to-one matching) is not supported, only one-to-one", p.peek().s)
			}
		}
		prec := binPrec[op]
		next := prec + 1
		if op == "^" {
			next = prec // right associative
		}
		rhs, err := p.parseExpr(next)
		if err != nil {
			return nil, err
		}
		b.RHS = rhs
		lhs = b
	}
}

func (p *parser) parseUnary() (Expr, error) {
	if p.isOp("-") || p.isOp("+") {
		op := p.next().s
		// Unary minus binds looser than ^: -2^2 is -(2^2).
		e, err := p.parseExpr(binPrec["^"])
		if err != nil {
			return nil, err
		}
		if n, ok := e.(NumberLit); ok {
			if op == "-" {
				n.V = -n.V
			}
			return n, nil
		}
		return &Unary{Op: op, Expr: e}, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (Expr, error) {
	e, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind == tRange {
		p.next()
		vs, ok := e.(*VectorSel)
		if !ok {
			if strings.Contains(t.s, ":") {
				return nil, fmt.Errorf("unsupported: subqueries are not supported")
			}
			return nil, fmt.Errorf("ranges only allowed for vector selectors")
		}
		if strings.Contains(t.s, ":") {
			return nil, fmt.Errorf("unsupported: subqueries are not supported")
		}
		d, err := parseDuration(t.s)
		if err != nil {
			return nil, err
		}
		if d <= 0 {
			return nil, fmt.Errorf("range must be positive, got %q", t.s)
		}
		e = &MatrixSel{VS: vs, Range: d}
	}
	if p.isKw("offset") {
		p.next()
		neg := false
		if p.isOp("-") {
			p.next()
			neg = true
		}
		t := p.next()
		if t.kind != tDuration {
			return nil, fmt.Errorf("expected duration after offset, got %s", desc(t))
		}
		d := t.dur
		if neg {
			d = -d
		}
		switch x := e.(type) {
		case *VectorSel:
			x.Offset = d
		case *MatrixSel:
			x.VS.Offset = d
		default:
			return nil, fmt.Errorf("offset modifier must be preceded by an instant or range vector selector")
		}
	}
	if p.isOp("@") {
		return nil, fmt.Errorf("unsupported: the @ modifier is not supported")
	}
	return e, nil
}

var aggOps = map[string]bool{
	"sum": true, "avg": true, "min": true, "max": true, "count": true, "topk": true, "bottomk": true,
	"stddev": true, "stdvar": true, "group": true, "quantile": true,
	"count_values": false, "limitk": false, "limit_ratio": false,
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tNumber:
		p.next()
		return NumberLit{V: t.num}, nil
	case tDuration:
		return nil, fmt.Errorf("unexpected duration %q at position %d", t.s, t.pos)
	case tString:
		p.next()
		return StringLit{S: t.s}, nil
	case tRange:
		return nil, fmt.Errorf("unexpected \"[%s]\" at position %d", t.s, t.pos)
	case tEOF:
		return nil, fmt.Errorf("unexpected end of input")
	case tOp:
		switch t.s {
		case "(":
			p.next()
			e, err := p.parseExpr(1)
			if err != nil {
				return nil, err
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			return &Paren{Expr: e}, nil
		case "{":
			return p.parseSelector("")
		}
		return nil, fmt.Errorf("unexpected %s at position %d", desc(t), t.pos)
	}
	// identifier
	name := t.s
	lname := strings.ToLower(name)
	p.next()
	if supported, isAgg := aggOps[lname]; isAgg && (p.isOp("(") || p.isKw("by") || p.isKw("without")) {
		if !supported {
			return nil, fmt.Errorf("unsupported: aggregation %s is not supported", lname)
		}
		return p.parseAgg(lname)
	}
	if p.isOp("(") {
		if _, ok := functions[name]; !ok {
			if unsupportedFuncs[name] {
				return nil, fmt.Errorf("unsupported: function %s is not supported", name)
			}
			return nil, fmt.Errorf("unknown function with name %q", name)
		}
		p.next()
		var args []Expr
		if !p.isOp(")") {
			for {
				a, err := p.parseExpr(1)
				if err != nil {
					return nil, err
				}
				args = append(args, a)
				if p.isOp(",") {
					p.next()
					continue
				}
				break
			}
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
		return &Call{Func: name, Args: args}, nil
	}
	switch lname {
	case "inf":
		return NumberLit{V: math.Inf(1)}, nil
	case "nan":
		return NumberLit{V: math.NaN()}, nil
	}
	return p.parseSelector(name)
}

func (p *parser) parseAgg(op string) (Expr, error) {
	a := &Agg{Op: op}
	readGrouping := func() error {
		if p.isKw("by") || p.isKw("without") {
			a.Without = strings.EqualFold(p.next().s, "without")
			g, err := p.labelList()
			if err != nil {
				return err
			}
			if g == nil {
				g = []string{}
			}
			a.Grouping = g
		}
		return nil
	}
	if err := readGrouping(); err != nil {
		return nil, err
	}
	if err := p.expect("("); err != nil {
		return nil, err
	}
	var args []Expr
	for {
		e, err := p.parseExpr(1)
		if err != nil {
			return nil, err
		}
		args = append(args, e)
		if p.isOp(",") {
			p.next()
			if p.isOp(")") {
				break
			}
			continue
		}
		break
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}
	if a.Grouping == nil {
		if err := readGrouping(); err != nil {
			return nil, err
		}
	}
	needParam := op == "topk" || op == "bottomk" || op == "quantile"
	switch {
	case needParam && len(args) != 2:
		return nil, fmt.Errorf("wrong number of arguments for aggregate expression provided, expected 2, got %d", len(args))
	case !needParam && len(args) != 1:
		return nil, fmt.Errorf("wrong number of arguments for aggregate expression provided, expected 1, got %d", len(args))
	}
	if needParam {
		a.Param, a.Expr = args[0], args[1]
	} else {
		a.Expr = args[0]
	}
	return a, nil
}

// labelList parses "(a, b, c)".
func (p *parser) labelList() ([]string, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	var out []string
	for !p.isOp(")") {
		t := p.next()
		if t.kind != tIdent && t.kind != tString {
			return nil, fmt.Errorf("expected label name, got %s at position %d", desc(t), t.pos)
		}
		out = append(out, t.s)
		if p.isOp(",") {
			p.next()
			continue
		}
		if !p.isOp(")") {
			return nil, fmt.Errorf("expected \",\" or \")\" in label list, got %s", desc(p.peek()))
		}
	}
	p.next()
	return out, nil
}

func (p *parser) parseSelector(name string) (Expr, error) {
	vs := &VectorSel{}
	if name != "" {
		m, _ := newMatcher(nameLabel, MatchEqual, name)
		vs.Matchers = append(vs.Matchers, m)
	}
	if p.isOp("{") {
		p.next()
		for !p.isOp("}") {
			t := p.next()
			if t.kind != tIdent && t.kind != tString {
				return nil, fmt.Errorf("expected label name in matcher, got %s at position %d", desc(t), t.pos)
			}
			// {"metric_name"} form (Prometheus 3 quoted names)
			if t.kind == tString && (p.isOp(",") || p.isOp("}")) {
				m, _ := newMatcher(nameLabel, MatchEqual, t.s)
				vs.Matchers = append(vs.Matchers, m)
			} else {
				opTok := p.next()
				mt := MatchType(opTok.s)
				if opTok.kind != tOp || (mt != MatchEqual && mt != MatchNotEqual && mt != MatchRegexp && mt != MatchNotRegex) {
					return nil, fmt.Errorf("expected label matching operator, got %s at position %d", desc(opTok), opTok.pos)
				}
				v := p.next()
				if v.kind != tString {
					return nil, fmt.Errorf("expected string label value, got %s at position %d", desc(v), v.pos)
				}
				m, err := newMatcher(t.s, mt, v.s)
				if err != nil {
					return nil, err
				}
				vs.Matchers = append(vs.Matchers, m)
			}
			if p.isOp(",") {
				p.next()
				continue
			}
			if !p.isOp("}") {
				return nil, fmt.Errorf("expected \",\" or \"}\" in selector, got %s", desc(p.peek()))
			}
		}
		p.next()
	}
	nonEmpty := false
	for _, m := range vs.Matchers {
		if !m.Matches("") {
			nonEmpty = true
		}
	}
	if !nonEmpty {
		return nil, fmt.Errorf("vector selector must contain at least one non-empty matcher")
	}
	return vs, nil
}

// ---- type checking ----

type valType int

const (
	typScalar valType = iota
	typVector
	typMatrix
	typString
)

func (t valType) String() string {
	return [...]string{"scalar", "instant vector", "range vector", "string"}[t]
}

func typeOf(e Expr) valType {
	switch x := e.(type) {
	case NumberLit:
		return typScalar
	case StringLit:
		return typString
	case *VectorSel:
		return typVector
	case *MatrixSel:
		return typMatrix
	case *Paren:
		return typeOf(x.Expr)
	case *Unary:
		return typeOf(x.Expr)
	case *Agg:
		return typVector
	case *Call:
		return functions[x.Func].ret
	case *Binary:
		if typeOf(x.LHS) == typScalar && typeOf(x.RHS) == typScalar {
			return typScalar
		}
		return typVector
	}
	return typScalar
}

// check validates operand types throughout the tree.
func check(e Expr) error {
	switch x := e.(type) {
	case *Paren:
		return check(x.Expr)
	case *Unary:
		if t := typeOf(x.Expr); t != typScalar && t != typVector {
			return fmt.Errorf("unary expression only allowed on expressions of type scalar or instant vector, got %s", t)
		}
		return check(x.Expr)
	case *Agg:
		if t := typeOf(x.Expr); t != typVector {
			return fmt.Errorf("expected type instant vector in aggregation expression, got %s", t)
		}
		if x.Param != nil {
			if t := typeOf(x.Param); t != typScalar {
				return fmt.Errorf("expected type scalar in aggregation parameter, got %s", t)
			}
			if err := check(x.Param); err != nil {
				return err
			}
		}
		return check(x.Expr)
	case *Call:
		f := functions[x.Func]
		if len(x.Args) < len(f.args)-f.optional || len(x.Args) > len(f.args) {
			return fmt.Errorf("expected %d argument(s) in call to %q, got %d", len(f.args), x.Func, len(x.Args))
		}
		for i, a := range x.Args {
			if t := typeOf(a); t != f.args[i] {
				return fmt.Errorf("expected type %s in call to function %q, got %s", f.args[i], x.Func, t)
			}
			if err := check(a); err != nil {
				return err
			}
		}
		return nil
	case *Binary:
		lt, rt := typeOf(x.LHS), typeOf(x.RHS)
		for _, t := range []valType{lt, rt} {
			if t != typScalar && t != typVector {
				return fmt.Errorf("binary expression must contain only scalar and instant vector types, got %s", t)
			}
		}
		if isSetOp(x.Op) && (lt != typVector || rt != typVector) {
			return fmt.Errorf("set operator %q not allowed in binary scalar expression", x.Op)
		}
		if isComparison(x.Op) && lt == typScalar && rt == typScalar && !x.Bool {
			return fmt.Errorf("comparisons between scalars must use BOOL modifier")
		}
		if x.Matching != nil && (lt != typVector || rt != typVector) {
			return fmt.Errorf("vector matching only allowed between instant vectors")
		}
		if err := check(x.LHS); err != nil {
			return err
		}
		return check(x.RHS)
	}
	return nil
}

// String renders a label list for error messages.
func labelString(l map[string]string) string {
	keys := sortedKeys(l)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(strconv.Quote(l[k]))
	}
	b.WriteByte('}')
	return b.String()
}
