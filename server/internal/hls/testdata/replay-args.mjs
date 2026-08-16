import fs from "node:fs";
import path from "node:path";
const scenarios = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
const out = [];
for (const sc of scenarios) {
  const PORT = 8000;
  const ALLOW_COPY = sc.allowCopy;
  const rawUrl = (i) => `http://127.0.0.1:${PORT}/raw/${i}`;
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
  const index = sc.index, meta = sc.meta, audio = sc.audio, subtitle = sc.subtitle;
  const start = sc.start;
  const dir = sc.dir;
  const playlistPath = path.join(dir, "index.m3u8");
  const segmentPattern = path.join(dir, "seg%05d.ts");
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
  out.push({ name: sc.name, args });
}
console.log(JSON.stringify(out, null, 2));
