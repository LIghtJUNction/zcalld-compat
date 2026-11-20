// Extracted from upstream proc.Command. The standalone entry refuses WSL,
// so it does not load Magpie settings or discover WSL installations.
package proc

import "os/exec"

func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	batch(cmd)
	hide(cmd)
	return cmd
}
