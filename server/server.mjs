import WebTorrent from 'webtorrent'
import http from 'node:http'
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import os from 'node:os'

const PORT = Number(process.env.PORT || 8000)
// Чуть меньше, чем XHR-таймаут телевизора на /api/probe (30 с), чтобы клиент
// получил внятную ошибку от сервера, а не сработал своим сторожевым таймером.
const PROBE_TIMEOUT_MS = Number(process.env.PROBE_TIMEOUT_MS || 25000)
// HLS_ALLOW_COPY=0 возвращает безусловное перекодирование — на случай, если
// телевизор всё-таки споткнётся о какой-нибудь «совместимый» поток.
const ALLOW_COPY = process.env.HLS_ALLOW_COPY !== '0'
// Куски торрента; каталог удаляется при завершении процесса
// (destroyStoreOnDestroy ниже), так что перезапуск качает эпизод заново.
const TORRENT_STORE = process.env.TORRENT_STORE || path.join(os.tmpdir(), 'webtorrent')
const torrentFile = process.argv[2]

if (!torrentFile) {
  console.error('Usage: node server.mjs /data/file.torrent')
  process.exit(1)
}

const client = new WebTorrent()
let torrent = null
const probeCache = new Map()
const probeInflight = new Map()
const prebuffering = new Set()
const hlsSessions = new Map()
let activeHlsSession = null

client.on('error', err => console.error('WebTorrent error:', err))
client.on('warning', err => console.warn('WebTorrent warning:', err.message || err))

// Access-Control-Allow-Origin: * стоит на всех ответах сознательно: виджет
// Tizen ходит сюда кросс-ориджином. Своей авторизации у сервера нет вообще —
// он рассчитан на публикацию через reverse-proxy с токеном в пути.
function sendJson(res, status, data) {
  const body = JSON.stringify(data)
  res.writeHead(status, {
    'Content-Type': 'application/json; charset=utf-8',
    'Content-Length': Buffer.byteLength(body),
    'Cache-Control': 'no-store',
    'Access-Control-Allow-Origin': '*'
  })
  res.end(body)
}

function sendText(res, status, text, type = 'text/plain; charset=utf-8') {
  res.writeHead(status, {
    'Content-Type': type,
    'Content-Length': Buffer.byteLength(text),
    'Cache-Control': 'no-store',
    'Access-Control-Allow-Origin': '*'
  })
  res.end(text)
}

function getFile(index) {
  if (!torrent) return null
  const n = Number(index)
  if (!Number.isInteger(n) || n < 0 || n >= torrent.files.length) return null
  return torrent.files[n]
}

function videoFiles() {
  if (!torrent) return []
  return torrent.files
    .map((file, index) => ({ index, name: file.name, length: file.length }))
    .filter(x => /\.(mp4|m4v|mkv|webm)$/i.test(x.name))
}

function nextVideoIndex(index, list = videoFiles()) {
  const pos = list.findIndex(x => x.index === index)
  return pos >= 0 && pos + 1 < list.length ? list[pos + 1].index : null
}

function prevVideoIndex(index, list = videoFiles()) {
  const pos = list.findIndex(x => x.index === index)
  return pos > 0 ? list[pos - 1].index : null
}

// Латиница и кириллица проверяются разными шаблонами не для красоты: `\b` в JS
// определён через `\w` = [A-Za-z0-9_], поэтому вокруг кириллических букв
// границы слова не существует и /\bрус\b/ не совпадает ни с чем никогда.
const LANGUAGES = [
  { code: 'rus', label: 'Russian', latin: /\b(rus|ru|russian)\b/, cyrillic: /рус/ },
  { code: 'eng', label: 'English', latin: /\b(eng|en|english)\b/, cyrillic: /англ/ },
  { code: 'tha', label: 'Thai', latin: /\b(tha|th|thai)\b/, cyrillic: /тай/ }
]

function trackTitle(tags) {
  const title = String(tags.title || '').replace(/\s+/g, ' ').trim()
  return title.length > 40 ? `${title.slice(0, 39)}…` : title
}

function normalizeLanguage(stream, ordinal, kind) {
  const tags = stream.tags || {}
  const raw = String(tags.language || '').toLowerCase()
  const title = trackTitle(tags)
  const haystack = `${raw} ${title.toLowerCase()}`

  for (const lang of LANGUAGES) {
    if (!lang.latin.test(haystack) && !lang.cyrillic.test(haystack)) continue
    // Название дорожки идёт в подпись: у рипов с двумя дорожками одного языка
    // это единственное, чем «Дубляж» отличается от «Войсовер» в меню.
    return { code: lang.code, label: title ? `${lang.label} — ${title}` : lang.label }
  }

  if (raw && raw !== 'und') {
    return { code: raw, label: title || raw.toUpperCase() }
  }

  return {
    code: `${kind}-${ordinal + 1}`,
    label: title || `${kind === 'audio' ? 'Audio' : 'Subtitles'} ${ordinal + 1}`
  }
}

// Коды дорожек обязаны быть уникальными: chooseTrack ищет дорожку по коду,
// и при двух русских дорожках (дубляж + войсовер — обычное дело для рипов)
// вторая была бы недостижима. Первая дорожка сохраняет чистый код, чтобы
// сохранённое у клиента предпочтение ('rus') продолжало работать.
function disambiguateTracks(tracks) {
  const usedCodes = new Set()
  const usedLabels = new Set()
  for (const track of tracks) {
    let code = track.code
    let codeSeq = 1
    while (usedCodes.has(code)) code = `${track.code}-${++codeSeq}`
    usedCodes.add(code)
    track.code = code

    let label = track.label
    let labelSeq = 1
    while (usedLabels.has(label)) label = `${track.label} ${++labelSeq}`
    usedLabels.add(label)
    track.label = label
  }
  return tracks
}

function rawUrl(index) {
  return `http://127.0.0.1:${PORT}/raw/${index}`
}

function runCapture(command, args, timeoutMs = 0) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { stdio: ['ignore', 'pipe', 'pipe'] })
    const stdout = []
    const stderr = []
    let timedOut = false

    // Без таймера запрос может не завершиться никогда: ffprobe читает файл
    // через /raw, то есть через торрент, и на раздаче без пиров просто висит.
    const timer = timeoutMs > 0
      ? setTimeout(() => {
          timedOut = true
          try { child.kill('SIGKILL') } catch {}
        }, timeoutMs)
      : null
    if (timer && timer.unref) timer.unref()

    child.stdout.on('data', chunk => stdout.push(chunk))
    child.stderr.on('data', chunk => stderr.push(chunk))
    child.on('error', err => {
      if (timer) clearTimeout(timer)
      reject(err)
    })
    child.on('close', code => {
      if (timer) clearTimeout(timer)
      const out = Buffer.concat(stdout).toString('utf8')
      const err = Buffer.concat(stderr).toString('utf8')
      if (timedOut) reject(new Error(`${command} timed out after ${Math.round(timeoutMs / 1000)}s`))
      else if (code === 0) resolve(out)
      else reject(new Error(`${command} exited with ${code}: ${err}`))
    })
  })
}

// Один ffprobe на файл: телевизор и веб-клиент легко просят один индекс
// одновременно (плюс пинг /api/probe/next перед концом эпизода), а каждый
// лишний ffprobe — это отдельное чтение торрента.
function probeFile(index) {
  if (probeCache.has(index)) return Promise.resolve(probeCache.get(index))
  const pending = probeInflight.get(index)
  if (pending) return pending
  const task = runProbe(index).finally(() => probeInflight.delete(index))
  probeInflight.set(index, task)
  return task
}

async function runProbe(index) {
  const file = getFile(index)
  if (!file) throw new Error('File not found')

  const output = await runCapture('ffprobe', [
    '-v', 'error',
    '-show_streams',
    '-show_format',
    '-of', 'json',
    rawUrl(index)
  ], PROBE_TIMEOUT_MS)

  const data = JSON.parse(output)
  const streams = data.streams || []
  const audioRaw = streams.filter(s => s.codec_type === 'audio')
  const subtitleRaw = streams.filter(s => s.codec_type === 'subtitle')
  const video = streams.find(s => s.codec_type === 'video') || null

  const audio = disambiguateTracks(audioRaw.map((s, i) => {
    const lang = normalizeLanguage(s, i, 'audio')
    return {
      index: s.index,
      relativeIndex: i,
      code: lang.code,
      label: lang.label,
      codec: s.codec_name || '',
      // profile/channels/sampleRate нужны, чтобы решить, можно ли отдать
      // дорожку как есть вместо перекодирования (см. canCopyAudio).
      profile: s.profile || '',
      channels: Number(s.channels) || 0,
      sampleRate: Number(s.sample_rate) || 0,
      default: Boolean(s.disposition && s.disposition.default)
    }
  }))

  const subtitles = disambiguateTracks(subtitleRaw.map((s, i) => {
    const lang = normalizeLanguage(s, i, 'sub')
    return {
      index: s.index,
      relativeIndex: i,
      code: lang.code,
      label: lang.label,
      codec: s.codec_name || '',
      default: Boolean(s.disposition && s.disposition.default)
    }
  }))

  const list = videoFiles()
  const duration = Number(data.format && data.format.duration) || 0
  const result = {
    index,
    name: file.name,
    duration,
    video: video ? {
      index: video.index,
      codec: video.codec_name || '',
      width: video.width || 0,
      height: video.height || 0,
      pixFmt: video.pix_fmt || '',
      profile: video.profile || '',
      level: Number(video.level) || 0,
      fieldOrder: video.field_order || ''
    } : null,
    audio,
    subtitles,
    next: nextVideoIndex(index, list),
    prev: prevVideoIndex(index, list)
  }

  probeCache.set(index, result)
  return result
}

// Ремукс без перекодирования ловит форматы, которых декодер 2015 года не
// понимает, поэтому whitelist узкий: ровно то, во что мы иначе перекодировали бы
// сами. Всё, что вне списка (10 бит, 4:2:2, 4K, level 5+, чересстрочное),
// по-прежнему идёт через libx264. Аварийный выключатель — HLS_ALLOW_COPY=0.
const H264_SAFE_PROFILES = new Set(['constrained baseline', 'baseline', 'main', 'high'])

function canCopyVideo(video, start) {
  if (!ALLOW_COPY || !video) return false
  // Output-side seek режет поток не по ключевому кадру, а вставить свои
  // ключевые кадры в копируемый поток нельзя — при перемотке только перекодирование.
  if (start > 0.05) return false
  if (video.codec !== 'h264' || video.pixFmt !== 'yuv420p') return false
  if (!H264_SAFE_PROFILES.has(String(video.profile).toLowerCase())) return false
  if (!video.level || video.level > 41) return false
  if (video.width > 1920 || video.height > 1088) return false
  if (video.fieldOrder && video.fieldOrder !== 'progressive') return false
  return true
}

function canCopyAudio(track, start) {
  if (!ALLOW_COPY || !track) return false
  if (start > 0.05) return false
  if (track.codec !== 'aac' || String(track.profile).toUpperCase() !== 'LC') return false
  if (!track.channels || track.channels > 2) return false
  return track.sampleRate === 44100 || track.sampleRate === 48000
}

function videoArgs(copy) {
  if (copy) return ['-c:v', 'copy']
  return [
    '-c:v', 'libx264',
    '-preset', 'veryfast',
    '-crf', '20',
    '-pix_fmt', 'yuv420p',
    '-profile:v', 'high',
    '-level:v', '4.0',
    '-sc_threshold', '0',
    // Ключевой кадр раз в 4 с — ровно под -hls_time 4, чтобы сегменты резались
    // одинаковыми. В режиме copy этого рычага нет: ffmpeg режет по тем ключевым
    // кадрам, которые уже есть в файле.
    '-force_key_frames', 'expr:gte(t,n_forced*4)'
  ]
}

function audioArgs(copy) {
  if (copy) return ['-c:a', 'copy']
  return ['-c:a', 'aac', '-b:a', '160k', '-ac', '2', '-ar', '48000']
}

function chooseTrack(tracks, preference) {
  if (!tracks.length) return null
  if (preference) {
    const exact = tracks.find(t => t.code === preference)
    if (exact) return exact
  }
  return tracks.find(t => t.default) || tracks[0]
}

function safeRm(dir) {
  try { fs.rmSync(dir, { recursive: true, force: true }) } catch {}
}

// Готовность сеанса считается только по числу готовых сегментов на диске,
// и только вверх: preparing → buffering (есть первый) → ready (есть два)
// → finished (ffmpeg вышел сам). Причина остановки живёт отдельно, в stopReason,
// иначе фаза и причина затирают друг друга.
function advancePhase(session, segments) {
  if (session.phase === 'finished') return
  if (segments >= 2) session.phase = 'ready'
  else if (segments >= 1) session.phase = 'buffering'
}

// Наружу фаза, причина остановки и ошибка сводятся в одно поле state:
// ошибка важнее остановки, остановка важнее фазы.
function sessionState(session) {
  if (session.error) return 'error'
  return session.stopReason || session.phase
}

// Сегменты считаются инкрементально: ffmpeg нумерует их по порядку, а
// `-hls_flags temp_file` делает появление файла атомарным, поэтому готовый
// сегмент уже не изменится — достаточно посчитать его один раз. Полный обход
// каталога на каждом опросе стоил бы O(числа сегментов) синхронных statSync
// на event loop: у 45-минутного эпизода это ~700 файлов каждые 500 мс, на том
// же цикле, который отдаёт сегменты телевизору и /raw ffmpeg'у.
function pollSegments(session) {
  if (session.nextSeq === null) {
    // Первый проход: узнаём, с какого номера ffmpeg начал нумерацию, чтобы
    // не завязываться на значение -start_number.
    let lowest = null
    try {
      for (const name of fs.readdirSync(session.dir)) {
        const m = /^seg(\d+)\.ts$/.exec(name)
        if (!m) continue
        const seq = Number(m[1])
        if (lowest === null || seq < lowest) lowest = seq
      }
    } catch {}
    if (lowest === null) return
    session.nextSeq = lowest
  }

  for (;;) {
    const name = `seg${String(session.nextSeq).padStart(5, '0')}.ts`
    let stat
    try { stat = fs.statSync(path.join(session.dir, name)) }
    catch { break }
    session.segments++
    session.bytesOut += stat.size
    session.nextSeq++
    session.lastOutputAt = Date.now()
  }
}

// Чистая функция: ничего не читает с диска, поэтому её можно звать на каждый
// /api/status и /api/hls-status. Счётчики обновляет монитор сеанса.
function sessionSnapshot(session) {
  if (!session) return null
  return {
    id: session.id,
    index: session.index,
    name: session.name,
    audio: session.audio,
    sub: session.sub,
    videoMode: session.videoMode,
    audioMode: session.audioMode,
    start: session.start,
    state: sessionState(session),
    startedAt: session.startedAt,
    segments: session.segments,
    bytesOut: session.bytesOut,
    lastOutputAt: session.lastOutputAt,
    ffmpegPid: session.ffmpegPid,
    exitCode: session.exitCode,
    error: session.error,
    // Tizen 2.3 cannot parse #EXT-X-MEDIA (introduced on Samsung TV in Tizen 2.4).
    // Always give AVPlay the plain A/V media playlist. The app fetches the
    // subtitle HLS rendition itself and renders WebVTT cues as an HTML overlay.
    playlist: `/hls/${session.id}/index.m3u8`,
    subtitlePlaylist: session.sub !== 'off' ? `/hls/${session.id}/index_vtt.m3u8` : null,
    format: session.sub !== 'off' ? 'HLS/MPEG-TS + app-rendered WebVTT' : 'HLS/MPEG-TS',
    downloadedSinceStart: torrent ? Math.max(0, torrent.downloaded - (session.downloadedAtStart || 0)) : 0
  }
}

function stopHlsSession(session, reason = 'stopped') {
  if (!session) return
  if (!session.stopReason) session.stopReason = reason
  if (session.monitor) {
    clearInterval(session.monitor)
    session.monitor = null
  }
  if (session.ff && !session.exited) {
    try { session.ff.kill('SIGTERM') } catch {}
    // ffmpeg, заблокированный на чтении из подвисшего /raw, на SIGTERM может
    // не отреагировать вовсе — и продолжит перекодировать и тянуть торрент
    // уже после «остановки».
    if (!session.killTimer) {
      session.killTimer = setTimeout(() => {
        session.killTimer = null
        if (session.exited) return
        console.warn(`ffmpeg [${session.index}] ignored SIGTERM, sending SIGKILL`)
        try { session.ff.kill('SIGKILL') } catch {}
      }, 5000)
      if (session.killTimer.unref) session.killTimer.unref()
    }
  }
}

function scheduleHlsCleanup(session, delayMs = 2 * 60 * 1000) {
  if (!session || session.cleanupTimer) return
  session.cleanupTimer = setTimeout(() => {
    session.cleanupTimer = null

    // Never delete the HLS files of the session the TV is currently using.
    // ffmpeg can finish transcoding a whole episode much faster than realtime;
    // the player may still need those already-generated segments for many minutes.
    // Живой ffmpeg тоже держит каталог: удалить его под процессом — получить
    // поток ошибок записи вместо чистого выхода. В обоих случаях не выходим
    // молча, а перевзводим таймер, иначе каталог не удалится уже никогда.
    if (activeHlsSession === session || !session.exited) {
      scheduleHlsCleanup(session, 30 * 1000)
      return
    }

    hlsSessions.delete(session.id)
    safeRm(session.dir)
  }, delayMs)
  if (session.cleanupTimer.unref) session.cleanupTimer.unref()
}

async function startHlsSession(index, url) {
  const meta = await probeFile(index)
  if (!meta.video) throw new Error('No video stream')

  const audioPref = url.searchParams.get('audio') || ''
  const subPref = url.searchParams.get('sub') || 'off'
  const start = Math.max(0, Number(url.searchParams.get('start') || 0) || 0)
  const audio = chooseTrack(meta.audio, audioPref)
  const subtitle = subPref === 'off' ? null : chooseTrack(meta.subtitles, subPref)

  const id = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`
  const dir = path.join(os.tmpdir(), `tms-hls-${id}`)

  // Каталог создаётся до остановки предыдущего сеанса: если mkdir упадёт,
  // работающее воспроизведение не должно оказаться убитым зря.
  fs.mkdirSync(dir, { recursive: true })

  if (activeHlsSession) {
    stopHlsSession(activeHlsSession, 'replaced')
    scheduleHlsCleanup(activeHlsSession, 2 * 60 * 1000)
    activeHlsSession = null
  }

  const playlistPath = path.join(dir, 'index.m3u8')
  const segmentPattern = path.join(dir, 'seg%05d.ts')

  // MPEG-TS HLS is intentional: Samsung's 2015 streaming engine supports HLS v3,
  // while the previous fragmented-MP4 response is too new/fragile for that browser.
  const args = ['-hide_banner', '-loglevel', 'warning', '-i', rawUrl(index)]

  // Output-side seek keeps embedded-subtitle timing correct. For normal sequential
  // playback this is zero; resume/seek may have to read the already-viewed prefix.
  if (start > 0.05) args.push('-ss', start.toFixed(3))

  args.push('-map', `0:${meta.video.index}`)
  if (audio) args.push('-map', `0:${audio.index}`)
  else args.push('-an')

  // Do NOT burn embedded subtitles with the `subtitles=` video filter here.
  // That filter opens the media a second time and reads subtitle packets to EOF
  // before rendering starts, which forces a torrent-backed episode to download
  // almost completely before the first video segment can be produced.
  // Instead, selected subtitles are mapped once from the main input and emitted
  // as an out-of-band WebVTT HLS rendition.
  if (subtitle) args.push('-map', `0:${subtitle.index}`)

  // По умолчанию поток нормализуется под декодер 2015 года: H.264 + yuv420p
  // + AAC stereo в MPEG-TS. Если исходник уже ровно в этом виде, перекодировать
  // нечего — тогда дорожка отдаётся как есть (см. canCopyVideo/canCopyAudio).
  const copyVideo = canCopyVideo(meta.video, start)
  const copyAudio = Boolean(audio) && canCopyAudio(audio, start)

  args.push(...videoArgs(copyVideo))
  if (audio) args.push(...audioArgs(copyAudio))

  if (subtitle) args.push('-c:s', 'webvtt')
  else args.push('-sn')

  args.push(
    '-map_metadata', '-1',
    '-f', 'hls',
    '-hls_time', '4',
    '-hls_list_size', '0',
    '-hls_segment_type', 'mpegts',
    '-hls_flags', 'temp_file',
    '-hls_segment_filename', segmentPattern
  )

  // -var_stream_map нужен ровно для одного побочного эффекта: он заставляет
  // ffmpeg вынести субтитры в отдельную дорожку WebVTT (index_vtt.m3u8), которую
  // приложение забирает само. Появляющийся при этом master.m3u8 не читает никто:
  // Tizen 2.3 не разбирает #EXT-X-MEDIA, а браузерный клиент открывает
  // index.m3u8 напрямую. Имя A/V-плейлиста от этого не меняется.
  if (subtitle) {
    // Суффикс уникальности ('eng-2') — наша внутренняя добавка, в тег языка
    // должен уйти чистый код.
    const subLanguage = subtitle.code.replace(/-\d+$/, '')
    args.push(
      '-var_stream_map', `v:0${audio ? ',a:0' : ''},s:0,sgroup:subs,language:${subLanguage},default:yes`,
      '-master_pl_name', 'master.m3u8'
    )
  }

  args.push(playlistPath)

  const videoMode = copyVideo ? 'copy' : 'transcode'
  const audioMode = audio ? (copyAudio ? 'copy' : 'transcode') : 'none'
  console.log(`HLS [${index}] audio=${audio ? audio.code : 'none'} sub=${subtitle ? subtitle.code : 'off'} start=${start.toFixed(1)} video=${videoMode} audio-mode=${audioMode} mode=${subtitle ? 'webvtt' : 'no-subs'}`)

  const session = {
    id,
    dir,
    index: Number(index),
    name: meta.name,
    audio: audio ? audio.code : 'none',
    sub: subtitle ? subtitle.code : 'off',
    videoMode,
    audioMode,
    start,
    downloadedAtStart: torrent ? torrent.downloaded : 0,
    phase: 'preparing',
    stopReason: null,
    startedAt: Date.now(),
    lastOutputAt: null,
    ffmpegPid: null,
    exitCode: null,
    error: null,
    ff: null,
    exited: false,
    monitor: null,
    cleanupTimer: null,
    killTimer: null,
    segments: 0,
    bytesOut: 0,
    nextSeq: null
  }

  hlsSessions.set(id, session)
  activeHlsSession = session

  const ff = spawn('ffmpeg', args, { stdio: ['ignore', 'ignore', 'pipe'] })
  session.ff = ff
  session.ffmpegPid = ff.pid || null
  let stderr = ''

  ff.stderr.on('data', chunk => {
    stderr += chunk.toString('utf8')
    if (stderr.length > 16000) stderr = stderr.slice(-16000)
  })

  session.monitor = setInterval(() => {
    pollSegments(session)
    advancePhase(session, session.segments)
  }, 500)
  if (session.monitor.unref) session.monitor.unref()

  ff.on('error', err => {
    // Процесса нет — снимаем флаг, иначе чистка каталога будет ждать его вечно.
    session.exited = true
    session.error = err.message
    console.error('ffmpeg spawn error:', err)
  })

  ff.on('close', code => {
    session.exitCode = code
    session.exited = true
    if (session.killTimer) {
      clearTimeout(session.killTimer)
      session.killTimer = null
    }
    if (session.monitor) {
      clearInterval(session.monitor)
      session.monitor = null
    }
    // 255 и 143 — как ffmpeg уходит по сигналу, это не ошибка перекодирования.
    const wasStopped = Boolean(session.stopReason)
    if (!wasStopped && code && code !== 255 && code !== 143) {
      session.error = (stderr || `ffmpeg exited ${code}`).slice(-3000)
      console.error(`ffmpeg exited ${code}:\n${stderr}`)
    } else if (!wasStopped && !session.error) {
      session.phase = 'finished'
    }
    // Монитор уже остановлен — досчитываем последние сегменты сами.
    pollSegments(session)
    advancePhase(session, session.segments)
    session.lastOutputAt = Date.now()
    if (activeHlsSession === session) {
      console.log(`HLS [${session.index}] generation finished; keeping active session ${session.id} until playback stops or is replaced`)
    } else {
      scheduleHlsCleanup(session)
    }
  })

  return sessionSnapshot(session)
}

function serveHlsFile(res, sessionId, fileName) {
  const session = hlsSessions.get(sessionId)
  if (!session) return sendText(res, 404, 'HLS session not found')
  // ffmpeg may generate index.m3u8, master.m3u8, index_vtt.m3u8,
  // segNNNNN.ts and indexN.vtt. Only serve flat, safe media filenames.
  if (!/^[A-Za-z0-9._-]+\.(m3u8|ts|vtt)$/.test(fileName)) return sendText(res, 400, 'Bad HLS path')

  const full = path.join(session.dir, fileName)
  const type = fileName.endsWith('.m3u8')
    ? 'application/vnd.apple.mpegurl'
    : fileName.endsWith('.vtt')
      ? 'text/vtt; charset=utf-8'
      : 'video/mp2t'

  // Стримом, а не readFileSync: сегмент весит несколько мегабайт, и
  // синхронное чтение на каждый запрос вставало бы поперёк event loop.
  fs.stat(full, (err, stat) => {
    if (err || !stat.isFile()) return sendText(res, 404, 'Not ready')
    res.writeHead(200, {
      'Content-Type': type,
      'Content-Length': stat.size,
      'Cache-Control': fileName.endsWith('.m3u8') ? 'no-store' : 'public, max-age=3600',
      'Access-Control-Allow-Origin': '*'
    })
    const stream = fs.createReadStream(full)
    res.on('close', () => stream.destroy())
    stream.on('error', streamErr => res.destroy(streamErr))
    stream.pipe(res)
  })
}

function serveRaw(req, res, index) {
  const file = getFile(index)
  if (!file) return sendText(res, 404, 'File not found')

  const size = file.length
  const range = req.headers.range

  if (!range) {
    res.writeHead(200, {
      'Content-Type': file.type || 'application/octet-stream',
      'Content-Length': size,
      'Accept-Ranges': 'bytes',
      'Cache-Control': 'no-store'
    })
    if (req.method === 'HEAD') return res.end()
    const stream = file.createReadStream()
    res.on('close', () => stream.destroy())
    stream.on('error', err => res.destroy(err))
    return stream.pipe(res)
  }

  const match = /^bytes=(\d*)-(\d*)$/.exec(range)
  if (!match) {
    res.writeHead(416, { 'Content-Range': `bytes */${size}` })
    return res.end()
  }

  let start
  let end
  if (match[1] === '' && match[2] !== '') {
    const suffix = Number(match[2])
    start = Math.max(size - suffix, 0)
    end = size - 1
  } else {
    start = match[1] ? Number(match[1]) : 0
    end = match[2] ? Number(match[2]) : size - 1
  }

  if (!Number.isFinite(start) || !Number.isFinite(end) || start < 0 || end < start || start >= size) {
    res.writeHead(416, { 'Content-Range': `bytes */${size}` })
    return res.end()
  }
  end = Math.min(end, size - 1)

  res.writeHead(206, {
    'Content-Type': file.type || 'application/octet-stream',
    'Content-Length': end - start + 1,
    'Content-Range': `bytes ${start}-${end}/${size}`,
    'Accept-Ranges': 'bytes',
    'Cache-Control': 'no-store'
  })

  if (req.method === 'HEAD') return res.end()

  const stream = file.createReadStream({ start, end })
  res.on('close', () => stream.destroy())
  stream.on('error', err => res.destroy(err))
  stream.pipe(res)
}

function prebuffer(index, bytes = 8 * 1024 * 1024) {
  if (prebuffering.has(index)) return
  const file = getFile(index)
  if (!file) return
  prebuffering.add(index)
  const end = Math.min(file.length - 1, bytes - 1)
  const stream = file.createReadStream({ start: 0, end })
  stream.on('data', () => {})
  stream.on('error', err => {
    console.warn(`Prebuffer [${index}] failed:`, err.message)
  })
  stream.on('end', () => {
    console.log(`Prebuffered [${index}] ${Math.round((end + 1) / 1024 / 1024)} MB`)
  })
  // Флаг снимается на 'close': он приходит и после 'end', и после 'error',
  // и после уничтожения потока — иначе индекс остался бы залипшим навсегда.
  stream.on('close', () => prebuffering.delete(index))
}

function appHtml() {
  return `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Torrent Media Server</title>
<style>
  body { margin:0; background:#101010; color:#eee; font-family:Arial,sans-serif; font-size:20px; }
  .wrap { max-width:1100px; margin:0 auto; padding:22px; }
  h1 { font-size:30px; margin:0 0 18px; }
  .row { display:flex; gap:12px; align-items:center; flex-wrap:wrap; margin:12px 0; }
  select, button, input { font-size:20px; padding:10px 14px; background:#222; color:#fff; border:1px solid #555; border-radius:6px; }
  button:focus, select:focus, input:focus { outline:3px solid #ddd; }
  #episode { min-width:520px; max-width:100%; }
  video { display:block; width:100%; height:auto; max-height:65vh; background:#000; margin-top:16px; object-fit:contain; }
  video:fullscreen,
  video:-webkit-full-screen,
  video:-moz-full-screen {
    width:100vw !important;
    height:100vh !important;
    max-width:none !important;
    max-height:none !important;
    margin:0 !important;
    object-fit:contain !important;
    background:#000 !important;
  }
  .grow { flex:1; }
  .small { font-size:16px; color:#aaa; }
  #seek { width:100%; box-sizing:border-box; }
  #status { min-height:24px; color:#bbb; }
  .statusbox { margin:14px 0; padding:14px 16px; background:#191919; border:1px solid #333; border-radius:8px; }
  .statusline { margin:5px 0; }
  .status-ok { color:#79d279; }
  .status-warn { color:#ffd166; }
  .status-bad { color:#ff7777; }
  .status-muted { color:#aaa; }
</style>
</head>
<body>
<div class="wrap">
  <h1 id="title">Torrent Media Server</h1>

  <div class="row">
    <button id="prev">◀ Previous</button>
    <select id="episode" class="grow"></select>
    <button id="next">Next ▶</button>
  </div>

  <div class="row">
    <label>Audio <select id="audio"></select></label>
    <label>Subtitles <select id="sub"><option value="off">Off</option></select></label>
    <label><input id="autonext" type="checkbox"> Auto next</label>
    <button id="play">▶ Play</button>
  </div>

  <div class="statusbox">
    <div class="statusline"><strong>Status:</strong> <span id="humanStatus">Starting server status…</span></div>
    <div class="statusline small" id="torrentStatus">Torrent: checking…</div>
    <div class="statusline small" id="playerStatus">Player: idle</div>
  </div>

  <video id="video" controls playsinline></video>

  <div class="row">
    <button id="back30">−30s</button>
    <input id="seek" class="grow" type="range" min="0" max="1" step="1" value="0">
    <button id="forward30">+30s</button>
  </div>
  <div class="row small">
    <span id="clock">00:00 / 00:00</span>
    <span id="status" class="grow"></span>
  </div>
</div>
<script>
(function () {
  var episodes = [];
  var meta = null;
  var currentIndex = null;
  var startOffset = 0;
  var saveTimer = 0;
  var prebufferedFor = null;
  var isPlaying = false;

  var episode = document.getElementById('episode');
  var audio = document.getElementById('audio');
  var sub = document.getElementById('sub');
  var autonext = document.getElementById('autonext');
  var video = document.getElementById('video');
  var seek = document.getElementById('seek');
  var clock = document.getElementById('clock');
  var status = document.getElementById('status');
  var humanStatus = document.getElementById('humanStatus');
  var torrentStatus = document.getElementById('torrentStatus');
  var playerStatus = document.getElementById('playerStatus');
  var lastServerStatus = null;
  var browserState = 'idle';

  function setFullscreenSizing(on) {
    if (on) {
      video.style.width = '100vw';
      video.style.height = '100vh';
      video.style.maxWidth = 'none';
      video.style.maxHeight = 'none';
      video.style.margin = '0';
      video.style.objectFit = 'contain';
    } else {
      video.style.width = '100%';
      video.style.height = 'auto';
      video.style.maxWidth = '';
      video.style.maxHeight = '65vh';
      video.style.margin = '16px 0 0';
      video.style.objectFit = 'contain';
    }
  }

  function syncFullscreenSizing() {
    var fs = document.fullscreenElement || document.webkitFullscreenElement || document.mozFullScreenElement || document.msFullscreenElement;
    setFullscreenSizing(fs === video || !!document.webkitIsFullScreen || !!document.mozFullScreen);
  }

  if (document.addEventListener) {
    document.addEventListener('fullscreenchange', syncFullscreenSizing, false);
    document.addEventListener('webkitfullscreenchange', syncFullscreenSizing, false);
    document.addEventListener('mozfullscreenchange', syncFullscreenSizing, false);
  }
  video.addEventListener('webkitbeginfullscreen', function () { setFullscreenSizing(true); }, false);
  video.addEventListener('webkitendfullscreen', function () { setFullscreenSizing(false); }, false);

  function get(key, fallback) {
    try {
      var v = localStorage.getItem(key);
      return v === null ? fallback : v;
    } catch (e) { return fallback; }
  }

  function set(key, value) {
    try { localStorage.setItem(key, String(value)); } catch (e) {}
  }

  function positions() {
    try { return JSON.parse(get('tms.positions', '{}')) || {}; }
    catch (e) { return {}; }
  }

  function savePosition(value) {
    if (currentIndex === null) return;
    var p = positions();
    p[String(currentIndex)] = Math.max(0, Math.floor(value));
    try { localStorage.setItem('tms.positions', JSON.stringify(p)); } catch (e) {}
  }

  function clearPosition(index) {
    var p = positions();
    delete p[String(index)];
    try { localStorage.setItem('tms.positions', JSON.stringify(p)); } catch (e) {}
  }

  function xhrJson(url, callback) {
    var x = new XMLHttpRequest();
    x.open('GET', url, true);
    x.onreadystatechange = function () {
      if (x.readyState !== 4) return;
      if (x.status >= 200 && x.status < 300) {
        try { callback(null, JSON.parse(x.responseText)); }
        catch (e) { callback(e); }
      } else callback(new Error('HTTP ' + x.status));
    };
    x.send();
  }

  function ping(url) {
    var x = new XMLHttpRequest();
    x.open('GET', url, true);
    x.send();
  }

  function fmt(seconds) {
    seconds = Math.max(0, Math.floor(seconds || 0));
    var h = Math.floor(seconds / 3600);
    var m = Math.floor((seconds % 3600) / 60);
    var s = seconds % 60;
    if (h) return h + ':' + (m < 10 ? '0' : '') + m + ':' + (s < 10 ? '0' : '') + s;
    return m + ':' + (s < 10 ? '0' : '') + s;
  }

  function fmtBytes(bytes) {
    bytes = Number(bytes || 0);
    if (bytes < 1024) return Math.round(bytes) + ' B';
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
    if (bytes < 1024 * 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1) + ' MB';
    return (bytes / 1024 / 1024 / 1024).toFixed(2) + ' GB';
  }

  function fmtSpeed(bytes) {
    return fmtBytes(bytes) + '/s';
  }

  function setHumanStatus(text, cls) {
    humanStatus.textContent = text;
    humanStatus.className = cls || '';
  }

  function refreshHumanStatus() {
    var s = lastServerStatus;
    if (!s || !s.ready) {
      setHumanStatus('Loading torrent metadata…', 'status-warn');
      return;
    }
    if (s.playback && s.playback.state === 'error') {
      setHumanStatus('Playback failed — ffmpeg returned an error.', 'status-bad');
      return;
    }
    if (browserState === 'error') {
      setHumanStatus('Browser could not play the stream.', 'status-bad');
      return;
    }
    if (s.peers === 0) {
      setHumanStatus('No torrent peers yet — waiting for sources.', 'status-warn');
      return;
    }
    if (browserState === 'playing') {
      setHumanStatus('Playing normally.', 'status-ok');
      return;
    }
    if (browserState === 'waiting' || browserState === 'stalled') {
      if (s.downloadSpeed > 0) setHumanStatus('Buffering — torrent data is arriving, please wait.', 'status-warn');
      else setHumanStatus('Buffering — connected to peers, but no data is arriving right now.', 'status-warn');
      return;
    }
    if (s.playback && (s.playback.state === 'preparing' || s.playback.state === 'buffering')) {
      if (s.downloadSpeed > 0) setHumanStatus('Preparing Samsung-compatible HLS — generating startup segments…', 'status-warn');
      else setHumanStatus('Preparing HLS — waiting for required torrent pieces…', 'status-warn');
      return;
    }
    if (s.playback && (s.playback.state === 'ready' || s.playback.state === 'finished') && s.playback.bytesOut > 0) {
      setHumanStatus('HLS segments are ready; browser is opening the native TV stream.', 'status-warn');
      return;
    }
    if (isPlaying) {
      setHumanStatus('Starting playback…', 'status-warn');
      return;
    }
    setHumanStatus('Ready.', 'status-ok');
  }

  function pollServerStatus() {
    xhrJson('/api/status?_=' + Date.now(), function (err, s) {
      if (err) {
        torrentStatus.textContent = 'Torrent: status request failed: ' + err.message;
        torrentStatus.className = 'statusline small status-bad';
        setHumanStatus('Cannot reach the media server.', 'status-bad');
        return;
      }
      lastServerStatus = s;
      if (!s.ready) {
        torrentStatus.textContent = 'Torrent: loading metadata…';
        torrentStatus.className = 'statusline small status-warn';
      } else {
        torrentStatus.textContent = 'Torrent: ' + s.peers + ' peers · ' + fmtSpeed(s.downloadSpeed) + ' · ' + fmtBytes(s.downloaded) + ' received · total torrent ' + (Number(s.progress || 0) * 100).toFixed(2) + '%';
        torrentStatus.className = 'statusline small ' + (s.peers > 0 ? 'status-ok' : 'status-warn');
      }
      if (!s.playback) {
        playerStatus.textContent = 'Player: idle';
        playerStatus.className = 'statusline small status-muted';
      } else {
        var pb = s.playback;
        var age = Math.max(0, Math.floor((Date.now() - pb.startedAt) / 1000));
        var extra = pb.bytesOut ? ' · ' + fmtBytes(pb.bytesOut) + ' produced · ' + (pb.segments || 0) + ' HLS segments' : '';
        if (typeof pb.downloadedSinceStart === 'number') extra += ' · ' + fmtBytes(pb.downloadedSinceStart) + ' downloaded for this playback';
        if (pb.lastOutputAt) extra += ' · last output ' + Math.max(0, Math.floor((Date.now() - pb.lastOutputAt) / 1000)) + 's ago';
        playerStatus.textContent = 'Player: ' + pb.state + ' · episode [' + pb.index + '] · audio=' + pb.audio + ' · subtitles=' + pb.sub +
          ' · video ' + (pb.videoMode || '?') + '/audio ' + (pb.audioMode || '?') + ' · ' + age + 's' + extra;
        playerStatus.className = 'statusline small ' + (pb.state === 'error' ? 'status-bad' : ((pb.state === 'ready' || pb.state === 'finished') ? 'status-ok' : 'status-warn'));
      }
      refreshHumanStatus();
    });
  }

  function absoluteTime() {
    return startOffset + (isFinite(video.currentTime) ? video.currentTime : 0);
  }

  function option(select, value, label) {
    var o = document.createElement('option');
    o.value = value;
    o.textContent = label;
    select.appendChild(o);
  }

  function pickValue(select, preferred) {
    var i;
    for (i = 0; i < select.options.length; i++) {
      if (select.options[i].value === preferred) {
        select.selectedIndex = i;
        return true;
      }
    }
    return false;
  }

  function populateTracks() {
    var preferredAudio = get('tms.audio', 'rus');
    var preferredSub = get('tms.sub', 'off');
    var i;

    audio.innerHTML = '';
    for (i = 0; i < meta.audio.length; i++) {
      option(audio, meta.audio[i].code, meta.audio[i].label + (meta.audio[i].codec ? ' (' + meta.audio[i].codec + ')' : ''));
    }
    if (!pickValue(audio, preferredAudio) && audio.options.length) audio.selectedIndex = 0;

    sub.innerHTML = '';
    option(sub, 'off', 'Off');
    for (i = 0; i < meta.subtitles.length; i++) {
      option(sub, meta.subtitles[i].code, meta.subtitles[i].label + (meta.subtitles[i].codec ? ' (' + meta.subtitles[i].codec + ')' : ''));
    }
    if (!pickValue(sub, preferredSub)) sub.value = 'off';
  }

  function updateClock() {
    var now = absoluteTime();
    var total = meta ? meta.duration : 0;
    clock.textContent = fmt(now) + ' / ' + fmt(total);
    if (total > 0) {
      seek.max = Math.max(1, Math.floor(total));
      if (!seek.matches || !seek.matches(':active')) seek.value = Math.min(total, now);
    }

    if (meta && meta.next !== null && total > 0 && total - now < 90 && prebufferedFor !== meta.next) {
      prebufferedFor = meta.next;
      ping('/api/prebuffer/' + meta.next);
      // Also probe the next episode so language matching is ready.
      ping('/api/probe/' + meta.next);
    }
  }

  function waitForHls(sessionId, playlist, automatic) {
    xhrJson('/api/hls-status/' + encodeURIComponent(sessionId) + '?_=' + Date.now(), function (err, s) {
      if (err) {
        status.textContent = 'HLS status failed: ' + err.message;
        browserState = 'error';
        refreshHumanStatus();
        return;
      }
      if (s.state === 'error') {
        status.textContent = 'HLS preparation failed.';
        browserState = 'error';
        refreshHumanStatus();
        return;
      }
      // Сеанс остановлен или заменён более новым /api/start: его сегменты ещё
      // лежат на диске, но открывать их нельзя. Молча выходим — экраном
      // распоряжается тот опрос, который заменил этот.
      if (s.state === 'stopped' || s.state === 'replaced') return;
      if (s.segments >= 2 || s.state === 'finished') {
        status.textContent = 'Opening Samsung-compatible HLS stream…';
        browserState = 'loading';
        video.src = playlist + '?_=' + Date.now();
        video.load();
        isPlaying = true;
        var p = video.play();
        if (p && p.catch) p.catch(function () {
          if (automatic) status.textContent = 'Press Play on the remote to continue.';
        });
        return;
      }
      status.textContent = 'Preparing HLS: ' + (s.segments || 0) + '/2 startup segments…';
      browserState = 'waiting';
      refreshHumanStatus();
      setTimeout(function () { waitForHls(sessionId, playlist, automatic); }, 700);
    });
  }

  function playFrom(seconds, automatic) {
    if (!meta) return;
    seconds = Math.max(0, Math.min(seconds || 0, Math.max(0, meta.duration - 1)));
    startOffset = seconds;
    prebufferedFor = null;
    set('tms.autonext', autonext.checked ? '1' : '0');

    video.pause();
    video.removeAttribute('src');
    video.load();
    status.textContent = 'Starting Samsung-compatible HLS…';
    browserState = 'starting';
    refreshHumanStatus();

    var startUrl = '/api/start/' + currentIndex + '?audio=' + encodeURIComponent(audio.value || '') +
      '&sub=' + encodeURIComponent(sub.value || 'off') + '&start=' + encodeURIComponent(seconds.toFixed(3)) + '&_=' + Date.now();
    xhrJson(startUrl, function (err, data) {
      if (err) {
        status.textContent = 'Cannot start HLS: ' + err.message;
        browserState = 'error';
        refreshHumanStatus();
        return;
      }
      waitForHls(data.id, data.playlist, automatic);
    });
  }

  function loadEpisode(index, automatic) {
    index = Number(index);
    currentIndex = index;
    set('tms.lastEpisode', index);
    episode.value = String(index);
    status.textContent = 'Reading tracks...';
    browserState = 'probing';
    refreshHumanStatus();
    video.pause();
    video.removeAttribute('src');
    video.load();
    isPlaying = false;

    xhrJson('/api/probe/' + index, function (err, data) {
      if (err) {
        status.textContent = 'Probe failed: ' + err.message;
        return;
      }
      meta = data;
      document.getElementById('title').textContent = data.name;
      populateTracks();
      var saved = Number(positions()[String(index)] || 0);
      if (saved > 10 && (!data.duration || saved < data.duration - 30)) startOffset = saved;
      else startOffset = 0;
      seek.max = Math.max(1, Math.floor(data.duration || 1));
      seek.value = Math.floor(startOffset);
      updateClock();
      status.textContent = 'Ready';
      browserState = 'idle';
      refreshHumanStatus();
      if (automatic) playFrom(0, true);
    });
  }

  function goNext(automatic) {
    if (!meta || meta.next === null) return;
    clearPosition(currentIndex);
    loadEpisode(meta.next, automatic);
  }

  function goPrev() {
    if (!meta || meta.prev === null) return;
    loadEpisode(meta.prev, false);
  }

  episode.onchange = function () { loadEpisode(Number(episode.value), false); };
  document.getElementById('prev').onclick = goPrev;
  document.getElementById('next').onclick = function () { goNext(false); };
  document.getElementById('play').onclick = function () { playFrom(startOffset || Number(seek.value) || 0, false); };

  audio.onchange = function () {
    set('tms.audio', audio.value);
    if (isPlaying) playFrom(absoluteTime(), false);
  };

  sub.onchange = function () {
    set('tms.sub', sub.value);
    if (isPlaying) playFrom(absoluteTime(), false);
  };

  autonext.onchange = function () { set('tms.autonext', autonext.checked ? '1' : '0'); };

  document.getElementById('back30').onclick = function () { playFrom(Math.max(0, absoluteTime() - 30), false); };
  document.getElementById('forward30').onclick = function () { playFrom(Math.min(meta ? meta.duration - 1 : 0, absoluteTime() + 30), false); };

  seek.onchange = function () { playFrom(Number(seek.value) || 0, false); };

  video.addEventListener('loadstart', function () { browserState = 'loading'; status.textContent = 'Loading stream…'; refreshHumanStatus(); });
  video.addEventListener('loadedmetadata', function () { browserState = 'metadata'; status.textContent = 'Video metadata received…'; refreshHumanStatus(); });
  video.addEventListener('canplay', function () { browserState = 'canplay'; status.textContent = 'Enough data to start…'; refreshHumanStatus(); });
  video.addEventListener('playing', function () { status.textContent = ''; isPlaying = true; browserState = 'playing'; refreshHumanStatus(); });
  video.addEventListener('waiting', function () { status.textContent = 'Buffering…'; browserState = 'waiting'; refreshHumanStatus(); });
  video.addEventListener('stalled', function () { status.textContent = 'Stream stalled…'; browserState = 'stalled'; refreshHumanStatus(); });
  video.addEventListener('pause', function () { if (!video.ended && browserState === 'playing') { browserState = 'paused'; refreshHumanStatus(); } });
  video.addEventListener('error', function () {
    var code = video.error ? video.error.code : 0;
    status.textContent = 'Playback error' + (code ? ' (code ' + code + ')' : '') + '.';
    browserState = 'error';
    refreshHumanStatus();
  });
  video.addEventListener('timeupdate', function () {
    updateClock();
    var now = absoluteTime();
    var t = Date.now();
    if (t - saveTimer > 5000) {
      saveTimer = t;
      savePosition(now);
    }
  });
  video.addEventListener('ended', function () {
    isPlaying = false;
    browserState = 'ended';
    clearPosition(currentIndex);
    if (autonext.checked && meta && meta.next !== null) goNext(true);
    else status.textContent = 'Episode finished';
  });

  autonext.checked = get('tms.autonext', '1') !== '0';
  pollServerStatus();
  setInterval(pollServerStatus, 1000);

  xhrJson('/api/files', function (err, data) {
    if (err) {
      status.textContent = 'Cannot load file list: ' + err.message;
      return;
    }
    episodes = data.files;
    document.getElementById('title').textContent = data.torrent;
    episode.innerHTML = '';
    for (var i = 0; i < episodes.length; i++) option(episode, String(episodes[i].index), episodes[i].name);
    var last = Number(get('tms.lastEpisode', episodes.length ? episodes[0].index : 0));
    var exists = false;
    for (var j = 0; j < episodes.length; j++) if (episodes[j].index === last) exists = true;
    if (!exists && episodes.length) last = episodes[0].index;
    if (episodes.length) loadEpisode(last, false);
  });
})();
</script>
</body>
</html>`
}

const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url, `http://${req.headers.host || 'localhost'}`)

    if (req.method === 'OPTIONS') {
      res.writeHead(204, {
        'Access-Control-Allow-Origin': '*',
        'Access-Control-Allow-Methods': 'GET,HEAD,OPTIONS',
        'Access-Control-Allow-Headers': 'Content-Type, Range'
      })
      return res.end()
    }

    if (url.pathname === '/') return sendText(res, 200, appHtml(), 'text/html; charset=utf-8')

    if (url.pathname === '/api/files') {
      if (!torrent) return sendJson(res, 503, { error: 'Torrent metadata is loading' })
      return sendJson(res, 200, { torrent: torrent.name, files: videoFiles() })
    }

    if (url.pathname === '/api/status') {
      if (!torrent) return sendJson(res, 200, { ready: false })
      return sendJson(res, 200, {
        ready: true,
        peers: torrent.numPeers,
        downloadSpeed: torrent.downloadSpeed,
        downloaded: torrent.downloaded,
        progress: torrent.progress,
        // Считается на месте: снапшот дешёвый, а отдельное зеркало оставалось
        // висеть после остановки и показывало клиенту мёртвый сеанс.
        playback: sessionSnapshot(activeHlsSession)
      })
    }

    if (url.pathname === '/api/stop') {
      if (activeHlsSession) {
        const session = activeHlsSession
        stopHlsSession(session, 'stopped')
        activeHlsSession = null
        scheduleHlsCleanup(session, 2 * 60 * 1000)
        return sendJson(res, 200, { ok: true, stopped: session.id })
      }
      return sendJson(res, 200, { ok: true, stopped: null })
    }

    let m = url.pathname.match(/^\/api\/probe\/(\d+)$/)
    if (m) {
      if (!torrent) return sendJson(res, 503, { error: 'Torrent metadata is loading' })
      const index = Number(m[1])
      if (!getFile(index)) return sendJson(res, 404, { error: 'File not found' })
      // Текст ошибки здесь функциональный (таймаут ffprobe, битый файл) —
      // оба клиента показывают его на экране.
      try { return sendJson(res, 200, await probeFile(index)) }
      catch (err) { return sendJson(res, 500, { error: err.message }) }
    }

    m = url.pathname.match(/^\/api\/prebuffer\/(\d+)$/)
    if (m) {
      if (!torrent) return sendJson(res, 503, { error: 'Torrent metadata is loading' })
      const index = Number(m[1])
      if (!getFile(index)) return sendJson(res, 404, { error: 'File not found' })
      prebuffer(index)
      return sendJson(res, 200, { ok: true })
    }

    m = url.pathname.match(/^\/raw\/(\d+)$/)
    if (m) return serveRaw(req, res, Number(m[1]))

    m = url.pathname.match(/^\/api\/start\/(\d+)$/)
    if (m) {
      if (!torrent) return sendJson(res, 503, { error: 'Torrent metadata is loading' })
      const index = Number(m[1])
      if (!getFile(index)) return sendJson(res, 404, { error: 'File not found' })
      try { return sendJson(res, 200, await startHlsSession(index, url)) }
      catch (err) { return sendJson(res, 500, { error: err.message }) }
    }

    m = url.pathname.match(/^\/api\/hls-status\/([a-z0-9-]+)$/i)
    if (m) {
      const session = hlsSessions.get(m[1])
      if (!session) return sendJson(res, 404, { error: 'HLS session not found' })
      return sendJson(res, 200, sessionSnapshot(session))
    }

    m = url.pathname.match(/^\/hls\/([a-z0-9-]+)\/([A-Za-z0-9._-]+\.(?:m3u8|ts|vtt))$/i)
    if (m) return serveHlsFile(res, m[1], m[2])

    return sendText(res, 404, 'Not found')
  } catch (err) {
    console.error(err)
    // Наружу — только общая формулировка: в err.message тут попадают пути
    // на диске и хвост stderr ffmpeg.
    if (!res.headersSent) sendText(res, 500, 'Internal server error')
    else res.destroy(err)
  }
})

server.listen(PORT, '0.0.0.0', () => {
  console.log(`HTTP server listening on :${PORT}`)
})

client.add(torrentFile, {
  path: TORRENT_STORE,
  deselect: true,
  destroyStoreOnDestroy: true
}, t => {
  torrent = t
  torrent.on('error', err => console.error('Torrent error:', err))
  torrent.on('warning', err => console.warn('Torrent warning:', err.message || err))
  console.log(`Torrent loaded: ${torrent.name}`)
  console.log(`Video files: ${videoFiles().length}`)
})

let shuttingDown = false

async function shutdown() {
  if (shuttingDown) return
  shuttingDown = true
  console.log('\nStopping...')

  // Процесс всё равно уходит — ffmpeg добиваем сразу, ждать вежливого выхода
  // незачем. Каталоги сеансов удаляем здесь же: иначе `tms-hls-*` остаётся
  // в tmpdir после каждого запуска, с сегментами целого эпизода внутри.
  for (const session of hlsSessions.values()) {
    if (session.monitor) clearInterval(session.monitor)
    if (session.cleanupTimer) clearTimeout(session.cleanupTimer)
    if (session.killTimer) clearTimeout(session.killTimer)
    if (session.ff && !session.exited) {
      try { session.ff.kill('SIGKILL') } catch {}
    }
    safeRm(session.dir)
  }
  hlsSessions.clear()
  activeHlsSession = null

  // Открытое чтение /raw или сокеты webtorrent могут не отпустить процесс —
  // уходим по таймеру, а не висим.
  const watchdog = setTimeout(() => process.exit(0), 5000)
  if (watchdog.unref) watchdog.unref()

  await new Promise(resolve => server.close(resolve))
  try { await client.destroy() } catch {}
  process.exit(0)
}

process.on('SIGINT', shutdown)
process.on('SIGTERM', shutdown)
