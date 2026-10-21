package installer

import "os/exec"

// Exec is the seam between logic and machine: everything that shells out
// (launchctl, lsof, plutil, ioreg, script runs) goes through it so tests
// never touch the real system.
type Exec interface {
	Run(name string, args ...string) (string, error)
}

type RealExec struct{}

func (RealExec) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
