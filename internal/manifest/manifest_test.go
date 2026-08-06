package manifest

import (
	"strings"
	"testing"
	"time"
)

const valid = `
[frontend]
path  = "web"
build = [["npm", "ci"], ["npm", "run", "build"]]
dist  = "dist"

[backend]
path        = "."
exclude     = ["docs/", "*.md"]
build       = [["go", "build", "-o", "bin/server", "."]]
run         = ["./bin/server", "--addr", ":{port}", "--data-dir", "{state_dir}"]
health_path = "/api/health"
`

func TestParseValid(t *testing.T) {
	m, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if m.Frontend.Path != "web" || m.Frontend.Dist != "dist" || m.Backend.Path != "." {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if time.Duration(m.Backend.StartTimeout) != DefaultStartTimeout {
		t.Fatalf("start_timeout default = %v", m.Backend.StartTimeout)
	}
	if time.Duration(m.Backend.IdleTimeout) != DefaultIdleTimeout {
		t.Fatalf("idle_timeout default = %v", m.Backend.IdleTimeout)
	}
}

func TestParseTimeouts(t *testing.T) {
	src := valid + "\nstart_timeout = \"5s\"\nidle_timeout = \"1h\"\n"
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(m.Backend.StartTimeout) != 5*time.Second {
		t.Fatalf("start_timeout = %v, want 5s", m.Backend.StartTimeout)
	}
	if time.Duration(m.Backend.IdleTimeout) != time.Hour {
		t.Fatalf("idle_timeout = %v, want 1h", m.Backend.IdleTimeout)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct {
		mutate func(string) string
		want   string
	}{
		"unknown key": {
			func(s string) string { return s + "\ntypo_key = true\n" },
			"unknown keys",
		},
		"missing frontend path": {
			func(s string) string { return strings.Replace(s, `path  = "web"`, "", 1) },
			"frontend.path is required",
		},
		"absolute path": {
			func(s string) string { return strings.Replace(s, `path  = "web"`, `path = "/web"`, 1) },
			"relative path",
		},
		"escaping path": {
			func(s string) string { return strings.Replace(s, `path  = "web"`, `path = "../web"`, 1) },
			"relative path",
		},
		"missing run": {
			func(s string) string {
				return strings.Replace(s, `run         = ["./bin/server", "--addr", ":{port}", "--data-dir", "{state_dir}"]`, "", 1)
			},
			"backend.run is required",
		},
		"bad health path": {
			func(s string) string { return strings.Replace(s, `health_path = "/api/health"`, `health_path = "api/health"`, 1) },
			"health_path",
		},
		"empty build step": {
			func(s string) string {
				return strings.Replace(s, `build       = [["go", "build", "-o", "bin/server", "."]]`, `build = [[]]`, 1)
			},
			"non-empty argv",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.mutate(valid)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}
