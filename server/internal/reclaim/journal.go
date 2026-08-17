package reclaim

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Journal — когда какую серию смотрели в последний раз.
//
// Ключ — путь файла относительно хранилища («Друзья/Сезон 01/s01e01.mkv»),
// а не индекс и не id торрента. Индекс принадлежит торренту и после
// переключения означает другой файл, а выселяется именно файл на диске —
// значит и помнить надо про него.
//
// Отметка ставится на запуске сеанса перекодирования (/api/start): это
// единственное место, где сервер точно знает, что серию СМОТРЯТ. Ни разбор
// (/api/probe), ни листание меню сюда не попадают — иначе «пролистал всё
// подряд» переписывало бы историю просмотров целиком.
//
// Журнал переживает перезапуск: он лежит рядом с .tms-active в каталоге
// библиотеки. Без этого каждая выкатка обнуляла бы порядок выселения,
// и чистка после неё удаляла бы что попало.
type Journal struct {
	path string

	mu   sync.Mutex
	seen map[string]int64
}

// LoadJournal читает журнал с диска. Отсутствие файла и любая ошибка чтения —
// это пустой журнал, а не отказ: без истории просмотров чистка переходит
// на mtime файлов и продолжает работать.
func LoadJournal(path string) *Journal {
	j := &Journal{path: path, seen: make(map[string]int64)}
	if path == "" {
		return j
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("журнал просмотров %s: %v (начинаем с пустого)", path, err)
		}
		return j
	}
	if err := json.Unmarshal(data, &j.seen); err != nil {
		log.Printf("журнал просмотров %s: %v (начинаем с пустого)", path, err)
		j.seen = make(map[string]int64)
	}
	return j
}

// Touch отмечает, что файл сейчас смотрят.
func (j *Journal) Touch(rel string, at time.Time) {
	if rel == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seen[rel] = at.UnixMilli()
	// Пишется сразу, а не по таймеру: запуск серии случается несколько раз
	// за вечер, а потерять отметку — значит выселить то, что смотрят чаще
	// всего. Файл маленький, запись атомарная.
	j.saveLocked()
}

// At отдаёт время последнего просмотра в миллисекундах.
func (j *Journal) At(rel string) (int64, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	at, ok := j.seen[rel]
	return at, ok
}

// Len — сколько записей в журнале; нужен тестам и логу при старте.
func (j *Journal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.seen)
}

// saveLocked пишет журнал через временный файл и rename.
//
// Ошибка записи не отменяет отметку в памяти: каталог библиотеки может быть
// смонтирован только на чтение, и тогда история просто не переживёт
// перезапуск — это лучше, чем ронять запуск серии.
//
// Записи об исчезнувших файлах не убираются намеренно: выселенную серию
// могут докачать обратно, и её история должна пережить выселение. Строка
// на файл — это десятки килобайт на всю библиотеку.
func (j *Journal) saveLocked() {
	if j.path == "" {
		return
	}
	// MarshalIndent, а не компактно: файл читают глазами с VPS, а ключи
	// encoding/json и так сортирует.
	data, err := json.MarshalIndent(j.seen, "", "  ")
	if err != nil {
		log.Printf("журнал просмотров: %v", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(j.path), ".tms-watched-*")
	if err != nil {
		log.Printf("журнал просмотров %s: %v", j.path, err)
		return
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op после успешного rename

	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		log.Printf("журнал просмотров %s: %v", j.path, err)
		return
	}
	if err := tmp.Close(); err != nil {
		log.Printf("журнал просмотров %s: %v", j.path, err)
		return
	}
	if err := os.Chmod(name, 0o644); err != nil {
		log.Printf("журнал просмотров %s: %v", j.path, err)
		return
	}
	if err := os.Rename(name, j.path); err != nil {
		log.Printf("журнал просмотров %s: %v", j.path, err)
	}
}
