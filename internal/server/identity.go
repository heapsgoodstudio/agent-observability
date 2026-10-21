package server

import (
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"time"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
)

var uuidRe = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)

// MachineID takes the exec function as a parameter so tests never shell out.
func MachineID(run func(name string, args ...string) (string, error)) string {
	out, err := run("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	if err != nil {
		return ""
	}
	if m := uuidRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

func execRun(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

func identity() *claudeobsv1.Identity {
	host, _ := os.Hostname()
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return &claudeobsv1.Identity{
		MachineId:   MachineID(execRun),
		Hostname:    host,
		User:        name,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
}
