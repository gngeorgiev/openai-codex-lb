package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gngeorgiev/openai-codex-lb/internal/lb"
	toml "github.com/pelletier/go-toml/v2"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	if len(argv) == 0 {
		return runCodex(argv)
	}

	switch argv[0] {
	case "proxy":
		return runProxy(argv[1:])
	case "account":
		return runAccount(argv[1:])
	case "login-with":
		return runLoginWith(argv[1:])
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
	if len(argv) > 0 {
		switch argv[0] {
		case "logs":
			return runProxyLogs(argv[1:])
		case "help", "-h", "--help":
			fmt.Print(`Usage: codexlb proxy [flags]
       codexlb proxy logs [flags]

Subcommands:
  logs    Fetch/tail proxy event logs over HTTP
`)
			return 0
		}
	}

	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	root := fs.String("root", defaultRootFlagValue(), "State directory (default: $CODEXLB_ROOT or ~/.codex-lb)")
	listen := fs.String("listen", "", "Listen address, e.g. 127.0.0.1:8765")
	upstream := fs.String("upstream", "", "Upstream base URL, e.g. https://chatgpt.com/backend-api")
	maxAttempts := fs.Int("max-attempts", 0, "Retry attempts per request before returning last upstream response")
	usageTimeoutMS := fs.Int("usage-timeout-ms", 0, "Timeout for quota/auth refresh API requests")
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

Environment:
  CODEXLB_PROXY_NAME        proxy name override for the running proxy
  CODEXLB_CHILD_PROXY_URLS   comma/newline/space-separated child proxy URLs
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

	overrides := lb.RuntimeSettingsOverrides{}
	if proxyName := strings.TrimSpace(os.Getenv("CODEXLB_PROXY_NAME")); proxyName != "" {
		overrides.ProxyName = &proxyName
	}
	childProxyURLs := parseProxyURLListEnv(os.Getenv("CODEXLB_CHILD_PROXY_URLS"))
	if *listen != "" || *upstream != "" || *maxAttempts > 0 || *usageTimeoutMS > 0 || *cooldownDefaultSeconds > 0 || *quotaRefreshMinutes > 0 || *quotaRefreshMessages > 0 || *quotaCacheTTLMinutes > 0 {
		if *listen != "" {
			overrides.Listen = listen
		}
		if *upstream != "" {
			val := strings.TrimRight(*upstream, "/")
			overrides.UpstreamBaseURL = &val
		}
		if *maxAttempts > 0 {
			overrides.MaxAttempts = maxAttempts
		}
		if *usageTimeoutMS > 0 {
			overrides.UsageTimeoutMS = usageTimeoutMS
		}
		if *cooldownDefaultSeconds > 0 {
			overrides.CooldownDefaultSeconds = cooldownDefaultSeconds
		}
		if *quotaRefreshMinutes > 0 {
			overrides.RefreshIntervalMinutes = quotaRefreshMinutes
		}
		if *quotaRefreshMessages > 0 {
			overrides.RefreshIntervalMessages = quotaRefreshMessages
		}
		if *quotaCacheTTLMinutes > 0 {
			overrides.CacheTTLMinutes = quotaCacheTTLMinutes
		}
	}
	if len(childProxyURLs) > 0 {
		overrides.ChildProxyURLs = &childProxyURLs
	}
	if overrides.ProxyName != nil || overrides.Listen != nil || overrides.UpstreamBaseURL != nil || overrides.ChildProxyURLs != nil || overrides.MaxAttempts != nil || overrides.UsageTimeoutMS != nil || overrides.CooldownDefaultSeconds != nil || overrides.RefreshIntervalMinutes != nil || overrides.RefreshIntervalMessages != nil || overrides.CacheTTLMinutes != nil {
		store.SetRuntimeSettingsOverrides(overrides)
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
	fmt.Printf("codexlb proxy %s listening on http://%s (upstream=%s, logs=%s)\n", snapshot.Settings.Proxy.Name, snapshot.Settings.Proxy.Listen, snapshot.Settings.Proxy.UpstreamBaseURL, logFile)
	events.Log("proxy.started", map[string]any{
		"proxy_name": snapshot.Settings.Proxy.Name,
		"listen":     snapshot.Settings.Proxy.Listen,
		"upstream":   snapshot.Settings.Proxy.UpstreamBaseURL,
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

func runProxyLogs(argv []string) int {
	fs := flag.NewFlagSet("proxy logs", flag.ContinueOnError)
	root := fs.String("root", defaultRootFlagValue(), "State directory (default: $CODEXLB_ROOT or ~/.codex-lb)")
	proxyURL := fs.String("proxy-url", defaultProxyURLFlagValue(), "Proxy URL (default: $CODEXLB_PROXY_URL, else proxy.proxy_url or http://<listen-from-store>)")
	tail := fs.Int("tail", 100, "Number of most recent log lines for initial fetch")
	offset := fs.Int64("offset", -1, "Start reading from byte offset (overrides --tail)")
	limit := fs.Int("limit", 500, "Maximum lines per request")
	follow := fs.Bool("follow", false, "Poll continuously for new log lines")
	interval := fs.Duration("interval", 2*time.Second, "Polling interval for --follow")
	timeout := fs.Duration("timeout", 10*time.Second, "HTTP timeout for log requests")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: codexlb proxy logs [flags]

Fetch or tail proxy event logs from a running proxy instance.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), `
Examples:
  codexlb proxy logs
  codexlb proxy logs --proxy-url http://10.0.0.25:8765 --tail 200
  codexlb proxy logs --proxy-url https://proxy.example.com --follow
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
	baseURL := resolveProxyURL(store, *proxyURL)
	logURL := strings.TrimRight(baseURL, "/") + "/logs"
	client := &http.Client{Timeout: *timeout}

	curOffset := *offset
	if curOffset < 0 && *tail <= 0 {
		curOffset = 0
	}

	fetchOnce := func() (int64, int) {
		reqURL := logURL
		params := []string{}
		if curOffset >= 0 {
			params = append(params, fmt.Sprintf("offset=%d", curOffset))
		} else {
			params = append(params, fmt.Sprintf("tail=%d", *tail))
		}
		params = append(params, fmt.Sprintf("limit=%d", *limit))
		if len(params) > 0 {
			reqURL += "?" + strings.Join(params, "&")
		}
		resp, err := client.Get(reqURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "query proxy logs %s: %v\n", reqURL, err)
			return curOffset, 1
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read proxy logs response: %v\n", err)
			return curOffset, 1
		}
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "proxy logs error: status=%d body=%s\n", resp.StatusCode, strings.TrimSpace(string(body)))
			return curOffset, 1
		}
		if len(body) > 0 {
			_, _ = os.Stdout.Write(body)
			if body[len(body)-1] != '\n' {
				fmt.Println()
			}
		}
		next := curOffset
		if raw := strings.TrimSpace(resp.Header.Get("X-Next-Offset")); raw != "" {
			if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v >= 0 {
				next = v
			}
		}
		return next, 0
	}

	next, code := fetchOnce()
	if code != 0 {
		return code
	}
	curOffset = next
	if !*follow {
		return 0
	}

	for {
		time.Sleep(*interval)
		next, code = fetchOnce()
		if code != 0 {
			return code
		}
		curOffset = next
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
	case "pin":
		return runAccountPin(argv[1:])
	case "unpin":
		return runAccountUnpin(argv[1:])
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
	root := fs.String("root", defaultRootFlagValue(), "State directory (default: $CODEXLB_ROOT or ~/.codex-lb)")
	proxyURL := fs.String("proxy-url", defaultProxyURLFlagValue(), "Remote proxy admin URL (default: $CODEXLB_PROXY_URL)")
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
	if strings.TrimSpace(*proxyURL) != "" {
		resp, err := loginAccountRemote(store, *proxyURL, alias, *codexBin, loginArgs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "login account (remote): %v\n", err)
			return 1
		}
		fmt.Printf("registered account %s (total=%d)\n", alias, resp.Total)
		return 0
	}
	if shouldAutoRemoteAccount(store, *root) {
		if resp, err := tryRemoteLoginWithFallback(store, alias, *codexBin, loginArgs); err == nil {
			fmt.Printf("registered account %s (total=%d)\n", alias, resp.Total)
			return 0
		} else if !isRemoteAdminUnavailable(err) {
			fmt.Fprintf(os.Stderr, "login account (remote): %v\n", err)
			return 1
		}
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
	root := fs.String("root", defaultRootFlagValue(), "State directory (default: $CODEXLB_ROOT or ~/.codex-lb)")
	proxyURL := fs.String("proxy-url", defaultProxyURLFlagValue(), "Remote proxy admin URL (default: $CODEXLB_PROXY_URL)")
	from := fs.String("from", "", "Existing CODEX_HOME directory to import from (default: $CODEX_HOME or ~/.codex)")
	into := fs.String("into", "local", "Import target: local or proxy")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: codexlb account import [flags] [<alias>]

Imports auth.json from a local existing Codex home into ~/.codex-lb/accounts/<alias> or uploads it to a remote proxy.
If <alias> is omitted, codexlb derives one from the source config/auth when possible, otherwise generates one.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return parseFlagError(err)
	}
	args := fs.Args()
	if len(args) > 1 {
		fs.Usage()
		return 2
	}
	target := strings.ToLower(strings.TrimSpace(*into))
	switch target {
	case "local":
	case "proxy":
	default:
		fmt.Fprintln(os.Stderr, "account import: --into must be one of: local, proxy")
		return 2
	}

	sourceHome, err := resolveAccountImportSourceHome(*from)
	if err != nil {
		fmt.Fprintf(os.Stderr, "account import: resolve source home: %v\n", err)
		return 1
	}

	var store *lb.Store
	openStore := func() (*lb.Store, error) {
		if store != nil {
			return store, nil
		}
		st, err := lb.OpenStore(*root)
		if err != nil {
			return nil, err
		}
		store = st
		return store, nil
	}

	alias := ""
	if len(args) == 1 {
		alias = args[0]
	}

	if target == "proxy" {
		targetProxyURL := strings.TrimSpace(*proxyURL)
		if targetProxyURL == "" {
			store, err := openStore()
			if err != nil {
				fmt.Fprintf(os.Stderr, "open store: %v\n", err)
				return 1
			}
			if shouldAutoRemoteAccount(store, *root) {
				targetProxyURL = strings.TrimSpace(store.Snapshot().Settings.ProxyURL)
			}
		}
		if strings.TrimSpace(targetProxyURL) == "" {
			fmt.Fprintln(os.Stderr, "account import: --into=proxy requires --proxy-url, $CODEXLB_PROXY_URL, or proxy.proxy_url")
			return 2
		}
		alias = resolveImportAlias(alias, sourceHome, remoteImportAliasSet(targetProxyURL))
		resp, err := remoteAdminImportHome(targetProxyURL, alias, sourceHome)
		if err != nil {
			fmt.Fprintf(os.Stderr, "import account (remote): %v\n", err)
			return 1
		}
		fmt.Printf("imported account %s (total=%d)\n", alias, resp.Total)
		return 0
	}

	store, err = openStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	alias = resolveImportAlias(alias, sourceHome, localImportAliasSet(store))
	if err := lb.ImportAccount(store, alias, sourceHome); err != nil {
		fmt.Fprintf(os.Stderr, "import account: %v\n", err)
		return 1
	}
	snap := store.Snapshot()
	fmt.Printf("imported account %s (total=%d)\n", alias, len(snap.Accounts))
	return 0
}

func runAccountList(argv []string) int {
	fs := flag.NewFlagSet("account list", flag.ContinueOnError)
	root := fs.String("root", defaultRootFlagValue(), "State directory (default: $CODEXLB_ROOT or ~/.codex-lb)")
	proxyURL := fs.String("proxy-url", defaultProxyURLFlagValue(), "Remote proxy admin URL (default: $CODEXLB_PROXY_URL)")
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
	if strings.TrimSpace(*proxyURL) != "" {
		accounts, err := remoteAdminListAccounts(*proxyURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "list accounts (remote): %v\n", err)
			return 1
		}
		printAccountList(accounts)
		return 0
	}
	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	if shouldAutoRemoteAccount(store, *root) {
		if accounts, err := tryRemoteListWithFallback(store); err == nil {
			printAccountList(accounts)
			return 0
		} else if !isRemoteAdminUnavailable(err) {
			fmt.Fprintf(os.Stderr, "list accounts (remote): %v\n", err)
			return 1
		}
	}
	printAccountList(lb.ListAccounts(store))
	return 0
}

func runAccountRemove(argv []string) int {
	fs := flag.NewFlagSet("account rm", flag.ContinueOnError)
	root := fs.String("root", defaultRootFlagValue(), "State directory (default: $CODEXLB_ROOT or ~/.codex-lb)")
	proxyURL := fs.String("proxy-url", defaultProxyURLFlagValue(), "Remote proxy admin URL (default: $CODEXLB_PROXY_URL)")
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
	if strings.TrimSpace(*proxyURL) != "" {
		if _, err := remoteAdminRemove(*proxyURL, args[0]); err != nil {
			fmt.Fprintf(os.Stderr, "remove account (remote): %v\n", err)
			return 1
		}
		fmt.Printf("removed account %s\n", args[0])
		return 0
	}
	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	if shouldAutoRemoteAccount(store, *root) {
		if _, err := tryRemoteRemoveWithFallback(store, args[0]); err == nil {
			fmt.Printf("removed account %s\n", args[0])
			return 0
		} else if !isRemoteAdminUnavailable(err) {
			fmt.Fprintf(os.Stderr, "remove account (remote): %v\n", err)
			return 1
		}
	}
	if err := lb.RemoveAccount(store, args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "remove account: %v\n", err)
		return 1
	}
	fmt.Printf("removed account %s\n", args[0])
	return 0
}

func runAccountPin(argv []string) int {
	return runPinCommand(
		argv,
		"account pin",
		`Usage: codexlb account pin [flags] <alias>

Pins account selection to a specific alias while it remains healthy.

Flags:
`,
		"pin account",
		"pinned account %s\n",
	)
}

func runPinCommand(argv []string, flagSetName, usage, errPrefix, successFormat string) int {
	fs := flag.NewFlagSet(flagSetName, flag.ContinueOnError)
	root := fs.String("root", defaultRootFlagValue(), "State directory (default: $CODEXLB_ROOT or ~/.codex-lb)")
	proxyURL := fs.String("proxy-url", defaultProxyURLFlagValue(), "Remote proxy admin URL (default: $CODEXLB_PROXY_URL)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
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
	alias := args[0]
	if strings.TrimSpace(*proxyURL) != "" {
		if _, err := remoteAdminPin(*proxyURL, alias); err != nil {
			fmt.Fprintf(os.Stderr, "%s (remote): %v\n", errPrefix, err)
			return 1
		}
		fmt.Printf(successFormat, alias)
		return 0
	}

	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	if shouldAutoRemoteAccount(store, *root) {
		if err := tryRemotePinWithFallback(store, alias); err == nil {
			fmt.Printf(successFormat, alias)
			return 0
		} else if !isRemoteAdminUnavailable(err) {
			fmt.Fprintf(os.Stderr, "%s (remote): %v\n", errPrefix, err)
			return 1
		}
	}
	snapshot := store.Snapshot()
	pinnedID := ""
	for _, account := range snapshot.Accounts {
		if account.Alias == alias {
			pinnedID = account.ID
			break
		}
	}
	if pinnedID == "" {
		fmt.Fprintf(os.Stderr, "%s: alias not found: %s\n", errPrefix, alias)
		return 1
	}
	if err := store.Update(func(sf *lb.StoreFile) error {
		sf.State.PinnedAccountID = pinnedID
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", errPrefix, err)
		return 1
	}
	fmt.Printf(successFormat, alias)
	return 0
}

func runAccountUnpin(argv []string) int {
	fs := flag.NewFlagSet("account unpin", flag.ContinueOnError)
	root := fs.String("root", defaultRootFlagValue(), "State directory (default: $CODEXLB_ROOT or ~/.codex-lb)")
	proxyURL := fs.String("proxy-url", defaultProxyURLFlagValue(), "Remote proxy admin URL (default: $CODEXLB_PROXY_URL)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: codexlb account unpin [flags]

Clears pinned account selection.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return parseFlagError(err)
	}
	if len(fs.Args()) != 0 {
		fs.Usage()
		return 2
	}
	if strings.TrimSpace(*proxyURL) != "" {
		if _, err := remoteAdminUnpin(*proxyURL); err != nil {
			fmt.Fprintf(os.Stderr, "unpin account (remote): %v\n", err)
			return 1
		}
		fmt.Println("unpinned account selection")
		return 0
	}

	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		return 1
	}
	if shouldAutoRemoteAccount(store, *root) {
		if err := tryRemoteUnpinWithFallback(store); err == nil {
			fmt.Println("unpinned account selection")
			return 0
		} else if !isRemoteAdminUnavailable(err) {
			fmt.Fprintf(os.Stderr, "unpin account (remote): %v\n", err)
			return 1
		}
	}
	if err := store.Update(func(sf *lb.StoreFile) error {
		sf.State.PinnedAccountID = ""
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "unpin account: %v\n", err)
		return 1
	}
	fmt.Println("unpinned account selection")
	return 0
}

func runLoginWith(argv []string) int {
	alias := ""
	parseArgv := argv
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		alias = argv[0]
		parseArgv = argv[1:]
	}

	fs := flag.NewFlagSet("login-with", flag.ContinueOnError)
	root := fs.String("root", defaultRootFlagValue(), "State directory (default: $CODEXLB_ROOT or ~/.codex-lb)")
	username := fs.String("username", "", "ChatGPT/OpenAI username or email")
	password := fs.String("password", "", "Password passed directly on the command line")
	passwordStdin := fs.Bool("password-stdin", false, "Read password from stdin")
	codexHome := fs.String("codex-home", "", "Host CODEX_HOME destination (default: $CODEX_HOME or ~/.codex)")
	dockerBin := fs.String("docker-bin", "docker", "Docker executable path")
	dockerImage := fs.String("docker-image", lb.DefaultLoginDockerImage, "Docker image used for the automated login flow")
	dockerNetwork := fs.String("docker-network", "bridge", "Docker network to join while running the login container")
	timeout := fs.Duration("timeout", 10*time.Minute, "Overall timeout for the login flow")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: codexlb login-with [flags] <alias>

Run codex login inside a published Docker image, complete the ChatGPT browser flow in Chromium,
and import the resulting credentials back into the host CODEX_HOME and the named codexlb account alias.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), `
Examples:
  codexlb login-with work --username you@example.com --password-stdin
  codexlb login-with work --username you@example.com --password 's3cret' --docker-network vpn_net
  codexlb login-with work --username you@example.com --codex-home ~/.codex-work --docker-image ghcr.io/acme/custom-login:latest
`)
	}
	if err := fs.Parse(parseArgv); err != nil {
		return parseFlagError(err)
	}
	if alias == "" {
		if len(fs.Args()) != 1 {
			fs.Usage()
			return 2
		}
		alias = fs.Args()[0]
	} else if len(fs.Args()) != 0 {
		fs.Usage()
		return 2
	}
	if strings.TrimSpace(*username) == "" {
		fmt.Fprintln(os.Stderr, "login-with: --username is required")
		return 2
	}
	if (*password == "") == (!*passwordStdin) {
		fmt.Fprintln(os.Stderr, "login-with: specify exactly one of --password or --password-stdin")
		return 2
	}

	secret := *password
	if *passwordStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "login-with: read password from stdin: %v\n", err)
			return 1
		}
		secret = strings.TrimRight(string(data), "\r\n")
	}
	if strings.TrimSpace(secret) == "" {
		fmt.Fprintln(os.Stderr, "login-with: password is empty")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	store, err := lb.OpenStore(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "login-with: open store: %v\n", err)
		return 1
	}

	if err := lb.LoginWithDocker(ctx, lb.DockerLoginOptions{
		Username:      *username,
		Password:      secret,
		DockerBin:     *dockerBin,
		DockerImage:   *dockerImage,
		DockerNetwork: *dockerNetwork,
		CodexHome:     *codexHome,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "login-with: %v\n", err)
		return 1
	}

	targetHome := *codexHome
	if strings.TrimSpace(targetHome) == "" {
		var err error
		targetHome, err = lb.DefaultCodexHome()
		if err != nil {
			fmt.Fprintf(os.Stderr, "login-with: %v\n", err)
			return 1
		}
	}
	if err := lb.ImportAccount(store, alias, targetHome); err != nil {
		fmt.Fprintf(os.Stderr, "login-with: import account %s: %v\n", alias, err)
		return 1
	}
	fmt.Printf("imported login credentials into %s and registered account %s\n", targetHome, alias)
	return 0
}

func tryRemotePinWithFallback(store *lb.Store, alias string) error {
	client := remoteAdminFallbackClient()
	_, err := remoteAdminPinWithClient(client, resolveProxyURL(store, ""), alias)
	return err
}

func tryRemoteUnpinWithFallback(store *lb.Store) error {
	client := remoteAdminFallbackClient()
	_, err := remoteAdminUnpinWithClient(client, resolveProxyURL(store, ""))
	return err
}

func tryRemoteLoginWithFallback(store *lb.Store, alias, codexBin string, loginArgs []string) (lb.AdminMutationResponse, error) {
	return loginAccountRemoteWithClient(store, remoteAdminFallbackClient(), resolveProxyURL(store, ""), alias, codexBin, loginArgs)
}

func tryRemoteImportWithFallback(store *lb.Store, alias, from string) (lb.AdminMutationResponse, error) {
	return remoteAdminImportWithClient(remoteAdminFallbackClient(), resolveProxyURL(store, ""), alias, from)
}

func loginAccountRemote(store *lb.Store, proxyURL, alias, codexBin string, loginArgs []string) (lb.AdminMutationResponse, error) {
	return loginAccountRemoteWithClient(store, http.DefaultClient, proxyURL, alias, codexBin, loginArgs)
}

func loginAccountRemoteWithClient(store *lb.Store, client *http.Client, proxyURL, alias, codexBin string, loginArgs []string) (lb.AdminMutationResponse, error) {
	loginHome, err := os.MkdirTemp("", "codexlb-login-*")
	if err != nil {
		return lb.AdminMutationResponse{}, fmt.Errorf("create temp login home: %w", err)
	}
	defer os.RemoveAll(loginHome)

	if err := lb.LoginAccountToHome(store, alias, loginHome, codexBin, loginArgs); err != nil {
		return lb.AdminMutationResponse{}, err
	}
	return remoteAdminImportHomeWithClient(client, proxyURL, alias, loginHome)
}

func tryRemoteListWithFallback(store *lb.Store) ([]lb.Account, error) {
	return remoteAdminListAccountsWithClient(remoteAdminFallbackClient(), resolveProxyURL(store, ""))
}

func tryRemoteRemoveWithFallback(store *lb.Store, alias string) (lb.AdminMutationResponse, error) {
	return remoteAdminRemoveWithClient(remoteAdminFallbackClient(), resolveProxyURL(store, ""), alias)
}

func remoteAdminFallbackClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Second}
}

func shouldAutoRemoteAccount(store *lb.Store, rootArg string) bool {
	if strings.TrimSpace(rootArg) == "" {
		return true
	}
	snapshot := store.Snapshot()
	return strings.TrimSpace(snapshot.Settings.ProxyURL) != ""
}

func isRemoteAdminUnavailable(err error) bool {
	var urlErr *neturl.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var responseErr *remoteAdminUnexpectedResponseError
	if errors.As(err, &responseErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func printAccountList(accounts []lb.Account) {
	if len(accounts) == 0 {
		fmt.Println("no accounts")
		return
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
}

func runCodex(argv []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	root := fs.String("root", defaultRootFlagValue(), "State directory (default: $CODEXLB_ROOT or ~/.codex-lb)")
	proxyURL := fs.String("proxy-url", defaultProxyURLFlagValue(), "Proxy URL (default: $CODEXLB_PROXY_URL, else proxy.proxy_url or http://<listen-from-store>)")
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
	root := fs.String("root", defaultRootFlagValue(), "State directory (default: $CODEXLB_ROOT or ~/.codex-lb)")
	proxyURL := fs.String("proxy-url", defaultProxyURLFlagValue(), "Proxy URL (default: $CODEXLB_PROXY_URL, else proxy.proxy_url or http://<listen-from-store>)")
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
	url := resolveProxyURL(store, *proxyURL)
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

func resolveProxyURL(store *lb.Store, proxyURL string) string {
	url := strings.TrimSpace(proxyURL)
	if url != "" {
		return url
	}
	snapshot := store.Snapshot()
	if snapshot.Settings.ProxyURL != "" {
		return snapshot.Settings.ProxyURL
	}
	return "http://" + snapshot.Settings.Proxy.Listen
}

func printStatusShort(status lb.ProxyStatus) {
	active := "none"
	for _, a := range status.Accounts {
		if a.Active && a.ProxyName == status.ProxyName {
			active = a.Alias
			break
		}
	}
	if active == "none" {
		for _, child := range status.ChildProxies {
			if child.Active {
				active = child.Name
				break
			}
		}
	}
	reason := noneIfEmpty(status.SelectionReason)
	mode := noneIfEmpty(string(status.Policy.Mode))
	fmt.Printf("lb=%s reason=%s mode=%s\n", active, reason, mode)
}

func printStatusTable(status lb.ProxyStatus) {
	pinnedAlias := pinnedAliasForStatus(status)
	selected := status.SelectedAccountID
	if selected == "" {
		selected = status.SelectedProxyName
		if selected == "" {
			selected = status.SelectedProxyURL
		}
	}
	fmt.Printf("proxy=%s policy=%s selected=%s pinned=%s reason=%s generated_at=%s\n", noneIfEmpty(status.ProxyName), status.Policy.Mode, noneIfEmpty(selected), noneIfEmpty(pinnedAlias), noneIfEmpty(status.SelectionReason), status.GeneratedAt)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ACTIVE\tPIN\tPROXY\tALIAS\tID\tEMAIL\tSTATUS\tDAILY_LEFT\tDAILY_RESET\tWEEKLY_LEFT\tWEEKLY_RESET\tSCORE\tLAST_SWITCH\tQUOTA")
	pinnedID := strings.TrimSpace(status.State.PinnedAccountID)
	for _, a := range status.Accounts {
		active := ""
		if a.Active {
			active = "*"
		}
		pin := ""
		if a.Pinned || (pinnedID != "" && (a.ID == pinnedID || a.Alias == pinnedID)) {
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
		dailyReset := formatStatusResetAt(a.DailyResetAt)
		weekly := "-"
		if a.WeeklyLeftPct >= 0 {
			weekly = fmt.Sprintf("%.1f%%", a.WeeklyLeftPct)
		}
		weeklyReset := formatStatusResetAt(a.WeeklyResetAt)
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
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.3f\t%s\t%s\n",
			active, pin, noneIfEmpty(a.ProxyName), a.Alias, a.ID, email, state, daily, dailyReset, weekly, weeklyReset, a.Score, lastSwitch, quota)
	}
	_ = w.Flush()

	if len(status.ChildProxies) == 0 {
		return
	}

	fmt.Println()
	childWriter := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(childWriter, "ACTIVE\tNAME\tURL\tSTATUS\tSCORE\tSELECTED\tREASON\tLAST_SWITCH\tERROR")
	for _, child := range status.ChildProxies {
		active := ""
		if child.Active {
			active = "*"
		}
		state := "ready"
		if !child.Reachable {
			state = "unreachable"
		} else if child.CooldownSeconds > 0 {
			state = fmt.Sprintf("cooldown(%ds)", child.CooldownSeconds)
		} else if !child.Healthy {
			state = "unhealthy"
		}
		_, _ = fmt.Fprintf(childWriter, "%s\t%s\t%s\t%s\t%.3f\t%s\t%s\t%s\t%s\n",
			active,
			noneIfEmpty(child.Name),
			child.URL,
			state,
			child.Score,
			noneIfEmpty(child.SelectedTarget),
			noneIfEmpty(child.SelectionReason),
			noneIfEmpty(child.LastSwitchReason),
			noneIfEmpty(child.LastError),
		)
	}
	_ = childWriter.Flush()
}

func formatStatusResetAt(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

func pinnedAliasForStatus(status lb.ProxyStatus) string {
	pinnedID := strings.TrimSpace(status.State.PinnedAccountID)
	if pinnedID == "" {
		for _, a := range status.Accounts {
			if a.Pinned {
				return a.Alias
			}
		}
		return ""
	}
	for _, a := range status.Accounts {
		if a.Alias == pinnedID || a.ID == pinnedID {
			return a.Alias
		}
	}
	return pinnedID
}

func noneIfEmpty(v string) string {
	if strings.TrimSpace(v) == "" {
		return "none"
	}
	return v
}

func defaultRootFlagValue() string {
	return strings.TrimSpace(os.Getenv("CODEXLB_ROOT"))
}

func defaultProxyURLFlagValue() string {
	return strings.TrimSpace(os.Getenv("CODEXLB_PROXY_URL"))
}

func resolveAccountImportSourceHome(from string) (string, error) {
	from = strings.TrimSpace(from)
	if from != "" {
		return from, nil
	}
	return lb.DefaultCodexHome()
}

func resolveImportAlias(alias, sourceHome string, taken map[string]struct{}) string {
	alias = strings.TrimSpace(alias)
	if alias != "" {
		return alias
	}
	base := inferImportAlias(sourceHome)
	if base == "" {
		base = randomImportAlias()
	}
	return makeUniqueImportAlias(base, taken)
}

func inferImportAlias(sourceHome string) string {
	if candidate := importAliasFromConfig(sourceHome); candidate != "" {
		return candidate
	}
	if info, err := lb.LoadAuth(sourceHome); err == nil {
		if candidate := sanitizeImportAliasCandidate(emailAliasLocalPart(info.UserEmail)); candidate != "" {
			return candidate
		}
		if candidate := sanitizeImportAliasCandidate(info.ChatGPTAccountID); candidate != "" {
			return candidate
		}
	}

	baseName := filepath.Base(filepath.Clean(sourceHome))
	switch strings.ToLower(strings.Trim(baseName, ".")) {
	case "", "codex":
		return randomImportAlias()
	default:
		if candidate := sanitizeImportAliasCandidate(baseName); candidate != "" {
			return candidate
		}
	}
	return randomImportAlias()
}

func importAliasFromConfig(sourceHome string) string {
	configPath := filepath.Join(sourceHome, "config.toml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var config map[string]any
	if err := toml.Unmarshal(data, &config); err != nil {
		return ""
	}
	return sanitizeImportAliasCandidate(findImportAliasHint(config))
}

func findImportAliasHint(v any) string {
	switch node := v.(type) {
	case map[string]any:
		for _, key := range []string{"alias", "profile"} {
			if value, ok := node[key].(string); ok && strings.TrimSpace(value) != "" {
				return value
			}
		}
		for _, child := range node {
			if hint := findImportAliasHint(child); hint != "" {
				return hint
			}
		}
	case []any:
		for _, child := range node {
			if hint := findImportAliasHint(child); hint != "" {
				return hint
			}
		}
	}
	return ""
}

func emailAliasLocalPart(email string) string {
	email = strings.TrimSpace(email)
	if idx := strings.IndexByte(email, '@'); idx >= 0 {
		email = email[:idx]
	}
	return email
}

func sanitizeImportAliasCandidate(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}

	var b strings.Builder
	lastSeparator := false
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			if b.Len() == 0 && !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
				continue
			}
			b.WriteRune(r)
			lastSeparator = false
		default:
			if b.Len() == 0 || lastSeparator {
				continue
			}
			b.WriteByte('-')
			lastSeparator = true
		}
	}

	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return ""
	}
	if len(out) > 64 {
		out = strings.TrimRight(out[:64], "._-")
	}
	if out == "" {
		return ""
	}
	first := out[0]
	if (first < 'a' || first > 'z') && (first < '0' || first > '9') {
		return ""
	}
	return out
}

func localImportAliasSet(store *lb.Store) map[string]struct{} {
	taken := map[string]struct{}{}
	for _, account := range store.Snapshot().Accounts {
		if alias := strings.ToLower(strings.TrimSpace(account.Alias)); alias != "" {
			taken[alias] = struct{}{}
		}
	}
	return taken
}

func remoteImportAliasSet(proxyURL string) map[string]struct{} {
	accounts, err := remoteAdminListAccountsWithClient(remoteAdminFallbackClient(), proxyURL)
	if err != nil {
		return nil
	}
	taken := make(map[string]struct{}, len(accounts))
	for _, account := range accounts {
		if alias := strings.ToLower(strings.TrimSpace(account.Alias)); alias != "" {
			taken[alias] = struct{}{}
		}
	}
	return taken
}

func makeUniqueImportAlias(base string, taken map[string]struct{}) string {
	base = sanitizeImportAliasCandidate(base)
	if base == "" {
		base = randomImportAlias()
	}
	if !importAliasTaken(base, taken) {
		return base
	}
	for i := 2; i < 1000; i++ {
		candidate := appendImportAliasSuffix(base, strconv.Itoa(i))
		if !importAliasTaken(candidate, taken) {
			return candidate
		}
	}
	for {
		candidate := appendImportAliasSuffix(base, randomImportAliasSuffix())
		if !importAliasTaken(candidate, taken) {
			return candidate
		}
	}
}

func appendImportAliasSuffix(base, suffix string) string {
	suffix = sanitizeImportAliasCandidate(suffix)
	if suffix == "" {
		suffix = randomImportAliasSuffix()
	}
	if base == "" {
		base = "import"
	}
	maxBaseLen := 64 - len(suffix) - 1
	if maxBaseLen < 1 {
		return randomImportAlias()
	}
	if len(base) > maxBaseLen {
		base = strings.TrimRight(base[:maxBaseLen], "._-")
	}
	if base == "" {
		base = "import"
	}
	return base + "-" + suffix
}

func importAliasTaken(alias string, taken map[string]struct{}) bool {
	if len(taken) == 0 {
		return false
	}
	_, ok := taken[strings.ToLower(alias)]
	return ok
}

func randomImportAlias() string {
	return "import-" + randomImportAliasSuffix()
}

func randomImportAliasSuffix() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
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
  pin     pin account selection to alias
  unpin   clear pinned account selection

Run 'codexlb account <subcommand> --help' for detailed flags.
`)
}

func printUsage() {
	fmt.Print(`codexlb - local multi-account proxy for Codex CLI

Usage:
  codexlb <command> [flags]

Commands:
  proxy    Run proxy server (or use 'proxy logs')
  account  Manage enrolled accounts (login/import/list/rm/pin/unpin)
  login-with  Run a Dockerized browser login flow and import the resulting auth into host CODEX_HOME
  status   Show runtime status table from running proxy
  run      Run codex with proxy endpoint environment wiring

Run 'codexlb <command> --help' for detailed flags and examples.

Environment:
  CODEXLB_ROOT       default state directory used by --root
  CODEXLB_PROXY_URL  default proxy/admin URL used by --proxy-url
  CODEXLB_CODEX_BIN  default codex binary used by account login/run
  CODEXLB_PROXY_NAME  proxy name override for the running proxy
  CODEXLB_CHILD_PROXY_URLS  default child proxy URLs for proxy chaining
`)
}

func parseProxyURLListEnv(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', '\n', '\r', '\t', ' ':
			return true
		default:
			return false
		}
	})
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		url := strings.TrimRight(strings.TrimSpace(part), "/")
		if url == "" {
			continue
		}
		if _, ok := seen[url]; ok {
			continue
		}
		seen[url] = struct{}{}
		out = append(out, url)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
