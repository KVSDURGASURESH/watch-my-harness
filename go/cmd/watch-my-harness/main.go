// Command watch-my-harness is the single-binary build of WATCH MY HARNESS:
// scan agent session logs into SQLite and serve the analytics UI.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tokendashboard "github.com/KVSDURGASURESH/watch-my-harness"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/scanner"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/server"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/sources"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/tips"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/window"
)

const usage = `WATCH MY HARNESS — local analytics for AI-agent token usage

Usage: watch-my-harness [command] [flags]

Commands:
  dashboard   start the server and open the UI (default; opens an app window)
  scan        scan session logs into the database and exit
  today       token totals for today (UTC)
  stats       all-time token totals
  tips        print token-saving suggestions
  recommend   print a draft CLAUDE.md recommendations block
  licence     print the licence and third-party notices
  version     print the version

Flags (any command):
  --db PATH               SQLite cache (default ~/.claude/token-dashboard.db, or $TOKEN_DASHBOARD_DB)
  --projects-dir PATH     Claude Code transcripts (default ~/.claude/projects); pins sources to exactly this
  --source NAME=KIND:PATH extra log source, repeatable; kinds: claude, codex, hermes, generic
  --codex                 shorthand for --source codex=codex:~/.codex/sessions
Dashboard flags:
  --app                   chromeless app window, reusing a running instance (default with no command)
  --no-open               do not open any window or browser
  --no-scan               serve without the background scanner
  --host H --port N       listen address (default 127.0.0.1:8191, or $HOST / $PORT)
`

type opts struct {
	cmd         string
	db          string
	projectsDir string
	sources     []string
	codex       bool
	app, noOpen bool
	noScan      bool
	host        string
	port        int
	explicit    bool // user chose sources/db on the command line: skip the setup gate
}

func parseArgs(args []string) (*opts, error) {
	o := &opts{host: envOr("HOST", "127.0.0.1")}
	o.port, _ = strconv.Atoi(envOr("PORT", "8191"))
	val := func(i *int, name string) (string, error) {
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, inline, hasInline := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") {
			name, hasInline = a, false
		}
		get := func() (string, error) {
			if hasInline {
				return inline, nil
			}
			return val(&i, name)
		}
		var err error
		var v string
		switch name {
		case "--db":
			o.db, err = get()
		case "--projects-dir":
			o.projectsDir, err = get()
		case "--source":
			if v, err = get(); err == nil {
				o.sources = append(o.sources, v)
			}
		case "--host":
			o.host, err = get()
		case "--port":
			if v, err = get(); err == nil {
				o.port, err = strconv.Atoi(v)
			}
		case "--codex":
			o.codex = true
		case "--app":
			o.app = true
		case "--no-open":
			o.noOpen = true
		case "--no-scan":
			o.noScan = true
		case "-h", "--help", "help":
			o.cmd = "help"
		case "-v", "--version":
			o.cmd = "version"
		default:
			if strings.HasPrefix(a, "-") || o.cmd != "" {
				return nil, fmt.Errorf("unknown argument %q", a)
			}
			o.cmd = a
		}
		if err != nil {
			return nil, err
		}
	}
	o.explicit = o.projectsDir != "" || len(o.sources) > 0 || o.codex || os.Getenv("CLAUDE_PROJECTS_DIR") != "" || os.Getenv("TOKEN_DASHBOARD_NO_SETUP") == "1"
	return o, nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (o *opts) dbPath() string {
	if o.db != "" {
		return o.db
	}
	if v := os.Getenv("TOKEN_DASHBOARD_DB"); v != "" {
		return v
	}
	return store.DefaultPath()
}

func (o *opts) claudeDir() string {
	if o.projectsDir != "" {
		return o.projectsDir
	}
	if v := os.Getenv("CLAUDE_PROJECTS_DIR"); v != "" {
		return v
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude", "projects")
}

func (o *opts) manager() *sources.Manager {
	extra := append([]string{}, o.sources...)
	if o.codex {
		extra = append([]string{"codex=codex:" + sources.CodexDefault}, extra...)
	}
	pinned := o.projectsDir != "" || os.Getenv("CLAUDE_PROJECTS_DIR") != ""
	return sources.NewManager(o.claudeDir(), pinned, extra, "")
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "Watch My Harness:", err)
	os.Exit(1)
}

func main() {
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		die(err)
	}
	switch o.cmd {
	case "help":
		fmt.Print(usage)
		return
	case "version":
		fmt.Println("watch-my-harness", server.Version)
		return
	case "licence", "license", "notices":
		for _, f := range []string{"LICENSE", "NOTICE"} {
			b, _ := fs.ReadFile(tokendashboard.Assets, f)
			fmt.Printf("===== %s =====\n%s\n", f, b)
		}
		fmt.Printf("Licence tier: %s (placeholder; nothing is enforced)\n", sources.LoadAppConfig().Licence)
		return
	case "", "dashboard":
		if o.cmd == "" {
			o.app = true // default: start the server and open the app window
		}
		runDashboard(o)
		return
	}
	db, err := store.Open(o.dbPath())
	if err != nil {
		die(err)
	}
	defer db.Close()
	switch o.cmd {
	case "scan":
		srcs, err := o.manager().Effective()
		if err != nil {
			die(err)
		}
		n, err := scanner.ScanSources(db, srcs, nil)
		if err != nil {
			die(err)
		}
		fmt.Printf("Watch My Harness: scanned %d files, %d messages, %d tool calls\n", n.Files, n.Messages, n.Tools)
	case "today", "stats":
		var t store.Row
		if o.cmd == "today" {
			now := time.Now().UTC()
			start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
			t = store.OverviewTotals(db, start.Format("2006-01-02T15:04:05")+"+00:00", start.AddDate(0, 0, 1).Format("2006-01-02T15:04:05")+"+00:00", "")
			fmt.Println("Watch My Harness — today")
		} else {
			t = store.OverviewTotals(db, "", "", "")
			fmt.Println("Watch My Harness — all time")
		}
		fmt.Printf("  sessions: %d    turns: %d\n", store.Int(t["sessions"]), store.Int(t["turns"]))
		fmt.Printf("  input:    %12s    output: %12s\n", comma(store.Int(t["input_tokens"])), comma(store.Int(t["output_tokens"])))
		if o.cmd == "today" {
			fmt.Printf("  cache rd: %12s    cache cr: %12s\n", comma(store.Int(t["cache_read_tokens"])), comma(store.Int(t["cache_create_5m_tokens"])+store.Int(t["cache_create_1h_tokens"])))
		}
	case "tips":
		ts := tips.All(db, "", "")
		if len(ts) == 0 {
			fmt.Println("Watch My Harness: no suggestions")
		}
		for _, t := range ts {
			fmt.Printf("[%s] %s\n  %s\n\n", t["category"], t["title"], t["body"])
		}
	case "recommend":
		md := tips.RecommendationsMarkdown(db, "", "")
		if md == "" {
			fmt.Println("Watch My Harness: no recommendations right now — nothing crossed a threshold.")
			return
		}
		fmt.Println(md)
		fmt.Println("Draft only — review it, then merge whatever you agree with into ~/.claude/CLAUDE.md yourself. Nothing was written automatically.")
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func runDashboard(o *opts) {
	base := fmt.Sprintf("http://%s:%d/", o.host, o.port)
	// Single instance: if a dashboard already answers, just open a window to it.
	if (o.app || !o.noOpen) && window.IsRunning(base) {
		fmt.Printf("Watch My Harness already running on %s\n", base)
		if o.app {
			window.OpenApp(base)
		} else {
			window.OpenBrowser(base)
		}
		return
	}
	db, err := store.Open(o.dbPath())
	if err != nil {
		die(err)
	}
	defer db.Close()
	interval := 30 * time.Second
	if o.noScan {
		interval = 0
	}
	srv, err := server.New(server.Options{DB: db, Assets: tokendashboard.Assets, Sources: o.manager(),
		ScanInterval: interval, RequireSetup: !o.explicit})
	if err != nil {
		die(err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(o.host, strconv.Itoa(o.port)))
	if err != nil {
		die(fmt.Errorf("cannot listen on %s:%d: %w (is another program using the port? try --port)", o.host, o.port, err))
	}
	fmt.Printf("Watch My Harness listening on %s\n", base)
	switch {
	case o.noOpen && !o.app:
	case o.app:
		if how, err := window.OpenApp(base); err != nil {
			fmt.Fprintln(os.Stderr, "could not open a window:", err)
		} else if how == "browser" {
			fmt.Println("(no Chrome/Edge/Chromium found: opened your default browser)")
		}
	default:
		window.OpenBrowser(base)
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	if os.Getenv("TOKEN_DASHBOARD_WATCHDOG") == "1" {
		go watchdog(o.port)
	}
	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		hs.Shutdown(ctx)
		close(done)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		die(err)
	}
	<-done
}

// watchdog exits the process when /healthz stops answering so a restart policy brings it back.
func watchdog(port int) {
	bad := 0
	for {
		time.Sleep(30 * time.Second)
		c := http.Client{Timeout: 5 * time.Second}
		resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			bad = 0
			continue
		}
		if err == nil {
			resp.Body.Close()
		}
		if bad++; bad >= 3 {
			fmt.Println("watch-my-harness watchdog: unhealthy, exiting for restart")
			os.Exit(1)
		}
	}
}
