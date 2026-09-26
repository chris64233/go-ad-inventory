package adinventory

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Money 是不可变的高精度十进制金额，内部以 10 进制定点数表示
// （value = coeff * 10^-scale），不使用二进制浮点，杜绝 0.1+0.2 类误差。
// 零值即金额 0，可直接使用。
type Money struct {
	coeff *big.Int
	scale int
}

var moneyZero = Money{coeff: new(big.Int)}

// MoneyZero 返回金额 0。
func MoneyZero() Money { return moneyZero }

// MoneyFromInt 把整数金额转换为 Money。
func MoneyFromInt(v int64) Money {
	return Money{coeff: big.NewInt(v)}
}

// ParseMoney 解析十进制金额字符串，如 "100"、"12.345"、"-0.01"，
// 不接受千分位、科学计数法等歧义写法。
func ParseMoney(s string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Money{}, errors.New("adinventory: empty money")
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	if s == "" {
		return Money{}, fmt.Errorf("adinventory: invalid money %q", s)
	}

	intPart, fracPart := s, ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart, fracPart = s[:dot], s[dot+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return Money{}, fmt.Errorf("adinventory: invalid money %q", s)
		}
	}
	if intPart == "" || !allDigits(intPart) || fracPart != "" && !allDigits(fracPart) {
		return Money{}, fmt.Errorf("adinventory: invalid money %q", s)
	}

	coeff, ok := new(big.Int).SetString(intPart+fracPart, 10)
	if !ok {
		return Money{}, fmt.Errorf("adinventory: invalid money %q", s)
	}
	if neg {
		coeff.Neg(coeff)
	}
	return normalize(coeff, len(fracPart)), nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// normalize去掉系数末尾多余的 0，统一金额的规范表示。
func normalize(coeff *big.Int, scale int) Money {
	zero := new(big.Int)
	for scale > 0 && new(big.Int).Mod(coeff, big.NewInt(10)).Cmp(zero) == 0 {
		coeff.Quo(coeff, big.NewInt(10))
		scale--
	}
	return Money{coeff: coeff, scale: scale}
}

func (m Money) orZero() Money {
	if m.coeff == nil {
		return moneyZero
	}
	return m
}

// align 把两个金额对齐到同一精度，返回对齐后的系数。
func align(a, b Money) (*big.Int, *big.Int, int) {
	a, b = a.orZero(), b.orZero()
	scale := a.scale
	if b.scale > scale {
		scale = b.scale
	}
	ca := new(big.Int).Mul(a.coeff, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale-a.scale)), nil))
	cb := new(big.Int).Mul(b.coeff, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale-b.scale)), nil))
	return ca, cb, scale
}

// Add 返回 m+o。
func (m Money) Add(o Money) Money {
	ca, cb, scale := align(m, o)
	return normalize(new(big.Int).Add(ca, cb), scale)
}

// Sub 返回 m-o。
func (m Money) Sub(o Money) Money {
	ca, cb, scale := align(m, o)
	return normalize(new(big.Int).Sub(ca, cb), scale)
}

// Cmp 返回 -1/0/1，分别表示 m 小于/等于/大于 o。
func (m Money) Cmp(o Money) int {
	ca, cb, _ := align(m, o)
	return ca.Cmp(cb)
}

// Equal 报告两笔金额是否相等。
func (m Money) Equal(o Money) bool { return m.Cmp(o) == 0 }

// Sign 返回 -1/0/1。
func (m Money) Sign() int {
	if m.coeff == nil {
		return 0
	}
	return m.coeff.Sign()
}

// IsNegative 报告金额是否为负。
func (m Money) IsNegative() bool { return m.Sign() < 0 }

// IsZero 报告金额是否为 0。
func (m Money) IsZero() bool { return m.Sign() == 0 }

// String 返回规范化的十进制字符串。
func (m Money) String() string {
	m = m.orZero()
	if m.scale == 0 {
		return m.coeff.String()
	}
	neg := m.coeff.Sign() < 0
	abs := new(big.Int).Abs(m.coeff).String()
	if len(abs) <= m.scale {
		abs = strings.Repeat("0", m.scale+1-len(abs)) + abs
	}
	out := abs[:len(abs)-m.scale] + "." + abs[len(abs)-m.scale:]
	if neg {
		out = "-" + out
	}
	return out
}

// MarshalText 实现 encoding.TextMarshaler，金额始终以字符串序列化。
func (m Money) MarshalText() ([]byte, error) {
	return []byte(m.String()), nil
}

// UnmarshalText 实现 encoding.TextUnmarshaler。
func (m *Money) UnmarshalText(data []byte) error {
	v, err := ParseMoney(string(data))
	if err != nil {
		return err
	}
	*m = v
	return nil
}
