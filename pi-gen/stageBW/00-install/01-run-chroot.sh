#!/bin/bash -e
systemctl enable bordwaechter.service
systemctl enable bordwaechter-firstboot.service
# VNC (RealVNC im Desktop-Image vorhanden); tolerant, falls Lite-Variante
raspi-config nonint do_vnc 0 || true
