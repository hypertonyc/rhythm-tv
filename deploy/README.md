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

Образ помечается голым sha коммита. Только им и деплоим: он неизменяем,
и `docker compose ps` на VPS называет точный коммит. `latest` существует
для рук, деплоить его нельзя.

## Что настраивается один раз руками

### 1. Репозиторий

```sh
brew install gh && gh auth login
gh repo create <owner>/torrent-media --private --source=. --remote=origin --push
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
sudo install -d -o tmsdeploy -g tmsdeploy -m 750 /srv/tms /srv/tms/torrents
sudo install -d -o tmsdeploy -g tmsdeploy -m 700 /home/tmsdeploy/.ssh

# forced command: ключ не сможет выполнить ничего, кроме tms-deploy
sudo install -m 755 deploy/tms-deploy /usr/local/bin/tms-deploy
# в /home/tmsdeploy/.ssh/authorized_keys ОДНОЙ строкой:
#   command="/usr/local/bin/tms-deploy",restrict ssh-ed25519 AAAA... ci-deploy
sudo chown tmsdeploy:tmsdeploy /home/tmsdeploy/.ssh/authorized_keys
sudo chmod 600 /home/tmsdeploy/.ssh/authorized_keys

# доступ к приватному пакету GHCR
sudo -u tmsdeploy bash -c 'read -rs PAT && echo "$PAT" | docker login ghcr.io -u <owner> --password-stdin'

sudo -u tmsdeploy cp deploy/.env.example /srv/tms/.env
sudo chmod 600 /srv/tms/.env      # и отредактировать под себя
sudo mv /path/to/*.torrent /srv/tms/torrents/
sudo chown -R tmsdeploy:tmsdeploy /srv/tms/torrents
```

`restrict` (OpenSSH ≥ 7.2) уже включает запрет проброса портов, агента, pty
и X11. На старом sshd их надо перечислить явно.

Про группу `docker`: она равносильна root. Сдерживает это forced command —
ключ умеет ровно четыре глагола. Если стоящая привилегия не нравится, уберите
`usermod -aG docker` и дайте вместо неё строку в sudoers
`tmsdeploy ALL=(root) NOPASSWD: /usr/local/bin/tms-deploy`, а forced command
сделайте `sudo /usr/local/bin/tms-deploy`.

**Приватный пакет в GHCR** требует credential на VPS: fine-grained PAT
с правом `Packages: read`, срок год, напоминание в календарь. Альтернатива —
сделать пакет публичным при приватном репозитории: в образе только ffmpeg
и бинарник, ни токена, ни медиа, ни `.torrent`. Тогда credential не нужен вовсе.

### 4. Проверить перед первым деплоем

```sh
# С ЧУЖОЙ машины: не торчит ли сервер наружу без авторизации прямо сейчас?
curl http://<ip-vps>:8000/api/files
```

Ответ вместо таймаута означает, что старый `docker run -p 8000:8000` открыл
порт мимо ufw. Новый compose публикует только на `127.0.0.1`.

Ещё два вопроса к месту: reverse-proxy на хосте или в контейнере (во втором
случае `127.0.0.1:8000` ему недоступен — нужна общая сеть и `ports:` убрать),
и совпадает ли архитектура с `platforms: linux/amd64` в `server.yml`.

## Откат

Быстрый путь, работает даже если Actions лежит (нужна личная копия ключа):

```sh
ssh -i ~/.ssh/tms-deploy tmsdeploy@<vps> rollback
```

Предыдущий sha скрипт хранит в `/srv/tms/.env.prev`.

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
docker volume rm tms_torrent-store tms_hls-scratch
```

**Не включайте агрессивную чистку версий в GHCR:** деплой пинится на sha,
и вычистка sha-тегов удаляет цели отката. Держите хотя бы 20 версий.

## Что стоит завести отдельно

Каталог сегментов HLS не ограничен по размеру by design — счётчик идёт вверх,
старые `.ts` не удаляются до конца сеанса. Пока это не починено в сервере
(`HLS_MAX_DISK_MB`), нужен алерт по `df` на `/var/lib/docker`.
