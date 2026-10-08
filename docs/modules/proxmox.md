# Moduł proxmox

**Autodetekcja**: Katalog `/etc/pve` i polecenie `pvesh` dostępne.

**Źródło danych**: Lokalne API Proxmox VE przez `pvesh get … --output-format json` (nie używa `lvs`, `vgs` ani `pvesm`, które w sandboxie nie działają; dane storage ze `/cluster/resources` z danymi `pvestatd`).

**Narzędzia** (10 tools, Tier 1, read-only):

| Narzędzie | Co zwraca | Parametry |
|-----------|-----------|-----------|
| `proxmox_node_status` | Wersja PVE, kernel, uptime, CPU (model, gniazda, rdzenie), użycie CPU/IO wait/load, RAM, swap, root FS, KSM, tryb bootowania, stan firewalla PVE (włączony/wyłączony, wymuszony/niezwymuszony) | — |
| `proxmox_versions` | `pveversion -v` — wersje pakietów (pvemanager, kernel, qemu, lxc, ceph, corosync, zfs itp.) | — |
| `proxmox_cluster` | Tryb standalone/klaster, quorum, węzły online, stan HA; przy klastrze dodatkowo `pvecm status` | — |
| `proxmox_guests` | Maszyny wirtualne i kontenery LXC: ID, nazwa, węzeł, stan, CPU, RAM, dysk, uptime, HA, szablon, tagi | `type` (qemu, lxc, all), `status` (running, stopped, all) |
| `proxmox_guest` | Konfiguracja i stan bieżący jednej VM/CT; wartości `pass`/`secret`/`token`/`*key` redagowane, klucze SSH tylko liczone | `vmid` (VM/CT ID, obowiązkowy) |
| `proxmox_storage` | Storage: typ, stan, shared, zajętość (w % — także LVM-thin), content types | — |
| `proxmox_tasks` | Zadania węzła z UPID; filtry na błędy, okres, typ, VM ID | `errors_only` (bool), `since` (h/d, np. 24h, 7d), `type` (zadumpowanie, qmstart…), `vmid` (int), `limit` (≤200, default 30) |
| `proxmox_task_log` | Stan, **wynik** (`exitstatus`) i ostatnie N linii logu zadania; UPID walidowany wyrażeniem regularnym | `upid` (obowiązkowy), `lines` (default 100, max 1000) |
| `proxmox_updates` | Oczekujące aktualizacje (bez odświeżania list), repozytoria APT, ostrzeżenia PVE (np. brak włączonego repozytorium) | — |
| `proxmox_backups` | Zadania backupu, goście bez backupu, ostatnie zadania `vzdump` | — |

**Health checks** (`health_summary`, kontrole `proxmox/*`):

- `services`: Status usług pve-cluster, pvedaemon, pveproxy (CRIT), pvestatd, pvescheduler (WARN)
- `pmxcfs`: `/etc/pve` zamontowany (FUSE, pmxcfs). Gdy pmxcfs nie działa, API checks są pomijane (UNKNOWN), aby unikać timeoutów
- `cluster`: Standalone OK; klaster bez quorum CRIT; węzeł offline WARN
- `guests`: Liczby running/stopped/szablony; nietypowy stan lub HA `error` → WARN
- `storage`: Niedostępny storage CRIT; zajętość ≥85% WARN, ≥95% CRIT
- `firewall`: Stan firewalla PVE (wyłączony = OK z jawnym opisem; włączony, demon nie działa = WARN; włączony i demon działa = OK). Źródło: `/etc/pve/firewall/cluster.fw` i `/etc/pve/nodes/<node>/host.fw` (opcja `enable` w `[OPTIONS]`), stan demona (`pve-firewall` lub `proxmox-firewall`)
- `tasks`: Nieudane zadania w ostatnich 24 h → WARN (z typami)
- `updates`: Liczba oczekujących aktualizacji; brak włączonego repozytorium Proxmox VE → WARN

**Wydajność i ograniczenia**:

- Każde wywołanie `pvesh` ~ 0,9 s (start interpretera Perla) + ~175 MB RSS
- **Limit współbieżności**: maksymalnie 2 równoległe `pvesh` (kolejka z poszanowaniem timeoutu), aby uniknąć przekroczenia `MemoryMax=512M` (problem P1 z raportów)
- **Cache**: 5 s z łączeniem identycznych zapytań (e.g., wiele sesji calling `health_summary` równolegle)
- Błędy **nie są cache'owane** (szybka detekcja zmian)
- Health API calls działają równolegle (5 zapytań), ale z przesłanką współbieżności
- Możliwa optymalizacja na przyszłość: czytanie bezpośrednio z plików pmxcfs (`.vmlist`, `.members`, `.rrd`) zamiast `pvesh`, ale format nie jest udokumentowany
