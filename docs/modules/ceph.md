# Moduł ceph

Moduł pokazuje stan klastra Ceph i to, co dzieje się na konkretnym hoście. Działa z obiema popularnymi instalacjami:

- **cephadm** — demony w kontenerach, jednostki systemd `ceph-<fsid>@typ.id`, logi w journalu;
- **pakiety** — Proxmox VE (`pveceph`: konfiguracja w `/etc/pve`, logi w `/var/log/ceph`) oraz dystrybucyjne pakiety Ceph.

**Autodetekcja**: konfiguracja klastra Proxmox VE (`/etc/pve/ceph.conf`) albo katalogi demonów w `/var/lib/ceph`. Same pakiety klienckie (`ceph-common`, które bywają na każdym węźle Proxmoxa) nie włączają modułu.

**Dostęp do klastra**: wyłącznie natywne polecenie `ceph` z kluczem **tylko do odczytu** `client.omnibusmcp` (`mon 'allow r'`, `mgr 'allow r'`). Moduł nie używa kluczy administratora, `cephadm shell` ani `pvesh`. Każde wywołanie ma limit czasu połączenia z monitorami (10 s).

## Tryby pracy

| Tryb | Kiedy | Co pokazuje |
|------|-------|-------------|
| **Pełny** | jest klucz `client.omnibusmcp`, jest polecenie `ceph`, `ceph.cluster` nie jest `false` | stan klastra (narzędzia `ceph_status`, `ceph_osds`, `ceph_pools`, `ceph_pgs`, `ceph_daemons`, `ceph_crashes`, `ceph_cephfs`) oraz widok lokalny |
| **Lokalny** | brak klucza, brak polecenia `ceph`, albo `ceph.cluster: false` | tylko widok lokalny; bez żadnego zapytania do monitorów |

Widok lokalny (`ceph_local`, `ceph_logs`) działa zawsze. Tryb wybierany jest przy starcie usługi (patrz [restart po dodaniu klucza](#zakładanie-klucza)).

Administrator decyduje, na których hostach włączyć widok klastra. Każdy host z OmnibusMCP może pytać klaster, ale nie musi: zwykle wystarcza jeden lub dwa hosty (np. z etykietą `_admin` w cephadm). Pozostałe hosty mają wtedy widok lokalny.

## Zakładanie klucza

Klucz tworzy administrator **raz na klaster**, na węźle z kluczem administratora:

```bash
ceph auth get-or-create client.omnibusmcp mon 'allow r' mgr 'allow r' -o /etc/omnibusmcp/ceph.client.omnibusmcp.keyring
```

Uprawnienia `mon r` i `mgr r` wystarczają do wszystkich zapytań modułu. Klucz nie pozwala niczego zmieniać w klastrze.

### Proxmox VE (pveceph)

Katalog `/etc/pve` jest współdzielony przez wszystkie węzły klastra, więc klucz wystarczy zapisać raz:

```bash
ceph auth get-or-create client.omnibusmcp mon 'allow r' mgr 'allow r' -o /etc/pve/priv/ceph.client.omnibusmcp.keyring
```

Moduł szuka klucza w `/etc/omnibusmcp/`, a potem w `/etc/pve/priv/`, więc każdy węzeł znajdzie go bez kopiowania i działa w trybie pełnym. Jeśli klaster ma być odpytywany tylko z części węzłów, na pozostałych ustaw `ceph.cluster: false`.

### cephadm

Na hoście z etykietą `_admin` (ma klucz administratora i pakiet `ceph-common`):

```bash
install -d -m 700 /etc/omnibusmcp
( umask 077; ceph auth get-or-create client.omnibusmcp mon 'allow r' mgr 'allow r' -o /etc/omnibusmcp/ceph.client.omnibusmcp.keyring )
```

Bez `ceph-common` na hoście polecenie można wykonać w kontenerze; plik utworzony przez `-o` zostałby wtedy w kontenerze, dlatego wynik kieruje się na standardowe wyjście:

```bash
( umask 077; cephadm shell -- ceph auth get-or-create client.omnibusmcp mon 'allow r' mgr 'allow r' > /root/ceph.client.omnibusmcp.keyring )
```

Moduł potrzebuje polecenia `ceph` na hoście (pakiet `ceph-common`); na hostach cephadm bywa niezainstalowane (`cephadm install ceph-common`). Bez niego moduł działa w trybie lokalnym, a `ceph_local` podaje przyczynę.

### Kopiowanie na hosty

Plik klucza kopiuje się na hosty, na których ma działać widok klastra, do `/etc/omnibusmcp/ceph.client.omnibusmcp.keyring`, z prawami tylko dla roota, np. z hosta, na którym powstał:

```bash
for h in host2.example.com host3.example.com; do
  ssh root@$h 'install -d -m 700 /etc/omnibusmcp' &&
  scp -p /etc/omnibusmcp/ceph.client.omnibusmcp.keyring root@$h:/etc/omnibusmcp/
done
```

Klucz jest sekretem: nie wklejaj jego zawartości do czatu, zgłoszeń ani dokumentacji. Jeśli wyciek jest podejrzewany, usuń klucz (`ceph auth del client.omnibusmcp`) i utwórz ponownie.

### Restart usługi

Narzędzia klastrowe rejestrowane są przy starcie usługi. Po dodaniu klucza albo `ceph.conf` na hoście uruchom ponownie usługę:

```bash
sudo systemctl restart omnibusmcp
```

Sprawdzenie: `sudo omnibusmcp detect` oraz `ceph_local` (pole o widoku klastra) lub `ceph_status`.

## Konfiguracja

Sekcja `ceph:` w `/etc/omnibusmcp/config.yaml` jest opcjonalna. Puste wartości oznaczają ustawienia domyślne.

```yaml
ceph:
  # Konfiguracja klastra; bezwzględna ścieżka. Puste: /etc/ceph/ceph.conf,
  # a na hoście cephadm bez etykiety _admin: minimalna konfiguracja demona.
  conf: ""
  # Klucz client.omnibusmcp; bezwzględna ścieżka. Puste: /etc/omnibusmcp/ceph.client.omnibusmcp.keyring,
  # potem /etc/pve/priv/ceph.client.omnibusmcp.keyring.
  keyring: ""
  # auto: pytaj klaster, gdy jest klucz i konfiguracja; false: tylko widok lokalny.
  cluster: auto
```

Ścieżki z konfiguracji mają pierwszeństwo przed ścieżkami domyślnymi. Wartość `cluster` inna niż `auto` albo `false` oraz ścieżka względna powodują błąd konfiguracji.

## Narzędzia

Wszystkie narzędzia są w Tier 1 (tylko odczyt).

### Widok lokalny (zawsze)

| Narzędzie | Co zwraca | Parametry |
|-----------|-----------|-----------|
| `ceph_local` | Instalacja (cephadm, pveceph, pakiety), demony tego hosta i stan ich jednostek systemd (failed, nieaktualne jednostki przeniesionych demonów), mapowanie OSD na dyski, crashe niewysłane do klastra, dostępność widoku klastra | — |
| `ceph_logs` | Logi z tego hosta: pliki (pakiety) lub journal (cephadm). Źródło: `cluster` (log klastra, tylko na hoście z monitorem), `audit` (polecenia wykonane w klastrze, z zamaskowanymi sekretami) albo demon tego hosta (`osd.3`, `mon.a`). Poziom: `warn` (domyślnie), `error`, `all`. | `source`, `level`, `lines` (domyślnie 100, max 1000), `grep` |

### Widok klastra (tryb pełny)

| Narzędzie | Co zwraca | Parametry |
|-----------|-----------|-----------|
| `ceph_status` | Zdrowie i wszystkie aktywne kontrole z ich szczegółami, monitory i kworum, menedżery, liczby OSD (down/out), MDS, zajętość, stany PG, odzysk i ruch klienta. Punkt startowy diagnostyki. | — |
| `ceph_osds` | Podsumowanie OSD (up/in, średnie zajęcie, rozrzut), progi zapełnienia ustawione w klastrze, nietypowe flagi (np. `noout`), zajęcie per host, OSD z problemami (down, out, nearfull/backfillfull/full) i OSD z najwyższymi opóźnieniami. | `host`, `state` (`problems`, `all`, `down`, `out`, `nearfull`), `limit` (domyślnie 50, max 500) |
| `ceph_pools` | Pule: replikacja (size/min_size) lub profil erasure, liczba PG (i cel autoskalera), aplikacja, dane i zajętość, dostępne miejsce, obiekty; uwagi o ryzykownych ustawieniach (min_size 1), kwotach bliskich wyczerpania i pulach bez wolnego miejsca. | `pool` |
| `ceph_pgs` | Liczby PG według stanu, PG zablokowane (inactive, unclean, stale, undersized, degraded) z zestawami OSD up/acting i czasem ostatniego czystego stanu, OSD blokujące peering. | `limit` (domyślnie 50, max 500) |
| `ceph_daemons` | Wersje demonów wg typu (mieszane wersje oznaczone) oraz stan demonów. Z orkiestratorem cephadm: demony niedziałające (lub wszystkie) z hostem, stanem, wersją i czasem startu. Bez orkiestratora (np. Proxmox): monitory, menedżery, OSD i MDS z map klastra. | `type`, `host`, `state` (domyślnie: problemy, lub `all`), `limit` (domyślnie 50, max 500) |
| `ceph_crashes` | Crashe zgłoszone do klastra (najnowsze pierwsze; `new` tylko niezarchiwizowane) oraz crashe, których ten host nie wysłał. Z `id`: wersja, asercja, sygnatura stosu i backtrace jednego crashe'a. | `id`, `new`, `limit` (domyślnie 30, max 300) |
| `ceph_cephfs` | Systemy plików CephFS: pule, liczba klientów, demony MDS ze stanem (active, standby, replay...), ranga, tempo żądań, inody i capy. Brak aktywnego MDS oznacza niedostępny system plików. | — |

Wszystkie wyniki są ograniczone rozmiarem wyjścia (`limits.max_output_bytes`). Na dużych klastrach narzędzia pokazują podsumowanie i tylko elementy z problemami; filtry (`host`, `state`, `type`, `pool`) i `limit` zawężają wynik.

## Health checks

`health_summary` zawiera kontrole `ceph/*`:

| Kontrola | Status | Znaczenie |
|----------|--------|-----------|
| `version` | OK (informacyjnie) | Wersja, którą uruchamia większość demonów klastra (w trybie lokalnym: pakiety) |
| `cluster` | OK | Tylko w trybie lokalnym: powód, dla którego widok klastra jest wyłączony |
| `connection` | OK / **WARN** / **CRIT** | Wspólne sprawdzenie połączenia (`ceph status`). Przekroczony czas połączenia z monitorami = **CRIT** („possible loss of quorum or network failure”). Błąd klucza, uprawnień lub konfiguracji na tym hoście = **WARN**. |
| `health` | OK / WARN / CRIT | `HEALTH_WARN` = WARN, `HEALTH_ERR` = CRIT; w szczegółach aktywne kontrole klastra (wyciszone pominięte) |
| `quorum` | OK / WARN | WARN, gdy któryś monitor jest poza kworum |
| `osds` | OK / WARN | WARN, gdy są OSD down lub out |
| `pgs` | OK / WARN / CRIT | Wszystkie PG active+clean = OK; nieaktywne PG = **CRIT**; degraded, peering i podobne = WARN; scrubbing i inne prace w tle nie są alarmem |
| `daemons` | OK / WARN | Liczba demonów działających na tym hoście; WARN za failed jednostki Ceph. Jednostki cephadm demonów nieobecnych w `orch ps` (przeniesionych) są tylko informacją. |
| `crashes` | OK / WARN | WARN za crashe z ostatnich 30 dni, które nie zostały wysłane do klastra; starsze tylko informacja |

Gdy połączenie z klastrem nie działa, pozostałe kontrole klastrowe (`health`, `quorum`, `osds`, `pgs`) dostają status UNKNOWN z adnotacją „skipped”, zamiast każda czekać na monitory osobno.

Progi zapełnienia OSD (nearfull, backfillfull, full) pochodzą z ustawień klastra, nie z wartości zaszytych w module.

## Ograniczenia

- **Log klastra i audytu** (`ceph_logs` z `source: cluster` lub `audit`) są dostępne tylko na hostach z monitorem. Na pozostałych moduł podpowiada, gdzie szukać.
- **`ceph log last` nie jest używane**: w wydaniach reef i tentacle zwraca pustą listę, więc logi czytane są lokalnie (pliki lub journal).
- **Maskowanie sekretów** w logu audytu: klucze Ceph (`AQ…`), pola `password`, `secret`, `token` oraz wartości `config-key set` i `config set` dla kluczy o nazwach zawierających `pass`, `secret`, `token`, `key` lub `cred`.
- **Restart po zmianie klucza lub konfiguracji**: tryb (pełny lub lokalny) ustala się przy starcie usługi. Dodanie klucza wymaga `sudo systemctl restart omnibusmcp`.
- **Tylko odczyt**: moduł nie restartuje demonów ani nie zmienia klastra. Restart demonów Ceph to przyszłe narzędzie Tier 2.
- **Cache i limity**: identyczne zapytania do klastra są współdzielone przez 10 s, a równolegle działają najwyżej 3 wywołania `ceph`. Wiele hostów i agentów pytających naraz nie przeciąża monitorów.
- **Wersje**: zestaw poleceń przeanalizowano na Ceph 18.2 (reef, cephadm) i 20.2 (tentacle, Proxmox VE 9.2). Pełny test na działających klastrach jeszcze nie został wykonany, więc żadna wersja nie jest jeszcze oznaczona jako przetestowana (`version` w `health_summary` pokazuje to jako informację).
- **Sandbox usługi**: moduł działa w sandboxie `omnibusmcp.service` bez zmian w jego konfiguracji (sprawdzone na Ceph 18.2 i 20.2).
