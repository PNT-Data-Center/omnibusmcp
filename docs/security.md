# Bezpieczeństwo

## Jednorazowe lokalne CA (TLS)

Certyfikat serwera podpisuje lokalne CA utworzone wyłącznie w pamięci podczas `omnibusmcp tls generate`. **Klucz CA nie jest nigdzie zapisywany** — po podpisaniu przestaje istnieć.

- **Nikt nie wystawi innego certyfikatu pod tym CA**, także root na serwerze. Klient, który zaufał CA, ufa więc dokładnie jednemu certyfikatowi: serwera OmnibusMCP. Na serwerze zostaje tylko klucz samego certyfikatu (`key.pem`, 0600), który pozwala podszyć się jedynie pod ten serwer — tak jak każdy klucz TLS.
- **Brak ograniczeń nazw** (name constraints): bez klucza CA nie są potrzebne, a ograniczenia dla adresów IP są odrzucane przez natywnego Claude Code.
- **Adres `/ca.pem` bez tokenu**: certyfikat CA jest jawny (serwer wysyła go w każdym uzgadnianiu TLS), więc jego udostępnienie niczego nie ujawnia. Klient pobiera go jednak przez jeszcze niezweryfikowane połączenie, dlatego **musi porównać odcisk SHA-256** z wartością z konsoli serwera — polecenia z `omnibusmcp tls client-setup` robią to automatycznie i przy niezgodności niczego nie dodają. Serwer udostępnia wyłącznie CA utworzone przez OmnibusMCP, nigdy inny plik.
- **Strona `/` bez tokenu** pokazuje krótką instrukcję (adres MCP, adres CA, skąd wziąć token). Kto doda CA pobrane z `/ca.pem` bez porównania odcisku, ufa mu przy pierwszym użyciu: jeśli w tej chwili ktoś w sieci podszyje się pod serwer, klient zaufa jego certyfikatowi. Gdzie to ryzyko jest istotne, używaj poleceń z `omnibusmcp tls client-setup` (odcisk wpisany automatycznie, kopiowane z konsoli serwera).
- **Odnowienie = nowe CA**: certyfikat jest ważny 5 lat; po odnowieniu klienci muszą zaufać nowemu CA (ponownie `client-setup`).
- **Certyfikaty z innego CA** (np. firmowego) nie są nigdy odnawiane ani zastępowane przez OmnibusMCP.

## Audyt

Każde wywołanie narzędzia jest logowane w `/var/log/omnibusmcp/audit.log` (format: JSON lines). Log zawiera:
- **timestamp**: czas wywołania (RFC 3339)
- **session_id**: ID sesji klienta (UUID)
- **tool_name**: nazwa narzędzia
- **arguments**: parametry w surowej formie JSON (od klienta)
- **status**: wynik autoryzacji i wykonania:
  - `ok`: narzędzie zwróciło sukces
  - `error`: narzędzie zwróciło błąd lub odrzuciło parametry walidacją schematu
  - `rejected`: błąd protokołu MCP (np. nieznane narzędzie, brak wymaganego parametru)
- **duration_ms**: czas wykonania
- **output_bytes**: rozmiar zwróconego wyjścia
- **error**: wiadomość błędu (jeśli status != ok)

**Czytanie audytu:**
```bash
# Ostatnie 50 wpisów
tail -50 /var/log/omnibusmcp/audit.log

# Filtry — wszystkie wpisy narzędzia linux_journal
grep '"tool_name":"linux_journal"' /var/log/omnibusmcp/audit.log

# Wszystkie błędy
grep '"status":"error"' /var/log/omnibusmcp/audit.log

# Sformatowany (wymaga jq)
jq . /var/log/omnibusmcp/audit.log | less
```

## Wykonanie poleceń — limit równoczesności

Każde polecenie działa w osobnym procesie, a procesy potomne i wątki oczekujące na nie wliczają się do limitu `TasksMax=256` jednostki systemd. Serwer uruchamia więc najwyżej 8 poleceń naraz. Kolejne czekają na wolny slot najwyżej tyle, ile wynosi ich timeout, a potem kończą się błędem „server busy: too many commands running at once, retry shortly”. Serwer nie przekracza wtedy limitu i nie przerywa pracy pod dużym obciążeniem (np. wiele równoległych `health_summary`). Limity modułów pozostają: `pvesh` — 2 równoczesne wywołania, `proxmox-backup-debug` — 4.

## Crontaby użytkowników — reguła prywatności

`linux_scheduled_jobs` pokazuje dla crontabów użytkowników (`/var/spool/cron/crontabs` na Debianie/Ubuntu, `/var/spool/cron` na RHEL/AlmaLinux) **tylko właściciela i liczbę wpisów, nigdy treść** — polecenia w crontabach mogą zawierać hasła i tokeny. Błędne polecenie w crontabie użytkownika administrator sprawdza ręcznie: `crontab -u <użytkownik> -l`. Tabele crona systemowego (`/etc/crontab`, `/etc/cron.d/`) są pokazywane w całości, bo i tak są czytelne przez `linux_read_file`.

## Hardening Tier 1

Usługa działa jako root (świadoma decyzja); kompensacja poprzez zaostrzony hardening systemd:

**Capabilities** (ogromnie ograniczone):
```
CapabilityBoundingSet=CAP_DAC_READ_SEARCH CAP_SYS_PTRACE CAP_NET_BIND_SERVICE
AmbientCapabilities= (puste)
```
Wynik: `systemd-analyze security omnibusmcp.service` zwraca **2.4 OK** (wcześniej 7.5 EXPOSED). Proces wyświetla `CapEff=0x80404` (tylko 3 z 38 capabilities), `Seccomp=2`, `NoNewPrivs=1`.

**Sandbox**:
```
ProtectSystem=strict
ReadWritePaths=/var/log/omnibusmcp
PrivateDevices=yes
ProtectHome=read-only
PrivateTmp=yes
ProtectKernelLogs=yes
ProtectHostname=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
ProtectClock=yes
RestrictNamespaces=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service ~@privileged ~@resources
SystemCallErrorNumber=EPERM
UMask=0077
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
MemoryMax=512M
TasksMax=256
NoNewPrivileges=yes
```

**Ograniczenia dla przyszłych modułów**: Moduły wymagające specjalnych uprawnień (podman/cephadm, smartctl, ping) będą wymagały świadomego poluzowania unitu (profil zależny od modułów/tieru).

## Dodatkowe warstwy bezpieczeństwa

- **Brak generycznego shella**: Każde narzędzie to konkretne polecenie ze stałymi argv — brak możliwości command injection.

- **Tiery dostępu**: Każde narzędzie deklaruje minimalny tier; client nie może przekroczyć ustawionego tieru.

- **Audyt**: Każde wywołanie logowane w `/var/log/omnibusmcp/audit.log` (patrz sekcja „Audyt" wyżej). Middleware MCP rejestruje także żądania **odrzucone** (nieznane narzędzia, błędy schematu).

- **Sekrety**:
  - Akceptujemy ryzyko, że odczyt konfiguracji/logów/docker inspect może zwrócić sekrety.
  - **Odmowa dostępu do magazynów sekretów i kluczy prywatnych**: `linux_read_file` i `linux_list_dir` odbijają:
    - **Katalogi** `/etc/pve/priv/` (Proxmox: authkey.key, CA key, shadow.cfg, tfa.cfg, token.cfg), `/etc/ssl/private/`, `/etc/wireguard/`, `/etc/ipsec.d/private/`, `/etc/letsencrypt/{archive,keys}`, `/etc/NetworkManager/system-connections/`
    - **Dokładne ścieżki**: `/etc/shadow`, `/etc/gshadow`, `/etc/corosync/authkey`, `/etc/ipsec.secrets`
    - **Wzorce nazw**: `*.key` (klucze TLS/SSH), `*.keyring` (Ceph), `*_key` (klucze hosta)
    - **Treść**: Pliki zawierające blok `PRIVATE KEY-----` (PEM/OpenSSH) są odrzucane niezależnie od nazwy
    - **Symlinki**: Polityka sprawdzana na ścieżce żądanej i po rozwiązaniu, więc dowiązania nie mogą obejść blokady
  - **Odmowa odczytu własnych sekretów**: Dodatkowo blokujemy dostęp do:
    - Własnego **tokenu serwera** (`/etc/omnibusmcp/token`)
    - Własnego **klucza TLS** (`/etc/omnibusmcp/tls.key` lub ścieżka z `config.yaml`)
  - To zmniejsza ryzyko przypadkowego wklejenia sekretów do raportu agenta. Administrator wciąż odpowiada za inne sekrety w zwykłych plikach konfiguracyjnych (aplikacji, docker secrets, itp.).

## Znane problemy

**B8 — Dostęp do sekretów Proxmox VE** (NAPRAWIONO 2026-09-30)

Była: Narzędzia `linux_read_file` i `linux_list_dir` miały dostęp do katalogu `/etc/pve/priv/`, który zawiera wrażliwe sekrety (authkey.key, pve-root-ca.key, shadow.cfg, tfa.cfg, token.cfg). Posiadacz tokenu OmnibusMCP mógł uzyskać pełną kontrolę nad Proxmoxem.

**Poprawka**: Wdrożona polityka odmowy dostępu — całkowite katalogi (`/etc/pve/priv/`, `/etc/ssl/private/`, `/etc/wireguard/`, `/etc/letsencrypt/`, itp.), wzorce nazw (`*.key`, `*.keyring`, `*_key`), dokładne ścieżki (`/etc/shadow`, `/etc/corosync/authkey`), oraz analiza treści (blok `PRIVATE KEY-----`). Rezolucja dowiązań symbolicznych chroni przed obejściem (np. `/root/.ssh/authorized_keys` → `/etc/pve/priv/` jest odrzucany).
