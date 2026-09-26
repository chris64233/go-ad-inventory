package adinventory

import (
	"testing"
)

func TestParseMoneyAndArithmetic(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"100", "100"},
		{"12.345", "12.345"},
		{"-0.01", "-0.01"},
		{"+1.50", "1.5"},
		{"0.000", "0"},
		{"1000.00", "1000"},
	}
	for _, c := range cases {
		m, err := ParseMoney(c.in)
		if err != nil {
			t.Fatalf("ParseMoney(%q): %v", c.in, err)
		}
		if got := m.String(); got != c.want {
			t.Errorf("ParseMoney(%q).String() = %q, want %q", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"", " ", "abc", "1.2.3", "1e3", "1,000", ".", "-"} {
		if _, err := ParseMoney(bad); err == nil {
			t.Errorf("ParseMoney(%q) expected error", bad)
		}
	}
}

func TestMoneyPrecision(t *testing.T) {
	// 高精度十进制：0.1 + 0.2 必须精确等于 0.3，且可表达任意精度。
	a, _ := ParseMoney("0.1")
	b, _ := ParseMoney("0.2")
	want, _ := ParseMoney("0.3")
	if got := a.Add(b); got.Cmp(want) != 0 {
		t.Fatalf("0.1+0.2 = %s, want 0.3", got)
	}

	// 不同精度对齐运算。
	x, _ := ParseMoney("1.0001")
	y, _ := ParseMoney("2.0000000001")
	got := x.Add(y)
	want2, _ := ParseMoney("3.0001000001")
	if got.Cmp(want2) != 0 {
		t.Fatalf("1.0001+2.0000000001 = %s", got)
	}
	if got.String() != "3.0001000001" {
		t.Fatalf("precision lost: %s", got)
	}

	// 文本往返。
	raw, err := got.MarshalText()
	if err != nil || string(raw) != "3.0001000001" {
		t.Fatalf("MarshalText = %q, %v", raw, err)
	}
	var back Money
	if err := back.UnmarshalText(raw); err != nil || back.Cmp(got) != 0 {
		t.Fatalf("round trip failed: %s %v", back, err)
	}
}

func TestMoneyCmpAndSign(t *testing.T) {
	m1, _ := ParseMoney("1.10")
	m2, _ := ParseMoney("1.1")
	m3, _ := ParseMoney("-1.1")
	if m1.Cmp(m2) != 0 {
		t.Errorf("1.10 != 1.1")
	}
	if m2.Sub(m1).Sign() != 0 {
		t.Errorf("1.1-1.10 != 0")
	}
	if !m3.IsNegative() || m3.String() != "-1.1" {
		t.Errorf("negative handling: %s", m3)
	}
	if !MoneyZero().IsZero() || !(Money{}).IsZero() {
		t.Errorf("zero value should be zero money")
	}
}
