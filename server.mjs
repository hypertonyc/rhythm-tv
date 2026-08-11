import WebTorrent from 'webtorrent'
import http from 'node:http'
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import os from 'node:os'

const PORT = Number(process.env.PORT || 8000)
const torrentFile = process.argv[2]

if (!torrentFile) {
  console.error('Usage: node server.mjs /data/file.torrent')
  process.exit(1)
}

const client = new WebTorrent()
let torrent = null
const probeCache = new Map()
const prebuffering = new Set()
let playbackStatus = null
const hlsSessions = new Map()
let activeHlsSession = null

client.on('error', err => console.error('WebTorrent error:', err))
client.on('warning', err => console.warn('WebTorrent warning:', err.message || err))

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

function nextVideoIndex(index) {
  const list = videoFiles()
  const pos = list.findIndex(x => x.index === Number(index))
  return pos >= 0 && pos + 1 < list.length ? list[pos + 1].index : null
}

function prevVideoIndex(index) {
  const list = videoFiles()
  const pos = list.findIndex(x => x.index === Number(index))
  return pos > 0 ? list[pos - 1].index : null
}

function normalizeLanguage(stream, ordinal, kind) {
  const tags = stream.tags || {}
  const raw = String(tags.language || '').toLowerCase()
  const title = String(tags.title || '').toLowerCase()
  const haystack = `${raw} ${title}`

  if (/\b(rus|ru|russian|рус|русский)\b/.test(haystack)) return { code: 'rus', label: 'Russian' }
  if (/\b(eng|en|english|англ|английский)\b/.test(haystack)) return { code: 'eng', label: 'English' }
  if (/\b(tha|th|thai)\b/.test(haystack)) return { code: 'tha', label: 'Thai' }

  if (raw && raw !== 'und') {
    return { code: raw, label: tags.title || raw.toUpperCase() }
  }

  return {
    code: `${kind}-${ordinal + 1}`,
    label: tags.title || `${kind === 'audio' ? 'Audio' : 'Subtitles'} ${ordinal + 1}`
  }
}

function rawUrl(index) {
  return `http://127.0.0.1:${PORT}/raw/${index}`
}

function runCapture(command, args) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { stdio: ['ignore', 'pipe', 'pipe'] })
    const stdout = []
    const stderr = []
    child.stdout.on('data', chunk => stdout.push(chunk))
    child.stderr.on('data', chunk => stderr.push(chunk))
    child.on('error', reject)
    child.on('close', code => {
      const out = Buffer.concat(stdout).toString('utf8')
      const err = Buffer.concat(stderr).toString('utf8')
      if (code === 0) resolve(out)
      else reject(new Error(`${command} exited with ${code}: ${err}`))
    })
  })
}

async function probeFile(index) {
  index = Number(index)
  if (probeCache.has(index)) return probeCache.get(index)
  const file = getFile(index)
  if (!file) throw new Error('File not found')

  const output = await runCapture('ffprobe', [
    '-v', 'error',
    '-show_streams',
    '-show_format',
    '-of', 'json',
    rawUrl(index)
  ])

  const data = JSON.parse(output)
  const streams = data.streams || []
  const audioRaw = streams.filter(s => s.codec_type === 'audio')
  const subtitleRaw = streams.filter(s => s.codec_type === 'subtitle')
  const video = streams.find(s => s.codec_type === 'video') || null

  const audio = audioRaw.map((s, i) => {
    const lang = normalizeLanguage(s, i, 'audio')
    return {
      index: s.index,
      relativeIndex: i,
      code: lang.code,
      label: lang.label,
      codec: s.codec_name || '',
      default: Boolean(s.disposition && s.disposition.default)
    }
  })

  const subtitles = subtitleRaw.map((s, i) => {
    const lang = normalizeLanguage(s, i, 'sub')
    return {
      index: s.index,
      relativeIndex: i,
      code: lang.code,
      label: lang.label,
      codec: s.codec_name || '',
      default: Boolean(s.disposition && s.disposition.default)
    }
  })

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
      pixFmt: video.pix_fmt || ''
    } : null,
    audio,
    subtitles,
    next: nextVideoIndex(index),
    prev: prevVideoIndex(index)
  }

  probeCache.set(index, result)
  return result
}

function chooseTrack(tracks, preference) {
  if (!tracks.length) return null
  if (preference) {
    const exact = tracks.find(t => t.code === preference)
    if (exact) return exact
  }
  return tracks.find(t => t.default) || tracks[0]
}

function escapeFilterUrl(url) {
  // ffmpeg filter syntax needs ':' escaped and the URL quoted.
  return `'${url.replace(/\\/g, '\\\\').replace(/:/g, '\\:').replace(/'/g, "\\'")}'`
}

function safeRm(dir) {
  try { fs.rmSync(dir, { recursive: true, force: true }) } catch {}
}

function sessionSnapshot(session) {
  if (!session) return null
  let segments = 0
  let bytesOut = 0
  try {
    const names = fs.readdirSync(session.dir)
    for (const name of names) {
      if (/^seg\d+\.ts$/.test(name)) {
        segments++
        try { bytesOut += fs.statSync(path.join(session.dir, name)).size } catch {}
      }
    }
  } catch {}
  return {
    id: session.id,
    index: session.index,
    name: session.name,
    audio: session.audio,
    sub: session.sub,
    start: session.start,
    state: session.state,
    startedAt: session.startedAt,
    segments,
    bytesOut,
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
  if (session.state !== 'error' && session.state !== 'finished') session.state = reason
  if (session.monitor) {
    clearInterval(session.monitor)
    session.monitor = null
  }
  if (session.ff && !session.ff.killed) {
    try { session.ff.kill('SIGTERM') } catch {}
  }
}

function scheduleHlsCleanup(session, delayMs = 2 * 60 * 1000) {
  if (!session || session.cleanupTimer) return
  session.cleanupTimer = setTimeout(() => {
    session.cleanupTimer = null

    // Never delete the HLS files of the session the TV is currently using.
    // ffmpeg can finish transcoding a whole episode much faster than realtime;
    // the player may still need those already-generated segments for many minutes.
    if (activeHlsSession === session) return

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

  if (activeHlsSession) {
    stopHlsSession(activeHlsSession, 'replaced')
    scheduleHlsCleanup(activeHlsSession, 2 * 60 * 1000)
  }

  const id = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`
  const dir = path.join(os.tmpdir(), `tms-hls-${id}`)
  fs.mkdirSync(dir, { recursive: true })
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

  // Always normalize the stream for the 2015 Samsung decoder/browser:
  // H.264 + yuv420p + AAC in MPEG-TS HLS segments.
  args.push(
    '-c:v', 'libx264',
    '-preset', 'veryfast',
    '-crf', '20',
    '-pix_fmt', 'yuv420p',
    '-profile:v', 'high',
    '-level:v', '4.0',
    '-sc_threshold', '0',
    '-force_key_frames', 'expr:gte(t,n_forced*4)'
  )

  if (audio) {
    args.push('-c:a', 'aac', '-b:a', '160k', '-ac', '2', '-ar', '48000')
  }

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

  // With subtitles, generate a small HLS master playlist that points at the
  // MPEG-TS A/V stream plus an out-of-band WebVTT subtitle rendition. Samsung
  // Smart TV recommends WebVTT for HLS, and this allows the first media segments
  // to be generated immediately instead of pre-reading the full MP4.
  if (subtitle) {
    args.push(
      '-var_stream_map', `v:0${audio ? ',a:0' : ''},s:0,sgroup:subs,language:${subtitle.code},default:yes`,
      '-master_pl_name', 'master.m3u8'
    )
  }

  args.push(playlistPath)

  console.log(`HLS [${index}] audio=${audio ? audio.code : 'none'} sub=${subtitle ? subtitle.code : 'off'} start=${start.toFixed(1)} mode=${subtitle ? 'webvtt' : 'no-subs'}`)

  const session = {
    id,
    dir,
    index: Number(index),
    name: meta.name,
    audio: audio ? audio.code : 'none',
    sub: subtitle ? subtitle.code : 'off',
    start,
    playlistFile: 'index.m3u8',
    downloadedAtStart: torrent ? torrent.downloaded : 0,
    state: 'preparing',
    startedAt: Date.now(),
    lastOutputAt: null,
    ffmpegPid: null,
    exitCode: null,
    error: null,
    ff: null,
    monitor: null,
    cleanupTimer: null,
    lastSegmentCount: 0,
    lastBytesOut: 0
  }

  hlsSessions.set(id, session)
  activeHlsSession = session
  playbackStatus = sessionSnapshot(session)

  const ff = spawn('ffmpeg', args, { stdio: ['ignore', 'ignore', 'pipe'] })
  session.ff = ff
  session.ffmpegPid = ff.pid || null
  let stderr = ''

  ff.stderr.on('data', chunk => {
    stderr += chunk.toString('utf8')
    if (stderr.length > 16000) stderr = stderr.slice(-16000)
  })

  session.monitor = setInterval(() => {
    const snap = sessionSnapshot(session)
    if (snap.segments > session.lastSegmentCount || snap.bytesOut > session.lastBytesOut) {
      session.lastOutputAt = Date.now()
      session.lastSegmentCount = snap.segments
      session.lastBytesOut = snap.bytesOut
      if (snap.segments >= 2 && session.state === 'preparing') session.state = 'ready'
      else if (snap.segments >= 1 && session.state === 'preparing') session.state = 'buffering'
    }
    if (activeHlsSession === session) playbackStatus = sessionSnapshot(session)
  }, 500)
  if (session.monitor.unref) session.monitor.unref()

  ff.on('error', err => {
    session.state = 'error'
    session.error = err.message
    if (activeHlsSession === session) playbackStatus = sessionSnapshot(session)
    console.error('ffmpeg spawn error:', err)
  })

  ff.on('close', code => {
    session.exitCode = code
    if (session.monitor) {
      clearInterval(session.monitor)
      session.monitor = null
    }
    const wasStopped = ['replaced', 'stopped'].includes(session.state)
    if (!wasStopped && code && code !== 255 && code !== 143) {
      session.state = 'error'
      session.error = (stderr || `ffmpeg exited ${code}`).slice(-3000)
      console.error(`ffmpeg exited ${code}:\n${stderr}`)
    } else if (!wasStopped && session.state !== 'error') {
      session.state = 'finished'
    }
    session.lastOutputAt = Date.now()
    if (activeHlsSession === session) {
      playbackStatus = sessionSnapshot(session)
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
  if (!fs.existsSync(full)) return sendText(res, 404, 'Not ready')

  let body
  try { body = fs.readFileSync(full) }
  catch (err) { return sendText(res, 500, err.message) }

  const type = fileName.endsWith('.m3u8')
    ? 'application/vnd.apple.mpegurl'
    : fileName.endsWith('.vtt')
      ? 'text/vtt; charset=utf-8'
      : 'video/mp2t'
  res.writeHead(200, {
    'Content-Type': type,
    'Content-Length': body.length,
    'Cache-Control': fileName.endsWith('.m3u8') ? 'no-store' : 'public, max-age=3600',
    'Access-Control-Allow-Origin': '*'
  })
  res.end(body)
}

function serveRaw(req, res, index) {
  const file = getFile(index)
  if (!file) return sendText(res, 404, 'File not found')

  const size = file.length
  const range = req.headers.range

  if (req.method === 'HEAD' && !range) {
    res.writeHead(200, {
      'Content-Type': file.type || 'application/octet-stream',
      'Content-Length': size,
      'Accept-Ranges': 'bytes',
      'Cache-Control': 'no-store'
    })
    return res.end()
  }

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
  index = Number(index)
  if (prebuffering.has(index)) return
  const file = getFile(index)
  if (!file) return
  prebuffering.add(index)
  const end = Math.min(file.length - 1, bytes - 1)
  const stream = file.createReadStream({ start: 0, end })
  stream.on('data', () => {})
  stream.on('error', err => {
    console.warn(`Prebuffer [${index}] failed:`, err.message)
    prebuffering.delete(index)
  })
  stream.on('end', () => {
    console.log(`Prebuffered [${index}] ${Math.round((end + 1) / 1024 / 1024)} MB`)
    prebuffering.delete(index)
  })
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
        playerStatus.textContent = 'Player: ' + pb.state + ' · episode [' + pb.index + '] · audio=' + pb.audio + ' · subtitles=' + pb.sub + ' · ' + age + 's' + extra;
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

  function waitForHls(sessionId, playlist, automatic, attempt) {
    attempt = attempt || 0;
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
      setTimeout(function () { waitForHls(sessionId, playlist, automatic, attempt + 1); }, 700);
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
      waitForHls(data.id, data.playlist, automatic, 0);
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
        playback: playbackStatus
      })
    }

    if (url.pathname === '/api/stop') {
      if (activeHlsSession) {
        const session = activeHlsSession
        stopHlsSession(session, 'stopped')
        session.lastOutputAt = Date.now()
        playbackStatus = sessionSnapshot(session)
        activeHlsSession = null
        scheduleHlsCleanup(session, 2 * 60 * 1000)
        return sendJson(res, 200, { ok: true, stopped: session.id })
      }
      return sendJson(res, 200, { ok: true, stopped: null })
    }

    let m = url.pathname.match(/^\/api\/probe\/(\d+)$/)
    if (m) {
      if (!torrent) return sendJson(res, 503, { error: 'Torrent metadata is loading' })
      try { return sendJson(res, 200, await probeFile(Number(m[1]))) }
      catch (err) { return sendJson(res, 500, { error: err.message }) }
    }

    m = url.pathname.match(/^\/api\/prebuffer\/(\d+)$/)
    if (m) {
      prebuffer(Number(m[1]))
      return sendJson(res, 200, { ok: true })
    }

    m = url.pathname.match(/^\/raw\/(\d+)$/)
    if (m) return serveRaw(req, res, Number(m[1]))

    m = url.pathname.match(/^\/api\/start\/(\d+)$/)
    if (m) {
      if (!torrent) return sendJson(res, 503, { error: 'Torrent metadata is loading' })
      try { return sendJson(res, 200, await startHlsSession(Number(m[1]), url)) }
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
    if (!res.headersSent) sendText(res, 500, err.message)
    else res.destroy(err)
  }
})

server.listen(PORT, '0.0.0.0', () => {
  console.log(`HTTP server listening on :${PORT}`)
})

client.add(torrentFile, {
  path: '/tmp/webtorrent',
  deselect: true,
  destroyStoreOnDestroy: true
}, t => {
  torrent = t
  torrent.on('error', err => console.error('Torrent error:', err))
  torrent.on('warning', err => console.warn('Torrent warning:', err.message || err))
  console.log(`Torrent loaded: ${torrent.name}`)
  console.log(`Video files: ${videoFiles().length}`)
})

async function shutdown() {
  console.log('\nStopping...')
  for (const session of hlsSessions.values()) stopHlsSession(session)
  server.close()
  try { await client.destroy() } catch {}
  process.exit(0)
}

process.on('SIGINT', shutdown)
process.on('SIGTERM', shutdown)
