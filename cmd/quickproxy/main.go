package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/admin"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/oauth"
	"quickwork-openai/internal/proxy"
	"quickwork-openai/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		return runServer(nil)
	}
	switch args[0] {
	case "server":
		return runServer(args[1:])
	case "login":
		return login(args[1:])
	case "accounts", "list":
		return listAccounts(args[1:])
	case "refresh":
		return refreshAccounts(args[1:])
	case "logout":
		return logout(args[1:])
	case "passwd":
		return setPassword(args[1:])
	case "config":
		return renderConfig(args[1:])
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		if strings.HasPrefix(args[0], "-") {
			return runServer(args)
		}
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `quickwork-openai - Amazon Quick to OpenAI-compatible API

Usage:
  quickproxy server   [-config config.json]
  quickproxy login    [-config config.json] [-provider social|enterprise|gateway|social_gateway]
                      [-stage prod|beta|integ|dev] [-region REGION] [-port 18080]
                      [-timeout 1h] [-open] [-manual]
  quickproxy accounts [-config config.json]
  quickproxy refresh  [-config config.json] [-account ID]
  quickproxy logout   [-config config.json] (-all | -account ID)
  quickproxy passwd   [-config config.json] [-password PW]
  quickproxy config   [-config config.json] [-from-env=false]

Running without a subcommand is equivalent to "server".

config.json is created with defaults on first use. Accounts are stored in the
SQLite database named by "database" in config.json. When multiple accounts are
present, requests rotate through them round-robin.
`)
}

func loadConfig(path string) (*config.Config, error) {
	return config.Load(path)
}

func openStore(cfg *config.Config) (*store.Store, error) {
	st, err := store.Open(cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", cfg.Database, err)
	}
	return st, nil
}

func runServer(args []string) int {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	mgr := accounts.New(st, cfg)
	count, _ := mgr.EnabledCount(context.Background())
	px := proxy.NewServer(cfg, mgr)
	adminUI := admin.New(cfg, st, mgr, px)
	px.MountAdmin("/admin/", adminUI.Handler())
	handler := px.Handler()
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("quickwork-openai listening on %s (config=%s db=%s accounts=%d auth=%v)",
		cfg.Addr, cfg.Path, cfg.Database, count, len(cfg.APIKeys) > 0)
	log.Printf("models=%s default_model=%s request_timeout=%s refresh_before=%s max_attempts=%d keep_sessions=%v session_reuse=%v session_ttl=%s",
		strings.Join(cfg.Models, ","), cfg.DefaultModel, cfg.RequestTimeout.Duration(),
		cfg.RefreshBefore.Duration(), cfg.MaxAttempts, cfg.KeepSessions, cfg.SessionReuse, cfg.SessionTTL.Duration())
	if count == 0 {
		log.Printf("no accounts stored; run `quickproxy login` to add one")
	}
	switch {
	case !cfg.WebUIReady():
		log.Printf("management interface off: set webui.password or run `quickproxy passwd -password <pw>` to enable /admin")
	default:
		log.Printf("management interface on: %s/admin/", baseURL(cfg.Addr))
	}

	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Refresh tokens that enter the refresh window even when no chat request
	// selects the account, so an idle rotation never lets a token expire.
	refreshInterval := accounts.RefreshInterval(cfg.RefreshBefore.Duration())
	log.Printf("account refresh: window=%s sweep=%s", cfg.RefreshBefore.Duration(), refreshInterval)
	go mgr.KeepFresh(stop, refreshInterval, log.Printf)

	listenErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
			cancel()
		}
	}()

	<-stop.Done()
	log.Printf("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	select {
	case err := <-listenErr:
		log.Printf("listen: %v", err)
		return 1
	default:
		return 0
	}
}

func login(args []string) int {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	provider := fs.String("provider", "", "social, enterprise, gateway or social_gateway (default from config)")
	stage := fs.String("stage", "", "prod, beta, integ or dev (default from config)")
	region := fs.String("region", "", "IDC region (social) or enterprise discovery region prefix")
	port := fs.Int("port", 0, "localhost callback port (default from config)")
	timeout := fs.Duration("timeout", 0, "wait duration for the browser callback (default from config)")
	open := fs.Bool("open", false, "open the login URL in the default browser")
	manual := fs.Bool("manual", false, "paste browser callback URLs manually (for servers without a reachable localhost)")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	if *provider == "" {
		*provider = cfg.OAuth.Provider
	}
	if *stage == "" {
		*stage = cfg.OAuth.BuildStage
	}
	if *port == 0 {
		*port = cfg.OAuth.CallbackPort
	}
	if *timeout == 0 {
		*timeout = cfg.OAuth.Timeout.Duration()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger := log.New(os.Stdout, "[login] ", 0)
	opts := oauth.LoginOptions{
		Provider:    *provider,
		Stage:       *stage,
		Region:      *region,
		Port:        *port,
		Timeout:     *timeout,
		AppVersion:  cfg.AppVersion,
		OpenBrowser: *open,
		Logf:        logger.Printf,
	}
	var acct *store.Account
	if *manual {
		acct, err = oauth.LoginManual(ctx, opts, st, os.Stdin, os.Stdout)
	} else {
		acct, err = oauth.Login(ctx, opts, st)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "[login] failed:", err)
		return 1
	}
	fmt.Printf("account %d ready: %s (%s, %s)\n", acct.ID, displayAccount(acct), acct.Provider, acct.WSHost)
	return 0
}

func listAccounts(args []string) int {
	fs := flag.NewFlagSet("accounts", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	list, err := st.List(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "list accounts:", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Println("no accounts; run `quickproxy login`")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tEMAIL\tPROVIDER\tSTAGE\tREGION\tEXPIRES\tUSES\tSTATUS")
	now := time.Now()
	for i := range list {
		a := &list[i]
		status := "ready"
		if a.Disabled {
			status = "disabled"
		} else if a.ExpiresAt > 0 && a.ExpiresAt <= now.Unix() {
			status = "expired"
		}
		if a.LastError != "" {
			status += " (" + a.LastError + ")"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			a.ID, displayAccount(a), a.Provider, a.BuildStage, a.Region,
			formatEpoch(a.ExpiresAt), a.UseCount, status)
	}
	_ = tw.Flush()
	return 0
}

func refreshAccounts(args []string) int {
	fs := flag.NewFlagSet("refresh", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	accountID := fs.Int64("account", 0, "refresh only this account ID (default: all)")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	mgr := accounts.New(st, cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *accountID > 0 {
		a, err := mgr.RefreshOne(ctx, *accountID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "account %d: %v\n", *accountID, err)
			return 1
		}
		fmt.Printf("account %d refreshed: %s expires %s\n", a.ID, displayAccount(a), formatEpoch(a.ExpiresAt))
		return 0
	}

	results, err := mgr.RefreshAll(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "refresh:", err)
		return 1
	}
	if len(results) == 0 {
		fmt.Println("no accounts; run `quickproxy login`")
		return 0
	}
	failed := 0
	for _, res := range results {
		if res.Err != nil {
			failed++
			id := int64(0)
			if res.Account != nil {
				id = res.Account.ID
			}
			fmt.Fprintf(os.Stderr, "account %d: %v\n", id, res.Err)
			continue
		}
		fmt.Printf("account %d refreshed: %s expires %s\n",
			res.Account.ID, displayAccount(res.Account), formatEpoch(res.Account.ExpiresAt))
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func logout(args []string) int {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	accountID := fs.Int64("account", 0, "delete only this account ID")
	all := fs.Bool("all", false, "delete every stored account")
	_ = fs.Parse(args)

	if *accountID <= 0 && !*all {
		fmt.Fprintln(os.Stderr, "logout requires -all or -account ID")
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	ctx := context.Background()
	if *all {
		n, err := st.DeleteAll(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "logout:", err)
			return 1
		}
		fmt.Printf("removed %d account(s)\n", n)
		return 0
	}
	ok, err := st.Delete(ctx, *accountID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logout:", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "account %d not found\n", *accountID)
		return 1
	}
	fmt.Printf("removed account %d\n", *accountID)
	return 0
}

func displayAccount(a *store.Account) string {
	for _, v := range []string{a.Email, a.AccountName, a.AWSAccountID} {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "-"
}

func formatEpoch(v int64) string {
	if v <= 0 {
		return "unknown"
	}
	return time.Unix(v, 0).Local().Format("2006-01-02 15:04")
}

// adminURL turns the configured listen address into something clickable.
func baseURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// setPassword stores a pbkdf2-sha256 hash of the management password in
// config.json and clears any plaintext password, which is what the login
// endpoint reads.
// renderConfig writes config.json with environment overrides applied. The
// container entrypoint runs it before starting the server so .env drives the
// deployment while accounts and other fields stay in the data volume.
func renderConfig(args []string) int {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	fromEnv := fs.Bool("from-env", true, "apply ADDR, API_KEYS, WEBUI_PASSWORD and friends from the environment")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	var applied []string
	if *fromEnv {
		applied, err = config.ApplyEnv(cfg, os.LookupEnv)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			return 1
		}
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if err := config.Save(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "save config:", err)
		return 1
	}
	fmt.Printf("config %s written: addr=%s models=%s api_keys=%d webui=%s\n",
		cfg.Path, cfg.Addr, strings.Join(cfg.Models, ","), len(cfg.APIKeys), webuiState(cfg))
	if len(applied) == 0 {
		fmt.Println("env overrides: none")
	} else {
		fmt.Printf("env overrides: %s\n", strings.Join(applied, ", "))
	}
	return 0
}

// webuiState summarizes whether /admin will be reachable after this config.
func webuiState(cfg *config.Config) string {
	if cfg.WebUI == nil || !cfg.WebUI.Enabled {
		return "off"
	}
	switch {
	case strings.TrimSpace(cfg.WebUI.PasswordHash) != "":
		return "on (password_hash)"
	case strings.TrimSpace(cfg.WebUI.Password) != "":
		return "on (password)"
	default:
		return "on but no password -> /admin returns 503"
	}
}

func setPassword(args []string) int {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	password := fs.String("password", "", "management password (read from stdin when empty)")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	value := *password
	if value == "" {
		fmt.Fprint(os.Stderr, "management password: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(os.Stderr, "read password:", err)
			return 1
		}
		value = strings.TrimRight(line, "\r\n")
	}
	hash, err := config.HashPassword(value)
	if err != nil {
		fmt.Fprintln(os.Stderr, "passwd:", err)
		return 1
	}
	if cfg.WebUI == nil {
		cfg.WebUI = &config.WebUI{}
	}
	cfg.WebUI.Enabled = true
	cfg.WebUI.PasswordHash = hash
	cfg.WebUI.Password = ""
	if cfg.WebUI.SessionTTL <= 0 {
		cfg.WebUI.SessionTTL = config.Duration(12 * time.Hour)
	}
	if cfg.WebUI.MaxAttempts <= 0 {
		cfg.WebUI.MaxAttempts = 10
	}
	if err := config.Save(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "save config:", err)
		return 1
	}
	fmt.Printf("management password updated in %s (pbkdf2-sha256)\n", cfg.Path)
	fmt.Printf("restart `quickproxy server` if it is running, then open %s/admin/\n", baseURL(cfg.Addr))
	return 0
}
