package main

// Проверка доступности и задержки сервисов (по мотивам utils/test zapret.ps1 Flowseal):
// HTTPS-запросы с TLS 1.2/1.3 и докачка ~48 КБ (ловит «заморозку» на 16–20 КБ).

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

type Target struct {
	Group string // youtube | discord | google
	Name  string
	URL   string
}

var scanTargets = []Target{
	{"youtube", "YouTube", "https://www.youtube.com"},
	{"youtube", "Картинки YouTube", "https://i.ytimg.com"},
	{"youtube", "Видео-серверы", "https://redirector.googlevideo.com"},
	{"youtube", "youtu.be", "https://youtu.be"},
	{"discord", "Discord", "https://discord.com"},
	{"discord", "Gateway", "https://gateway.discord.gg"},
	{"discord", "CDN", "https://cdn.discordapp.com"},
	{"discord", "Обновления", "https://updates.discord.com"},
}

var pingTargets = []Target{
	{"youtube", "YouTube", "https://www.youtube.com"},
	{"discord", "Discord", "https://discord.com"},
	{"google", "Google", "https://www.google.com"},
}

type Check struct {
	Group string `json:"group"`
	Name  string `json:"name"`
	Kind  string `json:"kind"` // tls12 | tls13 | load
	OK    bool   `json:"ok"`
	Ms    int    `json:"ms"`
}

type GroupStat struct {
	OK    int `json:"ok"`
	Total int `json:"total"`
	Ms    int `json:"ms"` // средняя задержка успешных проверок
}

type ScanResult struct {
	Strategy string               `json:"strategy"`
	OK       int                  `json:"ok"`
	Total    int                  `json:"total"`
	Ms       int                  `json:"ms"`
	Groups   map[string]GroupStat `json:"groups"`
	Checks   []Check              `json:"checks"`
	Baseline bool                 `json:"baseline,omitempty"`
}

func newClient(minV, maxV uint16, timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 3 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil, // системный прокси не должен влиять на тест
			DialContext:         d.DialContext,
			TLSClientConfig:     &tls.Config{MinVersion: minV, MaxVersion: maxV},
			TLSHandshakeTimeout: timeout,
			DisableKeepAlives:   true,
			ForceAttemptHTTP2:   false,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// probeHead — время до получения ответа сервера (TCP + TLS + HTTP), мс.
func probeHead(url string, ver uint16, timeout time.Duration) (bool, int) {
	c := newClient(ver, ver, timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	t := time.Now()
	resp, err := c.Do(req)
	ms := int(time.Since(t).Milliseconds())
	if err != nil {
		return false, ms
	}
	resp.Body.Close()
	return true, ms
}

// probeLoad — скачивает до 48 КБ; DPI, режущий соединение после 16–20 КБ, даст ошибку.
func probeLoad(url string, timeout time.Duration) (bool, int) {
	c := newClient(tls.VersionTLS12, tls.VersionTLS13, timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36")
	t := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return false, int(time.Since(t).Milliseconds())
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 48<<10))
	ms := int(time.Since(t).Milliseconds())
	if err != nil && n < 48<<10 {
		return false, ms
	}
	return true, ms
}

// RunChecks выполняет набор проверок параллельно. full=true — TLS 1.2 и 1.3 для каждого адреса.
func RunChecks(full bool) []Check {
	const to = 4 * time.Second
	type job struct {
		t    Target
		kind string
	}
	var jobs []job
	for _, t := range scanTargets {
		jobs = append(jobs, job{t, "tls13"})
		if full {
			jobs = append(jobs, job{t, "tls12"})
		}
	}
	jobs = append(jobs, job{scanTargets[0], "load"}, job{scanTargets[4], "load"})

	out := make([]Check, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			var ok bool
			var ms int
			switch j.kind {
			case "tls12":
				ok, ms = probeHead(j.t.URL, tls.VersionTLS12, to)
			case "tls13":
				ok, ms = probeHead(j.t.URL, tls.VersionTLS13, to)
			default:
				ok, ms = probeLoad(j.t.URL, 6*time.Second)
			}
			out[i] = Check{Group: j.t.Group, Name: j.t.Name, Kind: j.kind, OK: ok, Ms: ms}
		}(i, j)
	}
	wg.Wait()
	return out
}

func Summarize(name string, checks []Check) ScanResult {
	r := ScanResult{Strategy: name, Groups: map[string]GroupStat{}, Checks: checks}
	sum := map[string]int{}
	tot, totMs := 0, 0
	for _, c := range checks {
		g := r.Groups[c.Group]
		g.Total++
		r.Total++
		if c.OK {
			g.OK++
			r.OK++
			if c.Kind != "load" {
				sum[c.Group] += c.Ms
				tot++
				totMs += c.Ms
			}
		}
		r.Groups[c.Group] = g
	}
	for k, g := range r.Groups {
		n := 0
		for _, c := range checks {
			if c.Group == k && c.OK && c.Kind != "load" {
				n++
			}
		}
		if n > 0 {
			g.Ms = sum[k] / n
		}
		r.Groups[k] = g
	}
	if tot > 0 {
		r.Ms = totMs / tot
	}
	return r
}

// SortResults: больше успешных проверок — выше; при равенстве — меньше задержка.
func SortResults(rs []ScanResult) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Baseline != rs[j].Baseline {
			return !rs[i].Baseline
		}
		if rs[i].OK != rs[j].OK {
			return rs[i].OK > rs[j].OK
		}
		if rs[i].Ms == 0 || rs[j].Ms == 0 {
			return rs[i].Ms != 0
		}
		return rs[i].Ms < rs[j].Ms
	})
}

type PingResult struct {
	Group string `json:"group"`
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Ms    int    `json:"ms"`
}

// QuickPing — быстрая проверка текущего состояния (без смены стратегии).
func QuickPing() []PingResult {
	out := make([]PingResult, len(pingTargets))
	var wg sync.WaitGroup
	for i, t := range pingTargets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			// два замера, берём лучший — первый часто включает DNS
			ok1, ms1 := probeHead(t.URL, tls.VersionTLS13, 4*time.Second)
			ok2, ms2 := probeHead(t.URL, tls.VersionTLS13, 4*time.Second)
			ok, ms := ok1 || ok2, ms1
			if ok2 && (!ok1 || ms2 < ms1) {
				ms = ms2
			}
			out[i] = PingResult{Group: t.Group, Name: t.Name, OK: ok, Ms: ms}
		}(i, t)
	}
	wg.Wait()
	return out
}
