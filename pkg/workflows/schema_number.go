package workflows

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
)

// schemaDecimal keeps significant digits and decimal order separate. Its storage
// follows the encoded number's length, even for very large decimal exponents.
type schemaDecimal struct {
	sign   int
	digits string
	order  *big.Int
}

func parseSchemaDecimal(value any) (schemaDecimal, bool) {
	var text string
	switch value := value.(type) {
	case int:
		text = strconv.Itoa(value)
	case int64:
		text = strconv.FormatInt(value, 10)
	case float64:
		text = strconv.FormatFloat(value, 'g', -1, 64)
	case json.Number:
		text = value.String()
	case string:
		text = value
		// Preserve legacy inline-schema coercion of numeric strings.
		if !decimalNumberPattern.MatchString(text) {
			if number, ok := asFloat(value); ok {
				text = strconv.FormatFloat(number, 'g', -1, 64)
			}
		}
	default:
		return schemaDecimal{}, false
	}
	if !decimalNumberPattern.MatchString(text) {
		return schemaDecimal{}, false
	}
	sign := 1
	if text[0] == '-' {
		sign = -1
		text = text[1:]
	} else if text[0] == '+' {
		text = text[1:]
	}
	exponent := new(big.Int)
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		exponent.SetString(text[index+1:], 10)
		text = text[:index]
	}
	places := 0
	if index := strings.IndexByte(text, '.'); index >= 0 {
		places = len(text) - index - 1
		text = text[:index] + text[index+1:]
	}
	digits := strings.TrimLeft(text, "0")
	if digits == "" {
		return schemaDecimal{}, true
	}
	order := big.NewInt(int64(len(digits) - places))
	order.Add(order, exponent)
	return schemaDecimal{sign: sign, digits: strings.TrimRight(digits, "0"), order: order}, true
}

func (number schemaDecimal) isInteger() bool {
	return number.sign == 0 || number.order.Cmp(big.NewInt(int64(len(number.digits)))) >= 0
}

func (number schemaDecimal) compare(other schemaDecimal) int {
	if number.sign < other.sign {
		return -1
	}
	if number.sign > other.sign {
		return 1
	}
	if number.sign == 0 {
		return 0
	}
	if comparison := number.order.Cmp(other.order); comparison != 0 {
		return number.sign * comparison
	}
	// Trailing zeroes were removed, so lexical order matches the significands.
	return number.sign * strings.Compare(number.digits, other.digits)
}
