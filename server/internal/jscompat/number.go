// Package jscompat воспроизводит те правила JavaScript, в которых Go ведёт себя
// иначе. Пакет существует не ради чистоты: контракт с телевизором заморожен
// байт-в-байт, а Tizen 2.3 ловит расхождения молча — приложение просто гаснет.
// Всё, что зависит от семантики JS, живёт здесь и здесь же покрыто тестами.
package jscompat

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
)

// decimalLiteral — StrDecimalLiteral из ECMA-262 (StringNumericLiteral).
// Разделитель-подчёркивание в строковой форме не разрешён, поэтому Number("1_000")
// это NaN — в отличие от strconv.ParseFloat, который его принимает.
var decimalLiteral = regexp.MustCompile(`^[+-]?(?:(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?)$`)

// ToNumber повторяет ECMAScript ToNumber для строки (`Number(s)`).
//
// Отличий от strconv.ParseFloat столько, что подменять одно другим нельзя:
// JS принимает "0x10", "0b101", "0o17", "Infinity", "" (это 0) и отвергает
// "nan", "inf", "1_000", "0x1p-2" и любой мусор в хвосте; ParseFloat — ровно наоборот.
func ToNumber(s string) float64 {
	s = TrimJS(s)
	if s == "" { // Number("") === 0, и Number(" \n ") тоже
		return 0
	}

	// Знак разрешён только у десятичной формы: Number("-0x10") === NaN.
	if len(s) > 2 && s[0] == '0' {
		var base int
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			n, ok := new(big.Int).SetString(s[2:], base)
			if !ok {
				return math.NaN()
			}
			f, _ := new(big.Float).SetInt(n).Float64()
			return f
		}
	}

	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}

	if !decimalLiteral.MatchString(s) {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// Переполнение: Number("1e400") === Infinity, а ParseFloat отдаёт
		// ±Inf вместе с ErrRange — значение уже верное, ошибку игнорируем.
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f
		}
		return math.NaN()
	}
	return f
}

// Or0 — это хвост `|| 0` в выражениях вида `Number(x) || 0`.
//
// Ровно поэтому оно не называется «распарсить или ноль»: обнуляются NaN и ноль,
// а вот отрицательные значения проходят насквозь. ffprobe отдаёт level: -99
// для неизвестного уровня, и canCopyVideo его пропускает (-99 > 41 — ложь).
// Это латентный баг Node-сервера; он воспроизведён намеренно, а не пропущен.
func Or0(f float64) float64 {
	if math.IsNaN(f) || f == 0 {
		return 0
	}
	return f
}

// NumberOr0 — `Number(s) || 0`.
func NumberOr0(s string) float64 { return Or0(ToNumber(s)) }

// ToNumberAny — `Number(x)` для значения, разобранного из JSON в any.
//
// В JS Number(undefined) это NaN, а Number(null) это 0; в Go оба приходят
// как nil. Расхождение неразличимо: каждая точка вызова в server.mjs
// заканчивается на `|| 0`, который сводит NaN и 0 к одному ответу.
func ToNumberAny(v any) float64 {
	switch t := v.(type) {
	case nil:
		return math.NaN()
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return t
	case string:
		return ToNumber(t)
	default:
		return math.NaN()
	}
}

// NumberAnyOr0 — `Number(x) || 0` для значения из JSON.
func NumberAnyOr0(v any) float64 { return Or0(ToNumberAny(v)) }

// ToFixed повторяет Number.prototype.toFixed.
//
// Расхождение, из-за которого нельзя взять strconv.FormatFloat: спецификация
// требует «при двух равноудалённых кандидатах брать больший», то есть половины
// округляются вверх, а FormatFloat округляет к чётному. (0.0625).toFixed(3)
// в JS даёт "0.063", FormatFloat — "0.062". Знак снимается до округления,
// поэтому для неотрицательных значений (а start всегда прошёл через Math.max(0, …))
// это совпадает с «от нуля», что и делает big.Rat.FloatString.
func ToFixed(f float64, digits int) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	// toFixed уходит в экспоненциальную запись начиная с 1e21.
	if math.Abs(f) >= 1e21 {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return new(big.Rat).SetFloat64(f).FloatString(digits)
}

// Base36 — Number.prototype.toString(36). В JS цифры строчные, у Go тоже.
func Base36(n int64) string { return strconv.FormatInt(n, 36) }

// IsInteger — Number.isInteger. Используется getFile: индекс из пути должен быть
// целым и попадать в границы списка файлов, иначе 404.
func IsInteger(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f)
}
