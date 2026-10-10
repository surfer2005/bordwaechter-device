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

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
echo
echo "Fertig. Oberfläche:  http://${IP:-<pi>}:8090"
echo "Dort KAI-Adresse + Edge-Token eintragen, dann läuft es."
