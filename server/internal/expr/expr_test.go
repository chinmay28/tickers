package expr

import (
	"errors"
	"math"
	"slices"
	"testing"
)

func TestParseCanonicalises(t *testing.T) {
	cases := []struct {
		src  string
		text string
		key  string
	}{
		{"VTI/GLD", "VTI/GLD", "VTI/GLD"},
		{"  vti / gld  ", "VTI/GLD", "VTI/GLD"},
		{"p/VTI", "P/VTI", "P/VTI"},
		{"BTC-USD/GLD", "BTC-USD/GLD", "BTC-USD/GLD"},
		{"VTI - GLD", "VTI - GLD", "VTI-GLD"},
		{"(VTI+GLD)/2", "(VTI + GLD)/2", "(VTI+GLD)/2"},
		{"VTI/GLD/2", "VTI/GLD/2", "VTI/GLD/2"},
		{"VTI/(GLD*2)", "VTI/(GLD*2)", "VTI/(GLD*2)"},
		{"^GSPC/VTI", "^GSPC/VTI", "^GSPC/VTI"},
		{"EURUSD=X*VTI", "EURUSD=X*VTI", "EURUSD=X*VTI"},
	}
	for _, tc := range cases {
		e, err := Parse(tc.src)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.src, err)
			continue
		}
		if e.String() != tc.text {
			t.Errorf("Parse(%q).String() = %q, want %q", tc.src, e.String(), tc.text)
		}
		if e.Key() != tc.key {
			t.Errorf("Parse(%q).Key() = %q, want %q", tc.src, e.Key(), tc.key)
		}
	}
}

// The canonical form is what gets stored and shown back in the edit box, so it
// has to survive a round trip — otherwise saving a composite twice without
// touching it would keep rewriting the row.
func TestCanonicalFormReparsesToItself(t *testing.T) {
	for _, src := range []string{
		"VTI/GLD", "VTI - GLD", "(VTI+GLD)/2", "VTI/(GLD-P)", "-VTI/GLD", "BTC-USD/GLD",
	} {
		first, err := Parse(src)
		if err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		second, err := Parse(first.String())
		if err != nil {
			t.Fatalf("Parse(%q) [canonical of %q]: %v", first.String(), src, err)
		}
		if second.String() != first.String() {
			t.Errorf("%q canonicalised to %q, which re-canonicalises to %q",
				src, first.String(), second.String())
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, src := range []string{
		"", "   ",
		"VTI/",         // missing operand
		"VTI +",        // missing operand
		"(VTI/GLD",     // unclosed
		"VTI $ GLD",    // stray character
		"VTI/GLD;DROP", // stray character
		"((((((((((((((((((((((((((VTI))))))))))))))))))))))))))", // too deep
	} {
		if e, err := Parse(src); err == nil {
			t.Errorf("Parse(%q) accepted, giving %q", src, e.String())
		}
	}

	long := make([]byte, MaxLen+1)
	for i := range long {
		long[i] = 'A'
	}
	if _, err := Parse(string(long)); err == nil {
		t.Error("Parse accepted a formula past MaxLen")
	}
}

// A single symbol parses fine — it is the store that rejects it as a
// composite, on the strength of this count.
func TestOperatorsCountsCombination(t *testing.T) {
	for src, want := range map[string]int{
		"VTI":         0,
		"(VTI)":       0,
		"BTC-USD":     0,
		"VTI/GLD":     1,
		"(VTI+GLD)/2": 2,
	} {
		e, err := Parse(src)
		if err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		if e.Operators() != want {
			t.Errorf("Parse(%q).Operators() = %d, want %d", src, e.Operators(), want)
		}
	}
}

func TestSymbolsAreDeduplicatedInOrder(t *testing.T) {
	e, err := Parse("VTI/GLD + VTI/P")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"VTI", "GLD", "P"}
	if !slices.Equal(e.Symbols(), want) {
		t.Errorf("Symbols() = %v, want %v", e.Symbols(), want)
	}
}

func TestEval(t *testing.T) {
	values := map[string]float64{"VTI": 300, "GLD": 200, "P": 30, "BTC-USD": 68000}
	cases := []struct {
		src  string
		want float64
	}{
		{"VTI/GLD", 1.5},
		{"P/VTI", 0.1},
		{"VTI - GLD", 100},
		{"(VTI+GLD)/2", 250},
		{"VTI/GLD/2", 0.75},
		{"VTI/(GLD*2)", 0.75},
		{"-VTI/GLD", -1.5},
		{"BTC-USD/GLD", 340},
		{"VTI*2 - GLD", 400},
	}
	for _, tc := range cases {
		e, err := Parse(tc.src)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.src, err)
			continue
		}
		got, err := e.Eval(values)
		if err != nil {
			t.Errorf("Eval(%q): %v", tc.src, err)
			continue
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("Eval(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

func TestEvalReportsTheMissingSymbol(t *testing.T) {
	e, err := Parse("VTI/GLD")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = e.Eval(map[string]float64{"VTI": 300})

	var missing *MissingError
	if !errors.As(err, &missing) {
		t.Fatalf("Eval error is %v, want a *MissingError", err)
	}
	if missing.Symbol != "GLD" {
		t.Errorf("missing symbol is %q, want GLD", missing.Symbol)
	}
}

func TestEvalRejectsDivisionByZero(t *testing.T) {
	e, err := Parse("VTI/GLD")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := e.Eval(map[string]float64{"VTI": 300, "GLD": 0}); !errors.Is(err, ErrDivideByZero) {
		t.Errorf("Eval with a zero divisor returned %v, want ErrDivideByZero", err)
	}
}

// The hyphen is the one genuinely ambiguous character: part of BTC-USD, and
// also subtraction. Spacing decides, and this is the specification of that.
func TestHyphenBelongsToTheSymbolUnlessSpaced(t *testing.T) {
	glued, err := Parse("BTC-USD/GLD")
	if err != nil {
		t.Fatalf("parse glued: %v", err)
	}
	if !slices.Equal(glued.Symbols(), []string{"BTC-USD", "GLD"}) {
		t.Errorf("BTC-USD/GLD references %v, want [BTC-USD GLD]", glued.Symbols())
	}

	spaced, err := Parse("VTI - GLD")
	if err != nil {
		t.Fatalf("parse spaced: %v", err)
	}
	if !slices.Equal(spaced.Symbols(), []string{"VTI", "GLD"}) {
		t.Errorf("VTI - GLD references %v, want [VTI GLD]", spaced.Symbols())
	}
}

func TestLooks(t *testing.T) {
	for text, want := range map[string]bool{
		"VTI/GLD":     true,
		"(VTI+GLD)/2": true,
		"VTI - GLD":   true,
		"VTI*2":       true,
		"VTI":         false,
		"BTC-USD":     false,
		"BRK.B":       false,
		"^GSPC":       false,
		"EURUSD=X":    false,
	} {
		if got := Looks(text); got != want {
			t.Errorf("Looks(%q) = %v, want %v", text, got, want)
		}
	}
}
