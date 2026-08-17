/* Rhythm TV client for Samsung Tizen 2.3 (2015 TV).
 * Keep this file ES5-compatible: no let/const, arrows, fetch, Promise, async/await.
 */
(function () {
    'use strict';

    /* Baked in at package time from DEFAULT_SERVER in .env, via the generated
     * js/config.js. Entering an address with the remote is painful, so the TV
     * should never have to; empty only means we fall back to the setup screen. */
    var DEFAULT_SERVER = (window.RTV_CONFIG && window.RTV_CONFIG.defaultServer) || '';

    var serverBase = '';
    /* Имя активного торрента (из /api/files). Под ним хранятся позиции
     * просмотра, и по нему же замечается переключение торрента с телефона. */
    var torrentKey = '';
    var lastLibraryCheck = 0;
    var lostSessionPolls = 0;
    /* id сеанса, который мы сейчас играем: по нему отличается «сеанс погасили»
     * от «сервер перезапустился и подобрал наш каталог». */
    var currentSessionId = '';
    var episodes = [];
    var seasons = [];
    var seasonPos = 0;
    var episodePos = 0;
    var meta = null;
    var audioPos = 0;
    var subPos = 0; /* 0 = off, then meta.subtitles + 1 */
    var autoNext = true;
    var menuRow = 0;
    var setupRow = 0;
    var mode = 'boot';
    var startOffset = 0;
    var lastPlayerTime = 0;
    /* Максимум, до которого доходили часы плеера в этом сеансе. Отдельно
     * от lastPlayerTime, потому что прошивка умеет откатить время назад,
     * а вопрос «доигрывали ли мы до конца» после отката всё равно надо
     * задавать — см. reachedEndOnce. */
    var maxPlayerTime = 0;
    /* Когда AVPlay последний раз сообщал ДРУГОЕ время. Ноль — картинка ещё
     * не пошла. По этой отметке сторожится молчаливый простой, см. watchForStall. */
    var lastPlayTimeAt = 0;
    var stallLimitMs = 30000;
    /* Сколько до конца серии считается «доиграли» и сколько после этого ждём
     * onstreamcompleted, прежде чем закончить серию самим. См. endOfEpisodeStuck. */
    var endZoneSec = 3;
    var endGraceMs = 10000;
    var endZoneSince = 0;
    var lastPlaytimeCallAt = 0;
    var lastSavedAt = 0;
    var prebufferedFor = null;
    var hudTimer = null;
    var statusTimer = null;
    var restarting = false;
    var subtitlePlaylistUrl = '';
    var subtitlePollTimer = null;
    var subtitleCues = [];
    var subtitleSegments = {};
    var subtitleLastText = null;
    var subtitleSizes = [
        { key: 'small', label: 'Small', px: 40 },
        { key: 'medium', label: 'Medium', px: 48 },
        { key: 'large', label: 'Large', px: 58 },
        { key: 'xlarge', label: 'Extra large', px: 70 }
    ];
    var subtitleSizePos = 2;

    var menuRows = ['season', 'episode', 'audio', 'sub', 'subsize', 'autonext', 'play', 'server'];

    function el(id) { return document.getElementById(id); }

    function escapeHtml(value) {
        return String(value === null || value === undefined ? '' : value)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;');
    }

    /* The TV gives us no console, so anything fatal goes on screen. */
    function fatal(message) {
        var box = el('fatal');
        if (!box) return;
        box.className = 'fatal';
        box.innerHTML = escapeHtml(message);
    }

    window.onerror = function (message, source, line) {
        fatal('JS error: ' + message + '\n' + source + ':' + line);
        return false;
    };

    function storeGet(key, fallback) {
        try {
            var v = localStorage.getItem(key);
            return v === null ? fallback : v;
        } catch (e) { return fallback; }
    }

    function storeSet(key, value) {
        try { localStorage.setItem(key, String(value)); } catch (e) {}
    }

    function readJson(key) {
        try { return JSON.parse(storeGet(key, '{}')); } catch (e) { return {}; }
    }

    function writeJson(key, value) {
        try { storeSet(key, JSON.stringify(value)); } catch (e) {}
    }

    /* Позиции просмотра хранятся ПО ТОРРЕНТУ: с тех пор как торрент можно
     * переключить с телефона, индекс файла сам по себе ничего не значит —
     * тот же индекс в другом торренте это другая серия.
     * Ключ — имя торрента из /api/files.
     *
     * Старый плоский формат {индекс: секунды} подбирается при первом чтении
     * и приписывается тому торренту, который активен сейчас: когда его
     * записывали, торрент был один. */
    function allPositions() {
        var data = readJson('rtv.positions');
        var wrapped, k;
        if (!data || typeof data !== 'object') return {};
        for (k in data) {
            if (!Object.prototype.hasOwnProperty.call(data, k)) continue;
            if (data[k] === null || typeof data[k] !== 'object') {
                wrapped = {};
                if (torrentKey) wrapped[torrentKey] = data;
                writeJson('rtv.positions', wrapped);
                return wrapped;
            }
        }
        return data;
    }

    function positions() {
        return allPositions()[torrentKey] || {};
    }

    function storePositions(p) {
        var all = allPositions();
        all[torrentKey] = p;
        writeJson('rtv.positions', all);
    }

    function savedPosition(index) {
        var p = positions();
        return Number(p[String(index)] || 0) || 0;
    }

    function savePosition(index, seconds) {
        if (index === null || index === undefined || !isFinite(seconds)) return;
        var p = positions();
        p[String(index)] = Math.max(0, Math.floor(seconds));
        storePositions(p);
    }

    function clearPosition(index) {
        var p = positions();
        delete p[String(index)];
        storePositions(p);
    }

    /* rtv.lastEpisode — тоже по торренту; старый формат был просто числом. */
    function allLastEpisodes() {
        var data = readJson('rtv.lastEpisode');
        var wrapped;
        if (data !== null && typeof data === 'object') return data;
        wrapped = {};
        if (torrentKey && typeof data === 'number') wrapped[torrentKey] = data;
        writeJson('rtv.lastEpisode', wrapped);
        return wrapped;
    }

    function lastEpisode(fallback) {
        var v = allLastEpisodes()[torrentKey];
        return typeof v === 'number' ? v : fallback;
    }

    function saveLastEpisode(index) {
        var all = allLastEpisodes();
        all[torrentKey] = Number(index);
        writeJson('rtv.lastEpisode', all);
    }

    function cleanBase(value) {
        value = String(value || '').replace(/^\s+|\s+$/g, '');
        while (value.length > 1 && value.charAt(value.length - 1) === '/') value = value.slice(0, -1);
        if (value && !/^https?:\/\//i.test(value)) value = 'http://' + value;
        return value;
    }

    /* Wraps a request so exactly one callback always happens. On this WRT a request
     * refused by WARP can end without firing onerror/ontimeout at all, which would
     * otherwise leave the caller waiting forever. */
    function guardedRequest(xhr, url, timeout, callback) {
        var settled = false;
        var watchdog = setTimeout(function () {
            if (settled) return;
            settled = true;
            try { xhr.abort(); } catch (e) {}
            callback(new Error('No response from ' + url));
        }, timeout + 1500);

        return function settle(err, data) {
            if (settled) return;
            settled = true;
            clearTimeout(watchdog);
            callback(err, data);
        };
    }

    function api(path, callback, timeout) {
        var xhr = new XMLHttpRequest();
        var url = serverBase + path;
        var settle;
        timeout = timeout || 12000;
        settle = guardedRequest(xhr, url, timeout, callback);
        xhr.open('GET', url, true);
        xhr.timeout = timeout;
        xhr.onreadystatechange = function () {
            if (xhr.readyState !== 4) return;
            if (xhr.status >= 200 && xhr.status < 300) {
                var parsed;
                try { parsed = JSON.parse(xhr.responseText); }
                catch (e) { settle(new Error('Bad JSON from ' + path)); return; }
                settle(null, parsed);
            } else {
                /* Код кладётся в саму ошибку: вызывающему бывает важно отличить
                 * «сервер ответил, что не знает» от «до сервера не достучались».
                 * См. confirmSessionLost — там на этом держится выход в меню. */
                var httpErr = new Error('HTTP ' + xhr.status + ' for ' + path);
                httpErr.status = xhr.status;
                settle(httpErr);
            }
        };
        xhr.onerror = function () { settle(new Error('Network error: ' + url)); };
        xhr.ontimeout = function () { settle(new Error('Timeout: ' + url)); };
        try { xhr.send(); }
        catch (e) { settle(e); }
    }

    function apiIgnore(path) {
        api(path, function () {}, 5000);
    }

    function xhrText(url, callback, timeout) {
        var xhr = new XMLHttpRequest();
        var settle;
        timeout = timeout || 8000;
        settle = guardedRequest(xhr, url, timeout, callback);
        xhr.open('GET', url, true);
        xhr.timeout = timeout;
        xhr.onreadystatechange = function () {
            if (xhr.readyState !== 4) return;
            if (xhr.status >= 200 && xhr.status < 300) settle(null, xhr.responseText || '');
            else settle(new Error('HTTP ' + xhr.status + ' for ' + url));
        };
        xhr.onerror = function () { settle(new Error('Network error: ' + url)); };
        xhr.ontimeout = function () { settle(new Error('Timeout: ' + url)); };
        try { xhr.send(); } catch (e) { settle(e); }
    }

    function parseVttTime(value) {
        var parts = String(value || '').replace(/^\s+|\s+$/g, '').split(':');
        var h = 0, m = 0, sec = 0;
        if (parts.length === 3) {
            h = Number(parts[0]) || 0;
            m = Number(parts[1]) || 0;
            sec = Number(parts[2]) || 0;
        } else if (parts.length === 2) {
            m = Number(parts[0]) || 0;
            sec = Number(parts[1]) || 0;
        } else return 0;
        return h * 3600 + m * 60 + sec;
    }

    function stripVttMarkup(text) {
        return String(text || '')
            .replace(/<br\s*\/?>/gi, '\n')
            .replace(/<[^>]+>/g, '')
            .replace(/&nbsp;/g, ' ')
            .replace(/&amp;/g, '&')
            .replace(/&lt;/g, '<')
            .replace(/&gt;/g, '>');
    }

    function parseVtt(text) {
        var normalized = String(text || '').replace(/\r/g, '');
        var blocks = normalized.split(/\n\s*\n/);
        var cues = [];
        var i, j, lines, arrow, timing, left, right, start, end, body;
        for (i = 0; i < blocks.length; i++) {
            lines = blocks[i].split('\n');
            arrow = -1;
            for (j = 0; j < lines.length; j++) {
                if (lines[j].indexOf('-->') >= 0) { arrow = j; break; }
            }
            if (arrow < 0) continue;
            timing = lines[arrow].split('-->');
            if (timing.length < 2) continue;
            left = timing[0].replace(/^\s+|\s+$/g, '');
            right = timing[1].replace(/^\s+|\s+$/g, '').split(/\s+/)[0];
            start = parseVttTime(left);
            end = parseVttTime(right);
            body = stripVttMarkup(lines.slice(arrow + 1).join('\n'))
                .replace(/^\s+|\s+$/g, '')
                .replace(/\n\s*\n+/g, '\n');
            if (body && end > start) cues.push({ start: start, end: end, text: body });
        }
        return cues;
    }

    function resetSubtitles() {
        if (subtitlePollTimer) {
            clearTimeout(subtitlePollTimer);
            subtitlePollTimer = null;
        }
        subtitlePlaylistUrl = '';
        subtitleCues = [];
        subtitleSegments = {};
        subtitleLastText = null;
        clearSubtitleCanvas();
    }

    function subtitleBaseUrl(url) {
        var q = url.indexOf('?');
        if (q >= 0) url = url.substring(0, q);
        return url.substring(0, url.lastIndexOf('/') + 1);
    }

    function loadSubtitleSegment(url) {
        if (subtitleSegments[url]) return;
        subtitleSegments[url] = 'loading';
        xhrText(url + (url.indexOf('?') >= 0 ? '&' : '?') + '_=' + new Date().getTime(), function (err, text) {
            if (err) {
                delete subtitleSegments[url];
                return;
            }
            subtitleSegments[url] = 'done';
            var fresh = parseVtt(text);
            var i;
            for (i = 0; i < fresh.length; i++) subtitleCues.push(fresh[i]);
            subtitleCues.sort(function (a, b) { return a.start - b.start; });
            updateSubtitleOverlay();
        }, 8000);
    }

    function pollSubtitlePlaylist() {
        if (!subtitlePlaylistUrl || mode !== 'player') return;
        xhrText(subtitlePlaylistUrl + (subtitlePlaylistUrl.indexOf('?') >= 0 ? '&' : '?') + '_=' + new Date().getTime(), function (err, text) {
            if (!err) {
                var lines = String(text || '').replace(/\r/g, '').split('\n');
                var base = subtitleBaseUrl(subtitlePlaylistUrl);
                var i, line;
                for (i = 0; i < lines.length; i++) {
                    line = lines[i].replace(/^\s+|\s+$/g, '');
                    if (!line || line.charAt(0) === '#') continue;
                    if (/\.vtt(?:\?|$)/i.test(line)) {
                        if (/^https?:\/\//i.test(line)) loadSubtitleSegment(line);
                        else loadSubtitleSegment(base + line);
                    }
                }
            }
            if (subtitlePlaylistUrl && mode === 'player') {
                subtitlePollTimer = setTimeout(pollSubtitlePlaylist, 1200);
            }
        }, 5000);
    }

    function startSubtitleOverlay(relativeUrl) {
        resetSubtitles();
        if (!relativeUrl || selectedSubCode() === 'off') return;
        subtitlePlaylistUrl = /^https?:\/\//i.test(relativeUrl) ? relativeUrl : serverBase + relativeUrl;
        pollSubtitlePlaylist();
    }

    function ensureSubtitleCanvas() {
        var canvas = el('subtitleCanvas');
        if (!canvas) return null;
        var w = window.innerWidth || 1920;
        var h = window.innerHeight || 1080;
        if (canvas.width !== w) canvas.width = w;
        if (canvas.height !== h) canvas.height = h;
        return canvas;
    }

    function clearSubtitleCanvas() {
        var canvas = ensureSubtitleCanvas();
        if (!canvas) return;
        var ctx = canvas.getContext('2d');
        if (ctx) ctx.clearRect(0, 0, canvas.width, canvas.height);
    }

    function wrapSubtitleLine(ctx, text, maxWidth) {
        var words = String(text || '').split(/\s+/);
        var lines = [];
        var line = '';
        var i, test;
        for (i = 0; i < words.length; i++) {
            if (!words[i]) continue;
            test = line ? (line + ' ' + words[i]) : words[i];
            if (line && ctx.measureText(test).width > maxWidth) {
                lines.push(line);
                line = words[i];
            } else {
                line = test;
            }
        }
        if (line) lines.push(line);
        if (!lines.length) lines.push('');
        return lines;
    }

    function roundedRect(ctx, x, y, w, h, r) {
        if (r > w / 2) r = w / 2;
        if (r > h / 2) r = h / 2;
        ctx.beginPath();
        ctx.moveTo(x + r, y);
        ctx.lineTo(x + w - r, y);
        ctx.quadraticCurveTo(x + w, y, x + w, y + r);
        ctx.lineTo(x + w, y + h - r);
        ctx.quadraticCurveTo(x + w, y + h, x + w - r, y + h);
        ctx.lineTo(x + r, y + h);
        ctx.quadraticCurveTo(x, y + h, x, y + h - r);
        ctx.lineTo(x, y + r);
        ctx.quadraticCurveTo(x, y, x + r, y);
        ctx.closePath();
    }

    function drawSubtitle(text) {
        if (text === subtitleLastText) return;
        subtitleLastText = text;

        var canvas = ensureSubtitleCanvas();
        if (!canvas) return;
        var ctx = canvas.getContext('2d');
        if (!ctx) return;
        ctx.clearRect(0, 0, canvas.width, canvas.height);
        if (!text) return;

        var fontSize = subtitleSizes[subtitleSizePos].px;
        var lineHeight = Math.round(fontSize * 1.24);
        var maxWidth = Math.round(canvas.width * 0.86);
        var rawLines = String(text).split(/\r?\n/);
        var lines = [];
        var i, j, part;

        ctx.font = 'bold ' + fontSize + 'px sans-serif';
        ctx.textAlign = 'center';
        ctx.textBaseline = 'top';

        for (i = 0; i < rawLines.length; i++) {
            if (!/\S/.test(rawLines[i])) continue;
            part = wrapSubtitleLine(ctx, rawLines[i], maxWidth);
            for (j = 0; j < part.length; j++) {
                if (part[j]) lines.push(part[j]);
            }
        }
        if (!lines.length) return;

        var widest = 0;
        for (i = 0; i < lines.length; i++) {
            var mw = ctx.measureText(lines[i]).width;
            if (mw > widest) widest = mw;
        }

        var padX = Math.max(18, Math.round(fontSize * 0.38));
        var padY = Math.max(8, Math.round(fontSize * 0.18));
        var boxW = Math.min(canvas.width * 0.92, widest + padX * 2);
        var boxH = lines.length * lineHeight + padY * 2;
        var boxX = (canvas.width - boxW) / 2;
        var boxY = canvas.height - 105 - boxH;

        ctx.fillStyle = 'rgba(0,0,0,0.62)';
        roundedRect(ctx, boxX, boxY, boxW, boxH, 9);
        ctx.fill();

        ctx.fillStyle = '#ffffff';
        ctx.shadowColor = 'rgba(0,0,0,0.9)';
        ctx.shadowBlur = 3;
        ctx.shadowOffsetX = 0;
        ctx.shadowOffsetY = 2;

        for (i = 0; i < lines.length; i++) {
            ctx.fillText(lines[i], canvas.width / 2, boxY + padY + i * lineHeight);
        }

        ctx.shadowColor = 'transparent';
        ctx.shadowBlur = 0;
        ctx.shadowOffsetX = 0;
        ctx.shadowOffsetY = 0;
    }

    function updateSubtitleOverlay() {
        if (!subtitlePlaylistUrl || selectedSubCode() === 'off') {
            drawSubtitle('');
            return;
        }
        /* Время сеанса, а НЕ серии. Метки в VTT приезжают от ffmpeg, который
         * режет HLS с нуля при любом start: реплика на 16:33.514 в серии,
         * запущенной с 991.52, лежит в сегменте под меткой 00:01.994.
         * Сверка с absoluteTime() давала сдвиг ровно на start — субтитры
         * спешили на столько, с какой минуты продолжили просмотр. На сериях
         * с нуля обе величины совпадают, поэтому баг и не был виден. */
        var t = lastPlayerTime / 1000;
        var text = '';
        var i;
        for (i = 0; i < subtitleCues.length; i++) {
            if (t >= subtitleCues[i].start && t < subtitleCues[i].end) {
                text = text ? (text + '\n' + subtitleCues[i].text) : subtitleCues[i].text;
            }
            if (subtitleCues[i].start > t + 2) break;
        }
        drawSubtitle(text);
    }

    function fmt(seconds) {
        seconds = Math.max(0, Math.floor(Number(seconds) || 0));
        var h = Math.floor(seconds / 3600);
        var m = Math.floor((seconds % 3600) / 60);
        var s = seconds % 60;
        function p(n) { return n < 10 ? '0' + n : String(n); }
        return h ? (h + ':' + p(m) + ':' + p(s)) : (p(m) + ':' + p(s));
    }

    function fmtSpeed(bytes) {
        bytes = Number(bytes) || 0;
        if (bytes < 1024) return Math.round(bytes) + ' B/s';
        if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB/s';
        return (bytes / 1024 / 1024).toFixed(1) + ' MB/s';
    }

    function parseEpisode(file) {
        var m = /^S(\d+)E(\d+)\s*-\s*(.*?)(?:\.(?:mp4|m4v|mkv|webm))?$/i.exec(file.name);
        if (m) {
            return {
                index: file.index,
                name: file.name,
                season: Number(m[1]),
                episode: Number(m[2]),
                label: 'S' + (m[1].length < 2 ? '0' + m[1] : m[1]) + 'E' + (m[2].length < 2 ? '0' + m[2] : m[2]) + ' — ' + m[3]
            };
        }
        return {
            index: file.index,
            name: file.name,
            season: 999,
            episode: 1,
            label: file.name.replace(/\.[^.]+$/, '')
        };
    }

    function buildSeasons(files) {
        var groups = {};
        var order = [];
        var i, ep, key;
        episodes = [];
        for (i = 0; i < files.length; i++) {
            ep = parseEpisode(files[i]);
            episodes.push(ep);
            key = String(ep.season);
            if (!groups[key]) {
                groups[key] = [];
                order.push(ep.season);
            }
            groups[key].push(ep);
        }
        order.sort(function (a, b) { return a - b; });
        seasons = [];
        for (i = 0; i < order.length; i++) {
            key = String(order[i]);
            seasons.push({
                season: order[i],
                label: order[i] === 999 ? 'Specials' : 'Season ' + order[i],
                episodes: groups[key]
            });
        }
    }

    function currentSeason() { return seasons.length ? seasons[seasonPos] : null; }
    function currentEpisode() {
        var s = currentSeason();
        return s && s.episodes.length ? s.episodes[episodePos] : null;
    }

    function findEpisodeByIndex(index) {
        var i, j;
        index = Number(index);
        for (i = 0; i < seasons.length; i++) {
            for (j = 0; j < seasons[i].episodes.length; j++) {
                if (Number(seasons[i].episodes[j].index) === index) return { seasonPos: i, episodePos: j };
            }
        }
        return null;
    }

    function selectEpisodeIndex(index) {
        var found = findEpisodeByIndex(index);
        if (found) {
            seasonPos = found.seasonPos;
            episodePos = found.episodePos;
            saveLastEpisode(index);
            return true;
        }
        return false;
    }

    function selectedAudioCode() {
        if (!meta || !meta.audio || !meta.audio.length) return '';
        if (audioPos < 0 || audioPos >= meta.audio.length) audioPos = 0;
        return meta.audio[audioPos].code;
    }

    function selectedSubCode() {
        if (!meta || subPos <= 0) return 'off';
        if (!meta.subtitles || subPos > meta.subtitles.length) return 'off';
        return meta.subtitles[subPos - 1].code;
    }

    function loadSubtitleSize() {
        var pref = storeGet('rtv.subtitleSize', 'large');
        var i;
        subtitleSizePos = 2;
        for (i = 0; i < subtitleSizes.length; i++) {
            if (subtitleSizes[i].key === pref) { subtitleSizePos = i; break; }
        }
        applySubtitleSize();
    }

    function applySubtitleSize() {
        subtitleLastText = null;
        updateSubtitleOverlay();
    }

    function changeSubtitleSize(delta) {
        subtitleSizePos = (subtitleSizePos + delta + subtitleSizes.length) % subtitleSizes.length;
        storeSet('rtv.subtitleSize', subtitleSizes[subtitleSizePos].key);
        applySubtitleSize();
        refreshMenu();
    }

    function choosePreferredTracks() {
        var audioPref = storeGet('rtv.audio', 'rus');
        var subPref = storeGet('rtv.sub', 'off');
        var i;
        audioPos = 0;
        if (meta && meta.audio) {
            for (i = 0; i < meta.audio.length; i++) {
                if (meta.audio[i].code === audioPref) { audioPos = i; break; }
            }
        }
        subPos = 0;
        if (subPref !== 'off' && meta && meta.subtitles) {
            for (i = 0; i < meta.subtitles.length; i++) {
                if (meta.subtitles[i].code === subPref) { subPos = i + 1; break; }
            }
        }
    }

    function setStatus(text, kind) {
        var s = el('menuStatus');
        if (!s) return;
        s.className = 'status' + (kind ? ' ' + kind : '');
        s.innerHTML = text || '';
    }

    function showSetup(message) {
        mode = 'setup';
        el('playerScreen').className = 'player-screen hidden';
        el('menuScreen').className = 'screen hidden';
        el('setupScreen').className = 'screen';
        el('av-player').style.display = 'none';
        el('serverInput').value = serverBase || DEFAULT_SERVER;
        setupRow = 0;
        refreshSetupFocus();
        el('setupStatus').innerHTML = message || '';
    }

    function refreshSetupFocus() {
        el('serverInput').className = 'server-input' + (setupRow === 0 ? ' focused' : '');
        el('setupConnect').className = 'setup-button' + (setupRow === 1 ? ' focused' : '');
    }

    function connectFromSetup() {
        serverBase = cleanBase(el('serverInput').value);
        if (!serverBase) {
            el('setupStatus').innerHTML = 'Server address is empty.';
            return;
        }
        el('setupStatus').innerHTML = 'Connecting to ' + serverBase + '…';
        api('/api/files', function (err, data) {
            if (err) {
                el('setupStatus').innerHTML = 'Cannot connect: ' + err.message;
                return;
            }
            storeSet('rtv.server', serverBase);
            loadLibrary(data);
        }, 15000);
    }

    function loadLibrary(data) {
        /* Имя торрента — ключ, под которым хранятся позиции просмотра,
         * поэтому оно ставится ДО первого обращения к ним. */
        torrentKey = data.torrent || '';
        buildSeasons(data.files || []);
        el('torrentTitle').innerHTML = data.torrent || 'Rhythm TV';
        autoNext = storeGet('rtv.autonext', '1') !== '0';
        var last = lastEpisode(seasons.length && seasons[0].episodes.length ? seasons[0].episodes[0].index : 0);
        if (!selectEpisodeIndex(last) && seasons.length && seasons[0].episodes.length) {
            seasonPos = 0;
            episodePos = 0;
        }
        showMenu();
        refreshMeta();
        startStatusPolling();
    }

    /* Активный торрент выбирают с телефона, и телевизор обязан это заметить:
     * иначе он останется со списком серий прежнего торрента, а индексы в нём
     * указывают уже на другие файлы — «продолжить с 24-й минуты» открыло бы
     * чужую серию. Сравнивается имя торрента: отдельного поля в /api/files нет,
     * а менять его формат ради этого не стоит — он заморожен. */
    function checkLibrary() {
        api('/api/files', function (err, data) {
            if (err || !data) return;
            var name = data.torrent || '';
            if (name === torrentKey) return;
            meta = null;
            loadLibrary(data);
            setStatus('Torrent switched to ' + escapeHtml(name), 'ok');
        }, 8000);
    }

    function showMenu() {
        mode = 'menu';
        el('setupScreen').className = 'screen hidden';
        el('playerScreen').className = 'player-screen hidden';
        el('menuScreen').className = 'screen';
        el('av-player').style.display = 'none';
        refreshMenu();
    }

    function refreshMenuFocus() {
        var i;
        for (i = 0; i < menuRows.length; i++) {
            el('row-' + menuRows[i]).className = 'menu-row' + (menuRows[i] === 'play' ? ' action-row' : '') + (i === menuRow ? ' focused' : '');
        }
    }

    function refreshMenu() {
        var s = currentSeason();
        var ep = currentEpisode();
        if (!s || !ep) return;
        el('seasonValue').innerHTML = '‹  ' + s.label + '  ›';
        el('episodeValue').innerHTML = '‹  ' + ep.label + '  ›';
        el('episodeTitle').innerHTML = ep.label;
        if (meta && Number(meta.index) === Number(ep.index)) {
            el('episodeMeta').innerHTML = (meta.duration ? fmt(meta.duration) : '') + (meta.video ? ' · ' + meta.video.width + '×' + meta.video.height : '');
            if (meta.audio && meta.audio.length) {
                el('audioValue').innerHTML = '‹  ' + meta.audio[audioPos].label + '  ›';
            } else {
                el('audioValue').innerHTML = 'No audio track';
            }
            if (subPos === 0) el('subValue').innerHTML = '‹  Off  ›';
            else el('subValue').innerHTML = '‹  ' + meta.subtitles[subPos - 1].label + '  ›';
        } else {
            el('episodeMeta').innerHTML = 'Reading media tracks…';
            el('audioValue').innerHTML = '…';
            el('subValue').innerHTML = '…';
        }
        el('subtitleSizeValue').innerHTML = '‹  ' + subtitleSizes[subtitleSizePos].label + ' (' + subtitleSizes[subtitleSizePos].px + ' px)  ›';
        el('autonextValue').innerHTML = '‹  ' + (autoNext ? 'On' : 'Off') + '  ›';
        var saved = savedPosition(ep.index);
        el('playValue').innerHTML = saved > 10 ? ('Resume from ' + fmt(saved) + '  ▶') : 'Start  ▶';
        el('serverValue').innerHTML = serverBase;
        refreshMenuFocus();
    }

    function refreshMeta(callback) {
        var ep = currentEpisode();
        if (!ep) return;
        meta = null;
        refreshMenu();
        setStatus('Reading audio/subtitle tracks…', 'warn');
        api('/api/probe/' + ep.index, function (err, data) {
            /* Пока ответ ехал, пользователь мог уйти на другую серию. Ответ
             * про прежнюю обязан быть выброшен: иначе meta.index разъедется
             * с выбранной серией, refreshMenu уйдёт в ветку «…» — и останется
             * там навсегда, потому что нового запроса никто уже не сделает.
             * Ловится это только на разной длительности проб: пока разбор
             * приходил из кэша мгновенно, обгонять было нечему. */
            var still = currentEpisode();
            if (!still || Number(still.index) !== Number(ep.index)) {
                if (callback) callback(new Error('Episode changed'));
                return;
            }
            if (err) {
                setStatus('Probe failed: ' + err.message, 'error');
                if (callback) callback(err);
                return;
            }
            meta = data;
            choosePreferredTracks();
            saveLastEpisode(ep.index);
            setStatus('Ready.', 'ok');
            refreshMenu();
            if (callback) callback(null, data);
        }, 30000);
    }

    function changeSeason(delta) {
        if (!seasons.length) return;
        seasonPos = (seasonPos + delta + seasons.length) % seasons.length;
        episodePos = 0;
        refreshMeta();
    }

    function changeEpisode(delta) {
        var s = currentSeason();
        if (!s || !s.episodes.length) return;
        episodePos = (episodePos + delta + s.episodes.length) % s.episodes.length;
        refreshMeta();
    }

    function changeAudio(delta) {
        if (!meta || !meta.audio || !meta.audio.length) return;
        audioPos = (audioPos + delta + meta.audio.length) % meta.audio.length;
        storeSet('rtv.audio', selectedAudioCode());
        refreshMenu();
    }

    function changeSub(delta) {
        if (!meta) return;
        var count = (meta.subtitles ? meta.subtitles.length : 0) + 1;
        subPos = (subPos + delta + count) % count;
        storeSet('rtv.sub', selectedSubCode());
        refreshMenu();
    }

    function toggleAutoNext() {
        autoNext = !autoNext;
        storeSet('rtv.autonext', autoNext ? '1' : '0');
        refreshMenu();
    }

    function absoluteTime() {
        return Math.max(0, startOffset + (lastPlayerTime / 1000));
    }

    function durationSeconds() {
        return meta ? Number(meta.duration || 0) : 0;
    }

    /* Доходили ли часы плеера до конца серии хоть раз за сеанс. Спрашивается
     * после отката времени назад, когда absoluteTime() показывает уже начало,
     * поэтому считается по максимуму, а не по последнему значению. */
    function reachedEndOnce(withinSec) {
        var total = durationSeconds();
        return total > 0 && total - Math.max(0, startOffset + (maxPlayerTime / 1000)) < withinSec;
    }

    /* Плейлист доигран, а onstreamcompleted не пришёл. Прошивка в этом случае
     * молча заходит на плейлист заново и крутит seg00000 по кругу — снаружи
     * это «первые секунды серии повторяются». Сторож по откату времени
     * (см. oncurrentplaytime) такую петлю поймать не смог: 17.08.2026 часы
     * AVPlay после захода на второй круг не откатились, а продолжили идти,
     * и телевизор повторял начало серии, пока его не выключили пультом.
     *
     * Ловим по-другому и без опоры на то, что прошивка сделает с часами:
     * доиграв до конца серии, стоять в конце незачем. Даём прошивке
     * endGraceMs сказать это самой — в норме она успевает, от края зоны
     * до конца всего endZoneSec, — и, если не сказала, заканчиваем сами.
     *
     * Отсчёт идёт по времени, пока колбэк действительно звонит: на паузе
     * прошивка его не зовёт вовсе, и стенные часы засчитали бы паузу у самого
     * конца серии как залипание — пользователь вернулся бы к последним двум
     * секундам и тут же уехал на следующую серию. Разрыв в звонках сбрасывает
     * отсчёт, ровно как и явная пауза. */
    function endOfEpisodeStuck(now, gap) {
        if (restarting || durationSeconds() <= 0 || durationSeconds() - absoluteTime() > endZoneSec) {
            endZoneSince = 0;
            return false;
        }
        if (!endZoneSince || gap > 3000) {
            endZoneSince = now;
            return false;
        }
        if (now - endZoneSince < endGraceMs) return false;
        var state = '';
        try { state = webapis.avplay.getState(); } catch (e) {}
        if (state === 'PAUSED') {
            endZoneSince = now;
            return false;
        }
        return true;
    }

    function showHud(text) {
        if (mode !== 'player') return;
        el('playerHud').className = 'player-hud';
        if (text !== undefined && text !== null) el('hudStatus').innerHTML = text;
        if (hudTimer) clearTimeout(hudTimer);
        hudTimer = setTimeout(function () {
            if (mode === 'player') el('playerHud').className = 'player-hud hidden-hud';
        }, 4500);
    }

    function updateHudClock() {
        var total = durationSeconds();
        el('hudClock').innerHTML = fmt(absoluteTime()) + ' / ' + fmt(total);
    }

    /* Конец серии. Зовётся из onstreamcompleted и из oncurrentplaytime, когда
     * прошивка это событие проглотила, поэтому обязана быть идемпотентной:
     * playNextEpisode закрывает AVPlay и меняет meta, а stopPlayback уводит
     * из плеера, так что повторный заход отсекается проверкой meta и mode. */
    function streamCompleted() {
        if (!meta || mode !== 'player') return;
        clearPosition(meta.index);
        if (autoNext && meta.next !== null) {
            showHud('Episode finished. Starting next…');
            playNextEpisode();
        } else {
            stopPlayback(true, 'Episode finished');
        }
    }

    function playerListener() {
        return {
            onbufferingstart: function () { showHud('Buffering…'); },
            onbufferingprogress: function (percent) { showHud('Buffering ' + percent + '%'); },
            onbufferingcomplete: function () { showHud('Playing'); },
            oncurrentplaytime: function (currentTime) {
                var incoming = Number(currentTime) || 0;
                var now = new Date().getTime();
                var gap = lastPlaytimeCallAt ? now - lastPlaytimeCallAt : 0;
                lastPlaytimeCallAt = now;
                /* Доиграв плейлист с EXT-X-ENDLIST, AVPlay на этой прошивке
                 * не всегда зовёт onstreamcompleted: вместо этого он молча
                 * заходит на плейлист заново и тянет seg00000 по кругу.
                 * Снаружи это «первые пять секунд серии повторяются»,
                 * и выйти оттуда можно только пультом.
                 *
                 * Один из двух сторожей этой петли — откат времени назад
                 * (второй, на случай когда часы не откатываются вовсе, —
                 * endOfEpisodeStuck ниже). Перемотка сюда не попадает: она идёт
                 * через seekRelative с перезапуском сеанса (restarting), а конец
                 * серии сторожится отдельно — откат засчитывается только если
                 * до конца оставалось меньше полминуты. Иначе случайный ноль
                 * на буферизации уводил бы на следующую серию с середины. */
                if (!restarting && incoming + 60000 < lastPlayerTime && reachedEndOnce(30)) {
                    lastPlayerTime = incoming;
                    streamCompleted();
                    return;
                }
                /* Отметка простоя двигается вместе со временем, а не по самому
                 * факту вызова: прошивка умеет звать этот колбэк с одним и тем же
                 * значением, и обновление «на каждый звонок» делало watchForStall
                 * слепым к замершей картинке. Сторожить надо движение. */
                if (incoming !== lastPlayerTime) lastPlayTimeAt = now;
                lastPlayerTime = incoming;
                if (incoming > maxPlayerTime) maxPlayerTime = incoming;
                if (endOfEpisodeStuck(now, gap)) {
                    streamCompleted();
                    return;
                }
                updateHudClock();
                updateSubtitleOverlay();
                if (now - lastSavedAt > 5000) {
                    lastSavedAt = now;
                    savePosition(meta.index, absoluteTime());
                }
                if (meta && meta.next !== null && durationSeconds() > 0 && durationSeconds() - absoluteTime() < 90 && prebufferedFor !== meta.next) {
                    prebufferedFor = meta.next;
                    apiIgnore('/api/prebuffer/' + meta.next);
                    apiIgnore('/api/probe/' + meta.next);
                }
            },
            onstreamcompleted: function () { streamCompleted(); },
            onevent: function () {},
            onerror: function (eventType) {
                showHud('AVPlay error: ' + eventType);
            },
            onsubtitlechange: function () {}
        };
    }

    function closeAvplay() {
        try {
            var state = webapis.avplay.getState();
            if (state === 'PLAYING' || state === 'PAUSED' || state === 'READY') {
                try { webapis.avplay.stop(); } catch (e1) {}
            }
            if (webapis.avplay.getState() !== 'NONE') {
                try { webapis.avplay.close(); } catch (e2) {}
            }
        } catch (e) {
            try { webapis.avplay.close(); } catch (e3) {}
        }
    }

    function openPlaylist(url) {
        closeAvplay();
        lastPlayerTime = 0;
        maxPlayerTime = 0;
        lastPlayTimeAt = 0;
        endZoneSince = 0;
        lastPlaytimeCallAt = 0;
        el('av-player').style.display = 'block';
        el('playerScreen').className = 'player-screen';
        el('menuScreen').className = 'screen hidden';
        mode = 'player';
        el('hudTitle').innerHTML = meta ? meta.name : 'Playing';
        updateHudClock();
        showHud('Opening AVPlay…');

        try {
            webapis.avplay.open(url);
            webapis.avplay.setDisplayRect(0, 0, 1920, 1080);
            webapis.avplay.setListener(playerListener());
            try { webapis.avplay.setTimeoutForBuffering(30); } catch (e0) {}
            webapis.avplay.prepareAsync(function () {
                try {
                    webapis.avplay.play();
                    restarting = false;
                    showHud('Playing');
                } catch (e1) {
                    restarting = false;
                    showHud('Play failed: ' + e1.message);
                }
            }, function (err) {
                restarting = false;
                showHud('Prepare failed: ' + (err && err.message ? err.message : String(err)));
            });
        } catch (e) {
            restarting = false;
            showHud('AVPlay open failed: ' + e.message);
        }
    }

    function waitForHls(sessionId, playlist) {
        if (mode !== 'player' && !restarting) return;
        api('/api/hls-status/' + encodeURIComponent(sessionId) + '?_=' + new Date().getTime(), function (err, s) {
            if (err) {
                restarting = false;
                showHud('HLS status failed: ' + err.message);
                return;
            }
            if (s.state === 'error') {
                restarting = false;
                showHud('HLS generation failed');
                return;
            }
            /* Сеанс остановлен или заменён более новым /api/start: сегменты ещё
             * лежат на сервере, но открывать их нельзя. Выходим молча, не трогая
             * restarting и HUD — ими распоряжается опрос, заменивший этот. */
            if (s.state === 'stopped' || s.state === 'replaced') return;
            if ((s.segments || 0) >= 2 || s.state === 'finished') {
                openPlaylist(serverBase + playlist + '?_=' + new Date().getTime());
                return;
            }
            showHud('Preparing HLS: ' + (s.segments || 0) + '/2 startup segments…');
            setTimeout(function () { waitForHls(sessionId, playlist); }, 700);
        }, 12000);
    }

    function startPlayback(seconds) {
        if (!meta) {
            refreshMeta(function (err) { if (!err) startPlayback(seconds); });
            return;
        }
        seconds = Math.max(0, Math.min(Number(seconds) || 0, Math.max(0, durationSeconds() - 1)));
        startOffset = seconds;
        lastPlayerTime = 0;
        maxPlayerTime = 0;
        lastPlayTimeAt = 0;
        endZoneSince = 0;
        lastPlaytimeCallAt = 0;
        prebufferedFor = null;
        resetSubtitles();
        restarting = true;
        mode = 'player';
        el('menuScreen').className = 'screen hidden';
        el('playerScreen').className = 'player-screen';
        el('av-player').style.display = 'block';
        el('hudTitle').innerHTML = meta.name;
        updateHudClock();
        showHud('Starting HLS…');

        var path = '/api/start/' + meta.index + '?audio=' + encodeURIComponent(selectedAudioCode()) +
            '&sub=' + encodeURIComponent(selectedSubCode()) + '&start=' + encodeURIComponent(seconds.toFixed(3)) +
            '&_=' + new Date().getTime();
        api(path, function (err, data) {
            if (err) {
                restarting = false;
                showHud('Cannot start HLS: ' + err.message);
                return;
            }
            currentSessionId = data.id;
            startSubtitleOverlay(data.subtitlePlaylist || '');
            waitForHls(data.id, data.playlist);
        }, 30000);
    }

    function playFromMenu() {
        var ep = currentEpisode();
        if (!ep) return;
        if (!meta || Number(meta.index) !== Number(ep.index)) {
            refreshMeta(function (err) { if (!err) playFromMenu(); });
            return;
        }
        storeSet('rtv.audio', selectedAudioCode());
        storeSet('rtv.sub', selectedSubCode());
        var saved = savedPosition(ep.index);
        if (saved > 10 && (!meta.duration || saved < meta.duration - 30)) startPlayback(saved);
        else startPlayback(0);
    }

    function seekRelative(delta) {
        if (!meta || restarting) return;
        var target = absoluteTime() + delta;
        target = Math.max(0, Math.min(target, Math.max(0, durationSeconds() - 1)));
        savePosition(meta.index, target);
        showHud((delta < 0 ? 'Rewind to ' : 'Forward to ') + fmt(target));
        closeAvplay();
        startPlayback(target);
    }

    function togglePause() {
        if (restarting) return;
        try {
            var state = webapis.avplay.getState();
            if (state === 'PLAYING') {
                webapis.avplay.pause();
                showHud('Paused');
            } else if (state === 'PAUSED') {
                webapis.avplay.play();
                showHud('Playing');
            }
        } catch (e) { showHud('Play/Pause error: ' + e.message); }
    }

    function stopPlayback(toMenu, message) {
        if (meta) savePosition(meta.index, absoluteTime());
        restarting = false;
        currentSessionId = '';
        lostSessionPolls = 0;
        closeAvplay();
        apiIgnore('/api/stop');
        resetSubtitles();
        el('av-player').style.display = 'none';
        if (toMenu) {
            showMenu();
            refreshMenu();
            if (message) setStatus(message, 'ok');
        }
    }

    function playNextEpisode() {
        if (!meta || meta.next === null) {
            stopPlayback(true, 'No next episode.');
            return;
        }
        closeAvplay();
        resetSubtitles();
        apiIgnore('/api/stop');
        if (!selectEpisodeIndex(meta.next)) {
            stopPlayback(true, 'Next episode not found.');
            return;
        }
        meta = null;
        api('/api/probe/' + currentEpisode().index, function (err, data) {
            if (err) {
                stopPlayback(true, 'Next episode probe failed: ' + err.message);
                return;
            }
            meta = data;
            choosePreferredTracks();
            clearPosition(meta.index);
            startPlayback(0);
        }, 30000);
    }

    function refreshServerStatus() {
        if (!serverBase) return;
        api('/api/status?_=' + new Date().getTime(), function (err, s) {
            if (err) {
                el('serverStatus').innerHTML = 'Server unavailable';
                return;
            }
            if (!s.ready) {
                el('serverStatus').innerHTML = 'Torrent metadata loading…';
                return;
            }
            el('serverStatus').innerHTML = s.peers + ' peers · ' + fmtSpeed(s.downloadSpeed);
            if (mode === 'player' && s.playback && s.playback.state && s.playback.state !== 'ready' && s.playback.state !== 'finished') {
                el('hudStatus').innerHTML = 'Server: ' + s.playback.state + ' · ' + (s.playback.segments || 0) + ' segments';
            }
            if (mode === 'player') {
                watchForLostSession(s);
                watchForStall();
            } else if (mode === 'menu') maybeCheckLibrary();
        }, 5000);
    }

    /* Наш сеанс перестал быть активным: так выглядит и /api/stop с телефона,
     * и переключение активного торрента (оно гасит ffmpeg), и чужой /api/start.
     * Два опроса подряд, а не один, потому что между закрытием AVPlay
     * и новым /api/start сеанса законно нет — на этом и держится перемотка. */
    function watchForLostSession(s) {
        if (!currentSessionId || restarting) {
            lostSessionPolls = 0;
            return;
        }
        if (s.playback && s.playback.id === currentSessionId) {
            lostSessionPolls = 0;
            return;
        }
        lostSessionPolls++;
        if (lostSessionPolls < 2) return;
        lostSessionPolls = 0;
        confirmSessionLost();
    }

    /* Пропажа сеанса из /api/status сама по себе НЕ означает, что играть нечего:
     * после выкатки новый процесс подбирает каталоги прежнего, но активным
     * такой сеанс не делает намеренно (иначе сервер навсегда считался бы
     * занятым и заблокировал бы следующие выкатки). Наш сеанс при этом жив
     * и доигрывается — прерывать его нельзя. Отличает случаи только снимок
     * самого сеанса, поэтому спрашиваем именно про него.
     *
     * Ошибка ответа трактуется как «пока не знаем»: во время выкатки сервер
     * недоступен несколько секунд, и выходить в меню из-за этого незачем —
     * погашенный сеанс никуда не денется и ответит stopped на следующем опросе.
     *
     * Кроме 404. Две минуты живёт каталог сеанса, ЗАМЕНЁННОГО новым стартом,
     * а /api/stop сносит его сразу же — и тогда hls-status отвечает 404
     * навсегда. Пока 404 считался просто ошибкой, телевизор после остановки
     * с телефона оставался в плеере насовсем: сегментов нет, выхода нет,
     * помогает только Back на пульте. Сервер ответил — значит он жив
     * и про наш сеанс не знает, а это ровно то же, что stopped. */
    function confirmSessionLost() {
        var id = currentSessionId;
        api('/api/hls-status/' + encodeURIComponent(id) + '?_=' + new Date().getTime(), function (err, s) {
            if (mode !== 'player' || currentSessionId !== id) return;
            if (err && err.status === 404) {
                stopPlayback(true, 'Playback was stopped on the server.');
                checkLibrary();
                return;
            }
            if (err || !s) return;
            if (s.state !== 'stopped' && s.state !== 'replaced' && s.state !== 'error') return;
            stopPlayback(true, 'Playback was stopped on the server.');
            checkLibrary();
        }, 8000);
    }

    /* В меню — раз в 10 секунд; во время просмотра список серий не нужен,
     * а лишний XHR на этом железе стоит дороже, чем кажется. */
    /* Плеер молча встал. AVPlay на этой прошивке умеет остановиться, не сказав
     * ничего: не пришёл сегмент (сервер перезапускали посреди серии, сеть
     * моргнула) — и он просто перестаёт двигаться. Ни onerror, ни
     * onbufferingstart, ни выхода по setTimeoutForBuffering. Снаружи это
     * «зависло», и до этой ветки лечилось только пультом. Сеанс при этом
     * жив и здоров, поэтому watchForLostSession тут бессилен: сторожить
     * надо не сервер, а картинку.
     *
     * Ноль в отметке означает, что серия ещё не начинала играть — там сторожит
     * waitForHls. Заодно это ограничивает нас одной попыткой на сеанс:
     * перезапуск обнуляет отметку, и второй раз она взведётся, только если
     * картинка действительно пошла. */
    function watchForStall() {
        if (mode !== 'player' || restarting || !lastPlayTimeAt) return;
        if (new Date().getTime() - lastPlayTimeAt < stallLimitMs) return;

        var state = '';
        try { state = webapis.avplay.getState(); } catch (e) {}
        // На паузе время стоять и должно.
        if (state === 'PAUSED') return;

        var resumeAt = absoluteTime();
        lastPlayTimeAt = 0;
        showHud('Playback stalled, restarting…');
        startPlayback(resumeAt);
    }

    function maybeCheckLibrary() {
        var now = new Date().getTime();
        if (now - lastLibraryCheck < 10000) return;
        lastLibraryCheck = now;
        checkLibrary();
    }

    function startStatusPolling() {
        if (statusTimer) clearInterval(statusTimer);
        refreshServerStatus();
        statusTimer = setInterval(refreshServerStatus, 2000);
    }

    function menuLeftRight(delta) {
        var row = menuRows[menuRow];
        if (row === 'season') changeSeason(delta);
        else if (row === 'episode') changeEpisode(delta);
        else if (row === 'audio') changeAudio(delta);
        else if (row === 'sub') changeSub(delta);
        else if (row === 'subsize') changeSubtitleSize(delta);
        else if (row === 'autonext') toggleAutoNext();
    }

    function menuEnter() {
        var row = menuRows[menuRow];
        if (row === 'play') playFromMenu();
        else if (row === 'server') showSetup('');
        else if (row === 'autonext') toggleAutoNext();
        else if (row === 'season') changeSeason(1);
        else if (row === 'episode') changeEpisode(1);
        else if (row === 'audio') changeAudio(1);
        else if (row === 'sub') changeSub(1);
        else if (row === 'subsize') changeSubtitleSize(1);
    }

    function exitApp() {
        try { stopPlayback(false); } catch (e) {}
        try { tizen.application.getCurrentApplication().exit(); } catch (e2) {}
    }

    function registerRemoteKeys() {
        var names = ['MediaPlayPause', 'MediaPlay', 'MediaPause', 'MediaRewind', 'MediaFastForward'];
        var i;
        if (!window.tizen || !tizen.tvinputdevice) return;
        for (i = 0; i < names.length; i++) {
            try { tizen.tvinputdevice.registerKey(names[i]); } catch (e) {}
        }
    }

    function keyCode(name, fallback) {
        try {
            var k = tizen.tvinputdevice.getKey(name);
            return k ? k.code : fallback;
        } catch (e) { return fallback; }
    }

    function handleKey(event) {
        var code = event.keyCode;
        var LEFT = 37, UP = 38, RIGHT = 39, DOWN = 40, ENTER = 13, BACK = 10009;
        var PLAY_PAUSE = keyCode('MediaPlayPause', 10252);
        var PLAY = keyCode('MediaPlay', 415);
        var PAUSE = keyCode('MediaPause', 19);
        var REW = keyCode('MediaRewind', 412);
        var FF = keyCode('MediaFastForward', 417);

        if (mode === 'setup') {
            if (code === UP || code === DOWN) {
                setupRow = setupRow === 0 ? 1 : 0;
                refreshSetupFocus();
            } else if (code === ENTER) {
                if (setupRow === 0) {
                    try { el('serverInput').focus(); } catch (e0) {}
                } else connectFromSetup();
            } else if (code === BACK) {
                exitApp();
            }
            return;
        }

        if (mode === 'menu') {
            if (code === UP) {
                menuRow = (menuRow - 1 + menuRows.length) % menuRows.length;
                refreshMenuFocus();
            } else if (code === DOWN) {
                menuRow = (menuRow + 1) % menuRows.length;
                refreshMenuFocus();
            } else if (code === LEFT) menuLeftRight(-1);
            else if (code === RIGHT) menuLeftRight(1);
            else if (code === ENTER) menuEnter();
            else if (code === BACK) exitApp();
            return;
        }

        if (mode === 'player') {
            showHud();
            if (code === LEFT || code === REW) seekRelative(-30);
            else if (code === RIGHT || code === FF) seekRelative(30);
            else if (code === ENTER || code === PLAY_PAUSE || code === PLAY || code === PAUSE) togglePause();
            else if (code === BACK) stopPlayback(true);
        }
    }

    function boot() {
        registerRemoteKeys();
        document.addEventListener('keydown', handleKey, false);
        serverBase = cleanBase(storeGet('rtv.server', DEFAULT_SERVER));
        autoNext = storeGet('rtv.autonext', '1') !== '0';
        loadSubtitleSize();

        /* Put a screen up before touching the network — a slow or refused request
         * must never leave the TV on a black screen. */
        showSetup('Connecting to ' + serverBase + '…');

        /* Verify AVPlay before attempting network access. */
        if (!window.webapis || !webapis.avplay) {
            showSetup('AVPlay is not available on this TV.');
            return;
        }

        api('/api/files', function (err, data) {
            if (err) showSetup('Cannot reach ' + serverBase + ' — ' + err.message);
            else loadLibrary(data);
        }, 12000);
    }

    var booted = false;
    function bootOnce() {
        if (booted) return;
        booted = true;
        try { boot(); }
        catch (e) { fatal('Startup failed: ' + (e && e.message ? e.message : String(e))); }
    }

    /* Boot on whichever of these the WRT actually delivers: window.onload does not
     * fire on every Tizen build once the avplayer object is in the document. */
    if (document.addEventListener) document.addEventListener('DOMContentLoaded', bootOnce, false);
    window.onload = bootOnce;
    setTimeout(bootOnce, 2000);
})();
