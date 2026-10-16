// Package query implements the CloudWatch GetMetricData-compatible metric math
// engine: lexer/parser -> AST -> dependency DAG (topological sort with cycle
// detection) -> timestamp alignment with FILL policies -> vectorized evaluator.
package query

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ---------------------------------------------------------------- lexer

type tokKind int

const (
	tEOF tokKind = iota
	tNum
	tIdent
	tOp
	tLParen
	tRParen
	tComma
)

type token struct {
	kind tokKind
	text string
	num  float64
	pos  int
}

// SyntaxError reports a parse failure with its byte offset.
type SyntaxError struct {
	Pos int
	Msg string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("syntax error at %d: %s", e.Pos, e.Msg) }

func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := rune(src[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '(':
			toks = append(toks, token{kind: tLParen, text: "(", pos: i})
			i++
		case c == ')':
			toks = append(toks, token{kind: tRParen, text: ")", pos: i})
			i++
		case c == ',':
			toks = append(toks, token{kind: tComma, text: ",", pos: i})
			i++
		case strings.ContainsRune("+-*/^", c):
			toks = append(toks, token{kind: tOp, text: string(c), pos: i})
			i++
		case unicode.IsDigit(c) || c == '.':
			start := i
			for i < len(src) && (unicode.IsDigit(rune(src[i])) || src[i] == '.') {
				i++
			}
			// optional exponent: 1e-3
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				j := i + 1
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				if j < len(src) && unicode.IsDigit(rune(src[j])) {
					i = j
					for i < len(src) && unicode.IsDigit(rune(src[i])) {
						i++
					}
				}
			}
			f, err := strconv.ParseFloat(src[start:i], 64)
			if err != nil {
				return nil, &SyntaxError{start, "invalid number " + src[start:i]}
			}
			toks = append(toks, token{kind: tNum, text: src[start:i], num: f, pos: start})
		case unicode.IsLetter(c) || c == '_':
			start := i
			for i < len(src) && (unicode.IsLetter(rune(src[i])) || unicode.IsDigit(rune(src[i])) || src[i] == '_') {
				i++
			}
			toks = append(toks, token{kind: tIdent, text: src[start:i], pos: start})
		default:
			return nil, &SyntaxError{i, fmt.Sprintf("unexpected character %q", c)}
		}
	}
	toks = append(toks, token{kind: tEOF, pos: len(src)})
	return toks, nil
}

// ---------------------------------------------------------------- AST

// Node is an expression AST node.
type Node interface{ String() string }

// Num is a numeric literal.
type Num struct{ Value float64 }

// Ref references another query id (or a keyword like REPEAT inside FILL).
type Ref struct{ ID string }

// Unary is a prefix operator (only '-').
type Unary struct {
	Op string
	X  Node
}

// Binary is an infix arithmetic operation.
type Binary struct {
	Op   string
	L, R Node
}

// Call is a function invocation.
type Call struct {
	Name string
	Args []Node
}

func (n *Num) String() string    { return strconv.FormatFloat(n.Value, 'g', -1, 64) }
func (n *Ref) String() string    { return n.ID }
func (n *Unary) String() string  { return "(" + n.Op + n.X.String() + ")" }
func (n *Binary) String() string { return "(" + n.L.String() + " " + n.Op + " " + n.R.String() + ")" }
func (n *Call) String() string {
	parts := make([]string, len(n.Args))
	for i, a := range n.Args {
		parts[i] = a.String()
	}
	return n.Name + "(" + strings.Join(parts, ", ") + ")"
}

// ---------------------------------------------------------------- parser (Pratt)

type parser struct {
	toks []token
	p    int
}

// Parse parses a metric math expression.
func Parse(src string) (Node, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	ps := &parser{toks: toks}
	n, err := ps.expr(0)
	if err != nil {
		return nil, err
	}
	if t := ps.peek(); t.kind != tEOF {
		return nil, &SyntaxError{t.pos, "unexpected token " + t.text}
	}
	return n, nil
}

func (ps *parser) peek() token { return ps.toks[ps.p] }
func (ps *parser) next() token { t := ps.toks[ps.p]; ps.p++; return t }

// binding powers: + - : 10, * / : 20, ^ : 30 (right associative), unary - : 25
func infixBP(op string) (left, right int) {
	switch op {
	case "+", "-":
		return 10, 11
	case "*", "/":
		return 20, 21
	case "^":
		return 31, 30
	}
	return -1, -1
}

func (ps *parser) expr(minBP int) (Node, error) {
	lhs, err := ps.prefix()
	if err != nil {
		return nil, err
	}
	for {
		t := ps.peek()
		if t.kind != tOp {
			break
		}
		lbp, rbp := infixBP(t.text)
		if lbp < minBP {
			break
		}
		ps.next()
		rhs, err := ps.expr(rbp)
		if err != nil {
			return nil, err
		}
		lhs = &Binary{Op: t.text, L: lhs, R: rhs}
	}
	return lhs, nil
}

func (ps *parser) prefix() (Node, error) {
	t := ps.next()
	switch t.kind {
	case tNum:
		return &Num{Value: t.num}, nil
	case tOp:
		if t.text == "-" || t.text == "+" {
			x, err := ps.expr(25)
			if err != nil {
				return nil, err
			}
			if t.text == "+" {
				return x, nil
			}
			return &Unary{Op: "-", X: x}, nil
		}
	case tLParen:
		n, err := ps.expr(0)
		if err != nil {
			return nil, err
		}
		if c := ps.next(); c.kind != tRParen {
			return nil, &SyntaxError{c.pos, "expected )"}
		}
		return n, nil
	case tIdent:
		if ps.peek().kind == tLParen {
			ps.next()
			call := &Call{Name: strings.ToUpper(t.text)}
			if ps.peek().kind == tRParen {
				ps.next()
				return call, nil
			}
			for {
				a, err := ps.expr(0)
				if err != nil {
					return nil, err
				}
				call.Args = append(call.Args, a)
				c := ps.next()
				if c.kind == tRParen {
					break
				}
				if c.kind != tComma {
					return nil, &SyntaxError{c.pos, "expected , or )"}
				}
			}
			return call, nil
		}
		return &Ref{ID: t.text}, nil
	case tEOF:
		return nil, &SyntaxError{t.pos, "unexpected end of expression"}
	}
	return nil, &SyntaxError{t.pos, "unexpected token " + t.text}
}

// fillKeywords are identifiers that are FILL policy keywords, not query refs.
var fillKeywords = map[string]bool{"REPEAT": true, "LINEAR": true, "NONE": true}

// Refs returns the distinct query ids referenced by n (excluding keywords that
// appear as FILL policy arguments).
func Refs(n Node) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(Node, bool)
	walk = func(n Node, fillPolicyArg bool) {
		switch x := n.(type) {
		case *Ref:
			if fillPolicyArg && fillKeywords[strings.ToUpper(x.ID)] {
				return
			}
			if !seen[x.ID] {
				seen[x.ID] = true
				out = append(out, x.ID)
			}
		case *Unary:
			walk(x.X, false)
		case *Binary:
			walk(x.L, false)
			walk(x.R, false)
		case *Call:
			for i, a := range x.Args {
				walk(a, x.Name == "FILL" && i == 1)
			}
		}
	}
	walk(n, false)
	return out
}
