package jscompat

import (
	"bytes"
	"encoding/json"
)

// Marshal — JSON.stringify, а не encoding/json.Marshal.
//
// Два расхождения, каждое из которых меняет байты ответа:
//
//  1. Go по умолчанию экранирует <, > и & в <, >, &.
//     JSON.stringify — нет. Достаточно торрента с именем «Tom & Jerry»
//     или ffmpeg-ошибки со скобкой, чтобы Content-Length разъехался.
//  2. json.Encoder.Encode дописывает \n, JSON.stringify — нет.
//     Это +1 к Content-Length на КАЖДОМ ответе сервера.
//
// Остаточное расхождение: Go всегда экранирует U+2028/U+2029, JS — нет.
// В именах файлов они не встречаются; чинить не стали.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}
