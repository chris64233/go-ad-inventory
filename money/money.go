// Package money 提供精确的金额表示。
//
// 内部以 10^-6 元为最小单位（big.Int）存储，全程不使用浮点数，
// 避免二进制浮点导致的预算计算误差。
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Scale 是金额的小数位数（10^-6，即微米）。
const Scale = 6

var scalePow = big.NewInt(1e6)

// Money 是不可变金额：value 为以 10^-Scale 为单位的整数，currency 为 ISO 货币码。
type Money struct {
	value    *big.Int
	currency string
}

// zero 返回一个合法的 0 值（value 永不为 nil）。
func normalize(v *big.Int, currency string) Money {
	if v == nil {
		v = new(big.Int)
	}
	return Money{value: new(big.Int).Set(v), currency: strings.ToUpper(currency)}
}

// Zero 返回指定货币的 0 金额。
func Zero(currency string) Money {
	return normalize(nil, currency)
}

// FromMinor 以最小单位（10^-Scale）的整数构造金额。
func FromMinor(minorUnits int64, currency string) Money {
	return normalize(big.NewInt(minorUnits), currency)
}

// FromMinorBig 以最小单位的任意精度整数构造金额。
func FromMinorBig(minorUnits *big.Int, currency string) Money {
	return normalize(minorUnits, currency)
}

// MinorString 返回最小单位整数的十进制字符串（无损）。
func (m Money) MinorString() string {
	return m.intVal().String()
}

// Parse 解析十进制字符串，例如 "12.34"。超过 Scale 位小数会报错而不是静默截断。
func Parse(s, currency string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Money{}, errors.New("money: empty amount")
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
		return Money{}, errors.New("money: invalid amount")
	}
	parts := strings.SplitN(s, ".", 2)
	intPart, fracPart := parts[0], ""
	if len(parts) == 2 {
		fracPart = parts[1]
	}
	if intPart == "" || fracPart == "" && len(parts) == 2 {
		return Money{}, fmt.Errorf("money: invalid amount %q", s)
	}
	for _, r := range intPart + fracPart {
		if r < '0' || r > '9' {
			return Money{}, fmt.Errorf("money: invalid amount %q", s)
		}
	}
	if len(fracPart) > Scale {
		return Money{}, fmt.Errorf("money: amount %q exceeds max precision %d", s, Scale)
	}
	if len(fracPart) < Scale {
		fracPart += strings.Repeat("0", Scale-len(fracPart))
	}
	v, ok := new(big.Int).SetString(intPart+fracPart, 10)
	if !ok {
		return Money{}, fmt.Errorf("money: invalid amount %q", s)
	}
	if neg {
		v.Neg(v)
	}
	return normalize(v, currency), nil
}

// MustParse 与 Parse 相同，失败时 panic，仅用于测试与常量。
func MustParse(s, currency string) Money {
	m, err := Parse(s, currency)
	if err != nil {
		panic(err)
	}
	return m
}

// Currency 返回 ISO 货币码。
func (m Money) Currency() string { return m.currency }

func (m Money) intVal() *big.Int {
	if m.value == nil {
		return new(big.Int)
	}
	return m.value
}

// IsSameCurrency 判断两笔金额货币是否一致。
func (m Money) IsSameCurrency(o Money) bool {
	return m.currency == o.currency
}

func (m Money) checkCurrency(o Money) {
	if m.currency != o.currency {
		panic(fmt.Sprintf("money: currency mismatch %s vs %s", m.currency, o.currency))
	}
}

// Add 返回 m+o，不修改接收者。
func (m Money) Add(o Money) Money {
	m.checkCurrency(o)
	return normalize(new(big.Int).Add(m.intVal(), o.intVal()), m.currency)
}

// Sub 返回 m-o，不修改接收者。
func (m Money) Sub(o Money) Money {
	m.checkCurrency(o)
	return normalize(new(big.Int).Sub(m.intVal(), o.intVal()), m.currency)
}

// Cmp 返回 -1/0/1，分别表示 m 小于/等于/大于 o。
func (m Money) Cmp(o Money) int {
	m.checkCurrency(o)
	return m.intVal().Cmp(o.intVal())
}

// IsZero / IsPositive / IsNegative 是常用的比较快捷方式。
func (m Money) IsZero() bool     { return m.intVal().Sign() == 0 }
func (m Money) IsPositive() bool { return m.intVal().Sign() > 0 }
func (m Money) IsNegative() bool { return m.intVal().Sign() < 0 }

// Min 返回两笔金额中较小者。
func Min(a, b Money) Money {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}

// String 渲染为去掉尾随 0 的十进制字符串，例如 "12.34"。
func (m Money) String() string {
	v := m.intVal()
	neg := v.Sign() < 0
	abs := new(big.Int).Abs(v)
	q, r := new(big.Int).QuoRem(abs, scalePow, new(big.Int))
	whole := q.String()
	frac := fmt.Sprintf("%0*d", Scale, r)
	frac = strings.TrimRight(frac, "0")
	out := whole
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// MarshalJSON 将金额序列化为 {"amount":"12.34","currency":"CNY"}。
func (m Money) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`{"amount":%q,"currency":%q}`, m.String(), m.currency)), nil
}

// jsonMoney 是 Money 的 JSON 线格式。
type jsonMoney struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// UnmarshalJSON 与 MarshalJSON 互逆。
func (m *Money) UnmarshalJSON(data []byte) error {
	var jm jsonMoney
	if err := json.Unmarshal(data, &jm); err != nil {
		return err
	}
	parsed, err := Parse(jm.Amount, jm.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
