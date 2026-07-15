package cli

import (
	"os"
	"os/exec"
	"runtime"
)

// openCmd returns the platform launcher argv for a file path.
func openCmd(goos, path string) []string {
	switch goos {
	case "darwin":
		return []string{"open", path}
	case "windows":
		return []string{"rundll32", "url.dll,FileProtocolHandler", path}
	default: // linux and the BSDs
		return []string{"xdg-open", path}
	}
}

// openBrowser opens path with the platform default handler. Failures are
// deliberately silent: the report path is already printed in the summary,
// and a machine without a browser should not turn a successful scan red.
func openBrowser(path string) {
	argv := openCmd(runtime.GOOS, path)
	_ = exec.Command(argv[0], argv[1:]...).Start()
}

// isTerminal reports whether f is attached to a terminal, so the report is
// only auto-opened for interactive runs and never from CI or pipes.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
