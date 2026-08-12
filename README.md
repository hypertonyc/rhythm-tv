# Rhythm TV client v8-clean

Cleaned version after subtitle debugging.

Kept:
- original v2 WebVTT playlist/segment subtitle transport (known working)
- Tizen 2.3 AVPlay plain A/V HLS playlist workaround
- subtitle size setting
- Canvas subtitle renderer with translucent background (fixes old Tizen compositor issue)
- CORS and /api/stop on the server

Removed:
- AVPlay-driven extra subtitle polling from v4
- server JSON subtitle parsing/API from v5
- full subtitle snapshot polling from v6
- related cursors/diagnostic state

Replace client index.html, css/style.css, js/app.js.
If your server is currently v5/v7, replace it with server-tizen.mjs and rebuild Docker.
