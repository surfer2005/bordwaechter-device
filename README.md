# Bordwächter — On-Board-Dienst (Raspberry Pi)

Autonomes Sicherheits-/Assistenzgerät an Bord. Liest die GPS-Position (Teltonika per Modbus
oder Testquelle), holt aus **KAI** die Fahrrinne + das heutige Bordbuch + Schwellen, rechnet
**lokal in Echtzeit** die Kursabweichung + Pünktlichkeit, schlägt bei Bedarf **Alarm**
(Relais/Hupe/Lampe) mit **SIFA-Quittierung**, meldet Ereignisse/Position an KAI und zeigt alles
über eine kleine **lokale Web-Oberfläche**. Kein Framework, **nur Go-Standardbibliothek** →
ein statisches Binary, wartungsarm.

> ⚠️ Assistenz-/Frühwarnsystem — **kein** Autopilot, **kein** Ersatz für Ausguck, Seekarte und
> amtliche Navigationsmittel. Vor verbindlicher Nutzung Abnahme durch BSB/Behörde.

## Bauen

```bash
cd edge/bordwaechter-device
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bordwaechter .   # für Raspberry Pi (64-bit)
```
(Dieses Verzeichnis ist ein eigenständiges Go-Modul und kann 1:1 in ein eigenes GitHub-Repo
`bordwaechter-device` verschoben werden — Releases dann als fertiges Binary.)

## Installieren (auf dem Pi)

```bash
sudo bash install.sh      # Binary nach /usr/local/bin, systemd-Dienst, Config /etc/bordwaechter
```
Danach **http://&lt;pi&gt;:8090** öffnen und eintragen: **KAI-Adresse + Edge-Token** (aus
*KAI → Einstellungen → Schnittstellen → Bordwächter-Geräte*), Schiff, GPS-Quelle, SIFA, GPIO, Kamera.
→ Speichern, läuft.

## KAI-Anbindung (bereits serverseitig vorhanden)

- `GET /api/edge/bundle` — Fahrrinne + heutiges Bordbuch + Schwellen (Token `X-Edge-Token`).
- `POST /api/edge/position` — GPS direkt an KAI (falls HA das nicht liefert).
- `POST /api/edge/event` — Alarm/SIFA/Kamera + optional Snapshot.

## Lokale API (für HACS-Integration / Test)

- `GET /api/status` — kompletter Zustand (Phase, Halt, Soll, Kursabweichung, Alarm, SIFA, Position).
- `GET/POST /api/config` — Einstellungen lesen/speichern.
- `POST /api/ack` — Alarm quittieren / Anwesenheit bestätigen.
- `POST /api/test?out=horn|lamp` — Ausgang kurz testen.
- `POST /api/camera/event?secret=…` — Kamera-Webhook (Mobotix MxMessage).
- `POST /api/position?lat=&lng=&speed_kmh=` — manuelle Testposition.

Alles (außer dem Kamera-Webhook) ist mit dem **UI-Passwort** per Basic-Auth geschützt; die
HACS-Integration nutzt dasselbe Passwort.

## SIFA / Alarm-Logik

- **Ereignis-SIFA:** Auslöser = Kursabweichung > Grenze **und/oder** Kamera-Hinweis (Kombi
  einstellbar). Alarm (Hupe/Lampe) → **Quittieren** (Taster) → solange die Ursache besteht,
  **alle _N_ s erneut** Alarm, bis sauber geklärt.
- **Totmann-SIFA (optional):** Anwesenheit muss alle _N_ s bestätigt werden (Vorwarnung → Hupe).
- Ausgänge aus dem Pi (potenzialfreie Relais empfohlen): Hupe, Lampe. Eingang: Quittier-Taster.

## Verdrahtung / galvanische Trennung (Schiff)

Grundsatz: **alles galvanisch trennen, was den Pi verlässt** — Bordnetz (12/24 V), lange
Leitungen, EMV und Spannungsspitzen sind an Bord die Regel. Pins per Config:
`horn_pin` (Vorgabe 17), `lamp_pin` (27), `ack_pin` (22), `sifa_key_pin`, `active_low`.

- **Ausgänge (Hupe, Lampe):** Das Relais trennt bereits die **Last** (potenzialfrei). Zusätzlich
  die Seite **GPIO → Relaisspule** trennen: **opto-isolierte Relaismodule** (Eingang mit PC817)
  verwenden, die **JD-VCC-Brücke entfernen** und die Spulen aus einer **eigenen, isolierten 5 V**
  speisen — erst dann ist der Pi beidseitig getrennt. Ein zusätzlicher diskreter Optokoppler davor
  ist dann überflüssig. `active_low` passend zur Modul-Logik setzen; Freilaufdiode/TVS an der Last.
- **Eingänge (Quittier-Taster, SIFA-Taste): optokoppeln.** Taster schaltet die LED-Seite, der
  Fototransistor zieht den GPIO. Teile: **PC817** (einzeln) bzw. **TLP281** (4-fach SMD); für
  genormte 24-V-Pegel **ISO1212** (isolierter Digitaleingang, IEC 61131). Dazu Entprellung (Software
  + optional RC) und **TVS** gegen Bordnetz-Surges.
- **Empfohlen — Industrie-Pi mit Klemmen-I/O statt Eigenbau-Optokoppler:** fertige Hutschienen-
  Einheiten mit **isolierten Digitaleingängen UND Relais/isolierten Ausgängen auf Schraubklemmen**,
  Weitbereichseingang 9–36 V (Bordnetz direkt), verpolsicher, Watchdog/RTC: **Sfera Labs Iono Pi
  (Max)** (fast maßgeschneidert: Relais + opto-isolierte DI), **Kunbus RevPi Core/Connect + RevPi
  DIO**, alternativ UniPi / Techbase ModBerry / Strato Pi. Die Trennung ist dort ab Werk geprüft;
  bei Iono/RevPi deren Pin-/Treiber-Nummern nutzen (ggf. GPIO-Backend anpassen). Modelle/Preise vor
  Beschaffung am aktuellen Datenblatt prüfen.

## Einstellungen (alle Optionen)

Gerät-seitig editierbar in der **lokalen Oberfläche** (`http://<pi>:8090`, Zahnrad) bzw. direkt in
`config.json`; die schiffs-/schwellenwertbezogenen Werte kommen aus dem **KAI-Bundle**
(*KAI → Einstellungen → Schnittstellen → Bordwächter-Geräte*). Defaults in Klammern.

**Allgemein / KAI-Anbindung**
- `kai_base_url` — KAI-Adresse (z. B. `https://kai-bsb.de`); das Gerät ruft `<base>/api/edge/…`.
- `edge_token` — je Gerät, aus der KAI-Geräteverwaltung.
- `ship_id` (0 = aus dem Bundle übernehmen).
- `send_position` — GPS direkt an KAI melden (aus lassen, wenn Home Assistant die Position liefert).
- `listen` (`:8090`) — lokale UI/API.
- `ui_password` — Schutz der lokalen Oberfläche (leer = im Bordnetz offen).
- `ha_password` — eigenes Passwort für Home Assistant (leer = nutzt `ui_password`).
- `display_token` — Token der Kiosk-Anzeige (`/anzeige?token=…`); leer = Anzeige aus.
- `bundle_refresh_sec` — wie oft das Bundle (Schiff, Soll-Zeiten, Kurs-Grenzen) von KAI geholt wird.

**GPS** (`gps`)
- `source` — `teltonika` (Modbus) oder `manual` (`manual`).
- `host` / `port` (`502`) / `unit` (`1`) — Modbus-Verbindung.
- `lat_reg` (`0`) / `lng_reg` (`2`) / `speed_reg` (`-1` = keins) / `heading_reg` (`-1` = keins) —
  Holding-Register; **an der echten Teltonika verifizieren** (Defaults sind Platzhalter).
- `format` — `float32` (Vorgabe) / `int32_1e7` / `int32_1e6`.
- `poll_sec` (`5`) — Abfrageintervall. Ohne Speed-Register wird die Geschwindigkeit aus der Bewegung abgeleitet.

**SIFA / Alarm** (`sifa`)
- `trigger_combo` — Auslöser: `gps` | `camera` | `both` | `either` (`either`).
- `realarm_sec` (`15`) — Ereignis-SIFA: erneuter Alarm nach Quittieren, solange die Ursache besteht.
- `deadman_on` — Totmann-SIFA aktiv.
- `deadman_sec` (`30`) — Intervall, in dem Anwesenheit bestätigt werden muss.
- `deadman_prewarn_sec` (`5`) — Vorwarnzeit vor dem Totmann-Alarm.

**GPIO** (`gpio`, s. Abschnitt „Verdrahtung")
- `backend` — `sysfs` oder `none` (`none`).
- `horn_pin` (`17`) / `lamp_pin` (`27`) — Ausgänge (Relais).
- `ack_pin` (`22`, `-1` = keiner) — Quittier-Taster (Eingang).
- `sifa_key_pin` (`-1`) — Schlüsselschalter „SIFA an" (Eingang).
- `active_low` — Relais/Ausgang schaltet bei LOW.

**Kamera** (`camera`)
- `enabled` — Kamera-Auswertung aktiv.
- `webhook_secret` — MxMessage ruft `/api/camera/event?secret=…`.
- `snapshot_url` — HTTP-Snapshot, bei Alarm geholt und an KAI gehängt.
- `hold_sec` (`20`) — wie lange ein Kamera-Hinweis als aktiv gilt.

**Netzwerk** (`network`)
- `mode` — `dhcp` (Vorgabe) oder `static`.
- `interface` (`eth0`) / `address` (CIDR) / `gateway` / `dns` (komma-getrennt).

**Serverseitig (KAI-Bundle, wirkt aufs Gerät)**
- Kurs-Grenze je Abschnitt und Basiswert, Hafen-Zone — aus den GPS-Kiosk-Einstellungen.
- „Für die nächste Fahrt Bordbuch anlegen"-Hinweis nach Ankunft (Minuten) — Bundle.
- Schiff, Soll-Zeiten/Reststrecke, Fahrrinnen-Geometrie — Bundle.

## Hinweise / noch zu verifizieren

- **Teltonika-Modbus-Register** (lat/lng + Format) sind **konfigurierbar** und müssen an der
  echten Teltonika verifiziert werden (Defaults sind Platzhalter). Fehlt ein Speed-Register,
  leitet der Dienst die Geschwindigkeit aus der Bewegung ab.
- **GPIO** nutzt sysfs (`/sys/class/gpio`) — dependency-frei; auf aktuellen Pi-OS ggf. durch ein
  gpiochar-Backend ersetzen.
- **Zeitzone des Pi auf `Europe/Berlin` setzen** (`timedatectl set-timezone Europe/Berlin`) — die
  Soll-Zeiten des Bundles sind Ortszeit.
- **SSH + VNC** am Image aktivieren (Fernwartung), Overlay-/Read-only-FS + OS-`unattended-upgrades`
  für wartungsarmen Dauerbetrieb.
- Nicht in dieser Umgebung kompiliert (keine Go-Toolchain vorhanden) — Build per CI/auf dem Pi.

## Komplett-Image (flashen → fertig)

Der Workflow `.github/workflows/image.yml` erzeugt ein **vorkonfiguriertes Raspberry-Pi-Image**, ohne
selbst ein OS zu bauen: Er lädt das **offizielle Raspberry-Pi-OS-Desktop-Image** (SSH + VNC bereits
vorhanden) und legt per Loop-Mount nur noch den Bordwächter-Dienst + ein Erst-Start-Skript hinein
(SSH + VNC an, Hostname `bordwaechter`, Zeitzone Europe/Berlin, Dienst aktiviert). Kein apt/chroot im
Build → keine Debian-Keyring-/Paket-Probleme, baut in wenigen Minuten.

- Bauen: **Actions → image → Run** (Artefakt) oder Tag `img-*` → **Release mit `.img.xz`**.
- Flashen (Raspberry Pi Imager/Etcher), Pi starten — beim ersten Boot richtet sich alles selbst ein
  (SSH, VNC, Hostname, Zeitzone) und startet neu; danach läuft der Dienst.
- **Pro Schiff** nur eine Datei **`bordwaechter.json`** auf die **Boot-Partition** legen (Token/Schiff);
  der Dienst übernimmt sie als `/etc/bordwaechter/config.json`. Alternativ einmal im Web-UI eintragen.
- Standard-Login `pi` / `bordwaechter` (nach dem ersten Start ändern).

## Monitor direkt am Bordwächter-Pi (Geräte-Kiosk)

Der Bordwächter liefert eine Vollbild-Anzeige (`/anzeige` = Pilot, `/seekarte`). Zwei Wege:

- **Anderes Gerät:** im Browser `http://<bordwächter-ip>:8090/anzeige?token=<display_token>` öffnen.
- **Monitor direkt am Pi:** einmalig den Geräte-Kiosk mitinstallieren:
  `sudo bash install.sh --kiosk`
  Danach bootet der Pi in ein Vollbild-Chromium auf die lokale Anzeige (localhost braucht **keinen**
  Token). Startansicht über `display_view` (`pilot`|`seekarte`) in der Konfig-Seite.

## Wetter & Pegel in der Anzeige

Pilot- und Seekarten-Ansicht zeigen oben eine Leiste mit **Wetter** (Temperatur, Wind, Regen) und
**Pegel** (cm, Trend ▲/▼ + Rate, Mini-Verlaufslinie). Die Werte kommen aus **denselben Quellen wie
KAI** — Wetter von **Open-Meteo**, Pegel von **PEGELONLINE** —, also identische Zahlen, **ohne Eingriff
ins Pilot-Bundle**; es läuft mit UND ohne KAI-Verbindung. Konfig: *Wetter & Pegel (Anzeige)* (an/aus,
Koordinaten, Pegel-Station/-Name, Intervall). Vorgabe: beides an, Bodensee/Konstanz.
