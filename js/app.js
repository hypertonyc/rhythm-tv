/* Rhythm TV client for Samsung Tizen 2.3 (2015 TV).
 * Keep this file ES5-compatible: no let/const, arrows, fetch, Promise, async/await.
 */
(function () {
    'use strict';

    /* Edit this to your Mac's LAN address if you want zero setup on the TV. */
    var DEFAULT_SERVER = 'https://media.example.com/REDACTED';

    var serverBase = '';
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
    var lastSavedAt = 0;
    var hlsSessionId = null;
    var currentPlaylist = '';
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

    function storeGet(key, fallback) {
        try {
            var v = localStorage.getItem(key);
            return v === null ? fallback : v;
        } catch (e) { return fallback; }
    }

    function storeSet(key, value) {
        try { localStorage.setItem(key, String(value)); } catch (e) {}
    }

    function positions() {
        try { return JSON.parse(storeGet('rtv.positions', '{}')) || {}; }
        catch (e) { return {}; }
    }

    function savedPosition(index) {
        var p = positions();
        return Number(p[String(index)] || 0) || 0;
    }

    function savePosition(index, seconds) {
        if (index === null || index === undefined || !isFinite(seconds)) return;
        var p = positions();
        p[String(index)] = Math.max(0, Math.floor(seconds));
        try { storeSet('rtv.positions', JSON.stringify(p)); } catch (e) {}
    }

    function clearPosition(index) {
        var p = positions();
        delete p[String(index)];
        try { storeSet('rtv.positions', JSON.stringify(p)); } catch (e) {}
    }

    function cleanBase(value) {
        value = String(value || '').replace(/^\s+|\s+$/g, '');
        while (value.length > 1 && value.charAt(value.length - 1) === '/') value = value.slice(0, -1);
        if (value && !/^https?:\/\//i.test(value)) value = 'http://' + value;
        return value;
    }

    function api(path, callback, timeout) {
        var xhr = new XMLHttpRequest();
        var url = serverBase + path;
        xhr.open('GET', url, true);
        xhr.timeout = timeout || 12000;
        xhr.onreadystatechange = function () {
            if (xhr.readyState !== 4) return;
            if (xhr.status >= 200 && xhr.status < 300) {
                try { callback(null, JSON.parse(xhr.responseText)); }
                catch (e) { callback(new Error('Bad JSON from server')); }
            } else {
                callback(new Error('HTTP ' + xhr.status + ' for ' + path));
            }
        };
        xhr.onerror = function () { callback(new Error('Network error: ' + url)); };
        xhr.ontimeout = function () { callback(new Error('Timeout: ' + url)); };
        try { xhr.send(); }
        catch (e) { callback(e); }
    }

    function apiIgnore(path) {
        api(path, function () {}, 5000);
    }

    function xhrText(url, callback, timeout) {
        var xhr = new XMLHttpRequest();
        xhr.open('GET', url, true);
        xhr.timeout = timeout || 8000;
        xhr.onreadystatechange = function () {
            if (xhr.readyState !== 4) return;
            if (xhr.status >= 200 && xhr.status < 300) callback(null, xhr.responseText || '');
            else callback(new Error('HTTP ' + xhr.status + ' for ' + url));
        };
        xhr.onerror = function () { callback(new Error('Network error: ' + url)); };
        xhr.ontimeout = function () { callback(new Error('Timeout: ' + url)); };
        try { xhr.send(); } catch (e) { callback(e); }
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
        var t = absoluteTime();
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
                title: m[3],
                season: Number(m[1]),
                episode: Number(m[2]),
                label: 'S' + (m[1].length < 2 ? '0' + m[1] : m[1]) + 'E' + (m[2].length < 2 ? '0' + m[2] : m[2]) + ' — ' + m[3]
            };
        }
        return {
            index: file.index,
            name: file.name,
            title: file.name.replace(/\.[^.]+$/, ''),
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
            storeSet('rtv.lastEpisode', index);
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
        buildSeasons(data.files || []);
        el('torrentTitle').innerHTML = data.torrent || 'Rhythm TV';
        autoNext = storeGet('rtv.autonext', '1') !== '0';
        var last = Number(storeGet('rtv.lastEpisode', seasons.length && seasons[0].episodes.length ? seasons[0].episodes[0].index : 0));
        if (!selectEpisodeIndex(last) && seasons.length && seasons[0].episodes.length) {
            seasonPos = 0;
            episodePos = 0;
        }
        showMenu();
        refreshMeta();
        startStatusPolling();
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
            if (err) {
                setStatus('Probe failed: ' + err.message, 'error');
                if (callback) callback(err);
                return;
            }
            meta = data;
            choosePreferredTracks();
            storeSet('rtv.lastEpisode', ep.index);
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

    function playerListener() {
        return {
            onbufferingstart: function () { showHud('Buffering…'); },
            onbufferingprogress: function (percent) { showHud('Buffering ' + percent + '%'); },
            onbufferingcomplete: function () { showHud('Playing'); },
            oncurrentplaytime: function (currentTime) {
                lastPlayerTime = Number(currentTime) || 0;
                updateHudClock();
                updateSubtitleOverlay();
                var now = new Date().getTime();
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
            onstreamcompleted: function () {
                if (!meta) return;
                clearPosition(meta.index);
                if (autoNext && meta.next !== null) {
                    showHud('Episode finished. Starting next…');
                    playNextEpisode();
                } else {
                    stopPlayback(true, 'Episode finished');
                }
            },
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

    function trySelectTextTrack() {
        if (selectedSubCode() === 'off') return;
        try {
            var tracks = webapis.avplay.getTotalTrackInfo();
            var i;
            for (i = 0; i < tracks.length; i++) {
                if (tracks[i].type === 'TEXT') {
                    try { webapis.avplay.setSelectTrack('TEXT', tracks[i].index); } catch (e) {}
                    return;
                }
            }
        } catch (e2) {}
    }

    function openPlaylist(url) {
        closeAvplay();
        currentPlaylist = url;
        lastPlayerTime = 0;
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

    function waitForHls(sessionId, playlist, attempt) {
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
            if ((s.segments || 0) >= 2 || s.state === 'finished') {
                openPlaylist(serverBase + playlist + '?_=' + new Date().getTime());
                return;
            }
            showHud('Preparing HLS: ' + (s.segments || 0) + '/2 startup segments…');
            setTimeout(function () { waitForHls(sessionId, playlist, attempt + 1); }, 700);
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
            hlsSessionId = data.id;
            startSubtitleOverlay(data.subtitlePlaylist || '');
            waitForHls(data.id, data.playlist, 0);
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
        closeAvplay();
        apiIgnore('/api/stop');
        hlsSessionId = null;
        currentPlaylist = '';
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
        }, 5000);
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

        /* Verify AVPlay before attempting network access. */
        if (!window.webapis || !webapis.avplay) {
            showSetup('AVPlay is not available on this TV.');
            return;
        }

        api('/api/files', function (err, data) {
            if (err) showSetup('Cannot reach ' + serverBase + '. Check the Mac IP and server.');
            else loadLibrary(data);
        }, 12000);
    }

    window.onload = boot;
})();
