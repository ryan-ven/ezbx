package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ryan-ven/ezbx/internal/adapter"
	"github.com/ryan-ven/ezbx/internal/detect"
	"github.com/ryan-ven/ezbx/internal/gateway"
	"github.com/ryan-ven/ezbx/providers"
)

func runGateway(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gateway serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:8000", "address to listen on")
	upstream := fs.String("upstream", "", "engine address; empty means use the one engine that is detected")
	engine := fs.String("engine", "", "engine id shown in replies (set automatically when detected)")
	keyEnv := fs.String("key-env", "", "NAME of an environment variable holding the engine's key")
	logFile := fs.String("log", "-", "where to write the JSON log; - means standard output")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// The key comes from the environment, so it never appears in a command line.
	key := ""
	if *keyEnv != "" {
		key = os.Getenv(*keyEnv)
		if key == "" {
			fmt.Fprintf(stderr, "The environment variable %s is empty or not set.\n", *keyEnv)
			return 2
		}
	}

	if *upstream == "" {
		eng, err := onlyEngine()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		*upstream = eng.URL
		if *engine == "" {
			*engine = eng.ID
		}
	}

	logOut := stdout
	if *logFile != "-" {
		f, err := os.OpenFile(*logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, "cannot open the log file:", err)
			return 1
		}
		defer f.Close()
		logOut = f
	}

	gw, err := gateway.New(gateway.Config{Upstream: *upstream, EngineID: *engine, Key: key, Log: logOut})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No write timeout: a streamed answer can legitimately take minutes.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	fmt.Fprintf(stderr, "gateway listening on %s, forwarding to %s (engine %q)\n", *listen, *upstream, *engine)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// onlyEngine detects engines and returns the single one found. If there are
// none or several, it says so and asks the user to choose with --upstream.
func onlyEngine() (detect.Engine, error) {
	adapters, err := adapter.LoadFS(providers.FS)
	if err != nil {
		return detect.Engine{}, fmt.Errorf("internal error loading adapters: %w", err)
	}
	d := &detect.Detector{Adapters: adapters, Run: runCommand}
	res := d.Detect(context.Background(), nil)

	switch len(res.Engines) {
	case 1:
		return res.Engines[0], nil
	case 0:
		return detect.Engine{}, errors.New("no inference engine found. Start one, or pass --upstream http://127.0.0.1:PORT")
	default:
		return detect.Engine{}, errors.New("more than one engine found. Choose one with --upstream (see: ezbx engine detect)")
	}
}
