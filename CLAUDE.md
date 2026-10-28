# homeassistant-addon - Hinweise für Agenten

Repo `TimWeyand/homeassistant-addon` (Home-Assistant-Supervisor-Add-on-Repository, kein HACS). Kommunikation und diese Datei: Deutsch (Code/Identifier englisch). `README.md` und `CHANGELOG.md` des SMA-Add-ons sind **englisch** - Sprache und Stil dort beibehalten.

## Layout

- `SMA_Battery_Controller/` - Go-Add-on (eine Datei `sma_battery_controller.go`, Tests `internal_automatic_test.go`, `control_fixes_test.go`), Manifeste `config.yaml` + `config.json`, `Dockerfile` (HA-Image), `build.yaml` (Base-Images für `home-assistant/builder`), `Dockerfile.standalone` + `docker-compose.standalone.yml` (ohne HA), `run.sh` (Supervisor-Schleife mit Backoff), `README.md`, `CHANGELOG.md`.
- `comfoair2mqtt/` - zweites Add-on (Komfortlüftung, eigene Version/Doku). Nicht mit dem SMA-Add-on vermischen.
- `.github/workflows/ci.yml` - Jobs: SMA-Tests (vet, `go test -race`, govulncheck), SMA-Standalone-Build, SMA-/Comfoair-Publish.

## SMA-Architektur (Kurzfassung)

- Modbus TCP zum SMA Sunny Tripower SE 10; Steuerung über zwei Register: **40151 = SpntCom** (`802` = externe Steuerung aktiv, `803` = Wechselrichter automatisch) und **40149 = PwrAtCom** (Watt, negativ = laden, positiv = entladen). Es wird erst 40151, dann 40149 geschrieben.
- Modi: `Automatic`, `Balanced`, `Pause (charge ok)`, `Pause`, `Charge Battery`, `Discharge Battery`. `Balanced` wirkt nur als Overwrite (entlade-only, passt `battery_control` dynamisch an, wird beim Verlassen auf den vorherigen Nutzerwert zurückgesetzt).
- **Effektiver Modus:** Overwrite != `Off` gewinnt (auch Overwrite `Automatic` = aktiv 803); bei Overwrite `Off` gilt `Automatic Logic Selection`.
- MQTT (HA-Discovery, retained Selects/Number): `homeassistant/<typ>/<device_id>/<objekt>/{config,state,set}`; Verfügbarkeit `smastp_modbus/status` (LWT); HA-Birth `homeassistant/status` löst Republish aus. Sensoren sind nicht retained.
- PV-aware Umschaltung (`internalAutomatic`): `Pause (charge ok)` und `Charge Battery` geben den WR bei PV-Überschuss frei (Einspeisung > 100 W bzw. > 500 W). Laden aus PV über Setpoint/Ziel-SOC ist **immer gewollt**.
- `previousMode` wird nur nach erfolgreichem Write gesetzt -> Fehlschlag wird beim nächsten Poll-Tick wiederholt. Ein "kein Write"-Entscheid gibt eine frühere 802-Steuerung einmalig mit 803 frei (`lastWrittenSpntCom`).
- Fehlerzähler zählt aufeinanderfolgende Modbus-Fehler (Reset nach erfolgreichem Lesezyklus), > 20 -> `os.Exit`, `run.sh` startet neu. Lock-Reihenfolge: `controlMu -> readMu -> sensorMu/modbusMu`.
- `reset_interval_minutes` ist nur eine periodische Neubewertung der Steuerlogik, **kein** Reset auf Automatic.

## Build und Test (nur über Docker)

- Lokal ist kein Go installiert. Docker Desktop nutzen, Go-Image `golang:latest` (oder die Version aus `go.mod`/Dockerfile).
- **`Z:` lässt sich nicht als Bind-Mount einbinden (kommt leer an).** Rezept: Add-on-Ordner in ein lokales Scratch-Verzeichnis kopieren, dort ein Bash-Skript (`run.sh`) ablegen und starten:
  `docker run --rm -v "<scratch>\src:/src" -v "<scratch>:/chk" -v gomodcache:/go/pkg/mod -w /src golang:latest bash /chk/run.sh`
  Skript: `gofmt -l . ; go vet ./... ; go test -race -count=1 ./...` (optional `staticcheck`, `govulncheck`). Aus PowerShell 5.1 kein `bash -c "a && b"` inline (Quoting), immer Skriptdatei (Name z.B. `check.sh`, nicht `run.sh` - Verwechslung mit dem `run.sh` des Add-ons). Skript mit **LF und ohne BOM** schreiben (z.B. `[IO.File]::WriteAllText($pfad, $text, (New-Object Text.UTF8Encoding($false)))` mit `` `n `` im Text), **nicht** mit `Set-Content`/`Out-File` (PS 5.1 schreibt CRLF, `Out-File -Encoding utf8` zusätzlich ein BOM; bash scheitert am `\r`). Ergebnisse explizit zurück nach `Z:` kopieren.
- Image-Builds: `docker build --build-arg BUILD_ARCH=amd64 -f Dockerfile .` und `docker build -f Dockerfile.standalone .` (ebenfalls aus der lokalen Kopie).

## Release-Regeln

- Semver: autonom **nur `patch`** erhöhen; `minor`/`major` nur auf ausdrückliche Anweisung von Tim.
- Version steht in **`config.yaml` und `config.json`** (müssen inhaltlich identisch bleiben, nicht nur die Version) und im neuen `## x.y.z`-Eintrag oben in `CHANGELOG.md`. `go.mod`-Direktive (Mindestversion, CI-`setup-go` liest sie) und Dockerfile-Builder-Tags (aktuelles Patch-Release, z.B. `golang:1.27.3-alpine`) sind **getrennt**: nicht `go.mod` auf die Toolchain-Patchversion setzen; der Dockerfile-Tag muss die `go`-Direktive erfüllen, beide Dockerfiles gleich halten.
- README und Doku bei Verhaltensänderung im selben Zug aktualisieren; HomeNetwork-Doku (`Z:\HomeNetwork\Documentation\homeassistant\setup.md`) trägt die **installierte** Version.
- **Kein `git commit`/`git push` ohne ausdrückliche Anweisung.** Die CI veröffentlicht Images nur bei Push auf `main` (PR-Läufe bauen ohne Push/Login).

## Live-Fakten

- Fällt der Add-on-Prozess aus, übernimmt der Wechselrichter nach ca. 5-30 Minuten selbst und geht auf Automatik - ein Shutdown-Fail-Safe im Add-on ist nicht nötig.
- Die %-Stopp-Automation (`input_number.max_pv_soc_laden`, Automation `pvbm_manual_charge_target_soc`) lebt in Home Assistant, nicht im Add-on, gilt **nur für das manuelle Overwrite `Charge Battery`** und stoppt keine PV-Battery-Manager-Ladung. Overwrite `Off` gibt nur an `Automatic Logic Selection` zurück: steht die auf `Charge Battery`, wird weiter erzwungen geladen, sonst lädt der WR weiter aus PV (evtl. bis 100 %). Overwrite `Automatic` gibt den WR bedingungslos frei (803); harter Stopp = Overwrite `Pause`.
- HA-Zugang (URL, Token) steht in `Z:\HomeNetwork` (`CLAUDE.md`, `HA-API-Key`): nur lesen, nie ausgeben oder im Repo ablegen.

## Keine Secrets im Repo

Keine Zugangsdaten, echten Hostnamen oder IPs in Code, Compose, Doku, Commits oder Logs - Platzhalter (`<mqtt-user>`, `<inverter-ip>`) verwenden. Hinweis: frühere Stände von `docker-compose.standalone.yml` enthielten echte Werte in der Git-Historie (MQTT-Passwort rotieren).
