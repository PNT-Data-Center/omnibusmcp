# Zgodność wersji

OmnibusMCP jest testowany na konkretnych wersjach systemów operacyjnych i produktów infrastrukturalnych. Wersje spoza zakresu testowania działają normalnie — kontrola `version` w health każdego modułu zawsze zwraca **OK z adnotacją** (nigdy blokada). Decyzja właściciela: narzędzie diagnostyczne jest najczęściej potrzebne właśnie gdy coś się zmieniło, dlatego oddajemy do ręki użytkownika informację, a nie bronimy dostępu.

## Macierz testowanych wersji

| Produkt | Przetestowane | Oczekiwana zgodność | Uwagi |
|---------|---------------|--------------------|-------|
| **Debian** | 13 | 13, prawdopodobnie 12 | — |
| **Ubuntu** | — (nieprzetestowane) | Deklarowana (z AlmaLinux) | Architektura: POSIX systemd, dpkg |
| **AlmaLinux** | — (nieprzetestowane) | Deklarowana | Architektura: POSIX systemd, dpkg (wersja z przedrostkiem `el`) |
| **Proxmox VE** | 9.2.2 (9.2 major.minor) | 9.x | — |
| **Proxmox Backup Server** | 4.0.11 (4.0 major.minor) | 4.x | Wersje 3.x: API może się różnić (m.in. mount-status, przestrzenie nazw) |
| **Docker Engine** | 29.8.1 (29.8 major.minor) | 29.x, 20.10+ | — |
| **Ceph, Podman** | — (moduły jeszcze nie istnieją) | N/A | Planowane w fazie 5+ |

**Źródło prawdy**: Kolumna `compat.Tested` w `internal/compat/compat.go` (format `major.minor`).

## Jak działa sprawdzanie wersji

Każdy moduł zależny od produktu wersjonowanego implementuje metodę `Version()` zwracającą wynik `compat.Result`:

1. **Odczyt wersji**: Źródło zależy od produktu:
   - **Debian/Ubuntu/AlmaLinux**: Z `/etc/os-release` (VERSION_ID)
   - **Proxmox VE**: Z bazy dpkg (`pve-manager`), polecenia `pvesh` nie potrzebne
   - **Proxmox Backup Server**: Z bazy dpkg (`proxmox-backup-server`)
   - **Docker Engine**: Z `docker version --format '{{.Server.Version}}'`

2. **Kolumna VERSION w `omnibusmcp detect`**:
   ```
   MODULE    | DETECTED | ENABLED | VERSION              | DESCRIPTION
   linux     | yes      | yes     | Debian 13 (tested)   | —
   containers| yes      | yes     | Docker Engine 29.8.1 | —
   proxmox   | yes      | yes     | Proxmox VE 9.2.2 *   | — (asterysk → "9.2" w Tested, 9.2.2 jest w 9.2)
   ```
   Testowane wersje wyświetlane w zielonym kolorze; wersje spoza zakresu z asterysku (asteryskiem `*`) i adnotacją poniżej tabeli.

3. **Sekcja `compatibility` w `omnibus://server/info`** (zasób MCP, format JSON):
   ```json
   {
     "compatibility": [
       {"product": "debian", "version": "13", "tested": true, "note": "tested"},
       {"product": "docker", "version": "29.8.1", "tested": true, "note": "tested"},
       {"product": "pve", "version": "9.2.2", "tested": true, "note": "tested"}
     ]
   }
   ```

4. **Health check `version` w `health_summary`**:
   ```
   linux/version       OK  Debian 13 (tested)
   containers/version  OK  Docker Engine 29.8.1 (tested)
   proxmox/version     OK  Proxmox VE 9.2.2 (tested)
   ```

## Tolerancja API: Dekoder tolerancyjny (jsonx)

Nowa wersja produktu może zmienić format JSON API (liczba → string, 0/1 → true/false, nowe pola). Pakiet `internal/jsonx` dekoduje odpowiedzi tolerancyjnie:

1. **Pierwsza próba**: Ścisłe dekodowanie (`encoding/json`). Sukces → koniec.
2. **Fallback**: Gdy ścisłe dekodowanie nie powiedzie się:
   - Kompatybilne wartości są konwertowane: `"1"` → `1`, `"true"` → `true`, `true` → `1`
   - Niezgodne wartości pozostają na zero-wartości pola
   - Nieznane pola są ignorowane
   - Każde takie dekodowanie logowane: `lenient JSON decoding used: the API format differs from the tested one, source="<label>", detail="<błąd ścisłego dekodowania>"`

Logowanie `lenient JSON decoding used` jest sygnałem dla administratora, że wersja produktu zmienia API — wskazówka do uruchomienia testów kontraktowych i ewentualnie aktualizacji `compat.Tested`.

## Testy kontraktowe (nagrane odpowiedzi API)

Moduły parsują odpowiedzi produktów (Proxmox `pvesh`, PBS `proxmox-backup-debug api`, Docker CLI). Odpowiedzi API mogą się zmienić między wersjami. Testy kontraktowe sprawdzają, że każda testowana wersja dekoduje się ściśle i zwraca sensowne wyniki.

| Moduł | Katalog z nagraniami | Test | Wersje nagrane |
|-------|---------------------|------|-----------------|
| `proxmox` | `internal/modules/proxmox/testdata/contract/pve-*/` | `TestContractPVE` | 9.2 |
| `pbs` | `internal/modules/pbs/testdata/contract/pbs-*/` | `TestContractPBS`, `TestContractSurvivesTypeChanges` | 4.0 |
| `containers` | `internal/modules/containers/testdata/contract/docker-*/` | `TestContractDocker` | 29.8 |
