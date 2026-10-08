# Moduł pbs

**Autodetekcja**: Katalog `/etc/proxmox-backup` i polecenie `proxmox-backup-debug` dostępne.

**Źródło danych**: Lokalne API Proxmox Backup Server przez `proxmox-backup-debug api get … --output-format json` (nie używa `proxmox-backup-manager`, który w sandboxie nie pracuje — wymaga dostępu do zapisu w `/run/proxmox-backup/shmem/config-versions`).

**Narzędzia** (8 tools, Tier 1, read-only):

| Narzędzie | Co zwraca | Parametry |
|-----------|-----------|-----------|
| `pbs_node_status` | Wersja PBS, kernel, uptime, CPU, RAM, swap, root FS, tryb bootowania, stan subskrypcji, certyfikat API/proxy (ważność, odcisk dla klientów PVE) | — |
| `pbs_versions` | Wersje pakietów PBS (pvemanager, kernel, qemu-server, proxmox-backup itp.) | — |
| `pbs_datastores` | Wszystkie datastore'y: ścieżka, zajętość (used/total/available), szacowana data zapełnienia, stan montowania, tryb serwisowy, garbage collection (ostatnie uruchomienie, stan, oczekujące na zwolnienie). Datastore'y współdzielące system plików są oznaczane. | — |
| `pbs_datastore` | Jeden datastore: konfiguracja, zajętość, GC (w tym źle uszkodzone chunki), grupy backup'ów we wszystkich przestrzeniach nazw (liczby per przestrzeń i typ, snapshoty, najnowszy/najstarszy backup, grupy bez backupu > 48 h) | `store` (nazwa datastore'u, obowiązkowy) |
| `pbs_jobs` | Zadania scheduled: prune, verify, sync, tape backup; harmonogram, ostatnie uruchomienie i jego stan, następne uruchomienie (oznaczenie OVERDUE). Zadania prune pokazują opcje retencji (`keep-*`). | — |
| `pbs_tasks` | Ostatnie zadania (backup, prune, garbage collection, verify, sync, updates itd.) z statusami. Filtry: `errors_only` (boolean), `since` (okres: 24h, 7d), `type` (typ zadania), `store` (datastore), `limit` (max 200, default 30) | `errors_only`, `since`, `type`, `store`, `limit` |
| `pbs_task_log` | Status, wynik i ostatnie linie logu pojedynczego zadania, identyfikowanego przez UPID | `upid` (obowiązkowy), `lines` (default 100, max 1000) |
| `pbs_updates` | Oczekujące aktualizacje pakietów (z ostatniej aktualizacji APT, nie odświeżanej tutaj) i konfiguracja repozytoriów APT z ostrzeżeniami (np. brak włączonego repozytorium Proxmox Backup Server) | — |

**Health checks** (`health_summary`, kontrole `pbs/*`):

- **`services`**: Status usług `proxmox-backup` (API) i `proxmox-backup-proxy`. Gdy API nie działa → CRIT i pozostałe kontrole pominięte (brak dostępu do danych).
- **`datastores`**: Zajętość wszystkich datastore'ów. WARN ≥85%, CRIT ≥95% (ocena raz na system plików; datastore'y współdzielące FS mają wspólną zajętość). Zapełnienie < 30 dni → WARN. Stan montowania i tryb serwisowy → WARN/CRIT.
- **`gc`**: Stan ostatniego garbage collection na każdym datastore'e (OK/WARN/CRIT). CRIT dla uszkodzonych chunków (`still-bad` > 0).
- **`backups`**: Liczby grup backup'ów (snapshoty, typy). WARN gdy datastore z grupami nie ma żadnego nowego backupu > 48 h. Pojedyncze stare grupy (usunięte gośćmi, rzadkie harmonogramy) są liczone bez alarmu.
- **`jobs`**: Liczby zadań scheduled (prune/verify/sync/tape). WARN: nieudane ostatnie uruchomienie, zadania nigdy nieuruchomione mimo upłyniętego terminu, zadania prune bez opcji retencji, odwołania do nieistniejących datastore'ów.
- **`tasks`**: Liczby nieudanych zadań w ostatnich 24 h (per typ zadania).
- **`updates`**: Liczba oczekujących aktualizacji. WARN gdy brak włączonego repozytorium Proxmox Backup Server.
- **`certificate`**: Ważność certyfikatu API/proxy. WARN gdy <30 dni do wygaśnięcia, CRIT gdy wygasł.

**Wydajność i ograniczenia**:

- **Źródło danych**: `proxmox-backup-debug api get` ~ 0,15 s, ~ 28 MB RSS per call (Rust, znacznie taniej niż `pvesh` w PVE).
- **Limit współbieżności**: maksymalnie **4 równoległe** wywołania API (queue z poszanowaniem timeoutu), aby ograniczyć zużycie pamięci i CPU.
- **Cache**: **5 s** dla identycznych zapytań (health odpytuje ok. 20 endpointów, cache zmniejsza liczbę wywołań).
- **Przestrzenie nazw (namespaces)**: Moduł obsługuje namespaces — `/admin/datastore/<store>/namespace` z `--ns` dla każdej przestrzeni.
- **Współdzielone systemy plików**: Datastore'y mogą być montowane na jednym filesystem; zajętość oceniana raz per FS.
- **Data zapełnienia**: Gdy data zapełnienia z przeszłości (`estimated-full-date` < teraz) → wyświetlane jako „not growing" bez fałszywego alarmu.

**Testy**:

- **Test cases**: 14 przypadków (12 funkcjonalnych + auth + metadata); wszystkie PASS na produkcyjnym PBS 4.0.11.
- Moduł nie wymaga poluzowania sandboxa (ProtectSystem=strict).
