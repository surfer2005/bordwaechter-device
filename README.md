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
