#!/usr/bin/env bash
# Startet Chromium im Vollbild-Kiosk auf die lokale Bordwächter-Anzeige — für einen Monitor direkt am
# Pi. Läuft in der Desktop-Sitzung des Auto-Login-Nutzers, sowohl unter Wayland (labwc/wayfire) als
# auch X. Startansicht (Pilot/Seekarte) und Bildschirmdrehung kommen aus der Konfig (über /api/kiosk-target).
# Die Anzeige braucht hier KEINEN Token: Anfragen von localhost sind erlaubt.
set -u
BASE="http://127.0.0.1:8090"
LOG="${HOME:-/tmp}/.bordwaechter-kiosk.log"
exec >>"$LOG" 2>&1
echo "=== kiosk-browser $(date) session=${XDG_SESSION_TYPE:-?} wayland=${WAYLAND_DISPLAY:-} display=${DISPLAY:-} ==="

BIN=chromium-browser
command -v "$BIN" >/dev/null 2>&1 || BIN=chromium
command -v "$BIN" >/dev/null 2>&1 || { echo "FEHLER: kein chromium gefunden"; exit 1; }

# Bildschirmschoner/Energiesparen aus (X; unter Wayland ignoriert)
command -v xset >/dev/null 2>&1 && { xset s off; xset -dpms; xset s noblank; } 2>/dev/null

# Auf den Dienst warten und Startansicht + Drehung holen.
STARTPATH="/anzeige"; ROT="normal"
for i in $(seq 1 60); do
  curl -fsS --max-time 2 -o /tmp/bw-kt.json "$BASE/api/kiosk-target" 2>/dev/null && break
  sleep 1
done
if [ -s /tmp/bw-kt.json ]; then
  p=$(sed -n 's/.*"path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' /tmp/bw-kt.json | head -n1); [ -n "$p" ] && STARTPATH="$p"
  r=$(sed -n 's/.*"rotate"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' /tmp/bw-kt.json | head -n1); [ -n "$r" ] && ROT="$r"
fi
echo "startpath=$STARTPATH rotate=$ROT"

# Drehung anwenden: Wayland (wlr-randr) bevorzugt, sonst X (xrandr).
if [ "$ROT" != "normal" ] && [ -n "$ROT" ]; then
  if command -v wlr-randr >/dev/null 2>&1; then
    OUT=$(wlr-randr 2>/dev/null | awk 'NR==1{print $1; exit}')
    case "$ROT" in left) T=90;; right) T=270;; inverted) T=180;; *) T=normal;; esac
    [ -n "$OUT" ] && { echo "wlr-randr $OUT transform $T"; wlr-randr --output "$OUT" --transform "$T" 2>&1; }
  elif command -v xrandr >/dev/null 2>&1; then
    OUT=$(xrandr 2>/dev/null | awk '/ connected/{print $1; exit}')
    [ -n "$OUT" ] && { echo "xrandr $OUT rotate $ROT"; xrandr --output "$OUT" --rotate "$ROT" 2>&1; }
  else
    echo "HINWEIS: weder wlr-randr noch xrandr vorhanden → keine Drehung (apt install wlr-randr)"
  fi
fi

PROFILE="${HOME:-/home/pi}/.config/bordwaechter-kiosk"
mkdir -p "$PROFILE"
echo "starte $BIN $BASE$STARTPATH"
exec "$BIN" \
  --kiosk --app="$BASE$STARTPATH" \
  --user-data-dir="$PROFILE" \
  --password-store=basic \
  --noerrdialogs --disable-infobars --disable-session-crashed-bubble \
  --disable-features=TranslateUI --no-first-run --no-default-browser-check \
  --fast --fast-start --overscroll-history-navigation=0 --disable-pinch
