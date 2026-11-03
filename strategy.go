package main

// Разбор стратегий из .bat-файлов Flowseal (zapret-discord-youtube).
// Аргументы winws.exe берутся из оригинальных файлов без изменений,
// подставляются только пути и порты игрового фильтра.

import (
	"embed"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

//go:embed assets
var assetsFS embed.FS

type Strategy struct {
	Name string   // например "general (ALT2)"
	Raw  []string // токены командной строки с переменными %BIN%, %LISTS% и т.д.
}

var numRe = regexp.MustCompile(`\d+`)

// naturalKey повторяет сортировку Flowseal: числа дополняются нулями до 8 знаков.
func naturalKey(s string) string {
	return numRe.ReplaceAllStringFunc(strings.ToLower(s), func(m string) string {
		return strings.Repeat("0", max(0, 8-len(m))) + m
	})
}

func LoadStrategies() ([]Strategy, error) {
	entries, err := fs.ReadDir(assetsFS, "assets/strategies")
	if err != nil {
		return nil, err
	}
	var out []Strategy
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".bat") {
			continue
		}
		data, err := assetsFS.ReadFile(path.Join("assets/strategies", e.Name()))
		if err != nil {
			return nil, err
		}
		toks := ParseBat(string(data))
		if len(toks) == 0 {
			continue
		}
		out = append(out, Strategy{Name: strings.TrimSuffix(e.Name(), ".bat"), Raw: toks})
	}
	sort.Slice(out, func(i, j int) bool { return naturalKey(out[i].Name) < naturalKey(out[j].Name) })
	return out, nil
}

// ParseBat достаёт аргументы winws.exe из строки запуска (с продолжениями через ^).
func ParseBat(src string) []string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(src, "\n")
	var buf strings.Builder
	capture := false
	for _, ln := range lines {
		if !capture {
			i := strings.Index(strings.ToLower(ln), `winws.exe"`)
			if i < 0 {
				continue
			}
			capture = true
			ln = ln[i+len(`winws.exe"`):]
		}
		t := strings.TrimRight(ln, " \t")
		cont := strings.HasSuffix(t, "^")
		if cont {
			t = strings.TrimSuffix(t, "^")
		}
		buf.WriteString(t)
		buf.WriteByte(' ')
		if !cont {
			break
		}
	}
	cmd := strings.ReplaceAll(buf.String(), "^!", "!")
	return tokenize(cmd)
}

// tokenize делит строку по пробелам вне кавычек и убирает сами кавычки.
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			have = true
		case (r == ' ' || r == '\t') && !inQ:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// GameFilter — то же поведение, что у Flowseal: выключенный фильтр = порт 12 (заглушка).
type GameFilter struct {
	Mode     string // disabled | all | tcp | udp
	TCPRange string
	UDPRange string
}

func (g GameFilter) Ports() (tcp, udp string) {
	tcp, udp = "12", "12"
	tr, ur := g.TCPRange, g.UDPRange
	if tr == "" {
		tr = "1024-65535"
	}
	if ur == "" {
		ur = "1024-65535"
	}
	switch g.Mode {
	case "all":
		tcp, udp = tr, ur
	case "tcp":
		tcp = tr
	case "udp":
		udp = ur
	}
	return
}

// BuildArgs подставляет реальные пути и порты.
func BuildArgs(s Strategy, binDir, listsDir string, gf GameFilter) []string {
	tcp, udp := gf.Ports()
	sep := `\`
	if strings.Contains(binDir, "/") && !strings.Contains(binDir, `\`) {
		sep = "/"
	}
	r := strings.NewReplacer(
		"%BIN%", strings.TrimRight(binDir, `\/`)+sep,
		"%LISTS%", strings.TrimRight(listsDir, `\/`)+sep,
		"%GameFilterTCP%", tcp,
		"%GameFilterUDP%", udp,
		"%GameFilter%", tcp,
	)
	out := make([]string, len(s.Raw))
	for i, t := range s.Raw {
		out[i] = r.Replace(t)
	}
	return out
}

// ValidPortRange проверяет формат вида 1024-1934,1936-65535.
func ValidPortRange(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if s == "" {
		return false
	}
	for _, item := range strings.Split(s, ",") {
		a, b, ok := strings.Cut(item, "-")
		if !ok {
			b = a
		}
		x, okx := port(a)
		y, oky := port(b)
		if !okx || !oky || x > y {
			return false
		}
	}
	return true
}

func port(s string) (int, bool) {
	if s == "" || len(s) > 5 || s[0] == '0' {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, n >= 1 && n <= 65535
}
