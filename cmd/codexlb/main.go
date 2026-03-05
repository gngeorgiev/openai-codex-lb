package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gngeorgiev/openai-codex-lb/internal/lb"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	if len(argv) == 0 {
		printUsage()
		return 2
	}

	switch argv[0] {
	case "proxy":
		return runProxy(argv[1:])
	case "account":
		return runAccount(argv[1:])
	case "status":
		return runStatus(argv[1:])
	case "run":
		return runCodex(argv[1:])
	case "help", "-h", "--help":
		printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", argv[0])
		printUsage()
		return 2
	}
}

func runProxy(argv []string) int {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	root := fs.String("root", "", "State directory (default: ~/.codex-lb)")
	listen := fs.String("listen", "", "Listen address, e.g. 127.0.0.1:8765")
	upstream := fs.String("upstream", "", "Upstream base URL, e.g. https://chatgpt.com/backend-api")
	maxAttempts := fs.Int("max-attempts", 0, "Retry attempts per request before returning last upstream response")
	usageTimeoutMS := fs.Int("usage-timeout-ms", 0, "Timeout for usage-refresh API requests")
	cooldownDefaultSeconds := fs.Int("cooldown-default-seconds", 0, "Default cooldown on retryable transport/upstream errors")
	quotaRefreshMinutes := fs.Int("quota-refresh-minutes", 0, "Quota refresh interval in minutes")
	quotaRefreshMessages := fs.Int("quota-refresh-messages", 0, "Quota refresh interval in successful messages")
	quotaCacheTTLMinutes := fs.Int("quota-cache-ttl-minutes", 0, "Quota cache TTL in minutes (used by policy freshness)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: codexlb proxy [flags]

Runs the local load-balancing proxy.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), `
Examples:
  codexlb proxy
  codexlb proxy --listen 127.0.0.1:8765 --upstream https://chatgpt.com/backend-api
  codexlb proxy --max-attempts 4 --quota-refresh-minutes 5
`)
	}
	if err := fs.Parse(argv); err != nil {
		return parseFlagError(err)
	}

	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}

	if *listen != "" || *upstream != "" || *maxAttempts > 0 || *usageTimeoutMS > 0 || *cooldownDefaultSeconds > 0 || *quotaRefreshMinutes > 0 || *quotaRefreshMessages > 0 || *quotaCacheTTLMinutes > 0 {
		err = store.Update(func(sf *lb.StoreFile) error {
			if *listen != "" {
				sf.Settings.Proxy.Listen = *listen
			}
			if *upstream != "" {
				sf.Settings.Proxy.UpstreamBaseURL = strings.TrimRight(*upstream, "/")
			}
			if *maxAttempts > 0 {
				sf.Settings.Proxy.MaxAttempts = *maxAttempts
			}
			if *usageTimeoutMS > 0 {
				sf.Settings.Proxy.UsageTimeoutMS = *usageTimeoutMS
			}
			if *cooldownDefaultSeconds > 0 {
				sf.Settings.Proxy.CooldownDefaultS = *cooldownDefaultSeconds
			}
			if *quotaRefreshMinutes > 0 {
				sf.Settings.Quota.RefreshIntervalMinutes = *quotaRefreshMinutes
			}
			if *quotaRefreshMessages > 0 {
				sf.Settings.Quota.RefreshIntervalMessages = *quotaRefreshMessages
			}
			if *quotaCacheTTLMinutes > 0 {
				sf.Settings.Quota.CacheTTLMinutes = *quotaCacheTTLMinutes
			}
			for i := range sf.Accounts {
				if sf.Accounts[i].BaseURL == "" {
					sf.Accounts[i].BaseURL = sf.Settings.Proxy.UpstreamBaseURL
				}
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "update settings: %v\n", err)
			return 1
		}
		if err := store.PersistSettingsToConfig(); err != nil {
			fmt.Fprintf(os.Stderr, "persist config.toml: %v\n", err)
			return 1
		}
	}

	events, err := lb.OpenEventLogger(store.RootDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "open event logger: %v\n", err)
		return 1
	}
	defer events.Close()

	snapshot := store.Snapshot()
	proxyLogger := log.New(os.Stderr, "codexlb: ", log.LstdFlags)
	proxy := lb.NewProxyServer(store, proxyLogger, events)

	srv := &http.Server{
		Addr:              snapshot.Settings.Proxy.Listen,
		Handler:           proxy,
		ReadHeaderTimeout: 20 * time.Second,
	}

	logFile := fmt.Sprintf("%s/logs/proxy.current.jsonl", store.RootDir())
	fmt.Printf("codexlb proxy listening on http://%s (upstream=%s, logs=%s)\n", snapshot.Settings.Proxy.Listen, snapshot.Settings.Proxy.UpstreamBaseURL, logFile)
	events.Log("proxy.started", map[string]any{
		"listen":   snapshot.Settings.Proxy.Listen,
		"upstream": snapshot.Settings.Proxy.UpstreamBaseURL,
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	lb.StartConfigReloader(sigCtx, store, proxyLogger, events, time.Second)

	select {
	case <-sigCtx.Done():
		events.Log("proxy.stopping", map[string]any{"signal": sigCtx.Err().Error()})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		return 0
	case err := <-errCh:
		if err == nil || err == http.ErrServerClosed {
			return 0
		}
		events.Log("proxy.error", map[string]any{"error": err.Error()})
		fmt.Fprintf(os.Stderr, "proxy server error: %v\n", err)
		return 1
	}
}

func runAccount(argv []string) int {
	if len(argv) == 0 {
		printAccountUsage()
		return 2
	}
	switch argv[0] {
	case "login":
		return runAccountLogin(argv[1:])
	case "import":
		return runAccountImport(argv[1:])
	case "list":
		return runAccountList(argv[1:])
	case "rm":
		return runAccountRemove(argv[1:])
	case "help", "-h", "--help":
		printAccountUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown account subcommand: %s\n", argv[0])
		printAccountUsage()
		return 2
	}
}

func runAccountLogin(argv []string) int {
	fs := flag.NewFlagSet("account login", flag.ContinueOnError)
	root := fs.String("root", "", "State directory")
	codexBin := fs.String("codex-bin", os.Getenv("CODEXLB_CODEX_BIN"), "Codex executable path")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: codexlb account login [flags] <alias> [-- <codex-login-args...>]

Creates/uses ~/.codex-lb/accounts/<alias> as CODEX_HOME and executes 'codex login'.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return parseFlagError(err)
	}
	args := fs.Args()
	if len(args) < 1 {
		fs.Usage()
		return 2
	}
	alias := args[0]
	loginArgs := []string{}
	if len(args) > 1 {
		loginArgs = args[1:]
	}

	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	if err := lb.LoginAccount(store, alias, *codexBin, loginArgs); err != nil {
		fmt.Fprintf(os.Stderr, "login account: %v\n", err)
		return 1
	}
	snap := store.Snapshot()
	fmt.Printf("registered account %s (total=%d)\n", alias, len(snap.Accounts))
	return 0
}

func runAccountImport(argv []string) int {
	fs := flag.NewFlagSet("account import", flag.ContinueOnError)
	root := fs.String("root", "", "State directory")
	from := fs.String("from", "", "Existing CODEX_HOME directory to import from")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: codexlb account import [flags] --from <CODEX_HOME> <alias>

Imports auth.json from an existing Codex home into ~/.codex-lb/accounts/<alias>.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return parseFlagError(err)
	}
	args := fs.Args()
	if len(args) != 1 || *from == "" {
		fs.Usage()
		return 2
	}
	alias := args[0]

	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	if err := lb.ImportAccount(store, alias, *from); err != nil {
		fmt.Fprintf(os.Stderr, "import account: %v\n", err)
		return 1
	}
	snap := store.Snapshot()
	fmt.Printf("imported account %s (total=%d)\n", alias, len(snap.Accounts))
	return 0
}

func runAccountList(argv []string) int {
	fs := flag.NewFlagSet("account list", flag.ContinueOnError)
	root := fs.String("root", "", "State directory")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: codexlb account list [flags]

Lists currently registered accounts and status.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return parseFlagError(err)
	}
	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	accounts := lb.ListAccounts(store)
	if len(accounts) == 0 {
		fmt.Println("no accounts")
		return 0
	}
	for _, account := range accounts {
		status := "ready"
		if !account.Enabled || account.DisabledReason != "" {
			status = "disabled"
		}
		if account.CooldownUntilMS > time.Now().UnixMilli() {
			status = fmt.Sprintf("cooldown(%ds)", int((account.CooldownUntilMS-time.Now().UnixMilli())/1000)+1)
		}
		email := account.UserEmail
		if email == "" {
			email = "-"
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", account.Alias, account.ID, email, status)
	}
	return 0
}

func runAccountRemove(argv []string) int {
	fs := flag.NewFlagSet("account rm", flag.ContinueOnError)
	root := fs.String("root", "", "State directory")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: codexlb account rm [flags] <alias>

Removes an account and deletes its stored home directory.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return parseFlagError(err)
	}
	args := fs.Args()
	if len(args) != 1 {
		fs.Usage()
		return 2
	}
	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	if err := lb.RemoveAccount(store, args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "remove account: %v\n", err)
		return 1
	}
	fmt.Printf("removed account %s\n", args[0])
	return 0
}

func runCodex(argv []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	root := fs.String("root", "", "State directory")
	proxyURL := fs.String("proxy-url", "", "Proxy URL (default: http://<listen-from-store>)")
	codexBin := fs.String("codex-bin", os.Getenv("CODEXLB_CODEX_BIN"), "Codex executable path")
	codexHome := fs.String("codex-home", "", "CODEX_HOME for wrapper-run command")
	commandOnly := fs.Bool("command", false, "Print wrapped codex command and exit")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: codexlb run [flags] [<codex-args...>]

Runs codex with OPENAI_BASE_URL pointing to the local proxy and OPENAI_API_KEY set if missing.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), `
Examples:
  codexlb run
  codexlb run exec --json "fix this"
  codexlb run --command exec --json "fix this"
  codexlb run --proxy-url http://127.0.0.1:9000 --codex-home ~/.codex-lb/runtime-work exec
`)
	}
	if err := fs.Parse(argv); err != nil {
		return parseFlagError(err)
	}

	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	if *commandOnly {
		fmt.Println(lb.FormatRunCodexCommand(store, *codexBin, *proxyURL, *codexHome, fs.Args()))
		return 0
	}

	code, err := lb.RunCodex(store, *codexBin, *proxyURL, *codexHome, fs.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "run codex: %v\n", err)
		return 1
	}
	return code
}

func runStatus(argv []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	root := fs.String("root", "", "State directory")
	proxyURL := fs.String("proxy-url", "", "Proxy URL (default: http://<listen-from-store>)")
	timeout := fs.Duration("timeout", 3*time.Second, "HTTP timeout for status request")
	jsonOut := fs.Bool("json", false, "Print raw JSON status output")
	shortOut := fs.Bool("short", false, "Print one-line status for status bars")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: codexlb status [flags]

Queries the running proxy /status endpoint and prints account table.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), `
Examples:
  codexlb status
  codexlb status --proxy-url http://127.0.0.1:8765
  codexlb status --short
  codexlb status --json
`)
	}
	if err := fs.Parse(argv); err != nil {
		return parseFlagError(err)
	}
	if *jsonOut && *shortOut {
		fmt.Fprintln(os.Stderr, "status flags --json and --short are mutually exclusive")
		return 2
	}

	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	snapshot := store.Snapshot()
	url := *proxyURL
	if strings.TrimSpace(url) == "" {
		url = "http://" + snapshot.Settings.Proxy.Listen
	}
	url = strings.TrimRight(url, "/") + "/status"

	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query proxy status %s: %v\n", url, err)
		return 1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read proxy status response: %v\n", err)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "proxy status error: status=%d body=%s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}

	if *jsonOut {
		_, _ = os.Stdout.Write(body)
		if len(body) == 0 || body[len(body)-1] != '\n' {
			fmt.Println()
		}
		return 0
	}

	var status lb.ProxyStatus
	if err := json.Unmarshal(body, &status); err != nil {
		fmt.Fprintf(os.Stderr, "decode status JSON: %v\n", err)
		return 1
	}
	if *shortOut {
		printStatusShort(status)
		return 0
	}
	printStatusTable(status)
	return 0
}

func printStatusShort(status lb.ProxyStatus) {
	active := "none"
	for _, a := range status.Accounts {
		if a.Active {
			active = a.Alias
			break
		}
	}
	reason := noneIfEmpty(status.SelectionReason)
	mode := noneIfEmpty(string(status.Policy.Mode))
	fmt.Printf("lb=%s reason=%s mode=%s\n", active, reason, mode)
}

func printStatusTable(status lb.ProxyStatus) {
	fmt.Printf("policy=%s selected=%s reason=%s generated_at=%s\n", status.Policy.Mode, noneIfEmpty(status.SelectedAccountID), noneIfEmpty(status.SelectionReason), status.GeneratedAt)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ACTIVE\tPIN\tALIAS\tID\tEMAIL\tSTATUS\tDAILY_LEFT\tWEEKLY_LEFT\tSCORE\tLAST_SWITCH\tQUOTA")
	for _, a := range status.Accounts {
		active := ""
		if a.Active {
			active = "*"
		}
		pin := ""
		if a.Pinned {
			pin = "P"
		}
		state := "ready"
		if !a.Enabled || a.DisabledReason != "" {
			state = "disabled"
			if a.DisabledReason != "" {
				state += "(" + a.DisabledReason + ")"
			}
		} else if a.CooldownSeconds > 0 {
			state = fmt.Sprintf("cooldown(%ds)", a.CooldownSeconds)
		} else if !a.Healthy {
			state = "unhealthy"
		}

		daily := "-"
		if a.DailyLeftPct >= 0 {
			daily = fmt.Sprintf("%.1f%%", a.DailyLeftPct)
		}
		weekly := "-"
		if a.WeeklyLeftPct >= 0 {
			weekly = fmt.Sprintf("%.1f%%", a.WeeklyLeftPct)
		}
		email := "-"
		if a.Email != "" {
			email = a.Email
		}
		lastSwitch := "-"
		if a.LastSwitchReason != "" {
			lastSwitch = a.LastSwitchReason
		}
		quota := "-"
		if a.QuotaSource != "" {
			quota = a.QuotaSource
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.3f\t%s\t%s\n",
			active, pin, a.Alias, a.ID, email, state, daily, weekly, a.Score, lastSwitch, quota)
	}
	_ = w.Flush()
}

func noneIfEmpty(v string) string {
	if strings.TrimSpace(v) == "" {
		return "none"
	}
	return v
}

func parseFlagError(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

func printAccountUsage() {
	fmt.Print(`Usage: codexlb account <subcommand> [flags]

Subcommands:
  login   login a new account into an isolated CODEX_HOME
  import  import auth.json from existing CODEX_HOME
  list    show registered accounts
  rm      remove an account

Run 'codexlb account <subcommand> --help' for detailed flags.
`)
}

func printUsage() {
	fmt.Print(`codexlb - local multi-account proxy for Codex CLI

Usage:
  codexlb <command> [flags]

Commands:
  proxy    Run the local load-balancing proxy
  account  Manage enrolled accounts (login/import/list/rm)
  status   Show runtime status table from running proxy
  run      Run codex with proxy endpoint environment wiring

Run 'codexlb <command> --help' for detailed flags and examples.

Environment:
  CODEXLB_CODEX_BIN   default codex binary used by account login/run
`)
}
