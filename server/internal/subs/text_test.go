package subs

import "testing"

// TestDecodeUTF8 — обычный случай: файл уже в UTF-8, иногда с BOM.
func TestDecodeUTF8(t *testing.T) {
	const text = "1\n00:00:01,000 --> 00:00:02,000\nПривет, ёжик\n"
	for _, prefix := range []string{"", bom} {
		if got := decode([]byte(prefix + text)); got != text {
			t.Errorf("prefix %q: got %q, want %q", prefix, got, text)
		}
	}
}

// TestDecodeCP1251 — половина русских паков приезжает именно такой.
func TestDecodeCP1251(t *testing.T) {
	// «Привет, ёжик» в Windows-1251.
	raw := []byte{
		0xCF, 0xF0, 0xE8, 0xE2, 0xE5, 0xF2, ',', ' ', 0xB8, 0xE6, 0xE8, 0xEA,
	}
	if got, want := decode(raw), "Привет, ёжик"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestDecodeKeepsASCII — латиница и разметка не должны пострадать при разборе
// файла, который в UTF-8 не разобрался.
func TestDecodeKeepsASCII(t *testing.T) {
	raw := append([]byte("<i>Ok</i> "), 0xC0) // хвост делает файл не-UTF-8
	if got, want := decode(raw), "<i>Ok</i> А"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCP1251Table сверяет таблицу с определением кодировки, а не с самой собой.
//
// Опечатка в одной строке таблицы — это одна буква, молча испорченная
// в каждой реплике, и заметить её можно только на экране телевизора.
func TestCP1251Table(t *testing.T) {
	// Кириллица в Windows-1251 лежит одним непрерывным куском:
	// 0xC0-0xFF это U+0410-U+044F, от «А» до «я» без единой дырки.
	for b := 0xC0; b <= 0xFF; b++ {
		if got, want := cp1251[b-0x80], rune(0x0410+b-0xC0); got != want {
			t.Errorf("байт %#x: %U, ожидалось %U", b, got, want)
		}
	}
	// Ё и ё вынесены из этого куска, как и знак номера — самые частые
	// места, где таблицу и путают.
	for _, c := range []struct {
		b    int
		want rune
	}{
		{0xA8, 'Ё'}, {0xB8, 'ё'}, {0xB9, '№'},
		{0x97, '—'}, {0xAB, '«'}, {0xBB, '»'},
		// Невидимое записано escape-ами намеренно: NBSP в исходнике
		// не отличить от пробела, а U+FFFD — от любого другого
		// нечитаемого символа, и следующая правка обнулила бы кейс.
		{0xA0, '\u00a0'},
		{0x98, '\ufffd'}, // единственный неопределённый байт кодировки
	} {
		if got := cp1251[c.b-0x80]; got != c.want {
			t.Errorf("байт %#x: %U, ожидалось %U", c.b, got, c.want)
		}
	}
}
