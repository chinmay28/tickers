// Package expr parses and evaluates the small arithmetic formulas behind
// composite tickers — "VTI/GLD", "P/VTI", "(VTI+GLD)/2".
//
// A composite is a watchlist row whose price is *computed* from other symbols
// rather than fetched. Everything downstream of the price — the sparkline, the
// change against the previous close, pinning, publishing — then works on it
// unchanged, because by the time a composite reaches the store it is an
// ordinary quote with an ordinary number in it.
//
// The grammar is deliberately tiny:
//
//	expr   := term (('+' | '-') term)*
//	term   := factor (('*' | '/') factor)*
//	factor := '-' factor | '(' expr ')' | number | symbol
//
// The one wrinkle is the hyphen, which is both subtraction and a character in
// real symbols (BTC-USD, BRK-B). The lexer resolves it positionally: a hyphen
// wedged between two symbol characters with no space belongs to the symbol, and
// a hyphen with a space on either side is the operator. So "BTC-USD/GLD" reads
// as one ratio and "VTI - GLD" reads as a difference, which is what someone
// typing either of them means. The cost is that "VTI-GLD" lexes as a single
// (unpriceable) symbol; that is why Parse's error names the symbol it could not
// price, and why the UI asks for spaces around a minus.
package expr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MaxLen bounds a formula. Composites are ratios of a few symbols; the limit is
// here so a paste can't hand the parser something pathological.
const MaxLen = 200

// maxDepth caps nesting. Recursive descent on unbounded parentheses is a stack
// overflow waiting for a fuzzer.
const maxDepth = 24

// ErrDivideByZero is returned by Eval when a divisor computes to zero — which
// is a real possibility for a ratio whose denominator is a fresh, unpriced
// symbol reading as 0 rather than as missing.
var ErrDivideByZero = errors.New("the formula divides by zero")

// MissingError names a symbol the formula needs but wasn't given a value for.
// The engine unwraps it to attach the provider's own reason for the gap.
type MissingError struct{ Symbol string }

func (e *MissingError) Error() string { return "no price for " + e.Symbol }

// Expr is a parsed formula.
type Expr struct {
	root    node
	text    string
	key     string
	symbols []string
	ops     int
}

// String is the canonical form of the formula: uppercase, minimal parentheses,
// and spaces around + and − so it re-parses to the same tree. This is what gets
// stored, and what the edit form shows back.
func (e *Expr) String() string { return e.text }

// Key is the canonical form with every space removed — the composite's symbol,
// and so its published payload key and its history key. "VTI / GLD" and
// "VTI/GLD" are the same row.
func (e *Expr) Key() string { return e.key }

// Symbols are the market symbols the formula references, deduplicated, in the
// order they first appear. The refresh cycle fetches these.
func (e *Expr) Symbols() []string { return e.symbols }

// Operators counts the arithmetic operators in the formula. A "composite" with
// none is just a symbol wearing a costume, and the store rejects it.
func (e *Expr) Operators() int { return e.ops }

// Eval computes the formula against a symbol → value map. Callers use it twice
// per composite: once with the current prices, once with the previous closes,
// which is what gives a ratio a change and a change percentage.
func (e *Expr) Eval(values map[string]float64) (float64, error) {
	return e.root.eval(values)
}

// Looks reports whether text reads as a formula rather than a plain symbol. It
// is what lets someone type "VTI/GLD" into the ordinary symbol box and get a
// composite, without first finding a mode switch.
//
// A bare hyphen does not count: "BTC-USD" is a symbol, and a subtraction has to
// be written with spaces to be one.
func Looks(text string) bool {
	return strings.ContainsAny(text, "/*+()") || strings.Contains(text, " - ")
}

// Parse validates a formula and returns it in canonical form.
func Parse(src string) (*Expr, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return nil, errors.New("a formula is required")
	}
	if len(src) > MaxLen {
		return nil, fmt.Errorf("a formula cannot be longer than %d characters", MaxLen)
	}

	tokens, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}
	root, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	if !p.done() {
		return nil, fmt.Errorf("unexpected %q in the formula", p.peek().text)
	}

	e := &Expr{
		root:    root,
		text:    render(root, 0, false),
		key:     render(root, 0, true),
		symbols: collectSymbols(root),
		ops:     countOps(root),
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// Nodes
// ---------------------------------------------------------------------------

type node interface {
	eval(values map[string]float64) (float64, error)
	prec() int
}

type numNode float64

func (n numNode) eval(map[string]float64) (float64, error) { return float64(n), nil }
func (numNode) prec() int                                  { return 3 }

type symNode string

func (n symNode) eval(values map[string]float64) (float64, error) {
	v, ok := values[string(n)]
	if !ok {
		return 0, &MissingError{Symbol: string(n)}
	}
	return v, nil
}
func (symNode) prec() int { return 3 }

type negNode struct{ child node }

func (n negNode) eval(values map[string]float64) (float64, error) {
	v, err := n.child.eval(values)
	if err != nil {
		return 0, err
	}
	return -v, nil
}
func (negNode) prec() int { return 3 }

type binNode struct {
	op       byte
	lhs, rhs node
}

func (n binNode) eval(values map[string]float64) (float64, error) {
	lhs, err := n.lhs.eval(values)
	if err != nil {
		return 0, err
	}
	rhs, err := n.rhs.eval(values)
	if err != nil {
		return 0, err
	}
	switch n.op {
	case '+':
		return lhs + rhs, nil
	case '-':
		return lhs - rhs, nil
	case '*':
		return lhs * rhs, nil
	default:
		if rhs == 0 {
			return 0, ErrDivideByZero
		}
		return lhs / rhs, nil
	}
}

func (n binNode) prec() int {
	if n.op == '+' || n.op == '-' {
		return 1
	}
	return 2
}

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tokNumber tokenKind = iota
	tokSymbol
	tokOperator
	tokLParen
	tokRParen
	tokEOF
)

type token struct {
	kind tokenKind
	text string
	num  float64
}

func lex(src string) ([]token, error) {
	var out []token
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(':
			out = append(out, token{kind: tokLParen, text: "("})
			i++
		case c == ')':
			out = append(out, token{kind: tokRParen, text: ")"})
			i++
		case c == '+' || c == '-' || c == '*' || c == '/':
			out = append(out, token{kind: tokOperator, text: string(c)})
			i++
		case isDigit(c):
			start := i
			for i < len(src) && (isDigit(src[i]) || src[i] == '.') {
				i++
			}
			text := src[start:i]
			v, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, fmt.Errorf("%q is not a valid number", text)
			}
			out = append(out, token{kind: tokNumber, text: text, num: v})
		case isSymbolStart(c):
			start := i
			i++
			for i < len(src) {
				if isSymbolChar(src[i]) {
					i++
					continue
				}
				// The hyphen rule: part of the symbol only when it is glued to
				// symbol characters on both sides ("BTC-USD"), never when it is
				// spaced ("VTI - GLD").
				if src[i] == '-' && i+1 < len(src) && isAlphanumeric(src[i+1]) {
					i += 2
					continue
				}
				break
			}
			out = append(out, token{kind: tokSymbol, text: strings.ToUpper(src[start:i])})
		default:
			return nil, fmt.Errorf("%q is not something a formula can contain", string(c))
		}
	}
	return append(out, token{kind: tokEOF, text: "end of formula"}), nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isAlphanumeric(c byte) bool { return isLetter(c) || isDigit(c) }

// A symbol starts with a letter or a caret — the latter because index symbols
// are written "^GSPC".
func isSymbolStart(c byte) bool { return isLetter(c) || c == '^' }

// Continuation characters cover the shapes Yahoo actually uses: BRK.B, VWRL.L,
// EURUSD=X, ^GSPC. The hyphen is handled separately, above.
func isSymbolChar(c byte) bool {
	return isAlphanumeric(c) || c == '.' || c == '=' || c == '^'
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() token { return p.tokens[p.pos] }

func (p *parser) done() bool { return p.peek().kind == tokEOF }

func (p *parser) parseExpr(depth int) (node, error) {
	if depth > maxDepth {
		return nil, errors.New("the formula is nested too deeply")
	}
	left, err := p.parseTerm(depth)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tokOperator || (t.text != "+" && t.text != "-") {
			return left, nil
		}
		p.pos++
		right, err := p.parseTerm(depth)
		if err != nil {
			return nil, err
		}
		left = binNode{op: t.text[0], lhs: left, rhs: right}
	}
}

func (p *parser) parseTerm(depth int) (node, error) {
	left, err := p.parseFactor(depth)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tokOperator || (t.text != "*" && t.text != "/") {
			return left, nil
		}
		p.pos++
		right, err := p.parseFactor(depth)
		if err != nil {
			return nil, err
		}
		left = binNode{op: t.text[0], lhs: left, rhs: right}
	}
}

func (p *parser) parseFactor(depth int) (node, error) {
	if depth > maxDepth {
		return nil, errors.New("the formula is nested too deeply")
	}
	t := p.peek()
	switch {
	case t.kind == tokOperator && t.text == "-":
		p.pos++
		child, err := p.parseFactor(depth + 1)
		if err != nil {
			return nil, err
		}
		return negNode{child: child}, nil
	case t.kind == tokLParen:
		p.pos++
		inner, err := p.parseExpr(depth + 1)
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, errors.New("the formula is missing a closing parenthesis")
		}
		p.pos++
		return inner, nil
	case t.kind == tokNumber:
		p.pos++
		return numNode(t.num), nil
	case t.kind == tokSymbol:
		p.pos++
		return symNode(t.text), nil
	default:
		return nil, fmt.Errorf("the formula is missing a value before %q", t.text)
	}
}

// ---------------------------------------------------------------------------
// Rendering & inspection
// ---------------------------------------------------------------------------

// render writes a node back out, parenthesising only where precedence requires
// it. `compact` drops the spaces around + and −, which is how the composite's
// symbol (and therefore its payload key) is derived.
func render(n node, parentPrec int, compact bool) string {
	switch v := n.(type) {
	case numNode:
		return strconv.FormatFloat(float64(v), 'f', -1, 64)
	case symNode:
		return string(v)
	case negNode:
		return "-" + render(v.child, 3, compact)
	case binNode:
		sep := string(v.op)
		if !compact && (v.op == '+' || v.op == '-') {
			sep = " " + sep + " "
		}
		// The right operand of a non-associative operator needs parentheses at
		// equal precedence too: a/(b*c) is not a/b*c.
		out := render(v.lhs, v.prec(), compact) + sep + render(v.rhs, v.prec()+1, compact)
		if v.prec() < parentPrec {
			return "(" + out + ")"
		}
		return out
	}
	return ""
}

func collectSymbols(n node) []string {
	out := []string{}
	seen := map[string]bool{}
	var walk func(node)
	walk = func(n node) {
		switch v := n.(type) {
		case symNode:
			if !seen[string(v)] {
				seen[string(v)] = true
				out = append(out, string(v))
			}
		case negNode:
			walk(v.child)
		case binNode:
			walk(v.lhs)
			walk(v.rhs)
		}
	}
	walk(n)
	return out
}

func countOps(n node) int {
	switch v := n.(type) {
	case negNode:
		return countOps(v.child)
	case binNode:
		return 1 + countOps(v.lhs) + countOps(v.rhs)
	}
	return 0
}
