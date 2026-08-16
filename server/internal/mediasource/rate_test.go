package mediasource

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// TestMeterMatchesNodeGolden — сверка с настоящим npm-пакетом throughput@1.0.1,
// прогнанным по той же последовательности с подменённым Date.now.
//
// Эталон в testdata/rate.golden.json, генератор рядом (rate-gen.mjs).
// Последовательность специально проходит все ветки формулы: разогрев буфера,
// заполнение окна, устойчивый поток, простой длиннее окна и подтиковый вызов,
// при котором время не успело вырасти.
//
// Пересобрать (node в системе нет, поэтому через Docker):
//
//	docker run --rm -v "$PWD/testdata:/w" -w /w node:22-slim sh -c \
//	  'cd /tmp && npm pack throughput@1.0.1 && tar xzf *.tgz && cd /w && node rate-gen.mjs'
func TestMeterMatchesNodeGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/rate.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden []struct {
		Advance int64   `json:"advance"`
		Delta   float64 `json:"delta"`
		Rate    float64 `json:"rate"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden) == 0 {
		t.Fatal("эталон пуст")
	}

	now := int64(1700000000000)
	m := NewMeter(5, func() int64 { return now })

	for i, step := range golden {
		now += step.Advance
		got := m.Add(step.Delta)
		// Сравнение точное: обе стороны считают одни и те же операции
		// над float64 в одном порядке, так что расхождения быть не должно.
		if got != step.Rate {
			t.Fatalf("шаг %d (advance=%d delta=%v): получено %v, эталон %v",
				i, step.Advance, step.Delta, got, step.Rate)
		}
	}
}

// TestMeterRateIsReadOnly — get-вызов не должен двигать показания,
// кроме продвижения времени. В webtorrent downloadSpeed читается именно так.
func TestMeterRateIsReadOnly(t *testing.T) {
	now := int64(0)
	m := NewMeter(5, func() int64 { return now })
	for range 20 {
		now += 100
		m.Add(1000)
	}
	first := m.Rate()
	second := m.Rate()
	if first != second {
		t.Errorf("два подряд Rate() без хода времени разошлись: %v и %v", first, second)
	}
}

// TestMeterDecaysToZero — после простоя дольше окна скорость обязана обнулиться,
// а не залипнуть на последнем значении.
func TestMeterDecaysToZero(t *testing.T) {
	now := int64(0)
	m := NewMeter(5, func() int64 { return now })
	for range 60 {
		now += 100
		m.Add(10000)
	}
	if r := m.Rate(); r <= 0 {
		t.Fatalf("после потока скорость должна быть положительной, получено %v", r)
	}
	now += 10_000 // вдвое дольше окна
	if r := m.Rate(); math.Abs(r) > 1e-9 {
		t.Errorf("после простоя скорость = %v, ожидался ноль", r)
	}
}
