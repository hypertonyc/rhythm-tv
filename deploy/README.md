# Деплой

Пуш в `main` → сборка образа → GHCR → SSH на VPS → `docker compose up -d`.
Перед выкаткой пайплайн спрашивает сервер, не смотрит ли кто-нибудь серию,
и отказывается деплоить, если смотрит.

```
push main
  ↓
server.yml:  gofmt, go vet, go test -race
  ↓
             docker buildx → ghcr.io/<repo>/server:<sha>
  ↓
deploy.yml:  playback == idle?
             ├─ да  → ssh 'deploy <sha>' < deploy/compose.yaml → дымовой тест
             └─ нет → падаем; образ уже в GHCR, ничего не потеряно
```

**Автовыкатка выключена предохранителем** `DEPLOY_ENABLED` (переменная
репозитория) — пока на порту 8000 работает прежний сервер на Node, деплой
попытался бы встать туда же. Сборка при этом идёт как обычно, образ ложится
в GHCR. Включить в момент переезда:

```sh
gh variable set DEPLOY_ENABLED --repo hypertonyc/rhythm-tv --body true
```

Ручная выкатка (`gh workflow run deploy.yml`) работает независимо от него —
именно ей делается сам переезд и откаты.

Образ помечается голым sha коммита. Только им и деплоим: он неизменяем,
и `docker compose ps` на VPS называет точный коммит. `latest` существует
для рук, деплоить его нельзя.

## Что настраивается один раз руками

### 1. Репозиторий

```sh
brew install gh && gh auth login
gh repo create hypertonyc/rhythm-tv --private --source=. --remote=origin --push
```

В настройках репозитория:

- **Settings → Environments → New environment `production`.** Секреты кладутся
  в него, а не в репозиторий, — тогда PR из форка их не увидит.
- **Settings → Branches** — защита `main`.

Правило обязательных ревьюеров на приватных репозиториях требует Pro/Team.
На бесплатном тарифе окружение всё равно полезно (область видимости секретов),
а роль ручного гейта играет шаг «не выкатывать, если смотрят».

### 2. Ключ для деплоя

```sh
ssh-keygen -t ed25519 -f ~/.ssh/tms-deploy -C ci-deploy -N ''
ssh-keyscan -p <порт> <хост>       # отпечаток СВЕРИТЬ с панелью провайдера
```

Секреты окружения `production`:

| Секрет | Значение |
|---|---|
| `DEPLOY_SSH_KEY` | приватная половина `~/.ssh/tms-deploy`, без парольной фразы |
| `DEPLOY_HOST` | адрес VPS |
| `DEPLOY_USER` | `tmsdeploy` |
| `DEPLOY_PORT` | порт sshd |
| `DEPLOY_KNOWN_HOSTS` | строка из `ssh-keyscan` |

Токен обратного прокси в CI **не нужен и не должен туда попадать**: дымовой
тест бьёт в `http://127.0.0.1:8000/api/status` изнутри VPS. Это осознанно;
переделывать его во внешнюю проверку нельзя.

### 3. VPS

```sh
sudo adduser --disabled-password --gecos '' tmsdeploy
sudo usermod -aG docker tmsdeploy
# Каталоги на VPS уже есть со времён сервера на Node — /srv/rhythm-tv/{data,cache}.
# Заводим только место под конфигурацию деплоя и даём к ней доступ.
sudo install -d -o tmsdeploy -g tmsdeploy -m 750 /srv/rhythm-tv/deploy
sudo install -d -o tmsdeploy -g tmsdeploy -m 700 /home/tmsdeploy/.ssh
# tmsdeploy должен читать данные, которыми владеет rhythm (uid 10001):
sudo usermod -aG rhythm tmsdeploy

# forced command: ключ не сможет выполнить ничего, кроме tms-deploy
sudo install -m 755 deploy/tms-deploy /usr/local/bin/tms-deploy
# в /home/tmsdeploy/.ssh/authorized_keys ОДНОЙ строкой:
#   command="/usr/local/bin/tms-deploy",restrict ssh-ed25519 AAAA... ci-deploy
sudo chown tmsdeploy:tmsdeploy /home/tmsdeploy/.ssh/authorized_keys
sudo chmod 600 /home/tmsdeploy/.ssh/authorized_keys

# доступ к приватному пакету GHCR
sudo -u tmsdeploy bash -c 'read -rs PAT && echo "$PAT" | docker login ghcr.io -u hypertonyc --password-stdin'

sudo -u tmsdeploy cp deploy/.env.example /srv/rhythm-tv/deploy/.env
sudo chmod 600 /srv/rhythm-tv/deploy/.env      # и отредактировать под себя
```

`restrict` (OpenSSH ≥ 7.2) уже включает запрет проброса портов, агента, pty
и X11. На старом sshd их надо перечислить явно.

Про группу `docker`: она равносильна root. Сдерживает это forced command —
ключ умеет ровно четыре глагола. Если стоящая привилегия не нравится, уберите
`usermod -aG docker` и дайте вместо неё строку в sudoers
`tmsdeploy ALL=(root) NOPASSWD: /usr/local/bin/tms-deploy`, а forced command
сделайте `sudo /usr/local/bin/tms-deploy`.

**Образ работает под uid 10001** — это `rhythm` на хосте, владелец
`/srv/rhythm-tv/{data,cache}`. Совпадение обязательно: контейнер идёт
с `cap_drop: ALL`, а без `CAP_DAC_OVERRIDE` даже root не прочитает `.torrent`
с правами `-r--r-----`. Прежний сервер на Node работал под тем же uid.

**`/srv/rhythm-tv/data` должен быть доступен uid 10001 на ЗАПИСЬ.** С появлением
библиотеки торрентов каталог монтируется в `/data` без `:ro`: туда кладутся
загруженные с телефона `.torrent`, и там же сервер хранит `.tms-active` — файл
с id активного торрента. Разово, если права разъехались:

```sh
sudo chown rhythm:rhythm /srv/rhythm-tv/data
sudo chmod 750 /srv/rhythm-tv/data
```

Сами `.torrent` при этом могут оставаться `-r--r-----`: для создания и удаления
файлов нужны права на каталог, а не на файлы. Видео в `/data` не появляется
никогда — скачанное лежит в `STORE_DIR`.

**Пакет в GHCR публичный,** поэтому credential на VPS не нужен вовсе:
`docker pull` идёт анонимно. В образе только ffmpeg и статический бинарник —
ни токена, ни медиа, ни `.torrent`. Если когда-нибудь сделать пакет приватным,
понадобится fine-grained PAT с правом `Packages: read` и `docker login`
на VPS от имени `tmsdeploy`.

Репозиторий тоже публичный. Что из этого следует:

- Секреты и переменные PR из форков **не видят** — GitHub их туда не передаёт.
  Деплой вдобавок прибит к `refs/heads/main`, а в PR образ не пушится
  и логина в реестр нет.
- Адрес прода в репозитории не хардкодится: проверка в `secrets.yml` берёт
  шаблон из переменной `PROD_HOST_PATTERN`.
- Прогоны от сторонних участников по умолчанию требуют одобрения. Если репозиторий
  начнёт привлекать внимание, в Settings → Actions стоит ужесточить до
  «Require approval for all outside collaborators».

### 4. Что уже проверено на месте

- Архитектура VPS — **x86_64**, совпадает с `platforms: linux/amd64`.
- Порт 8000 публикуется **только на 127.0.0.1** — наружу не торчит.
- nginx работает **на хосте**, а не в контейнере, поэтому `127.0.0.1:8000`
  ему доступен и `ports:` убирать не нужно.
- Образ проверен с `read_only`, `cap_drop: ALL` и uid 10001: полный сеанс
  перекодирования проходит, сегменты отдаются.

## Переезд с сервера на Node — сделан 16.08.2026

Раскладка файлов на диске у anacrolix и webtorrent совпадает
(`<хранилище>/<имя торрента>/<путь>`), поэтому скачанное переехало как есть:
**27.3 ГиБ узнаны за 1 мин 56 с**, качать заново ничего не пришлось.
База готовности кусков (`.torrent.bolt.db`) после этого лежит в хранилище,
так что повторных проверок не будет и `TORRENT_VERIFY_ON_START` возвращён в 0.

Порядок, если придётся повторить (например, на другом сервере):

```sh
docker update --restart=no rhythm-tv   # иначе kill его воскресит
docker kill rhythm-tv                  # именно kill: stop сотрёт хранилище
# TORRENT_VERIFY_ON_START=1 в deploy/.env
gh workflow run deploy.yml -f image_tag=<sha>
# проверка занимает минуты, всё это время /api/status отдаёт ready:false
```

Две грабли, на которые я наступил при первом деплое, и обе уже исправлены
в этом каталоге: переменную окружения надо прокидывать в `environment:`
контейнера (в `.env` она служит только подстановке), а `.env` деплоя обязан
лежать в каталоге, куда деплой-пользователь может писать — рядом создаются
`.env.candidate` и `.env.prev`.

Сервер на Node **удалён с VPS полностью** (контейнер, образ, исходники,
старый compose). Откат к нему теперь означает пересборку из репозитория:
`server/legacy/` под контролем версий, там и `server.mjs`, и его Dockerfile.
Быстрый откат остался только между версиями Go-сервера — через `deploy.yml`
с прошлым sha.

**Про пиров.** На простое их ноль, и это правильно: куски имеют приоритет
`None`, качать нечего. При запуске воспроизведения рой набирается за ~15 секунд.

## Миграция под выключенные part-файлы — сделана 17.08.2026

Part-файлы anacrolix уничтожали уже скачанные серии, и их выключили
(`server/internal/mediasource/partfile.go` — там же разбор механизма). После
этого сервер пишет и читает только `<имя>`, а лежавшие на диске `<имя>.part`
стали бы невидимы: уцелевшее в них качалось бы заново. Поэтому разово:

```sh
# 1. Фикс должен быть уже собран, но НЕ выкачен: под старым кодом
#    переименование мгновенно наплодило бы фантомов — setCompletionFromPartFiles
#    отмечает готовыми все куски файла, если <имя> лежит полного размера.
sudo sed -i 's/^TORRENT_VERIFY_ON_START=.*/TORRENT_VERIFY_ON_START=1/' \
  /srv/rhythm-tv/deploy/.env
sudo docker stop tms-server        # stop безопасен: TORRENT_STORE_PERSIST=1
# 2. Переименовать *.part → <имя>, chmod 0644 (0444 остаётся от promotePartFile),
#    chown 10001:10001. Пара «<имя> + <имя>.part» — это баг, застигнутый
#    в работе: оставить тот файл, в котором больше блоков (stat -c %b),
#    второй удалить.
gh workflow run deploy.yml -f image_tag=<sha> -f force=true
# 3. Дождаться "verified in" в логе и вернуть TORRENT_VERIFY_ON_START=0.
#    Перезапуск для этого не нужен: процесс читает флаг только при старте,
#    а CI сохраняет значения .env между деплоями — и забытая единица
#    означала бы проверку хэшей на каждой выкатке.
```

Как прошло: простой 89 с (14:09:43 → 14:11:12 UTC), переименовано 79 файлов,
разобрано 2 пары, **13.0 ГиБ узнаны за 49 с**. Просмотр это переживает —
каталог сеанса остаётся на диске, новый процесс его подбирает, и телевизор
продолжил ту же серию с той же секунды. Проверка идёт **только по активному
торренту**, поэтому у второго (`tbbt`) уцелевшие куски переименованных файлов
докачаются по требованию: ~400 МБ, гоняться за ними отдельным прогоном
не стоило.

## Откат

Речь про откат между версиями Go-сервера. Быстрый путь, работает даже
если Actions лежит (нужна личная копия ключа):

```sh
ssh -i ~/.ssh/tms-deploy tmsdeploy@<vps> rollback
```

Предыдущий sha скрипт хранит в `/srv/rhythm-tv/deploy/.env.prev`.

Из GitHub:

```sh
gh workflow run deploy.yml -f image_tag=<прошлый-sha> -f force=true
```

`force=true` при откате правильный: откатываются потому, что уже сломано.

Если сломан сам `compose.yaml` — `git revert` и обычный деплой: файл едет
на stdin каждый раз, отдельной правки на VPS нет.

Полный сброс данных (битое хранилище, странная докачка):

```sh
docker compose --project-name tms down
# Хранилище и сегменты — bind-монтирования, а не тома; чистить руками:
sudo rm -rf /srv/rhythm-tv/cache/webtorrent /srv/rhythm-tv/cache/hls
```

**Не включайте агрессивную чистку версий в GHCR:** деплой пинится на sha,
и вычистка sha-тегов удаляет цели отката. Держите хотя бы 20 версий.

## Паки субтитров

Субтитры отдельными файлами лежат в `/srv/rhythm-tv/data/subs/` — это тот же
`/data`, что и `.torrent`, отдельного тома под них нет. CI их не возит: это
данные, а не код, и деплой ради нового пака не нужен. Сервер перечитывает
каталог раз в полминуты сам.

```sh
# с рабочей машины, пак целиком одним каталогом
tar --exclude '.DS_Store' -czf - <пак> | ssh <vps> '
  sudo mkdir -p /srv/rhythm-tv/data/subs &&
  sudo tar -xzf - -C /srv/rhythm-tv/data/subs &&
  sudo chown -R 10001:10001 /srv/rhythm-tv/data/subs &&
  sudo chmod -R u=rwX,g=rX,o= /srv/rhythm-tv/data/subs'
```

`chown` на 10001 обязателен: контейнер работает под этим uid и с `cap_drop: ALL`
чужие файлы не прочитает — даже root без `CAP_DAC_OVERRIDE` не обходит права.
Имя каталога пака стоит оставлять говорящим (`…_rus_…`, `rus/`, `Russian/`):
по нему сервер определяет язык дорожки.

## Что стоит завести отдельно

Каталог сегментов HLS не ограничен по размеру by design — счётчик идёт вверх,
старые `.ts` не удаляются до конца сеанса. Пока это не починено в сервере
(`HLS_MAX_DISK_MB`), нужен алерт по `df` на `/` (сегменты лежат в `/srv/rhythm-tv/cache/hls`).
