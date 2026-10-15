#!/usr/bin/env bash
# Startet Chromium im Vollbild-Kiosk auf die Bordwächter-Anzeige des LOKALEN Diensts — für einen
# Monitor, der direkt am Bordwächter-Pi hängt. Läuft in der Desktop-Sitzung des Auto-Login-Nutzers
# (per Openbox-Autostart). Die Anzeige braucht hier KEINEN Token: Anfragen von localhost sind erlaubt
# (der Token schützt nur den Zugriff aus dem Netz). Startansicht (Pilot/Seekarte) kommt aus der Konfig.
set -u

BASE="http://127.0.0.1:8090"
BIN=chromium-browser
command -v "$BIN" >/dev/null 2>&1 || BIN=chromium

PROFILE="${HOME:-/home/pi}/.config/bordwaechter-kiosk"
mkdir -p "$PROFILE"

# Bildschirmschoner/Energiesparen aus; Hintergrund ins Bordwächter-Dunkel, damit nichts weiß aufblitzt.
command -v xset >/dev/null 2>&1 && { xset s off; xset -dpms; xset s noblank; } 2>/dev/null
command -v xsetroot >/dev/null 2>&1 && xsetroot -solid "#081518" 2>/dev/null || true

# Auf den lokalen Dienst warten, dann die konfigurierte Startansicht holen (/anzeige oder /seekarte).
STARTPATH="/anzeige"
if command -v curl >/dev/null 2>&1; then
  for i in $(seq 1 60); do
    curl -fsS --max-time 2 -o /dev/null "$BASE/api/kiosk-target" 2>/dev/null && break
    sleep 1
  done
  p=$(curl -fsS --max-time 3 "$BASE/api/kiosk-target" 2>/dev/null | sed -n 's/.*"path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)
  [ -n "$p" ] && STARTPATH="$p"
fi

exec "$BIN" \
  --kiosk --app="$BASE$STARTPATH" \
  --user-data-dir="$PROFILE" \
  --noerrdialogs --disable-infobars --disable-session-crashed-bubble \
  --disable-features=TranslateUI --no-first-run --fast --fast-start \
  --overscroll-history-navigation=0 --disable-pinch
