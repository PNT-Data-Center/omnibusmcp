# Podłączenie klienta (HTTPS na sieci)

Serwer musi nasłuchiwać na adresie sieciowym z włączonym TLS (`listen: <IP>:8765` oraz `omnibusmcp tls generate`, patrz [HTTPS / TLS](tls.md)). Endpoint MCP: `https://HOST:8765/mcp`, transport **Streamable HTTP**, uwierzytelnianie nagłówkiem `Authorization: Bearer <token>`.

## Przygotowanie klienta: `omnibusmcp tls client-setup`

Serwer ma certyfikat podpisany przez własne, jednorazowe CA (patrz [HTTPS / TLS](tls.md)). Klient musi raz zaufać temu CA i dostać token. Gotowe polecenia wypisuje serwer:

```bash
sudo omnibusmcp tls client-setup            # adres z listen; inny: --host host.example.com
```

Ta sama instrukcja jest pod adresem głównym serwera (`curl -k https://192.0.2.10:8765/` albo przeglądarka), bez tokenu i bez dostępu do serwera. Przeglądarka dostaje sformatowaną stronę z przyciskami „Kopiuj” przy każdym poleceniu; `curl` dostaje czysty tekst. Poniżej jest ten sam tekst, dla adresu `192.0.2.10`.

Polecenia pobierają CA bez sprawdzania odcisku (trust on first use). Zasady bezpieczeństwa i opcjonalne ręczne porównanie odcisku: [HTTPS / TLS](tls.md) i [Bezpieczeństwo](security.md).

### 1. Zaufanie do certyfikatu serwera

#### Gemini CLI i inne klienty Node.js (bez sudo)

```bash
d="$HOME/.config/omnibusmcp"; mkdir -p "$d"
curl -fsSk https://192.0.2.10:8765/ca.pem -o "$d/ca-192.0.2.10.pem"
cat "$d"/ca-*.pem > "$d/ca-bundle.pem"
```

W `~/.bashrc` wystarczy raz dodać (jedna linia dla wszystkich serwerów):

```bash
export NODE_EXTRA_CA_CERTS="$HOME/.config/omnibusmcp/ca-bundle.pem"
```

#### Claude Code, curl i reszta systemu (sudo)

Debian/Ubuntu:

```bash
sudo curl -fsSk https://192.0.2.10:8765/ca.pem -o /usr/local/share/ca-certificates/omnibusmcp-192.0.2.10.crt && sudo update-ca-certificates
```

RHEL/AlmaLinux:

```bash
sudo curl -fsSk https://192.0.2.10:8765/ca.pem -o /etc/pki/ca-trust/source/anchors/omnibusmcp-192.0.2.10.pem && sudo update-ca-trust
```

Po odnowieniu certyfikatu na serwerze powtórz te same polecenia.

### 2. Token

Na serwerze:

```bash
sudo cat /etc/omnibusmcp/token
```

Na kliencie zapisz go w `~/.config/omnibusmcp/192.0.2.10.token` (chmod 600) i dodaj do `~/.bashrc`:

```bash
export OMNIBUS_TOKEN_192_0_2_10="$(cat ~/.config/omnibusmcp/192.0.2.10.token)"
```

Bezpieczniejszy zapis tokena: wartość nie pojawia się na ekranie ani w historii powłoki (plik dostaje tryb 0600 od razu):

```bash
install -d -m 700 ~/.config/omnibusmcp
( umask 077; read -rsp 'Token: ' T && printf '%s\n' "$T" > ~/.config/omnibusmcp/192.0.2.10.token; echo )
```

### 3. Claude Code

```bash
claude mcp add --transport http --scope user 192-0-2-10 https://192.0.2.10:8765/mcp --header 'Authorization: Bearer ${OMNIBUS_TOKEN_192_0_2_10}'
```

Sprawdzenie:

```bash
claude mcp list
```

Szczegóły, w tym dlaczego nagłówek musi być w apostrofach, opisane są w sekcji [Claude Code](#claude-code) poniżej.

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

Gemini CLI działa na Node.js: zaufanie przez `NODE_EXTRA_CA_CERTS` (krok 1, „Gemini CLI i inne klienty Node.js”) albo systemowy magazyn, jeśli Node.js go używa w danej instalacji.

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

Zaufanie do CA: najpierw systemowy magazyn (krok 1, „Claude Code, curl i reszta systemu”); jeśli Cursor nadal odrzuca certyfikat, spróbuj uruchomić go z `NODE_EXTRA_CA_CERTS=~/.config/omnibusmcp/ca-bundle.pem` (krok 1, „Gemini CLI i inne klienty Node.js”).

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
| TLS | zaufanie do CA serwera (`https://HOST:8765/ca.pem`, polecenia z `omnibusmcp tls client-setup`): magazyn systemowy lub mechanizm klienta; minimum TLS 1.2 |
| Klient tylko stdio | most `mcp-remote` jak w sekcji Antigravity |

Po podłączeniu klient powinien widzieć 12 narzędzi przy samym module `linux` (11 narzędzi modułu i `health_summary`, Tier 1); kolejne moduły dodają swoje. Każde wywołanie jest widoczne na serwerze w `/var/log/omnibusmcp/audit.log` razem z adresem IP klienta.

## Po odnowieniu certyfikatu

Certyfikat serwera jest ważny 5 lat. Odnowienie — automatyczne (timer `omnibusmcp-tls-renew`, ok. 30 dni przed wygaśnięciem) lub ręczne (`sudo omnibusmcp tls renew --force`), a także zmiana nazw hosta (`tls generate --force --hosts …`) — tworzy **nowe CA**, bo klucz poprzedniego nie istnieje. Klienci przestają wtedy ufać serwerowi: uruchom ponownie `sudo omnibusmcp tls client-setup` na serwerze i wykonaj krok 1 na każdym kliencie. `health_summary` ostrzega (`server/tls: WARN`) 30 dni przed wygaśnięciem.

Certyfikatów wystawionych przez inne CA (np. firmowe) OmnibusMCP nie odnawia ani nie zastępuje.
