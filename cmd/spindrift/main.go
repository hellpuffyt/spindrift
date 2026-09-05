// Command spindrift is the server binary.
//
//	spindrift serve CONFIG      run; SIGHUP reloads the config, SIGINT/SIGTERM drain and exit
//	spindrift check CONFIG      parse and print the pipeline
//	spindrift bench URL [-c N] [-n N]   keep-alive load generator
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hellpuffyt/spindrift/config"
	"github.com/hellpuffyt/spindrift/server"
)

func main() {
	if len(os.Args) < 3 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2])
	case "check":
		cfg, err := config.Load(os.Args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", os.Args[2], err)
			os.Exit(1)
		}
		fmt.Printf("listen %s  log %s  max_body %d  max_header %d\n", cfg.Listen, cfg.Log, cfg.MaxBody, cfg.MaxHeader)
		for _, r := range cfg.Rules {
			kind := "route     "
			if r.Middleware {
				kind = "middleware"
			}
			m := "*"
			if r.Methods != nil {
				m = fmt.Sprint(r.Methods)
			}
			fmt.Printf("%3d  %s %-14s %-24s -> %s\n", r.Line, kind, m, r.Pattern, r.Action.Name)
		}
	case "bench":
		bench(os.Args[2], os.Args[3:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: spindrift serve CONFIG | check CONFIG | bench URL [-c N] [-n N]")
	os.Exit(2)
}

func serve(path string) {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		os.Exit(1)
	}
	s := server.New(cfg, os.Stdout)
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", cfg.Listen, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "spindrift listening on %s (%d rules) — SIGHUP reloads, SIGINT drains\n", ln.Addr(), len(cfg.Rules))

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigs {
			if sig == syscall.SIGHUP {
				next, err := config.Load(path)
				if err != nil {
					fmt.Fprintf(os.Stderr, "reload failed, keeping current config: %v\n", err)
					continue
				}
				s.Reload(next)
				fmt.Fprintf(os.Stderr, "reloaded %s (%d rules)\n", path, len(next.Rules))
				continue
			}
			fmt.Fprintln(os.Stderr, "draining…")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := s.Shutdown(ctx)
			cancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "shutdown: %v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}()
	if err := s.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}

func bench(url string, args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	c := fs.Int("c", 32, "concurrent connections")
	n := fs.Int("n", 20000, "total requests")
	fs.Parse(args)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: *c, MaxConnsPerHost: *c}}
	var wg sync.WaitGroup
	var done, errs atomic.Int64
	lat := make([][]time.Duration, *c)
	start := time.Now()
	for w := 0; w < *c; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for done.Add(1) <= int64(*n) {
				t := time.Now()
				r, err := client.Get(url)
				if err != nil {
					errs.Add(1)
					continue
				}
				io.Copy(io.Discard, r.Body)
				r.Body.Close()
				if r.StatusCode >= 400 {
					errs.Add(1)
				}
				lat[w] = append(lat[w], time.Since(t))
			}
		}(w)
	}
	wg.Wait()
	el := time.Since(start)
	var all []time.Duration
	for _, l := range lat {
		all = append(all, l...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	pct := func(p float64) time.Duration {
		if len(all) == 0 {
			return 0
		}
		return all[int(float64(len(all)-1)*p)]
	}
	fmt.Printf("%d requests, %d connections, %d errors in %.2fs\n", *n, *c, errs.Load(), el.Seconds())
	fmt.Printf("%.0f req/s   p50 %v   p90 %v   p99 %v   max %v\n", float64(*n)/el.Seconds(), pct(0.5), pct(0.9), pct(0.99), pct(1))
}
