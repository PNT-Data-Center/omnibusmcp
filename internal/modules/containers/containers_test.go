package containers

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

const psFixture = `{"ID":"aaaaaaaaaaaaaaaa","Names":"web","Image":"nginx:alpine","State":"running","Status":"Up 2 hours (healthy)","HealthStatus":"healthy","Ports":"0.0.0.0:8080->80/tcp","Labels":"com.docker.compose.project=shop,com.docker.compose.service=web"}
{"ID":"bbbbbbbbbbbbbbbb","Names":"worker","Image":"alpine","State":"restarting","Status":"Restarting (1) 5 seconds ago","Labels":""}
{"ID":"cccccccccccccccc","Names":"api","Image":"api:1","State":"running","Status":"Up 10 minutes (unhealthy)","Labels":""}
{"ID":"dddddddddddddddd","Names":"batch","Image":"alpine","State":"exited","Status":"Exited (0) 1 hour ago","Labels":""}
{"ID":"eeeeeeeeeeeeeeee","Names":"hog","Image":"alpine","State":"exited","Status":"Exited (137) 3 minutes ago","Labels":""}
{"ID":"ffffffffffffffff","Names":"stopped","Image":"redis","State":"exited","Status":"Exited (143) 1 day ago","Labels":""}
`

func loadPS(t *testing.T) []psEntry {
	t.Helper()
	ps, err := jsonLines[psEntry](psFixture)
	if err != nil || len(ps) != 6 {
		t.Fatalf("%v %d", err, len(ps))
	}
	return ps
}

func TestPsEntryHelpers(t *testing.T) {
	ps := loadPS(t)
	if ps[0].health() != "healthy" || ps[2].health() != "unhealthy" || ps[3].health() != "" {
		t.Fatalf("health: %q %q %q", ps[0].health(), ps[2].health(), ps[3].health())
	}
	if ps[0].compose() != "shop/web" || ps[1].compose() != "" {
		t.Fatal("compose label")
	}
	if code, ok := ps[4].exitCode(); !ok || code != 137 {
		t.Fatalf("exit code %d %v", code, ok)
	}
	if _, ok := ps[0].exitCode(); ok {
		t.Fatal("running container has exit code")
	}
}

func TestEvalContainers(t *testing.T) {
	c := evalContainers(loadPS(t), nil)
	if c.Status != registry.Crit {
		t.Fatalf("restarting must be CRIT: %+v", c)
	}
	for _, want := range []string{"2 running", "3 stopped", "1 healthy", "worker restarting", "api unhealthy", "hog exited (137)"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("missing %q in %q", want, c.Detail)
		}
	}
	for _, clean := range []string{"batch exited", "stopped exited"} {
		if strings.Contains(c.Detail, clean) {
			t.Errorf("clean exit reported: %q", c.Detail)
		}
	}
	ok := evalContainers(loadPS(t)[:1], nil)
	if ok.Status != registry.OK || ok.Detail != "1 running, 0 stopped, 1 healthy" {
		t.Fatalf("%+v", ok)
	}
}

func TestEvalEvents(t *testing.T) {
	lines := ""
	for i := 0; i < 4; i++ {
		lines += `{"Type":"container","Action":"die","Actor":{"ID":"b","Attributes":{"name":"worker","exitCode":"1"}},"time":1}` + "\n"
	}
	lines += `{"Type":"container","Action":"die","Actor":{"ID":"s","Attributes":{"name":"stopped","exitCode":"143"}},"time":2}` + "\n"
	lines += `{"Type":"container","Action":"oom","Actor":{"ID":"h","Attributes":{"name":"hog"}},"time":3}` + "\n"
	lines += `{"Type":"container","Action":"die","Actor":{"ID":"h","Attributes":{"name":"hog","exitCode":"137"}},"time":3}` + "\n"
	ev, err := jsonLines[event](lines)
	if err != nil {
		t.Fatal(err)
	}
	c := evalEvents(ev)
	for _, want := range []string{"OOM kill: hog", "worker crash loop (4 failed exits)", "hog 1 failed exit(s)"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("missing %q in %q", want, c.Detail)
		}
	}
	if c.Status != registry.Warn || strings.Contains(c.Detail, "stopped") {
		t.Fatalf("%+v", c)
	}
	if c := evalEvents(nil); c.Status != registry.OK {
		t.Fatalf("%+v", c)
	}
	out := renderEvents(ev, time.Hour)
	for _, want := range []string{"7 event(s) in the last 1h:", "die=6", "oom=1", "exit code 1", "worker"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRedactEnv(t *testing.T) {
	cases := map[string]string{
		"DB_PASSWORD=hunter2":      "DB_PASSWORD=*** (redacted)",
		"API_TOKEN=abc":            "API_TOKEN=*** (redacted)",
		"AWS_SECRET_ACCESS_KEY=x":  "AWS_SECRET_ACCESS_KEY=*** (redacted)",
		"PATH=/usr/bin":            "PATH=/usr/bin",
		"NGINX_VERSION=1.27":       "NGINX_VERSION=1.27",
		"BASIC_AUTH=user:pass":     "BASIC_AUTH=*** (redacted)",
		"MALFORMED_NO_EQUALS_SIGN": "MALFORMED_NO_EQUALS_SIGN",
	}
	for in, want := range cases {
		if got := redactEnv(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestRenderInspect(t *testing.T) {
	raw := `[{"Id":"0123456789abcdef","Name":"/hog","State":{"Status":"exited","Running":false,"OOMKilled":true,"ExitCode":137,"StartedAt":"s","FinishedAt":"f",
	  "Health":{"Status":"unhealthy","FailingStreak":3,"Log":[{"Start":"t1","ExitCode":1,"Output":"curl: (7) Failed\nto connect"}]}},
	  "RestartCount":5,"Config":{"Image":"alpine","Cmd":["sh","-c","x"],"Env":["DB_PASSWORD=hunter2","PATH=/bin"],"Labels":{"a":"b"},
	  "Healthcheck":{"Test":["CMD","curl","-f","http://localhost"],"Interval":30000000000,"Retries":3}},
	  "HostConfig":{"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":5},"Memory":10485760,"NanoCpus":500000000,"NetworkMode":"bridge","LogConfig":{"Type":"json-file"}},
	  "Mounts":[{"Type":"volume","Name":"data","Source":"/var/lib/docker/volumes/data/_data","Destination":"/data","RW":true}],
	  "NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"8080"}]},"Networks":{"bridge":{"IPAddress":"172.17.0.5","Gateway":"172.17.0.1"}}}}]`
	var list []inspectInfo
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	out := renderInspect(list[0])
	for _, want := range []string{"Container:  hog (0123456789ab)", "exit code 137", "OOM KILLED", "Restarts:   5 (policy: on-failure, max 5)",
		"memory 10 MiB, CPUs 0.50", "Health:     unhealthy (failing streak 3)", "every 30s, retries 3", "curl: (7) Failed | to connect",
		"bridge: ip 172.17.0.5", "0.0.0.0:8080->80/tcp", "volume data -> /data (volume, rw)", "DB_PASSWORD=*** (redacted)", "PATH=/bin"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") {
		t.Fatal("secret leaked")
	}
}

func TestGrepLogs(t *testing.T) {
	stdout := "2026-09-30T10:00:01Z GET / 200\n2026-09-30T10:00:03Z GET /x 500\n"
	stderr := "2026-09-30T10:00:02Z error: upstream timed out\n2026-09-30T10:00:04Z ERROR: db down\n"
	out := grepLogs(stdout, stderr, regexp.MustCompile("(?i)error|500"), 2, "web")
	if !strings.Contains(out, "3 matching line(s)") || !strings.Contains(out, "GET /x 500  [stdout]") || !strings.HasSuffix(out, "db down  [stderr]\n") || strings.Contains(out, "upstream") {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(grepLogs("a\n", "", regexp.MustCompile("(?i)zzz"), 5, "web"), `No log lines of web match "zzz"`) {
		t.Fatal("no-match message")
	}
}

func TestValidation(t *testing.T) {
	for _, ok := range []string{"web", "shop-web-1", "a1b2c3d4e5f6", "my_app.v2"} {
		if validContainer(ok) != nil {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "-f", "--all", "a b", "a;b", "../x", "$(id)"} {
		if validContainer(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if d, err := sinceDuration("", time.Hour, 24*time.Hour); err != nil || d != time.Hour {
		t.Fatal("default since")
	}
	if _, err := sinceDuration("2d", 0, 24*time.Hour); err == nil {
		t.Fatal("max since not enforced")
	}
	if _, err := sinceDuration("1w", 0, 0); err == nil {
		t.Fatal("bad unit accepted")
	}
}

func TestRenderRuntimeAndLists(t *testing.T) {
	var info map[string]any
	json.Unmarshal([]byte(`{"ServerVersion":"29.8.1","OperatingSystem":"Debian","Containers":3,"ContainersRunning":2,"ContainersPaused":0,"ContainersStopped":1,"Driver":"overlayfs","Warnings":["No swap limit support"]}`), &info)
	out := renderRuntime(info)
	for _, want := range []string{"Docker 29.8.1", "3 total, 2 running", "overlayfs", "- No swap limit support"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	list := renderList(loadPS(t), "")
	if !strings.Contains(list, "6 container(s)") || !strings.Contains(list, "shop/web") || !strings.Contains(list, "aaaaaaaaaaaa ") && !strings.HasSuffix(strings.TrimSpace(list), "ffffffffffff") {
		t.Fatalf("%s", list)
	}
	if !strings.Contains(renderList(loadPS(t), "WOR"), "1 container(s)") {
		t.Fatal("name filter")
	}
	df := `{"Type":"Images","TotalCount":"2","Active":"1","Size":"250MB","Reclaimable":"10MB (4%)"}`
	imgs := `{"Repository":"nginx","Tag":"alpine","ID":"sha256:1234567890abcdef","Size":"50MB","CreatedSince":"2 days ago","Containers":"1"}
{"Repository":"<none>","Tag":"<none>","ID":"sha256:fedcba0987654321","Size":"5MB","CreatedSince":"3 weeks ago","Containers":"0"}`
	du := renderDiskUsage(df, imgs, nil, "", nil)
	for _, want := range []string{"Images", "10MB (4%)", "nginx:alpine", "1234567890ab", "2 image(s), 1 dangling", "Volumes:\n  none"} {
		if !strings.Contains(du, want) {
			t.Errorf("missing %q in:\n%s", want, du)
		}
	}
}

func TestHumanPeriod(t *testing.T) {
	cases := map[time.Duration]string{7 * 24 * time.Hour: "7d", time.Hour: "1h", 30 * time.Minute: "30m", 90 * time.Minute: "1h30m", 45 * time.Second: "45s"}
	for d, want := range cases {
		if got := humanPeriod(d); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
}

func TestEvalContainersWithInspect(t *testing.T) {
	ps := loadPS(t)
	insp := map[string]inspectInfo{}
	var hog, worker, web inspectInfo
	hog.State.OOMKilled = true
	worker.RestartCount = 11
	web.RestartCount = 4
	insp[ps[4].ID], insp[ps[1].ID], insp[ps[0].ID] = hog, worker, web
	c := evalContainers(ps, insp)
	for _, want := range []string{"hog OOM-killed (exit 137)", "worker restarting (restarted 11 times)", "web (restarted 4 times)"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("missing %q in %q", want, c.Detail)
		}
	}
	if strings.Contains(c.Detail, "hog exited (137)") || strings.Count(c.Detail, "restarted 11") != 1 {
		t.Errorf("duplicate reporting: %q", c.Detail)
	}
}
