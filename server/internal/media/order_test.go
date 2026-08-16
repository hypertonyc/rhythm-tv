package media

import (
	"sort"
	"testing"
)

func TestNaturalLessNumbersCompareAsNumbers(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		// Ради чего всё и затевалось.
		{"Season 2/e01.mkv", "Season 10/e01.mkv", true},
		{"Season 10/e01.mkv", "Season 2/e01.mkv", false},
		{"ep2.mkv", "ep10.mkv", true},
		// Ведущие нули не считаются: «09» это «9».
		{"s09e01.mkv", "s9e02.mkv", true},
		{"s9e02.mkv", "s09e01.mkv", false},
		// Настоящие имена из пака «Друзья»: сезон в каталоге, серия в имени.
		{"Сезон 09/s09e06 - The One with the Male Nanny.mkv",
			"Сезон 09/s09e23-24 - The One in Barbados.mkv", true},
		{"Сезон 09/s09e23-24 - The One in Barbados.mkv",
			"Сезон 10/s10e01 - The One After Joey and Rachel Kiss.mkv", true},
		// Строгость: одинаковые строки не меньше друг друга ни в какую сторону.
		{"a.mkv", "a.mkv", false},
		// Префикс короче целого.
		{"S01E01.mkv", "S01E01 - Pilot.mkv", false},
		// Регистр складывается, но порядок остаётся полным.
		{"a.mkv", "B.mkv", true},
		{"B.mkv", "a.mkv", false},
	}
	for _, c := range cases {
		if got := NaturalLess(c.a, c.b); got != c.want {
			t.Errorf("NaturalLess(%q, %q) = %v, ожидалось %v", c.a, c.b, got, c.want)
		}
	}
}

// TestNaturalLessIsStrictOrder — sort.Slice на нестрогом сравнении не падает,
// а тихо переставляет элементы по-разному от запуска к запуску. Проверяем,
// что различающиеся только регистром имена всё-таки упорядочены.
func TestNaturalLessIsStrictOrder(t *testing.T) {
	names := []string{"E01.mkv", "e01.mkv", "e1.mkv", "E1.mkv"}
	for _, a := range names {
		for _, b := range names {
			if a == b {
				if NaturalLess(a, b) {
					t.Errorf("NaturalLess(%q, %q) = true на равных строках", a, b)
				}
				continue
			}
			if NaturalLess(a, b) == NaturalLess(b, a) {
				t.Errorf("NaturalLess(%q, %q) и обратное совпали", a, b)
			}
		}
	}
}

func TestNaturalLessSortsEpisodeList(t *testing.T) {
	// Порядок, в котором файлы лежат в паке «Друзья»: по убыванию размера.
	got := []string{
		"Сезон 09/s09e23-24 - The One in Barbados.mkv",
		"Сезон 10/s10e17-18 - The Last One.mkv",
		"Сезон 09/s09e06 - The One with the Male Nanny.mkv",
		"Сезон 10/s10e01 - The One After Joey and Rachel Kiss.mkv",
		"Сезон 09/s09e13 - The One Where Monica Sings.mkv",
	}
	want := []string{
		"Сезон 09/s09e06 - The One with the Male Nanny.mkv",
		"Сезон 09/s09e13 - The One Where Monica Sings.mkv",
		"Сезон 09/s09e23-24 - The One in Barbados.mkv",
		"Сезон 10/s10e01 - The One After Joey and Rachel Kiss.mkv",
		"Сезон 10/s10e17-18 - The Last One.mkv",
	}
	sort.SliceStable(got, func(i, j int) bool { return NaturalLess(got[i], got[j]) })
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("позиция %d: %q, ожидалось %q", i, got[i], want[i])
		}
	}
}
