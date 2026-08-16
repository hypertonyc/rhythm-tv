package media

import "strings"

// NaturalLess сравнивает пути файлов так, как их читает человек: цепочки цифр
// сравниваются как числа, а не посимвольно. Без этого «Episode 10» встаёт между
// «Episode 1» и «Episode 2», а «Season 9» — после «Season 10».
//
// Регистр при сравнении складывается, но не теряется: если строки различаются
// только им, решает исходная пара. Порядок обязан быть строгим и полным —
// иначе sort.Slice на равных ключах переставляет серии от запуска к запуску.
func NaturalLess(a, b string) bool {
	if c := naturalCompare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c < 0
	}
	return a < b
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

// naturalCompare идёт по байтам, а не по рунам, и это верно для UTF-8:
// порядок байтов в нём совпадает с порядком кодовых точек, а цифры,
// которые тут единственные разбираются особо, лежат в ASCII.
func naturalCompare(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isASCIIDigit(a[i]) && isASCIIDigit(b[j]) {
			si, sj := i, j
			for i < len(a) && isASCIIDigit(a[i]) {
				i++
			}
			for j < len(b) && isASCIIDigit(b[j]) {
				j++
			}
			// Ведущие нули только мешают: «09» и «9» это одно число.
			// После их снятия числа одной длины сравниваются лексикографически
			// — для цифр это то же самое, что численно, и без переполнения.
			na := strings.TrimLeft(a[si:i], "0")
			nb := strings.TrimLeft(b[sj:j], "0")
			if len(na) != len(nb) {
				if len(na) < len(nb) {
					return -1
				}
				return 1
			}
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
			continue
		}
		if a[i] != b[j] {
			if a[i] < b[j] {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	switch {
	case i == len(a) && j == len(b):
		return 0
	case i == len(a):
		return -1
	default:
		return 1
	}
}
