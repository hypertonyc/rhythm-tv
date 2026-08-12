# Rhythm TV

Просмотр сериалов из торрента на старом Samsung Smart TV: сервер раздаёт файлы
из торрента как HLS, клиент на телевизоре их играет.

Репозиторий содержит обе части.

| Каталог | Что это |
|---|---|
| [client/](client/) | приложение для Samsung Smart TV (Tizen 2.3, телевизоры 2015 года) |
| [server/](server/) | медиасервер на Node.js: торрент → ffmpeg → HLS + встроенный веб-клиент |
| [scripts/](scripts/) | сборка и установка клиента на телевизор |
| [.vscode/](.vscode/) | задача VS Code, запускающая сборку |

## Как это работает

```
торрент  ──webtorrent──>  server/server.mjs  ──ffmpeg──>  HLS (MPEG-TS + WebVTT)
                                 │                              │
                                 │  /api/files, /api/probe       │  /hls/<id>/...
                                 v                              v
                          client/ на телевизоре  ──AVPlay──>  экран
```

Сервер держит один торрент на процесс, ничего не качает заранее и перекодирует
только тот эпизод, который просят, — H.264 + AAC в MPEG-TS, потому что декодер
2015 года другого не понимает. Клиент опрашивает готовность сегментов и стартует,
когда их появилось хотя бы два.

## Быстрый старт

Сервер (нужен Docker, либо Node 22 + ffmpeg):

```sh
cd server
docker build -t torrent-media-server .
docker run --rm -p 8000:8000 -v /path/to/torrents:/data torrent-media-server /data/file.torrent
```

Подробности и список эндпоинтов — [server/README.md](server/README.md).

Клиент (нужны Tizen SDK tools и сертификат для подписи):

```sh
cp .env.example .env   # пути к тулчейну, адрес телевизора, профиль, DEFAULT_SERVER
./scripts/tizen-build-install.sh
```

В VS Code то же самое — **Run Build Task** → `Tizen: Build & Install on TV`.
Подробности — [client/README.md](client/README.md).

Ограничения платформы и неочевидные решения в коде — [CLAUDE.md](CLAUDE.md).
