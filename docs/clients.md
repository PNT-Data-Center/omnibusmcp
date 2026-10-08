# Podłączenie klienta (HTTPS na sieci)

Serwer musi nasłuchiwać na adresie sieciowym z włączonym TLS (`listen: <IP>:8765` oraz `omnibusmcp tls generate`, patrz [HTTPS / TLS](tls.md)). Endpoint MCP: `https://HOST:8765/mcp`, transport **Streamable HTTP**, uwierzytelnianie nagłówkiem `Authorization: Bearer <token>`.

## Przygotowanie klienta: `omnibusmcp tls client-setup`

Serwer ma certyfikat podpisany przez własne, jednorazowe CA (patrz [HTTPS / TLS](tls.md)). Klient musi raz zaufać temu CA. Gotowe polecenia wypisuje serwer:

```bash
sudo omnibusmcp tls client-setup            # adres z listen; inny: --host host.example.com
```

Wynik zawiera odcisk SHA-256 CA i polecenia do wklejenia na kliencie. Kopiuj je **z konsoli serwera** (np. sesji SSH): CA jest pobierane przez jeszcze niezaufane połączenie (`curl -k`), więc bezpieczeństwo zapewnia wyłącznie porównanie z odciskiem wpisanym w polecenie. Przy niezgodności polecenie kończy się błędem `FINGERPRINT MISMATCH` i niczego nie dodaje.

Bez dostępu do serwera skróconą instrukcję pokazuje sam serwer pod adresem głównym — `curl -k https://192.0.2.10:8765/` albo przeglądarka (z wyjątkiem dla niezaufanego certyfikatu): adres MCP, adres certyfikatu CA (`/ca.pem`) do dodania do zaufanych, skąd wziąć token i przypomnienie o dodaniu serwera do agenta. Gotowe polecenia (poniżej) wypisuje `client-setup` na serwerze.

**1. Zaufanie do CA — systemowy magazyn** (Linux: Debian/Ubuntu, RHEL/AlmaLinux; wymaga `curl`, `openssl` oraz roota lub `sudo`). Działa dla Claude Code, `curl` i większości narzędzi. Przykład (adres 192.0.2.10, odcisk skrócony):

```bash
( set -e
  f=$(mktemp); trap 'rm -f "$f"' EXIT
  curl -fsSk https://192.0.2.10:8765/ca.pem -o "$f"
  fp=$(openssl x509 -in "$f" -noout -fingerprint -sha256 | cut -d= -f2)
  if [ "$fp" != "AA:BB:CC:…" ]; then echo "FINGERPRINT MISMATCH ($fp): CA NOT trusted" >&2; exit 1; fi
  s=sudo; [ "$(id -u)" -eq 0 ] && s=
  if [ -d /usr/local/share/ca-certificates ]; then
    $s install -m 0644 "$f" /usr/local/share/ca-certificates/omnibusmcp-192.0.2.10.crt && $s update-ca-certificates
  else
    $s install -m 0644 "$f" /etc/pki/ca-trust/source/anchors/omnibusmcp-192.0.2.10.pem && $s update-ca-trust
  fi
  echo "OK: OmnibusMCP CA of 192.0.2.10 is trusted" )
```

**1b. Zaufanie bez roota — tylko klienci oparci na Node.js** (Gemini CLI, most `mcp-remote`). Polecenie zapisuje CA w `~/.config/omnibusmcp/` i odbudowuje wspólny plik `ca-bundle.pem` (zmienna `NODE_EXTRA_CA_CERTS` przyjmuje jeden plik, a w pakiecie mieszczą się CA wielu serwerów); następnie w profilu powłoki:

```bash
export NODE_EXTRA_CA_CERTS=~/.config/omnibusmcp/ca-bundle.pem
```

Natywny Claude Code wymaga wariantu 1: certyfikatu self-signed nie przyjmuje w żadnej konfiguracji, a CA z `NODE_EXTRA_CA_CERTS` przyjmuje, ale magazyn systemowy działa bez dodatkowych zmiennych.

**2. Token.** Na serwerze: `sudo cat /etc/omnibusmcp/token`. Każdy serwer ma własny token, więc zmienna ma nazwę zależną od hosta (`OMNIBUS_TOKEN_` + adres lub nazwa wielkimi literami, znaki inne niż litery i cyfry zamienione na `_`). Na kliencie zapisz token do pliku 0600 bez umieszczania w historii powłoki i udostępniaj przez zmienną:

```bash
install -d -m 700 ~/.config/omnibusmcp
( umask 077; read -rsp 'Token: ' T && printf '%s\n' "$T" > ~/.config/omnibusmcp/192.0.2.10.token; echo )
export OMNIBUS_TOKEN_192_0_2_10="$(cat ~/.config/omnibusmcp/192.0.2.10.token)"   # np. w ~/.bashrc
```

## Claude Code

Polecenie z wyniku `client-setup` (nazwą serwera jest adres lub nazwa hosta; Claude Code dopuszcza w niej tylko litery, cyfry, `-` i `_`, dlatego kropki zamieniane są na `-`):

```bash
claude mcp add --transport http --scope user 192-0-2-10 https://192.0.2.10:8765/mcp \
  --header 'Authorization: Bearer ${OMNIBUS_TOKEN_192_0_2_10}'
```

Nagłówek musi być w **apostrofach**: wtedy w konfiguracji (`~/.claude.json` dla `--scope user`, `.mcp.json` dla `--scope project`) zapisuje się `${OMNIBUS_TOKEN_192_0_2_10}`, a wartość jest podstawiana przy uruchomieniu `claude`. W cudzysłowach powłoka wstawiłaby token jawnym tekstem. Sprawdzenie konfiguracji: `claude mcp get 192-0-2-10` (ma pokazywać `${OMNIBUS_TOKEN_192_0_2_10}`); połączenie: `claude mcp list` (`✔ Connected`; zły lub brakujący token: HTTP 401 „Server rejected the configured Authorization header”).

Ten sam wpis można zapisać ręcznie w `.mcp.json` projektu:

```json
{
  "mcpServers": {
    "omnibus": {
      "type": "http",
      "url": "https://HOST:8765/mcp",
      "headers": { "Authorization": "Bearer ${OMNIBUS_TOKEN_192_0_2_10}" }
    }
  }
}
```

## Gemini CLI

Gemini CLI działa na Node.js: zaufanie przez wariant 1b (`NODE_EXTRA_CA_CERTS`) albo systemowy magazyn, jeśli Node.js go używa w danej instalacji.

Plik `~/.gemini/settings.json` (dopisz do istniejącej konfiguracji):

```json
{
  "mcpServers": {
    "omnibus": {
      "httpUrl": "https://HOST:8765/mcp",
      "headers": { "Authorization": "Bearer $OMNIBUS_TOKEN_192_0_2_10" },
      "timeout": 60000
    }
  }
}
```

Polecenie (nagłówek w apostrofach):

```bash
gemini mcp add --transport http --scope user omnibus https://HOST:8765/mcp \
  --header 'Authorization: Bearer $OMNIBUS_TOKEN_192_0_2_10'
```

`--scope user` zapisuje do `~/.gemini/settings.json`. Weryfikacja: `gemini mcp list` lub w sesji `/mcp`.

## Cursor

**Nie testowano** z certyfikatem OmnibusMCP. Plik `~/.cursor/mcp.json` (globalnie) lub `.cursor/mcp.json` (w projekcie):

```json
{
  "mcpServers": {
    "omnibus": {
      "url": "https://HOST:8765/mcp",
      "headers": { "Authorization": "Bearer ${env:OMNIBUS_TOKEN_192_0_2_10}" }
    }
  }
}
```

Zaufanie do CA: najpierw wariant 1 (systemowy magazyn); jeśli Cursor nadal odrzuca certyfikat, spróbuj uruchomić go z `NODE_EXTRA_CA_CERTS=~/.config/omnibusmcp/ca-bundle.pem` (wariant 1b).

## Antigravity

Antigravity łączy się z serwerami zdalnymi tylko przez pole `serverUrl`, bez własnych nagłówków. Dlatego OmnibusMCP podłącza się przez most **stdio → HTTP** [`mcp-remote`](https://www.npmjs.com/package/mcp-remote) (wymaga Node.js), który dodaje nagłówek z tokenem. Plik `~/.gemini/config/mcp_config.json`:

```json
{
  "mcpServers": {
    "omnibus": {
      "command": "npx",
      "args": [
        "-y", "mcp-remote", "https://HOST:8765/mcp",
        "--header", "Authorization:${AUTH_HEADER}"
      ],
      "env": {
        "AUTH_HEADER": "Bearer <TOKEN>",
        "NODE_EXTRA_CA_CERTS": "/home/USER/.config/omnibusmcp/ca-bundle.pem"
      }
    }
  }
}
```

`<TOKEN>` zastąp zawartością `~/.config/omnibusmcp/HOST.token`, a plik konfiguracji zabezpiecz: `chmod 600 ~/.gemini/config/mcp_config.json`. `NODE_EXTRA_CA_CERTS` wymaga pełnej ścieżki (bez `~`). Weryfikacja: Additional Options (…) → MCP Servers.

## Każdy inny klient MCP

| Parametr | Wartość |
|---|---|
| Transport | **Streamable HTTP** (w konfiguracji często `http` / `streamable-http`); klienci obsługujący tylko starszy transport SSE się nie połączą |
| URL | `https://HOST:8765/mcp` |
| Nagłówek | `Authorization: Bearer <token>` (schemat `Bearer` bez rozróżniania wielkości liter) |
| TLS | zaufanie do CA serwera (`https://HOST:8765/ca.pem`, odcisk z `omnibusmcp tls client-setup`): magazyn systemowy lub mechanizm klienta; minimum TLS 1.2 |
| Klient tylko stdio | most `mcp-remote` jak w sekcji Antigravity |

Po podłączeniu klient powinien widzieć 12 narzędzi przy samym module `linux` (11 narzędzi modułu i `health_summary`, Tier 1); kolejne moduły dodają swoje. Każde wywołanie jest widoczne na serwerze w `/var/log/omnibusmcp/audit.log` razem z adresem IP klienta.

## Po odnowieniu certyfikatu

Certyfikat serwera jest ważny 5 lat. Odnowienie — automatyczne (timer `omnibusmcp-tls-renew`, ok. 30 dni przed wygaśnięciem) lub ręczne (`sudo omnibusmcp tls renew --force`), a także zmiana nazw hosta (`tls generate --force --hosts …`) — tworzy **nowe CA**, bo klucz poprzedniego nie istnieje. Klienci przestają wtedy ufać serwerowi: uruchom ponownie `sudo omnibusmcp tls client-setup` na serwerze i wykonaj krok 1 (lub 1b) na każdym kliencie. `health_summary` ostrzega (`server/tls: WARN`) 30 dni przed wygaśnięciem.

Certyfikatów wystawionych przez inne CA (np. firmowe) OmnibusMCP nie odnawia ani nie zastępuje.
