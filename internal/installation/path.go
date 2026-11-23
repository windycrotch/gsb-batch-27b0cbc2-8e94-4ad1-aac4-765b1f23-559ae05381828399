package installation

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// ReportPath checks the executable a fresh shell lookup will select. A child
// process cannot inspect or reset its parent shell's command cache or aliases.
func ReportPath(out io.Writer, installed string) {
	active, err := exec.LookPath("peep")
	if err != nil {
		fmt.Fprintf(out, "Add %s to the front of PATH to run this installation.\n", filepath.Dir(installed))
	} else {
		current, currentErr := os.Stat(active)
		updated, updatedErr := os.Stat(installed)
		if currentErr == nil && updatedErr == nil && os.SameFile(current, updated) {
			fmt.Fprintf(out, "PATH selects the installed executable: %s\n", active)
		} else {
			fmt.Fprintf(out, "WARNING: PATH still selects %s instead of the new installation.\nPut %s first in PATH, or upgrade the other copy using its original installation method.\n", active, filepath.Dir(installed))
		}
	}
	fmt.Fprintln(out, "If your shell still runs an older copy, run hash -r (Bash) or rehash (Zsh), then type -a peep.")
}
