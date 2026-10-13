// wukongim-v3-compat runs beside an unmodified WuKongIM v3 process. The two
// processes must share a network namespace for the unsigned callback listener.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Mininglamp-OSS/octo-server/internal/imcompat"
)

var Version = "dev"
var Commit = "unknown"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "wukongim-v3-compat:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("wukongim-v3-compat", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts imcompat.Options
	var apiListen, callbackListen, apiTokenFile, webhookSecretFile, managerPasswordFile string
	var version bool
	flags.StringVar(&apiListen, "api-listen", "127.0.0.1:5001", "private IM API listen address")
	flags.StringVar(&callbackListen, "webhook-listen", "127.0.0.1:5004", "numeric loopback callback listen address")
	flags.StringVar(&opts.CoreURL, "core-url", "http://127.0.0.1:5002", "WuKongIM v3 API root URL")
	flags.StringVar(&opts.ServerWebhookURL, "server-webhook-url", "", "Octo Server signed webhook URL (/v1/webhook)")
	flags.StringVar(&apiTokenFile, "api-token-file", "", "file containing the Server wukongIM.managerToken")
	flags.StringVar(&webhookSecretFile, "webhook-secret-file", "", "file containing Server TS_WEBHOOK_SECRET_KEY")
	flags.StringVar(&opts.ManagerURL, "manager-url", "", "optional authenticated Manager root URL")
	flags.StringVar(&opts.ManagerUser, "manager-user", "", "Manager account with cluster.channel read permission")
	flags.StringVar(&managerPasswordFile, "manager-password-file", "", "file containing the Manager password")
	flags.DurationVar(&opts.Timeout, "request-timeout", 10*time.Second, "timeout for one upstream request")
	flags.Int64Var(&opts.MaxBodyBytes, "max-body-bytes", imcompat.DefaultMaxBodyBytes, "request and adapted response byte limit")
	flags.BoolVar(&version, "version", false, "print build identity")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if version {
		_, err := fmt.Fprintf(stdout, "wukongim-v3-compat %s %s\n", Version, Commit)
		return err
	}
	if !imcompat.LoopbackAddress(callbackListen) {
		return errors.New("webhook-listen must use a numeric loopback IP; colocate the adapter with IM")
	}
	var err error
	if opts.APIToken, err = readSecret(apiTokenFile); err != nil {
		return fmt.Errorf("API token: %w", err)
	}
	if opts.WebhookSecret, err = readSecret(webhookSecretFile); err != nil {
		return fmt.Errorf("webhook secret: %w", err)
	}
	if managerPasswordFile != "" {
		if opts.ManagerPassword, err = readSecret(managerPasswordFile); err != nil {
			return fmt.Errorf("Manager password: %w", err)
		}
	}
	adapter, err := imcompat.New(opts)
	if err != nil {
		return err
	}
	apiSocket, err := net.Listen("tcp", apiListen)
	if err != nil {
		return errors.New("could not bind API listener")
	}
	defer apiSocket.Close()
	callbackSocket, err := net.Listen("tcp", callbackListen)
	if err != nil {
		return errors.New("could not bind callback listener")
	}
	defer callbackSocket.Close()
	api := &http.Server{Handler: adapter.APIHandler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: opts.Timeout, WriteTimeout: opts.Timeout*3 + time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10}
	callback := &http.Server{Handler: adapter.WebhookHandler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: opts.Timeout, WriteTimeout: opts.Timeout + time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 2)
	go func() { done <- api.Serve(apiSocket) }()
	go func() { done <- callback.Serve(callbackSocket) }()
	fmt.Fprintf(stdout, "wukongim-v3-compat started version=%s commit=%s api=%s webhook=%s\n", Version, Commit, apiSocket.Addr(), callbackSocket.Addr())
	select {
	case <-ctx.Done():
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	apiErr := api.Shutdown(shutdownCtx)
	callbackErr := callback.Shutdown(shutdownCtx)
	return errors.Join(err, apiErr, callbackErr)
}

func readSecret(path string) (string, error) {
	if path == "" {
		return "", errors.New("secret file is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open secret file")
	}
	defer f.Close()
	value, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(value) > 4096 {
		return "", errors.New("invalid or oversized secret file")
	}
	secret := strings.TrimSpace(string(value))
	if secret == "" {
		return "", errors.New("secret file is empty")
	}
	return secret, nil
}
