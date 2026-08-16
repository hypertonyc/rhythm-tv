package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/avdav/torrent-media/server/internal/mediasource"
)

// friendsOrder — как файлы уложены в паке «Друзья»: по убыванию размера,
// сезоны и серии вперемешку. Порядок метаинформации здесь несёт ноль смысла.
var friendsOrder = []string{
	"Сезон 09/s09e23-24 - The One in Barbados.mkv",
	"Сезон 10/s10e17-18 - The Last One.mkv",
	"Сезон 09/s09e06 - The One with the Male Nanny.mkv",
	"постер.jpg",
	"Сезон 10/s10e01 - The One After Joey and Rachel Kiss.mkv",
	"Сезон 09/s09e13 - The One Where Monica Sings.mkv",
}

func newFriendsServer(t *testing.T) (*Server, mediasource.Source) {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, 0, len(friendsOrder))
	for _, rel := range friendsOrder {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	src, err := mediasource.NewFake("Друзья", paths...)
	if err != nil {
		t.Fatal(err)
	}
	calls := make([]string, 0)
	lib := &fakeLibrary{calls: &calls, current: src, activeID: strings.Repeat("c", 40)}
	return New(Deps{Library: lib, HLS: &fakeSessions{calls: &calls}}), src
}

// TestFilesAreSortedByPathNotByMetainfoOrder — серии в /api/files идут по пути
// в торренте, а не в порядке метаинформации. В паке «Друзья» файлы уложены
// по убыванию размера, и без сортировки меню телевизора внутри сезона шло
// s09e23-24, s09e06, s09e13.
//
// Индексы при этом ОСТАЮТСЯ индексами файлов в торренте — по ним телевизор
// хранит позиции просмотра, а сервер ищет файл для /raw и /api/start.
func TestFilesAreSortedByPathNotByMetainfoOrder(t *testing.T) {
	s, _ := newFriendsServer(t)

	rec := do(s, http.MethodGet, "/api/files", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	want := `{"torrent":"Друзья","files":[` +
		`{"index":2,"name":"s09e06 - The One with the Male Nanny.mkv","length":1},` +
		`{"index":5,"name":"s09e13 - The One Where Monica Sings.mkv","length":1},` +
		`{"index":0,"name":"s09e23-24 - The One in Barbados.mkv","length":1},` +
		`{"index":4,"name":"s10e01 - The One After Joey and Rachel Kiss.mkv","length":1},` +
		`{"index":1,"name":"s10e17-18 - The Last One.mkv","length":1}]}`
	if got := rec.Body.String(); got != want {
		t.Errorf("тело:\n%s\nожидалось:\n%s", got, want)
	}
}

// TestNeighboursFollowSortedOrder — автопереход считается по тому же порядку.
// Без сортировки next после первой серии девятого сезона указывал на «The Last
// One», то есть на финал сериала: в метаинформации она стоит следующей по размеру.
func TestNeighboursFollowSortedOrder(t *testing.T) {
	s, src := newFriendsServer(t)

	// Индексы в торренте: 2 = s09e06, 5 = s09e13, 0 = s09e23-24,
	// 4 = s10e01, 1 = s10e17-18.
	cases := []struct {
		index      int
		next, prev *int
	}{
		{2, ptr(5), nil},
		{5, ptr(0), ptr(2)},
		{0, ptr(4), ptr(5)},
		{4, ptr(1), ptr(0)},
		{1, nil, ptr(4)},
		// Не видеофайл: в списке серий его нет, соседей тоже.
		{3, nil, nil},
	}
	for _, c := range cases {
		next, prev := s.neighbours(src, c.index)
		if !samePtr(next, c.next) {
			t.Errorf("index %d: next %s, ожидалось %s", c.index, showPtr(next), showPtr(c.next))
		}
		if !samePtr(prev, c.prev) {
			t.Errorf("index %d: prev %s, ожидалось %s", c.index, showPtr(prev), showPtr(c.prev))
		}
	}
}

// TestFileLookupIgnoresSortOrder — /raw, /api/probe и /api/start ищут файл
// по индексу в торренте. Сортировка списка серий их не касается, и это надо
// удержать: разъехавшись, они начали бы отдавать чужую серию.
func TestFileLookupIgnoresSortOrder(t *testing.T) {
	s, src := newFriendsServer(t)

	for i, rel := range friendsOrder {
		f, ok := s.file(src, i)
		if !ok {
			t.Fatalf("index %d не нашёлся", i)
		}
		if want := filepath.Base(rel); f.Name != want {
			t.Errorf("index %d: %q, ожидалось %q", i, f.Name, want)
		}
	}
}

func ptr(v int) *int { return &v }

func samePtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func showPtr(p *int) string {
	if p == nil {
		return "null"
	}
	return strconv.Itoa(*p)
}
