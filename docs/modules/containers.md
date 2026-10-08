# Moduł containers

**Autodetekcja**: Polecenie `docker` oraz gniazdo `/run/docker.sock` (lub starsze `/var/run/docker.sock`) albo unit systemd `docker.service`.

**Źródło danych**: Docker CLI (`docker … --format '{{json .}}'`) przez gniazdo unix (bez SSH/HTTP). Docker daemon jest osiągalny z sandboxa usługi (ProtectSystem=strict umożliwia połączenie z gniazdem).

**Obsługiwane**: Docker 20.10+. **Podman** nie jest obsługiwany w tej wersji — ma inne pola JSON w wynikach i jako narzędzie bez demona wymaga zapisu w `/var/lib/containers`, czego sandbox zabrania.

**Narzędzia** (8 tools, Tier 1, read-only):

| Narzędzie | Co zwraca | Parametry |
|-----------|-----------|-----------|
| `containers_runtime` | Wersja silnika (Docker/Podman), liczby kontenerów (uruchomione/zatrzymane), driver storage, driver logowania, cgroup version, katalog root, **ostrzeżenia silnika** | — |
| `containers_list` | Tabela: nazwa kontenera, stan (running/exited/paused itp.), health (healthy/unhealthy/none), obraz, projekt/usługa Compose, porty; agregacja: liczby running/stopped/paused; filtry `running_only` (bool), `name` (regex) | `running_only`, `name` |
| `containers_inspect` | Konfiguracja jednego kontenera: stan (kod wyjścia ostatniego uruchomienia), **OOM kill** (State.OOMKilled), błąd startu, **liczba restartów** (RestartCount), polityka restartu, limity zasobów (CPU/RAM/I/O), polecenie, sieci (IP, gateway), porty, wolumeny, etykiety (labels), **zmienne środowiskowe z redakcją** (wartości zawierające `pass`/`secret`/`token`/`key`/`credential`/`auth` zastępowane `***(redacted)`) | `container` (obowiązkowy) |
| `containers_logs` | Ostatnie linie stdout i stderr kontenera, w chronologicznym porządku ze znacznikami czasu; stdout i stderr od końca (gdy limit wyjścia; `docker logs` wysyła stderr na stderr, executor je scala); filtry: `lines` (≤2000, default 100), `since` (1h, 30m, 1d itp.), `grep` (regex bez rozróżniania wielkości liter, przeszukuje do 10 000 ostatnich linii) | `container` (obowiązkowy), `lines`, `since`, `grep` |
| `containers_stats` | Jedna próbka metryk: % CPU, użycie i limit RAM, I/O (sieć/dysk w bajtach/s), liczby procesów; aktualizacja `docker stats` (2 s domyślnie) | — |
| `containers_disk_usage` | Podsumowanie `docker system df`: zajętość obrazów, kontenerów, wolumenów (używane/możliwe do odzyskania); lista obrazów (w tym dangling), wolumenów | — |
| `containers_networks` | Sieci Dockera: nazwa, ID, driver (bridge/overlay/host), zakres (CIDR), podsieć, brama, kontenery podłączone z adresami IP, lista sterowników | — |
| `containers_events` | Zdarzenia cyklu życia (create, start, die, oom_kill_event, health_status itp.) z kodami wyjścia i zmianami health; okno czasu ≤7 dni; domyślnie bez szumu `exec_*` z healthchecków (parametr `all_actions: true` przywraca); **ostrzeżenie o limicie bufora demona** (256 ostatnich zdarzeń) | `container` (filtr), `since` (1h, 7d itp.), `all_actions` (bool) |

**Health checks** (`health_summary`, kontrole `containers/*`):

- **`containers/runtime`**: Demon Docker osiągalny (`docker version`). Jeśli nie, status **CRIT** i pozostałe kontrole pominięte.

- **`containers/containers`**: Stan kontenerów (dane z `docker inspect` dla wszystkich):
  - **CRIT** dla: stanu `restarting` (z liczbą restartów), stanu `dead`
  - **WARN** dla: `unhealthy` (healthcheck nieudany), **OOM kill** (`State.OOMKilled`), kodu wyjścia ≠ 0 i ≠ 143 (SIGTERM z `docker stop`), liczby restartów ≥ 3

- **`containers/events`**: Zdarzenia cyklu życia w ostatnich 7 dniach:
  - Nieudane wyjścia (exit code ≠ 0, 143) i OOM wśród zdarzeń
  - **Pętla restartów**:Alert, gdy jeden kontener ma ≥ 3 nieudane wyjścia (`die` z `exit_code` ≠ 0, 143) w ciągu ostatnich zdarzeń
  - Limitacja: demon Dockera przechowuje tylko **256 ostatnich zdarzeń**, dlatego health `containers/containers` czyta trwały stan (`docker inspect`) zamiast polegać na zdarzeniach.

**Redakcja zmiennych środowiskowych**:

Narzędzie `containers_inspect` redaguje zmienne środowiskowe (po nazwie):
- Regex: `(?i)pass|secret|token|key|credential|auth`
- Jeśli nazwa zawiera którekolwiek słowa kluczowe, wartość zastępowana jest `*** (redacted)`
- Przykład: `DB_PASSWORD=mypassword` → `DB_PASSWORD=*** (redacted)`
- Cel: zmniejszenie ryzyka wycieków sekretów w raporcie agenta (sekrety mogą być w zmiennych, ale nie ujawniamy ich jawnie)

**Ograniczenia**:

- **Demon Docker** trzyma tylko 256 ostatnich zdarzeń, następnie je wymazuje. Zdarzenia sprzed kilku godzin mogą nie być dostępne. Dlatego health opiera się na trwałym stanie (`docker inspect`), nie na historii zdarzeń.
- **Podman** nieobsługiwany w tej wersji: JSON z Podmana ma inne nazwy pól, a Podman wymaga zapisu w `/var/lib/containers` (niekompatybilne z `ProtectSystem=strict`). Planowany w przyszłości.
- **Wydajność**: `docker inspect` musi iterować wszystkie kontenery — przy setkach kontenerów może być wolne. W praktyce testy pokazują <100 ms na kilkadziesiąciu kontenerach (testowe środowisko: 5 kontenerów, ~28 ms).
