package mediasource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// Выселение скачанного: удалить файл с диска и снять отметки готовности
// с покрывающих его кусков.
//
// Порядок здесь несущий, и оба шага обязательны. Удалить файл, оставив
// отметки, — это ровно тот фантом из phantom.go: торрент считает данные
// скачанными, читатель не идёт в рой и мгновенно отдаёт нули, а наружу это
// не отказ, а порча — картинка идёт, субтитры пустые, в логе ни строчки.
// Снять отметки, не удалив файл, — просто перекачать то, что и так лежит.

// StoreFile — файл торрента на диске: где он лежит и какие куски его покрывают.
type StoreFile struct {
	// Index — номер файла в торренте, тот же, что у File.Index: по нему
	// телевизор хранит позиции просмотра, а сервер ищет файл для /raw.
	Index int
	// Rel — путь ОТНОСИТЕЛЬНО ХРАНИЛИЩА, разделитель «/»: имя раздачи плюс
	// путь внутри неё. Ровно это отдаёт anacrolix в File.Path().
	Rel string
	// Length — размер файла по метаинформации, а не занятое на диске.
	Length int64
	// FirstPiece и LastPiece — куски, которые файл ЗАДЕВАЕТ, включая
	// граничные, поделённые с соседями. Сосед из-за этого потеряет
	// один кусок и докачает его по требованию — это дешевле, чем оставить
	// отметку на куске, половины которого больше нет.
	FirstPiece, LastPiece int
}

// StoreFiles раскладывает метаинформацию по диску так же, как это делает
// anacrolix при добавлении торрента (Torrent.initFiles + storage.NewFileOpts).
//
// Повторять раскладку приходится потому, что место занимают ВСЕ торренты
// библиотеки, а в клиенте лежит только активный: у остальных ни *torrent.File,
// ни File.Path() спросить не у кого. Ошибка здесь означает удаление не того
// файла, поэтому вычисления взяты из initFiles дословно, а совпадение
// с настоящей раскладкой закреплено тестом на реальном хранилище.
func StoreFiles(info *metainfo.Info) []StoreFile {
	out := make([]StoreFile, 0, 16)
	var offset int64
	for i, fi := range info.UpvertedFiles() {
		f := StoreFile{
			Index: i,
			// Имя раздачи впереди всегда — так делает initFiles. Смещения
			// в fi.TorrentOffset брать нельзя: их считает UpvertedFiles,
			// и выравнивания по границе куска там нет.
			Rel:    strings.Join(append([]string{info.BestName()}, fi.BestPath()...), "/"),
			Length: fi.Length,
		}
		if info.PieceLength > 0 && fi.Length > 0 {
			f.FirstPiece = int(offset / info.PieceLength)
			f.LastPiece = int((offset + fi.Length - 1) / info.PieceLength)
		}
		out = append(out, f)

		offset += fi.Length
		if info.FilesArePieceAligned() {
			offset = (offset + info.PieceLength - 1) / info.PieceLength * info.PieceLength
		}
	}
	return out
}

// DropFile удаляет данные одного файла и снимает отметки готовности с его
// кусков. Возвращает, сколько места освободилось на диске.
//
// Торрент берётся по пути к .torrent, а не по уже добавленному источнику:
// выселять надо и из тех торрентов, которых в клиенте нет (активен всегда
// один — MULTI-TORRENT PIN). Хранилище общее на всю библиотеку, поэтому
// отметки снимаются через него же, а торренту, который сейчас в клиенте,
// дополнительно сбрасывается кэш готовности — иначе он до перезапуска
// продолжал бы считать удалённое скачанным.
func (c *Client) DropFile(torrentPath string, index int) (int64, error) {
	if c.store == nil {
		return 0, errors.New("хранилище не поднято")
	}
	mi, err := metainfo.LoadFromFile(torrentPath)
	if err != nil {
		return 0, fmt.Errorf("метаинформация %s: %w", torrentPath, err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return 0, fmt.Errorf("метаинформация %s: %w", torrentPath, err)
	}
	files := StoreFiles(&info)
	if index < 0 || index >= len(files) {
		return 0, fmt.Errorf("файл %d: в торренте %s таких нет", index, info.BestName())
	}
	f := files[index]

	full, err := c.storePath(f.Rel)
	if err != nil {
		return 0, err
	}
	freed, err := allocatedBytes(full)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", f.Rel, err)
	}

	hash := mi.HashInfoBytes()
	// Отметки снимаются ДО удаления: пока они стоят, любое чтение верит
	// файлу, а файла уже нет. Между этими двумя шагами кусок мог успеть
	// докачаться и снова пометиться готовым, поэтому после удаления они
	// снимаются ещё раз — второй проход стоит десятка записей в bolt.
	if err := c.forgetPieces(&info, hash, f.FirstPiece, f.LastPiece); err != nil {
		return 0, err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("удаление %s: %w", f.Rel, err)
	}
	if err := c.forgetPieces(&info, hash, f.FirstPiece, f.LastPiece); err != nil {
		return freed, err
	}
	return freed, nil
}

// storePath переводит путь относительно хранилища в путь на диске.
//
// Проверка обязательна: имя раздачи и пути внутри неё приходят
// из недоверенного .torrent, и промах здесь означает удаление чужого файла
// на VPS. Библиотека проверяет то же самое перед удалением каталога
// торрента целиком (library.removeDataLocked) — здесь речь про один файл,
// и цена ошибки та же.
func (c *Client) storePath(rel string) (string, error) {
	if c.dataDir == "" {
		return "", errors.New("каталог хранилища не задан")
	}
	base := filepath.Clean(c.dataDir)
	full := filepath.Clean(filepath.Join(base, filepath.FromSlash(rel)))
	inside, err := filepath.Rel(base, full)
	if err != nil || inside == "." || inside == ".." ||
		strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("небезопасный путь %q", rel)
	}
	return full, nil
}

// forgetPieces снимает отметки готовности с кусков [first, last].
func (c *Client) forgetPieces(info *metainfo.Info, hash metainfo.Hash, first, last int) error {
	// OpenTorrent на нашем хранилище дёшев и не имеет состояния: база
	// готовности одна на весь каталог и уже открыта, а Close у файлового
	// хранилища — no-op. Поэтому открывать торрент, который, возможно,
	// уже открыт клиентом, безопасно.
	impl, err := c.store.OpenTorrent(context.Background(), info, hash)
	if err != nil {
		return fmt.Errorf("хранилище %s: %w", info.BestName(), err)
	}
	if impl.Close != nil {
		defer impl.Close()
	}
	if impl.Piece == nil {
		return fmt.Errorf("хранилище %s: куски недоступны", info.BestName())
	}

	for i := first; i <= last; i++ {
		if err := impl.Piece(info.Piece(i)).MarkNotComplete(); err != nil {
			return fmt.Errorf("кусок %d: %w", i, err)
		}
	}

	// Клиент держит готовность кусков в своём кэше и в хранилище больше
	// не заглядывает. Без этого выселенный файл у активного торрента
	// остался бы «скачанным» до перезапуска процесса — то есть читался бы
	// нулями, как фантом.
	if c.client == nil {
		return nil
	}
	t, ok := c.client.Torrent(hash)
	// Info() == nil означает, что метаданные ещё не приехали: кусков у клиента
	// нет вовсе (t.Piece(i) полез бы за границы пустого среза), а готовность
	// он прочитает из хранилища, когда они появятся.
	if !ok || t.Info() == nil {
		return nil
	}
	for i := first; i <= last && i < t.NumPieces(); i++ {
		t.Piece(i).UpdateCompletion()
	}
	return nil
}
