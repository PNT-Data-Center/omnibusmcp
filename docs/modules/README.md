# Moduły OmnibusMCP

## Dostępne moduły

| Moduł | Zakres | Tier 1 | Autodetekcja |
|-------|--------|--------|--------------|
| **[linux](linux.md)** | systemd, journal, dyski, sieć, obciążenie, pamięć, tożsamość hosta, zadania cykliczne | 11 narzędzi | zawsze włączony |
| **[containers](containers.md)** | Docker: runtime, kontenery, logi, statystyki, zajętość dysku, sieci, zdarzenia | 8 narzędzi | `docker` + `/run/docker.sock` |
| **[proxmox](proxmox.md)** | Proxmox VE: węzeł, klaster/kworum, VM i kontenery, storage, zadania, aktualizacje, backupy | 10 narzędzi | `/etc/pve` + `pvesh` |
| **[pbs](pbs.md)** | Proxmox Backup Server: datastore'y, garbage collection, backupy, zadania, aktualizacje | 8 narzędzi | `/etc/proxmox-backup` + `proxmox-backup-debug` |
| **[ceph](ceph.md)** | Ceph (cephadm, Proxmox VE pveceph): stan klastra, OSD, pule, PG, CephFS; demony, dyski, crashe i logi tego hosta | 9 narzędzi (7 w trybie pełnym) | `/etc/pve/ceph.conf` lub dane demonów w `/var/lib/ceph` |

## Konfiguracja modułów

Moduły w OmnibusMCP są definiowane w sekcji `modules:` w `/etc/omnibusmcp/config.yaml`. Domyślnie włączony jest moduł `linux` (zawsze). Pozostałe moduły (`containers`, `proxmox`, `pbs` i `ceph`) mogą być auto-wykrywane lub wymuszone jawnie.

### Tryb auto-detekcji

`modules: [auto]` (domyślnie):
- Przy **każdym starcie usługi** (`systemctl start omnibusmcp` lub po instalacji) serwer sprawdza, które moduły są dostępne na hoście
- Włącza wszystkie **wykryte** moduły automatycznie
- Moduł `linux` jest **zawsze włączony**, niezależnie od ustawień
- Detekcja odbywa się **tylko przy starcie** — jeśli zainstalujemy Dockera po uruchomieniu usługi, wymagana jest restartacja: `sudo systemctl restart omnibusmcp`

**Kryteria autodetekcji:**
- **linux** — zawsze dostępny na każdym systemie Linux
- **containers** — dostępny, jeśli istnieje:
  - Polecenie `docker`, **oraz**
  - gniazdo `/run/docker.sock` (lub `/var/run/docker.sock`) **albo** unit systemd `docker.service`
- **proxmox** — dostępny, jeśli istnieje:
  - Katalog `/etc/pve` (klaster/node Proxmox VE)
  - **i** polecenie `pvesh`
- **pbs** — dostępny, jeśli istnieje:
  - Katalog `/etc/proxmox-backup`
  - **i** polecenie `proxmox-backup-debug`
- **ceph** — dostępny, jeśli istnieje:
  - Konfiguracja klastra Proxmox VE `/etc/pve/ceph.conf`, **albo** katalogi demonów w `/var/lib/ceph`
  - Same pakiety klienckie (np. `ceph-common` na węźle Proxmox) nie włączają modułu

### Jawna lista modułów

`modules: [linux, containers, proxmox, pbs]`:
- Włączone **dokładnie wymienione** moduły, **niezależnie od tego, czy są wykryte**
- Moduł `linux` jest **zawsze dodawany** (nawet jeśli go nie wymienisz)
- Pozostałe niewymienione moduły (np. `containers`) są wyłączone

**Przykład**: `modules: [linux]` — wyłącza wszystkie pozostałe, nawet jeśli są dostępne.

### Kombinowanie auto-detekcji z wymuszeniem

`modules: [auto, proxmox]`:
- Włączone wszystkie **auto-wykryte** moduły
- **Dodatkowo** wymuszony moduł `proxmox`, nawet jeśli nie jest wykryty
- Przydatne, gdy Proxmox jest zainstalowany, ale nie spełnia kryteriów autodetekcji (np. `/etc/pve` jest zamontowany inaczej)

### Walidacja i błędy

- **Nieznana nazwa modułu** (np. `modules: [linux, unknownmodule]`) → Błąd konfiguracji
  - Kod wyjścia: **78** (`EX_CONFIG`)
  - Usługa **nie restartuje się w pętli** (`RestartPreventExitStatus=78`)
  - Dostępne nazwy: `linux`, `containers`, `ceph`, `proxmox`, `pbs`
  - Błąd widoczny w: `sudo systemctl status omnibusmcp` lub `sudo journalctl -u omnibusmcp -n 50`

- **Wymuszenie modułu, którego oprogramowanie nie ma zainstalowanego**
  - Polecenia (np. `docker`) zwrócą błąd „command not found"
  - Nie zalecane, chyba że debugujesz konfigurację

### Sprawdzanie konfiguracji modułów

```bash
# Wyświetli tabelę MODULE/DETECTED/ENABLED
sudo omnibusmcp detect

# Wynik przykładowy na hoście z Dockerem, Proxmoxem i PBS:
# MODULE    | DETECTED | ENABLED
# linux     | yes      | yes
# containers| yes      | yes
# proxmox   | yes      | yes
# pbs       | yes      | yes
# ceph      | no       | no
```

```bash
# Lista narzędzi dla bieżącej konfiguracji i tieru
sudo omnibusmcp tools

# Wynik na Tier 1 z linux + containers + proxmox + pbs: 38 narzędzi
```

```bash
# Log startowy usługi pokazuje włączone moduły
sudo journalctl -u omnibusmcp -n 1 | grep "modules="
# modules=linux,containers,proxmox
```

```bash
# Zasób MCP omnibus://server/info zawiera listę dostępnych modułów
claude mcp call omnibus resources
```

### Ustawienie modułów podczas instalacji

```bash
# Auto-detekcja (domyślnie)
sudo omnibusmcp install --tier 1

# Jawna lista
sudo omnibusmcp install --tier 1 --modules linux,containers

# Kombinacja auto + wymuszenie
sudo omnibusmcp install --tier 1 --modules auto,proxmox

# Wymuszenie nadpisania istniejącej konfiguracji
sudo omnibusmcp install --tier 1 --modules linux,proxmox --force
```
