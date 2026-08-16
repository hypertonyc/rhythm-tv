package jscompat

import (
	"math"
	"testing"
)

// Ожидания снимались с реального движка JS, а не выводились из спецификации.
// Проверить любую строку можно так:
//   docker run --rm node:22-slim node -e 'console.log(Number("0x10"))'

func TestToNumber(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)

	cases := []struct {
		in   string
		want float64
	}{
		// Пустое и пробельное — ноль, а не ошибка.
		{"", 0},
		{"   ", 0},
		{"\t\n\r ", 0},
		{"\u00a0\ufeff", 0}, // NBSP и BOM тоже StrWhiteSpace
		// Обычные десятичные.
		{"0", 0},
		{"42", 42},
		{"-42", -42},
		{"+1.5", 1.5},
		{"1.5", 1.5},
		{".5", 0.5},
		{"5.", 5},
		{"1e3", 1000},
		{"1E3", 1000},
		{"1e-3", 0.001},
		{"  12.75  ", 12.75},
		{"0012", 12},
		// Недесятичные основания: знак перед ними запрещён.
		{"0x10", 16},
		{"0X10", 16},
		{"0b101", 5},
		{"0o17", 15},
		{"-0x10", nan},
		{"0x", nan},
		{"0xzz", nan},
		// Infinity — только с большой буквы и целиком.
		{"Infinity", inf},
		{"+Infinity", inf},
		{"-Infinity", math.Inf(-1)},
		{"infinity", nan},
		{"inf", nan},
		{"nan", nan},
		{"NaN", nan},
		// То, что принимает strconv.ParseFloat, но не принимает JS.
		{"1_000", nan},
		{"0x1p-2", nan},
		// Мусор в хвосте и внутри.
		{"12abc", nan},
		{"1 2", nan},
		{"--1", nan},
		{"1e", nan},
		// Переполнение — это Infinity, а не ошибка.
		{"1e400", inf},
	}

	for _, c := range cases {
		got := ToNumber(c.in)
		if math.IsNaN(c.want) {
			if !math.IsNaN(got) {
				t.Errorf("ToNumber(%q) = %v, ожидался NaN", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("ToNumber(%q) = %v, ожидалось %v", c.in, got, c.want)
		}
	}
}

func TestOr0(t *testing.T) {
	// Суть проверки — предпоследняя строка: отрицательное значение проходит
	// насквозь, и именно поэтому ffprobe-шный level:-99 доживает до canCopyVideo.
	cases := []struct{ in, want float64 }{
		{math.NaN(), 0},
		{0, 0},
		{math.Copysign(0, -1), 0},
		{42, 42},
		{-99, -99},
		{math.Inf(1), math.Inf(1)},
	}
	for _, c := range cases {
		if got := Or0(c.in); got != c.want {
			t.Errorf("Or0(%v) = %v, ожидалось %v", c.in, got, c.want)
		}
	}
}

func TestToFixed(t *testing.T) {
	// 0.0625 — тот самый случай: JS округляет половину вверх, strconv.FormatFloat
	// (round-half-to-even) дал бы "0.062".
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.000"},
		{0.0625, "0.063"},
		{1.0005, "1.000"}, // двоичное представление чуть меньше половины
		{12.3456, "12.346"},
		{1234.5, "1234.500"},
		{90, "90.000"},
		{123.4565, "123.457"},
		// Классика двоичного представления: обе «половины» на самом деле чуть
		// меньше половины, поэтому вверх не округляются.
		{2.675, "2.675"},
		{1.005, "1.005"},
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
	}
	for _, c := range cases {
		if got := ToFixed(c.in, 3); got != c.want {
			t.Errorf("ToFixed(%v, 3) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}

func TestBase36(t *testing.T) {
	// Date.now().toString(36) на фиксированной метке времени.
	if got := Base36(1755302400000); got != "medhq800" {
		t.Errorf("Base36(1755302400000) = %q", got)
	}
	if got := Base36(0); got != "0" {
		t.Errorf("Base36(0) = %q", got)
	}
}

func TestCollapseWhitespaceAndTrim(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"a   b", "a b"},
		{"  a\t\nb  ", " a b "},
		// Ради этой строки пакет и существует: с Go-шным \s NBSP уцелел бы,
		// label получился бы другим, а следом — другой code дорожки.
		{"Дубляж\u00a0(TVShows)", "Дубляж (TVShows)"},
		{"a\u2003\u2003b", "a b"},
		{"a\ufeffb", "a b"},
	}
	for _, c := range cases {
		if got := CollapseWhitespace(c.in); got != c.want {
			t.Errorf("CollapseWhitespace(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}

	trims := []struct{ in, want string }{
		{"  abc  ", "abc"},
		{"\u00a0abc\u00a0", "abc"},
		{"\ufeffabc\ufeff", "abc"}, // strings.TrimSpace это НЕ снимает
		{"abc", "abc"},
	}
	for _, c := range trims {
		if got := TrimJS(c.in); got != c.want {
			t.Errorf("TrimJS(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}

func TestTruncateUTF16(t *testing.T) {
	const (
		max = 40
		ell = "…"
	)
	short := "Дубляж"
	if got := TruncateUTF16(short, max, ell); got != short {
		t.Errorf("короткая строка изменилась: %q", got)
	}

	// Ровно 40 code unit'ов — обрезки быть не должно.
	// Буквы вне диапазона hex намеренно: строка из 40 цифр выглядит как
	// токен для скана секретов в CI, и ослаблять скан ради теста незачем.
	exact := "0123456789GHIJKLMNOP0123456789GHIJKLMNOP"
	if got := TruncateUTF16(exact, max, ell); got != exact {
		t.Errorf("строка длиной ровно 40 обрезана: %q", got)
	}

	// 41 символ — обрезаем до 39 плюс многоточие.
	long := exact + "X"
	want := exact[:39] + ell
	if got := TruncateUTF16(long, max, ell); got != want {
		t.Errorf("TruncateUTF16(41 символ) = %q, ожидалось %q", got, want)
	}

	// Кириллица: в BMP один code unit на руну, поэтому счёт совпадает с рунным.
	cyr := "Труднопроизносимое очень длинное название дорожки"
	got := TruncateUTF16(cyr, max, ell)
	if []rune(got)[39] != '…' || len([]rune(got)) != 40 {
		t.Errorf("кириллица обрезана неверно: %q (%d рун)", got, len([]rune(got)))
	}
}

func TestMarshalDoesNotEscapeHTMLOrAppendNewline(t *testing.T) {
	b, err := Marshal(map[string]string{"torrent": "Tom & Jerry <1965>"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"torrent":"Tom & Jerry <1965>"}`
	if string(b) != want {
		t.Errorf("Marshal = %s, ожидалось %s", b, want)
	}
}

func TestMarshalFloatFormatting(t *testing.T) {
	// Go форматирует float64 так же, как Number#toString: целое печатается
	// без .0, а длинный хвост — полностью.
	cases := []struct {
		in   float64
		want string
	}{
		{1440, "1440"},
		{0, "0"},
		{0.5, "0.5"},
		{2696.482, "2696.482"},
	}
	for _, c := range cases {
		b, err := Marshal(c.in)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("Marshal(%v) = %s, ожидалось %s", c.in, b, c.want)
		}
	}

	// NaN/Inf в JS дают null, в Go — ошибку маршалинга. Отсюда защита от
	// деления на ноль в progress: без неё /api/status начал бы отдавать 500.
	if _, err := Marshal(math.NaN()); err == nil {
		t.Error("Marshal(NaN) обязан вернуть ошибку — иначе защита в progress не нужна")
	}
}

func TestMarshalEmptySliceIsNotNull(t *testing.T) {
	// Клиент делает for (i = 0; i < meta.audio.length; i++). Если сюда уедет
	// null, на телевизоре это исключение в window.onerror и погасший экран.
	b, err := Marshal(map[string]any{"audio": []int{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"audio":[]}` {
		t.Errorf("пустой слайс сериализован как %s", b)
	}
}

// TestMarshalExponentialBoundary — progress в /api/status на старте бывает
// меньше 1e-6, а там оба языка переключаются на экспоненциальную запись.
// Значения сняты с node:22-slim, границы проверены с обеих сторон.
func TestMarshalExponentialBoundary(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{5.842955754641234e-05, "0.00005842955754641234"},
		{1e-6, "0.000001"}, // порог: ещё десятичная запись
		{9.5e-7, "9.5e-7"}, // уже экспоненциальная
		{1e-7, "1e-7"},     // Go сам приводит e-07 к e-7, как и JS
		{1e-21, "1e-21"},
		{1e21, "1e+21"},
		{sumFloat(0.1, 0.2), "0.30000000000000004"},
	}
	for _, c := range cases {
		b, err := Marshal(c.in)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("Marshal(%v) = %s, ожидалось %s", c.in, b, c.want)
		}
	}
}

// sumFloat не даёт компилятору свернуть выражение.
//
// В Go нетипизированные константные выражения вычисляются на этапе компиляции
// с произвольной точностью, поэтому литеральное 0.1+0.2 это ровно 0.3 —
// а в JS то же самое считается во float64 и даёт 0.30000000000000004.
// Проверять надо арифметику времени выполнения, иначе тест доказывает не то.
//
//go:noinline
func sumFloat(a, b float64) float64 { return a + b }
