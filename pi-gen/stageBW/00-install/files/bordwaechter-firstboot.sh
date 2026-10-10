#!/bin/bash
# Übernimmt eine pro Schiff auf die Boot-Partition gelegte Konfiguration (Token/Schiff/…).
for f in /boot/firmware/bordwaechter.json /boot/bordwaechter.json; do
  if [ -f "$f" ]; then
    install -D -m 0600 "$f" /etc/bordwaechter/config.json
    break
  fi
done
exit 0
