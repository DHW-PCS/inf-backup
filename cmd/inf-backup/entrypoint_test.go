package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func shellQuote(text string) string { return "'" + strings.ReplaceAll(text, "'", "'\\''") + "'" }

func TestEntrypointAlwaysExecutesGoAndIgnoresPanelStartup(t *testing.T) {
	source, err := os.ReadFile("../../entrypoint-posix.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "exec /usr/local/bin/inf-backup --config /home/container/config.yml") {
		t.Fatal("entrypoint no longer starts the fixed Go backend")
	}
	for _, startup := range []string{"", "python3 /app/app.py", "does-not-exist {{PLACEHOLDER}}", "$(touch \"$INF_ENTRYPOINT_INJECTION\") startup-placeholder-secret"} {
		t.Run(startup, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "work")
			if err := os.Mkdir(work, 0700); err != nil {
				t.Fatal(err)
			}
			backend := filepath.Join(root, "offline-backend")
			record := filepath.Join(root, "record")
			stub := "#!/bin/sh\npwd > \"$INF_ENTRYPOINT_RECORD\"\nprintf '%s\\n' \"$@\" \"$TZ\" >> \"$INF_ENTRYPOINT_RECORD\"\nexit 7\n"
			if err := os.WriteFile(backend, []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			// Redirect only fixed filesystem paths to isolated local fixtures.
			// No container, server data, real configuration or real backend is used.
			script := strings.NewReplacer("/usr/local/bin/inf-backup", shellQuote(backend), "/home/container", shellQuote(work)).Replace(string(source))
			scriptPath := filepath.Join(root, "entrypoint.sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			ip := filepath.Join(root, "ip")
			if err := os.WriteFile(ip, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			injection := filepath.Join(root, "must-not-exist")
			cmd := exec.Command("sh", scriptPath, "python3", "/app/app.py")
			cmd.Env = append(os.Environ(), "STARTUP="+startup, "TZ=Asia/Taipei", "PATH="+root+":"+os.Getenv("PATH"), "INF_ENTRYPOINT_RECORD="+record, "INF_ENTRYPOINT_INJECTION="+injection)
			output, err := cmd.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 7 {
				t.Fatalf("entrypoint did not exec backend: %v %s", err, output)
			}
			data, err := os.ReadFile(record)
			if err != nil || string(data) != work+"\n--config\n"+work+"/config.yml\nAsia/Taipei\n" {
				t.Fatalf("wrong backend directory, arguments or timezone: %v %q", err, data)
			}
			if _, err := os.Stat(injection); !os.IsNotExist(err) {
				t.Fatal("panel startup was evaluated")
			}
			if strings.Contains(string(output), "startup-placeholder-secret") || strings.Contains(string(output), "python3") {
				t.Fatal("panel startup was printed")
			}
		})
	}
}
