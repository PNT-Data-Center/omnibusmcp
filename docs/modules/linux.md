# Moduł linux

**Zawsze dostępny** — każdy system Linux.

**Narzędzia** (11 tools, Tier 1, read-only):

- `linux_host_overview` — Hostname, OS, kernel, uptime
- `linux_resources` — Load, CPU, memory, procesy
- `linux_disks` — Przestrzeń dysku, montowania (widok hosta z PID 1)
- `linux_network` — Interfejsy, routing, porty, DNS
- `linux_systemd_overview` — Stan systemd, usługi w błędzie
- `linux_service_status` — Status konkretnej usługi
- `linux_list_services` — Lista usług (filtrowalny stan)
- `linux_journal` — Logi systemd (filtry: unit, since/until, priority, grep, boot)
- `linux_read_file` — Odczyt pliku (allowlist: /etc, /var/log, /proc, /sys, /run; odmowa sekretów)
- `linux_list_dir` — Listowanie katalogu
- `linux_scheduled_jobs` — Zadania cykliczne: systemd timers i cron (sekcja timery: liczba, ostatnie uruchomienia i wyniki jednostek; daemon cron: aktywny/nieaktywny/niezainstalowany; pliki cron: /etc/crontab, /etc/cron.d/*, /etc/anacrontab z liczbą wpisów; katalogi /etc/cron.hourly|daily|weekly|monthly; crontaby użytkowników: tylko właściciel i liczba wpisów, zawartość nigdy nie wyświetlana — mogą zawierać poświadczenia; symblinki i pliki >1 MiB odrzucane)

**Health checks** (`health_summary`, kontrole `linux/*`):

- `host` — OS, kernel, uptime
- `systemd` — Stan ogółem, usługi w błędzie
- `filesystems` — Zajętość (WARN ≥85%, CRIT ≥95%); wyklucza read-only obrazy
- `memory` — Dostępna pamięć (WARN <10%, CRIT <5%)
- `load` — Obciążenie CPU (WARN ≥1.5 na CPU, CRIT ≥3.0)
- `time_sync` — Synchronizacja NTP (czytanie stanu z `timedatectl`, raport zawiera nazwę aktywnej usługi: chrony, systemd-timesyncd, ntpsec, ntp, ntpd, openntpd)
- `scheduler` — Liczba taski cyklicznych (timery + jednostki z nieudanymi ostatnimi uruchomieniami w kontekście; cron: OK gdy demon aktywny lub nie ma wpisów, WARN gdy demon nieaktywny ale są wpisy, lub demon niezainstalowany ale użytkownicy mają crontaby)
