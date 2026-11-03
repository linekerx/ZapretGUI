package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStrategies(t *testing.T) {
	ss, err := LoadStrategies()
	if err != nil || len(ss) == 0 {
		t.Fatalf("no strategies: %v", err)
	}
	bin, _ := filepath.Abs("assets/bin")
	lists, _ := filepath.Abs("assets/lists")
	gf := GameFilter{Mode: "all"}
	for _, s := range ss {
		args := BuildArgs(s, bin, lists, gf)
		if !strings.HasPrefix(args[0], "--wf-tcp=") {
			t.Errorf("%s: first arg %q", s.Name, args[0])
		}
		for _, a := range args {
			if strings.ContainsAny(a, `%"^`) {
				t.Errorf("%s: leftover chars in %q", s.Name, a)
			}
			if _, v, ok := strings.Cut(a, "="); ok && (strings.HasPrefix(v, bin) || strings.HasPrefix(v, lists)) {
				v = strings.TrimPrefix(v, "@")
				base := filepath.Base(v)
				if strings.HasSuffix(base, "-user.txt") || base == "ipset-all.txt" {
					continue // создаются при запуске
				}
				if _, err := os.Stat(v); err != nil {
					t.Errorf("%s: missing file %s", s.Name, v)
				}
			}
		}
		t.Logf("%-28s %d args", s.Name, len(args))
	}
	t.Log(strings.Join(BuildArgs(ss[len(ss)-1], `C:\ProgramData\ZapretGUI\bin`, `C:\ProgramData\ZapretGUI\lists`, GameFilter{})[:6], "\n"))
	if !ValidPortRange("1024-1934,1936-65535") || ValidPortRange("0-5") || ValidPortRange("70000") || ValidPortRange("5-3") {
		t.Error("port validation")
	}
}
