package hls

import "testing"

// Настоящий блок `-progress pipe:1` от ffmpeg. Записан целиком и не сокращён
// намеренно: разбор обязан выдерживать ключи, которых он не знает, — их там
// два десятка, и в новых версиях ffmpeg их становится больше.
const sampleBlock = "bitrate=2097.2kbits/s\n" +
	"total_size=1048576\n" +
	"out_time_us=2048000\n" +
	"out_time_ms=2048000\n" +
	"out_time=00:00:02.048000\n" +
	"dup_frames=0\n" +
	"drop_frames=0\n" +
	"speed=4.07x\n" +
	"progress=continue\n"

func TestProgressReadsCompletedBlock(t *testing.T) {
	w := &progressWriter{}
	mustWrite(t, w, sampleBlock)

	block, seen := w.snapshot()
	if !seen {
		t.Fatal("блок закрыт progress=continue, но снаружи его не видно")
	}
	if !block.HasTime || block.OutTimeMs != 2048 {
		t.Errorf("out_time = %d мс (has=%v), ожидалось 2048", block.OutTimeMs, block.HasTime)
	}
	if !block.HasSpeed || block.Speed != 4.07 {
		t.Errorf("speed = %v (has=%v), ожидалось 4.07", block.Speed, block.HasSpeed)
	}
}

// TestProgressIgnoresOutTimeMs — главная ловушка формата: out_time_ms, вопреки
// имени, содержит МИКРОсекунды. Взяв его как миллисекунды, мы показали бы
// 2048000 мс (34 минуты) вместо 2 секунд — прогресс мгновенно упирался бы
// в потолок, и на экране это выглядело бы как «готово, но ничего не идёт».
func TestProgressIgnoresOutTimeMs(t *testing.T) {
	w := &progressWriter{}
	mustWrite(t, w, "out_time_ms=2048000\nout_time=00:00:02.048000\nprogress=continue\n")

	block, _ := w.snapshot()
	if block.OutTimeMs != 2048 {
		t.Fatalf("взято %d мс — похоже, разбор поверил out_time_ms", block.OutTimeMs)
	}
}

// TestProgressSurvivesArbitrarySplits — exec отдаёт Write кусками произвольной
// длины, и границы блоков с ними никак не связаны. Проверяется каждая точка
// разреза: одна забытая склейка строки — и прогресс молча замирает.
func TestProgressSurvivesArbitrarySplits(t *testing.T) {
	for cut := 1; cut < len(sampleBlock); cut++ {
		w := &progressWriter{}
		mustWrite(t, w, sampleBlock[:cut])
		mustWrite(t, w, sampleBlock[cut:])

		block, seen := w.snapshot()
		if !seen || block.OutTimeMs != 2048 || block.Speed != 4.07 {
			t.Fatalf("разрез на %d: seen=%v out=%d speed=%v", cut, seen, block.OutTimeMs, block.Speed)
		}
	}
}

// TestProgressHidesUnfinishedBlock — значения одного блока относятся к одному
// моменту. Отдай мы их по мере поступления, снаружи можно было бы увидеть
// новое out_time со старым speed.
func TestProgressHidesUnfinishedBlock(t *testing.T) {
	w := &progressWriter{}
	mustWrite(t, w, "out_time=00:00:09.000000\nspeed=1.00x\nprogress=continue\n")
	mustWrite(t, w, "out_time=00:00:18.000000\n")

	block, _ := w.snapshot()
	if block.OutTimeMs != 9000 {
		t.Fatalf("виден незакрытый блок: out=%d, ожидалось 9000", block.OutTimeMs)
	}

	mustWrite(t, w, "speed=2.00x\nprogress=continue\n")
	if block, _ = w.snapshot(); block.OutTimeMs != 18000 || block.Speed != 2 {
		t.Fatalf("после закрытия блока out=%d speed=%v", block.OutTimeMs, block.Speed)
	}
}

func TestProgressNothingSeenBeforeFirstBlock(t *testing.T) {
	w := &progressWriter{}
	if _, seen := w.snapshot(); seen {
		t.Fatal("пустой писатель обязан отвечать «не измерено», а не нулём")
	}
	// «Не измерено» — это состояние ffmpeg, который ещё ничего не выдал:
	// у нас это значит «ждём данных из роя», и путать его с нулём нельзя.
	mustWrite(t, w, "out_time=00:00:01.000000\n")
	if _, seen := w.snapshot(); seen {
		t.Fatal("блок не закрыт, а снаружи он уже виден")
	}
}

// TestProgressSkipsUnmeasuredValues — N/A и отрицательное время ffmpeg пишет
// сам: первое в самых первых блоках, второе — при output-side -ss, пока он
// выбрасывает всё, что раньше точки входа.
func TestProgressSkipsUnmeasuredValues(t *testing.T) {
	w := &progressWriter{}
	mustWrite(t, w, "out_time=N/A\nspeed=N/A\nprogress=continue\n")
	block, seen := w.snapshot()
	if !seen {
		t.Fatal("блок закрыт — он обязан быть виден, пусть и пустым")
	}
	if block.HasTime || block.HasSpeed {
		t.Errorf("N/A принято за значение: time=%v speed=%v", block.HasTime, block.HasSpeed)
	}

	mustWrite(t, w, "out_time=-00:00:00.020000\nprogress=continue\n")
	if block, _ = w.snapshot(); !block.HasTime || block.OutTimeMs != 0 {
		t.Errorf("отрицательное время: out=%d has=%v, ожидалось 0/true", block.OutTimeMs, block.HasTime)
	}
}

// TestProgressReadsPaddedSpeed закрепляет то, что снято с живого ffmpeg 8.0,
// а не выведено из документации: скорость выравнивается по ширине и на
// больших значениях приезжает БЕЗ дробной части — `speed= 112x`. Такую строку
// легко не заметить, написав разбор по одному примеру вида `speed=1.02x`,
// а именно она и приходит в режиме copy, где ffmpeg обгоняет реальное время
// в сотню раз — то есть ровно в самом частом нашем случае.
func TestProgressReadsPaddedSpeed(t *testing.T) {
	w := &progressWriter{}
	mustWrite(t, w, "out_time=00:00:05.920000\nspeed= 112x\nprogress=end\n")

	block, seen := w.snapshot()
	if !seen || !block.HasSpeed || block.Speed != 112 {
		t.Fatalf("speed = %v (has=%v seen=%v), ожидалось 112", block.Speed, block.HasSpeed, seen)
	}
	if block.OutTimeMs != 5920 {
		t.Errorf("out_time = %d мс, ожидалось 5920", block.OutTimeMs)
	}
}

// TestProgressDropsGarbageTail — если в stdout вместо отчёта польётся что-то
// без переводов строки, буфер не должен расти без предела.
func TestProgressDropsGarbageTail(t *testing.T) {
	w := &progressWriter{}
	for i := 0; i < 10; i++ {
		mustWrite(t, w, string(make([]byte, 1000)))
	}
	if len(w.tail) > 4096 {
		t.Fatalf("хвост вырос до %d байт", len(w.tail))
	}
}

func TestProgressNilWriter(t *testing.T) {
	// Подобранный после выкатки сеанс своего ffmpeg не имеет вовсе.
	var w *progressWriter
	if _, seen := w.snapshot(); seen {
		t.Fatal("nil-писатель обязан отвечать «не измерено»")
	}
}

func mustWrite(t *testing.T, w *progressWriter, s string) {
	t.Helper()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("Write: %v", err)
	}
}
