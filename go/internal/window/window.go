// Package window opens the dashboard in a chromeless app window (Chrome / Edge /
// Chromium with its own profile), falling back to the default browser. It is the
// Go equivalent of cli.py's `dashboard --app`.
package window

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// IsRunning reports whether a dashboard already answers /healthz at base (e.g. http://127.0.0.1:8191/).
// A 503 still counts: the process is up.
func IsRunning(base string) bool {
	c := http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(base + "healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200 || resp.StatusCode == 503
}

func candidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		}
	case "windows":
		var out []string
		for _, env := range []string{"PROGRAMFILES", "PROGRAMFILES(X86)", "LOCALAPPDATA"} {
			if b := os.Getenv(env); b != "" {
				out = append(out,
					filepath.Join(b, "Google", "Chrome", "Application", "chrome.exe"),
					filepath.Join(b, "Microsoft", "Edge", "Application", "msedge.exe"),
					filepath.Join(b, "Chromium", "Application", "chrome.exe"))
			}
		}
		return out
	}
	return nil
}

// FindChromium returns a Chromium-family executable, or "".
func FindChromium() string {
	for _, c := range candidates() {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "microsoft-edge-stable", "brave-browser"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// OpenBrowser opens url in the system default browser.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// AppProfileDir is the dedicated --user-data-dir (own dock/taskbar entry, never touches the user's profile).
func AppProfileDir() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude", "token-dashboard-app")
}

// OpenApp opens url as a chromeless app window; returns the way it was opened ("app" or "browser").
func OpenApp(url string) (string, error) {
	exe := FindChromium()
	if exe == "" {
		return "browser", OpenBrowser(url)
	}
	cmd := exec.Command(exe, "--app="+url, "--user-data-dir="+AppProfileDir(), "--window-size=1440,900")
	if err := cmd.Start(); err != nil {
		return "browser", OpenBrowser(url)
	}
	go cmd.Wait() // reap; the window outlives us if the server stops
	return "app", nil
}
