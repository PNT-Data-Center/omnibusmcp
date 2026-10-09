# HTTPS / TLS

Domyślnie serwer nasłuchuje na `127.0.0.1:8765` (localhost, wymaga SSH tunelu). Do nasłuchu na innym interfejsie wymagana jest obsługa TLS lub flaga `allow_insecure_remote` (tylko dla tuneli bez dostępu do Internetu).

`omnibusmcp install --listen <adres>:8765` z adresem spoza loopbacka włącza TLS od razu: zapisuje config z `tls.cert_file`/`tls.key_file` (`/etc/omnibusmcp/tls/`), generuje certyfikat tak jak `tls generate` (istniejący zostaje) i wypisuje polecenia konfiguracji klientów. Zwykłe HTTP na adresie sieciowym wymaga jawnej flagi `install --allow-insecure-remote`.

## Model certyfikatu: jednorazowy lokalny CA

Serwer OmnibusMCP używa **jednorazowego lokalnego CA** zamiast certyfikatów self-signed. Przy generowaniu:
1. W pamięci tworzony jest lokalny CA (CN „OmnibusMCP CA <host>", O „OmnibusMCP", bez ograniczeń nazw)
2. CA podpisuje certyfikat serwera
3. **Klucz CA jest natychmiast odrzucany** — nigdy nie trafia na dysk, więc nikt (nawet root) nie może wydać dodatkowych certyfikatów pod tym CA
4. Klienci ufają temu CA (do pobrania z `https://HOST:8765/ca.pem`; zaufanie przy pierwszym użyciu, patrz niżej); certyfikat serwera jest ważny domyślnie 5 lat
5. Odnowienie tworzy **nowy CA** (klucza poprzedniego już nie ma) — klienci muszą mu zaufać ponownie: na serwerze `omnibusmcp tls client-setup`, na klientach wypisane polecenia; zdarza się to rzadko (domyślnie co ~5 lat)

## Generowanie certyfikatu

Certyfikat i lokalny CA zarządzane są przez aplikację:

```bash
# Generuj jednorazowy CA i certyfikat serwera na 5 lat (1826 dni)
# Automatycznie wykrywa SAN: hostname, FQDN, adresy interfejsów, localhost, loopback
sudo omnibusmcp tls generate

# (Opcjonalnie) Z jawnie podanym listem hostów
sudo omnibusmcp tls generate --hosts myhost.example.com,192.0.2.10

# Wymuszenie nadpisania istniejącego certyfikatu
sudo omnibusmcp tls generate --force

# Wyniki: /etc/omnibusmcp/tls/cert.pem (0644, łańcuch: serwer + CA), key.pem (0600),
# ca.pem (0644, publiczny certyfikat CA)
# Konfiguracja YAML automatycznie zaktualizowana
# Serwer restartuje się automatycznie
```

Po wygenerowaniu wypisane są:
- **Ważność**: data wygaśnięcia i liczba dni
- **SAN (Subject Alternative Names)**: hostnames i adresy IP w certyfikacie
- **Instrukcje dla klientów**: polecenia do `omnibusmcp tls client-setup`

## Pobieranie CA przez klienta

Serwer udostępnia publiczny certyfikat CA w endpoincie `/ca.pem`. Polecenia z `client-setup` pobierają go przez `curl -k`, czyli bez weryfikacji połączenia TLS, i od razu dodają do zaufanych. Zaufanie jest więc **przy pierwszym użyciu** (trust on first use, TOFU): klient ufa CA z pierwszego pobrania, bez sprawdzenia, że pochodzi od właściwego serwera.

Opcjonalnie można ręcznie porównać odcisk SHA-256 pobranego CA z odciskiem z konsoli serwera:

```bash
# Na kliencie: pobierz CA i wypisz odcisk
curl -k https://HOST:8765/ca.pem > ~/ca.pem
openssl x509 -in ~/ca.pem -noout -fingerprint -sha256
# Na serwerze: odcisk CA (sudo omnibusmcp tls status)
```

Porównanie ma sens przede wszystkim w sieci, której nie ufasz, i tylko wtedy, gdy odczyt z serwera idzie zaufanym kanałem (konsola, sesja SSH). Szczegóły ryzyka: [Bezpieczeństwo](security.md).

Polecenie `omnibusmcp tls client-setup` (uruchomione na serwerze) wypisuje gotowe do skopiowania polecenia dla klienta:

```bash
sudo omnibusmcp tls client-setup --host 192.0.2.10
```

Wynik to instrukcja w trzech krokach, ta sama, którą serwer podaje pod adresem `/` (patrz [Podłączenie klienta](clients.md)):
1. **Zaufanie do certyfikatu serwera** — dwa warianty: dla klientów Node.js bez sudo (zapis CA do `~/.config/omnibusmcp/` i zmienna `NODE_EXTRA_CA_CERTS`) oraz dla reszty systemu z sudo (`update-ca-certificates` na Debian/Ubuntu, `update-ca-trust` na RHEL/AlmaLinux)
2. **Token** — skąd wziąć go na serwerze (`sudo cat /etc/omnibusmcp/token`) i jak udostępnić na kliencie przez zmienną z nazwą zależną od hosta (np. `OMNIBUS_TOKEN_192_0_2_10`)
3. **Claude Code** — polecenie `claude mcp add` z tokenem jako zmienną (nie jawnym tekstem)

## Strona z instrukcją pod adresem głównym

Pod `https://HOST:8765/` serwer zwraca bez tokenu instrukcję po polsku. Przeglądarka dostaje sformatowaną stronę HTML z przyciskami „Kopiuj” przy każdym poleceniu, a `curl -k https://HOST:8765/` czysty tekst (serwer wybiera format nagłówkiem `Accept`). Na początku jest adres MCP, a dalej te same kroki co w `omnibusmcp tls client-setup` (zaufanie do certyfikatu, token, Claude Code), z adresem hosta, pod którym klient się połączył. Adresy na stronie używają nazwy hosta, pod którą klient się połączył (nagłówek `Host`, odrzucany przy nietypowych znakach); ścieżka tokenu pochodzi z konfiguracji.

## Migracja ze starych certyfikatów self-signed

Jeśli serwer ma starszy certyfikat self-signed (O „OmnibusMCP self-signed"), przełącz na jednorazowy CA:

```bash
# Najpierw sprawdź status
sudo omnibusmcp tls status
# Jeśli pokazuje "Legacy self-signed certificate", uruchom:

sudo omnibusmcp tls renew --force
# Zachowuje te same nazwy (SAN), tworzy nowe CA.
# Następnie: sudo omnibusmcp tls client-setup (na serwerze) i wypisane polecenia na każdym kliencie.
```

## Certyfikaty z innego CA

Jeśli `tls.cert_file` i `tls.key_file` wskazują na certyfikat wydany przez inny CA (np. Let's Encrypt, wewnętrzne PKI), OmnibusMCP nigdy go nie zastępuje. Polecenie `tls renew` pokaże tylko wydawcę i datę wygaśnięcia:

```bash
sudo omnibusmcp tls renew
# Wynik: "certificate issued by <CN> ..., not by OmnibusMCP: not renewed here"
```

Odnowienie takich certyfikatów należy do Ciebie.

## Automatyczne odnawianie

Timer systemd `omnibusmcp-tls-renew.timer` (codziennie, ±1h random delay) automatycznie:
- Sprawdza, czy certyfikat wydany przez OmnibusMCP wygasa w ciągu 30 dni
- Jeśli tak → generuje nowy CA i certyfikat (SAN zachowywane), restartuje serwer
- Certyfikaty z innego CA — nie są dotykane
- Jeśli nie → silent no-op, brak restartu

```bash
# Sprawdź status timera
sudo systemctl status omnibusmcp-tls-renew.timer

# Ręczne odnawianie (jeśli wygasa <30 dni)
sudo omnibusmcp tls renew

# Wymuszenie renew (niezależnie od ważności)
sudo omnibusmcp tls renew --force

# Zmiana progu (domyślnie 30 dni; tutaj: 60 dni)
sudo omnibusmcp tls renew --before 60d
```

## Status certyfikatu

```bash
# Wypisz szczegóły: ważność, SAN, odcisk SHA-256 certyfikatu i CA
sudo omnibusmcp tls status

# Wynik: OK, WARN (<30 dni) lub EXPIRED (kod błędu 1)
```

## Wyłączenie TLS (powrót do plain HTTP)

```bash
# Dla nasłuchu na loopback (tunel SSH) — opcja --allow-insecure-remote nie potrzebna
sudo omnibusmcp tls disable

# Dla nasłuchu na interfejsie sieciowym — wymaga oświadczenia
sudo omnibusmcp tls disable --allow-insecure-remote

# Certyfikaty pozostają na dysku, konfiguracja YAML zmieniana (tls.* puste)
# Serwer restartuje się, nasłuchuje plain HTTP
```

## Health check `server/tls`

Narzędzie `health_summary` (wywoływane przez klienta MCP) zawiera wiersz `server/tls`:
- **OK**: Certyfikat ważny, podaje datę i dni do wygaśnięcia oraz „signed by the single-use local CA (renewal brings a new CA: clients trust it again)"
- **OK (certyfikat z innego CA)**: Wypisuje wydawcę i datę, brak wiadomości o odnowieniu (zarządzane zewnętrznie)
- **WARN**: Starszy certyfikat self-signed (legacy) — zalecane uruchomienie `omnibusmcp tls generate --force` + ponowna konfiguracja klientów
- **WARN**: <30 dni do wygaśnięcia (sprawdzaj timer `omnibusmcp-tls-renew`)
- **CRIT**: Wygasł lub nieczytelny — `omnibusmcp tls renew --force` lub sprawdzanie uprawnień
- **OK (bez TLS)**: Plain HTTP, notatka „disabled"; jeśli non-loopback — ostrzeżenie „token travels unencrypted"

## Wymóg TLS dla dostępu sieciowego

Serwer **nie uruchomi się** bez TLS przy dostępie sieciowym (listen != loopback):
- Brak `tls.cert_file` / `tls.key_file` → kod wyjścia **78** (EX_CONFIG), brak restartów w pętli
- Flaga `allow_insecure_remote: true` pozwala omijać wymóg (tylko dla tuneli bez dostępu do Internetu)
- Błąd widoczny w logu: `systemctl status omnibusmcp` lub `journalctl -u omnibusmcp -n 50`
