# Сборка идёт в две стадии, потому что тулчейн Go в рантайме не нужен вовсе:
# в финальный образ уезжает один статический бинарник рядом с ffmpeg.
FROM golang:1.26-bookworm AS build
WORKDIR /src

# Зависимости отдельным слоем: их список меняется куда реже кода,
# и пересборка после правки одного файла не лезет в сеть.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO не нужен: anacrolix/torrent — чистый Go, и без cgo бинарник не тянет
# за собой glibc, то есть его можно положить куда угодно.
# Флаги -s -w выбрасывают таблицу символов и DWARF: минус ~25% размера,
# ценой читаемых трасс в панике (их всё равно смотрят по логам, а не в проде).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/server .

FROM debian:bookworm-slim
# ffmpeg с ffprobe — обязательная часть рантайма, сервер зовёт их процессами.
# ca-certificates нужны трекерам по https.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/server /usr/local/bin/server

# Каталоги под хранилище торрента и сегменты HLS. В проде оба монтируются
# томами: сегменты 45-минутной серии это 0.8-1.2 ГБ, а в режиме copy до 3-6,
# и держать их на overlayfs контейнера незачем.
ENV TORRENT_STORE=/var/lib/tms/store \
    TMPDIR=/var/lib/tms/hls \
    PORT=8000
RUN mkdir -p /var/lib/tms/store /var/lib/tms/hls \
    && chown -R 10001:10001 /var/lib/tms

# 10001 — не произвольное число, а uid пользователя rhythm на боевом хосте,
# которому принадлежат /srv/rhythm-tv/{data,cache}. Совпадение обязательно:
# контейнер запускается с cap_drop: ALL, а без CAP_DAC_OVERRIDE даже root
# не прочитает чужой файл с правами -r--r-----. Прежний сервер на Node
# работает под этим же uid.
USER 10001:10001

EXPOSE 8000
# Собственная проверка вместо curl: лишний пакет в образе ради одного
# запроса не нужен. /api/status отдаёт 200 ещё до загрузки метаданных,
# поэтому контейнер не флапает на старте.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/server", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/server"]
