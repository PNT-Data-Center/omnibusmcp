# Instalacja jako usługa systemd

Instalacja automatycznie tworzy jednostkę systemd, plik konfiguracji i generuje token autentykacji:

```bash
# Instalacja na Tier 1, auto-detekcja modułów, nasłuch na 127.0.0.1:8765
sudo ./omnibusmcp install --tier 1

# (Alternatywa) Instalacja na Tier 2, jawnie wskazane moduły
sudo ./omnibusmcp install --tier 2 --modules linux,containers,ceph

# (Alternatywa) Nasłuch na sieci (bez TLS, wymaga Internetu)
sudo ./omnibusmcp install --tier 1 --listen 0.0.0.0:8765
```

Po instalacji:

```bash
# Uruchomienie
sudo systemctl start omnibusmcp
sudo systemctl status omnibusmcp

# Odczytanie tokenu
sudo cat /etc/omnibusmcp/token

# Wyłączenie
sudo systemctl stop omnibusmcp
```

## Zarządzanie usługą — diagnostyka błędów

### Kolory w terminalu

Polecenia CLI (`status`, `detect`, `tools`, `version`, `tls` i `install`/`uninstall`) wypisują kolorowe wyjście, jeśli wyjście jest terminalem. Kolory wyłączają się automatycznie przy przekierowaniu/pipe (np. `omnibusmcp status | grep …` zwraca czysty tekst bez ANSI).

**Sterowanie kolorami**:
- `OMNIBUSMCP_COLOR=always` — wymusza kolory (nawet do pipe)
- `OMNIBUSMCP_COLOR=never` — wyłącza kolory (nawet na terminalu)
- `OMNIBUSMCP_COLOR=auto` (domyślnie) — auto-detect
- `NO_COLOR` — standard https://no-color.org/ (wyłącza kolory, gdy zmienna jest ustawiona)
- `TERM=dumb` — wyłącza kolory

**Znaczenie kolorów**:
- **Zielony** — działa, OK, ważny element (usługa aktywna, certyfikat ważny)
- **Żółty** — ostrzeżenie (usługa zatrzymana, certyfikat < 30 dni do wygaśnięcia, TLS wyłączony, Tier > 1, narzędzie nie tylko do odczytu)
- **Czerwony** — błąd (usługa nieinstaolowana, certyfikat wygasły, port nie nasłuchuje, brak usługi)
- **Cyjan** — adres, endpoint, nazwa modułu, polecenie
- **Przygaszony** — informacje drugorzędne

**Ważne**: Kolory nigdy nie trafiają do:
- Odpowiedzi narzędzi MCP (serwer zwraca czysty tekst dla agentów)
- Logów usługi (audit.log, systemd journal)
- Danych zwracanych przez moduły

Wyrównywanie tabel (w `detect`, `tools`) bierze pod uwagę tylko widoczną szerokość tekstu (kody ANSI nie wpływają na kolumny).

### Szybka diagnostyka — polecenie `status`

```bash
# Sprawdzenie stanu usługi i endpointu MCP
sudo omnibusmcp status

# Przykładowe wyjście (usługa aktywna):
# OmnibusMCP v0.1.0
#
# Service:     omnibusmcp.service installed, enabled
# State:       active (running) since Tue 2026-09-30 15:05:10 CEST
# PID:         12345
# Running:     version v0.1.0
#
# Endpoint:    https://192.0.2.10:8765/mcp
# Listening:   yes (192.0.2.10:8765 accepts connections)
# Config:      /etc/omnibusmcp/config.yaml
# Tier:        1
# Modules:     linux, containers, proxmox  (enabled at start; check with: omnibusmcp detect)
# TLS:         enabled, certificate valid until 2027-05-31 (244 days)
# TLS renewal: omnibusmcp-tls-renew.timer active, next run Wed 2026-10-01 07:30:15 CEST
```

**Kody wyjścia** (LSB convention):
- **0**: usługa aktywna (running)
- **3**: usługa zatrzymana lub w stanie `failed`
- **4**: usługa nie zainstalowana

Serwer może nie uruchomić się z powodu błędów konfiguracji (np. brakujący token, zły tier, port zajęty). W takim przypadku usługa **nie restartuje się w pętli** — przechodzi w stan `failed`.

**Jak zdiagnozować problem:**

```bash
# Sprawdzenie statusu
sudo systemctl status omnibusmcp

# Ostatnie logi (systemd journal)
sudo journalctl -u omnibusmcp -n 50

# Szczegółowy log (jeśli usługa się częściowo uruchomiła)
sudo tail -50 /var/log/omnibusmcp/audit.log
```

**Kody wyjścia:**
- **0**: sukces (zwykle przy `systemctl stop`)
- **78** (`EX_CONFIG`): błąd konfiguracji, tokenu lub audytu. Unit zawiera `RestartPreventExitStatus=78`, więc systemd nie podnosi usługi w pętli. Usuń błąd w `/etc/omnibusmcp/config.yaml` lub tokenie i restartuj ręcznie:
  ```bash
  sudo systemctl restart omnibusmcp
  ```
- **Inne kody**: awarie procesu (segfault itp.) — system podejmie `Restart=on-failure` (co 5 s, max 5 razy na 10 s).

Jeśli usługa przejdzie w `failed`, możesz zresetować status:
```bash
sudo systemctl reset-failed omnibusmcp
```
