package money

import (
	"encoding/json"
	"math/big"
	"testing"
)

func TestParseAndString(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"0", "0"},
		{"12", "12"},
		{"12.34", "12.34"},
		{"0.000001", "0.000001"},
		{"1.500000", "1.5"},
		{"-3.25", "-3.25"},
		{"+7", "7"},
		{"1000000000000.000001", "1000000000000.000001"},
	}
	for _, c := range cases {
		m, err := Parse(c.in, "cny")
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got := m.String(); got != c.out {
			t.Errorf("Parse(%q).String() = %q, want %q", c.in, got, c.out)
		}
		if m.Currency() != "CNY" {
			t.Errorf("currency not normalized: %q", m.Currency())
		}
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	for _, in := range []string{"", "abc", "1.2.3", "1.", ".5", "1,000", "0.0000001", "--1"} {
		if _, err := Parse(in, "CNY"); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
}

func TestArithmeticExact(t *testing.T) {
	// 经典浮点陷阱：0.1 + 0.2 != 0.3，定点数必须精确。
	a := MustParse("0.1", "CNY")
	b := MustParse("0.2", "CNY")
	sum := a.Add(b)
	if sum.Cmp(MustParse("0.3", "CNY")) != 0 {
		t.Fatalf("0.1+0.2 = %s, want 0.3", sum)
	}
	// 大额运算不丢精度。
	x := MustParse("99999999999999.999999", "CNY")
	y := MustParse("0.000001", "CNY")
	if got := x.Add(y).String(); got != "100000000000000" {
		t.Fatalf("got %s", got)
	}
	if got := x.Sub(x); !got.IsZero() {
		t.Fatalf("x-x = %s", got)
	}
}

func TestImmutability(t *testing.T) {
	a := MustParse("5", "CNY")
	b := MustParse("3", "CNY")
	_ = a.Add(b)
	_ = a.Sub(b)
	if a.String() != "5" {
		t.Fatalf("receiver mutated: %s", a)
	}
}

func TestCurrencyMismatchPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on currency mismatch")
		}
	}()
	MustParse("1", "CNY").Add(MustParse("1", "USD"))
}

func TestJSONRoundTrip(t *testing.T) {
	m := MustParse("12.3405", "CNY")
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Money
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Cmp(m) != 0 || back.Currency() != m.Currency() {
		t.Fatalf("round trip mismatch: %s vs %s", back, m)
	}
}

func TestMinorStringRoundTrip(t *testing.T) {
	m := MustParse("123.456789", "CNY")
	if m.MinorString() != "123456789" {
		t.Fatalf("MinorString = %s", m.MinorString())
	}
	v, ok := new(big.Int).SetString(m.MinorString(), 10)
	if !ok {
		t.Fatal("bad minor string")
	}
	if got := FromMinorBig(v, "CNY"); got.Cmp(m) != 0 {
		t.Fatalf("FromMinorBig round trip: %s", got)
	}
}
