package jscompat

import (
	"regexp"
	"strings"
	"unicode/utf16"
)

// JSWhitespace — StrWhiteSpace из ECMA-262: WhiteSpace + LineTerminator.
// Тот же набор стоит за `\s` в регулярках JS. Записан escape-ами намеренно:
// половина этих символов невидима, и литералами набор не отревьюишь.
//
// Go считает пробельными только [\t\n\f\r ] в `\s`, а unicode.IsSpace (то есть
// strings.TrimSpace) берёт третий набор: добавляет U+0085 и НЕ включает U+FEFF.
// Поэтому ни `\s`, ни TrimSpace здесь использовать нельзя.
const JSWhitespace = "\u0009\u000a\u000b\u000c\u000d\u0020\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

// jsSpaceRun — аналог /\s+/ из JS.
var jsSpaceRun = regexp.MustCompile(
	`[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`)

// CollapseWhitespace — .replace(/\s+/g, ' ').
//
// Практическая цена ошибки: в заголовке дорожки рипа регулярно встречается
// NBSP. Node его схлопывает, наивный Go — нет; получается другой label,
// от label зависит суффикс дизамбигуации, от суффикса — code, а телевизор
// хранит выбранную дорожку в localStorage именно как code.
func CollapseWhitespace(s string) string { return jsSpaceRun.ReplaceAllString(s, " ") }

// TrimJS — String.prototype.trim.
func TrimJS(s string) string { return strings.Trim(s, JSWhitespace) }

// TruncateUTF16 повторяет `s.length > max ? s.slice(0, max-1) + suffix : s`.
//
// В JS .length и .slice считают code unit'ы UTF-16, а не руны. Для кириллицы
// (BMP) это совпадает, для эмодзи — нет, и JS вдобавок способен разрезать
// суррогатную пару. Go такую половинку представить не может: на месте разреза
// получится U+FFFD. Расхождение остаточное и осознанное.
func TruncateUTF16(s string, max int, suffix string) string {
	u := utf16.Encode([]rune(s))
	if len(u) <= max {
		return s
	}
	return string(utf16.Decode(u[:max-1])) + suffix
}
