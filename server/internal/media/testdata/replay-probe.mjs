import fs from "node:fs";
const output = fs.readFileSync(process.argv[2], "utf8");
const index = 0; const file = { name: "S01E01 - Fixture.mkv" };
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
    next: null,
    prev: null
  }
console.log(JSON.stringify(result))
