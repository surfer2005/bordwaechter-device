#!/usr/bin/env bash
# Bordwächter-Gerät installieren: Binary + systemd-Dienst + Config-Verzeichnis.
# Aufruf auf dem Raspberry:  sudo bash install.sh
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
PREFIX=/usr/local/bin
CFG=/etc/bordwaechter

mkdir -p "$CFG"

# Binary: lokal gebautes ./bordwaechter bevorzugt, sonst aus Quellcode bauen (Go nötig).
if [ -f "$HERE/bordwaechter" ]; then
  install -m 0755 "$HERE/bordwaechter" "$PREFIX/bordwaechter"
elif command -v go >/dev/null 2>&1; then
  echo "Baue aus Quellcode …"
  ( cd "$HERE" && CGO_ENABLED=0 go build -o "$PREFIX/bordwaechter" . )
else
  echo "FEHLER: weder ./bordwaechter (Release) noch Go-Toolchain gefunden." >&2
  exit 1
fi

# Beispiel-Config nur anlegen, wenn noch keine da ist (Eintragung erfolgt danach im Web-UI).
if [ ! -f "$CFG/config.json" ] && [ -f "$HERE/config.example.json" ]; then
  cp "$HERE/config.example.json" "$CFG/config.json"
fi

install -m 0644 "$HERE/systemd/bordwaechter.service" /etc/systemd/system/bordwaechter.service
systemctl daemon-reload
systemctl enable --now bordwaechter

# --- Optionaler Geräte-Kiosk: ein Monitor DIREKT am Bordwächter-Pi zeigt die Anzeige ---
# Aufruf:  sudo bash install.sh --kiosk   (oder  BORDWAECHTER_KIOSK=1 )
# Ohne das bleibt das Gerät headless; die Anzeige ist dann von einem anderen Gerät aus erreichbar.
KIOSK=0
for a in "$@"; do [ "$a" = "--kiosk" ] && KIOSK=1; done
[ "${BORDWAECHTER_KIOSK:-0}" = "1" ] && KIOSK=1
if [ "$KIOSK" = "1" ]; then
  echo "Richte Geräte-Kiosk ein (Monitor am Pi) …"
  KUSER="${KIOSK_USER:-${SUDO_USER:-pi}}"
  apt-get update -y || true
  apt-get install -y --no-install-recommends xserver-xorg xinit openbox lightdm chromium-browser unclutter \
    || apt-get install -y --no-install-recommends xserver-xorg xinit openbox lightdm chromium unclutter
  install -m 0755 "$HERE/kiosk-browser.sh" "$PREFIX/kiosk-browser.sh"
  # Auto-Login (lightdm) in eine Openbox-Sitzung, die den Kiosk-Browser startet.
  # ⚠️ Debian/LightDM lässt den Auto-Login nur zu, wenn der Benutzer in der Gruppe 'autologin' ist.
  mkdir -p /etc/lightdm/lightdm.conf.d
  cat >/etc/lightdm/lightdm.conf.d/50-bordwaechter-kiosk.conf <<EOF
[Seat:*]
autologin-user=$KUSER
autologin-user-timeout=0
autologin-session=openbox
user-session=openbox
EOF
  groupadd -f autologin
  usermod -aG autologin "$KUSER"
  mkdir -p /etc/xdg/openbox
  echo '/usr/local/bin/kiosk-browser.sh &' >/etc/xdg/openbox/autostart
  systemctl set-default graphical.target
  systemctl enable lightdm 2>/dev/null || true
  echo "Kiosk aktiv nach dem Neustart. Startansicht über 'display_view' (pilot|seekarte) in der Konfig."
fi

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
echo
echo "Fertig. Oberfläche:  http://${IP:-<pi>}:8090"
echo "Dort KAI-Adresse + Edge-Token eintragen, dann läuft es."
