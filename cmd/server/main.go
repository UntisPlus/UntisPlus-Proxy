package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"untis-proxy/internal/proxy"
	"untis-proxy/internal/session"
	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

// buildVersion is the version the binary reports. Release builds overwrite it
// with -ldflags "-X main.buildVersion=<tag>", so an image knows what it is
// without anyone having to pass a flag or set an environment variable that can
// disagree with the tag it was published under. Plain `go build` leaves it "dev".
var buildVersion = "dev"

func main() {
	// 8509 is the default for both native runs and the Docker image; the
	// container can still override it with UNTIS_ADDR.
	addr := flag.String("addr", ":8509", "listen address")
	server := flag.String("server", "", "fallback upstream host; normally auto-resolved per school via WebUntis searchSchool")
	school := flag.String("school", "", "school name — REQUIRED, it is the key all data is stored under")
	db := flag.String("db", "untis.db", "sqlite database path")
	ttl := flag.Duration("ttl", 5*time.Minute, "timetable cache TTL")
	yearStart := flag.String("year-start", "", "school year start override (default: auto-derived)")
	yearEnd := flag.String("year-end", "", "school year end override (default: auto-derived)")
	env := flag.String("env", "", "deployment mode (dev|beta|prod); defaults to UNTIS_ENV, else dev")
	version := flag.String("version", buildVersion, "reported build version (baked in at build time; override only for a local build)")
	poll := flag.Duration("poll-interval", 60*time.Second, "timetable change-detection poll interval")
	ntfyBase := flag.String("ntfy-base", "https://ntfy.sh", "base URL for ntfy push delivery (self-hosted ntfy server)")
	publicBase := flag.String("public-base", "", "externally reachable base URL (scheme+host) for click-through links in notifications")
	admin := flag.String("admin", "", "comma-separated usernames to bootstrap as admins (once)")
	metricsAddr := flag.String("metrics-addr", "", "bind address for the Prometheus endpoint, e.g. 127.0.0.1:9109; empty disables /metrics entirely, which is the default because the metrics name the school")
	reconRefresh := flag.Int("recon-refresh", 21, "days a class's recon scan horizon may lag before it is re-enumerated")
	reconRescan := flag.Bool("recon-rescan", false, "re-enumerate every pooled class from the year start on boot instead of resuming")

	// Every flag is also settable as UNTIS_<FLAG>, so a compose file or a
	// container runtime can configure the proxy without rewriting its command
	// line. Previously only the Dockerfile's `sh -c` expansion did this, which
	// meant a new flag (e.g. -public-base, -ntfy-base) was silently ignored
	// when set as an env var in compose.
	applyEnvDefaults()

	flag.Parse()

	if *env != "" {
		os.Setenv("UNTIS_ENV", *env)
	}
	if *version != "" {
		os.Setenv("UNTIS_VERSION", *version)
	}

	// The school name is the storage key for the pool, recon, perms, sessions
	// and calendar tokens. Defaulting it to some particular school's name meant
	// a fresh deployment silently inherited that school's identity, so it is
	// required instead: failing here is far better than writing a year of data
	// under an empty or wrong key.
	if strings.TrimSpace(*school) == "" {
		log.Fatal("-school (or UNTIS_SCHOOL) is required: it is the key all data is " +
			"stored under. Set it to the school's WebUntis login name, e.g. " +
			"UNTIS_SCHOOL=myschool ./scripts/run.sh")
	}

	ys, ye := schoolYear(time.Now())
	if *yearStart != "" {
		ys = *yearStart
	}
	if *yearEnd != "" {
		ye = *yearEnd
	}

	st, err := store.Open(*db)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	if err := st.SetDefaultSchool(*school); err != nil {
		log.Printf("backfill users school: %v", err)
	}

	// Seed the -admin flag list once; the marked usernames hold the admin flag
	// in the DB afterwards and can be demoted/promoted via the /admin API or
	// untisctl without -admin re-applying on restart.
	if *admin != "" {
		seeded, err := st.AdminBootstrapSeeded()
		if err == nil && !seeded {
			for _, name := range strings.Split(*admin, ",") {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				if err := st.SetAdmin(name, true); err != nil {
					log.Printf("seed admin %q: %v", name, err)
					continue
				}
				log.Printf("seeded admin: %s", name)
			}
			_ = st.MarkAdminBootstrapSeeded()
		}
	}

	uc := untis.New(untis.Config{Server: *server, School: *school})
	sm := session.NewManager(24 * time.Hour)

	p := proxy.New(st, uc, sm, proxy.Options{
		School:           *school,
		TTL:              *ttl,
		ReconRefreshDays: *reconRefresh,
		ForceRescan:      *reconRescan,
	})
	p.LoadRecon(*school)
	p.StartRecon(*school, ys, ye)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	proxy.SetNtfyBase(*ntfyBase)
	proxy.SetPublicBase(*publicBase)
	pollDone := make(chan struct{})
	go p.StartPollLoop(*school, *poll, pollDone)

	// Deliveries are durably queued by the poller in the same transaction that
	// stamps the new class version; this worker is what actually sends them. It
	// must be running, or notifications queue up and never leave.
	outboxDone := make(chan struct{})
	outboxExited := make(chan struct{})
	go func() {
		defer close(outboxExited)
		p.StartOutboxWorker(5*time.Second, outboxDone)
	}()

	srv := &http.Server{Addr: *addr, Handler: p.Handler()}
	log.Printf("listening on %s (upstream %s, school %s)", *addr, *server, *school)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	// The metrics endpoint gets its own listener so it can be bound to loopback
	// or a private network without putting it on the address the tunnel exposes.
	var metricsSrv *http.Server
	if *metricsAddr != "" {
		metricsSrv = &http.Server{Addr: *metricsAddr, Handler: p.MetricsHandler()}
		log.Printf("metrics listening on %s (not exposed on %s)", *metricsAddr, *addr)
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("metrics server: %v", err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Printf("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		if metricsSrv != nil {
			_ = metricsSrv.Shutdown(shutCtx)
		}
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}

	close(pollDone)
	// Wait for the worker to finish the batch it is on before the store closes,
	// so a delivery in flight is not abandoned mid-request.
	close(outboxDone)
	<-outboxExited
	p.PersistRecon(*school)
	if err := st.Close(); err != nil {
		log.Printf("close store: %v", err)
	}
}

// schoolYear returns the bounds of the current WebUntis school year so the
// recon scan needs no periodic manual maintenance. German school years start
// in August: for any date in Aug..Dec the year runs from 1 Aug of the current
// year to 31 Jul of the next; from Jan..Jul it runs from 1 Aug of the previous
// year to 31 Jul of the current year.
func schoolYear(now time.Time) (string, string) {
	start := time.Date(now.Year(), time.August, 1, 0, 0, 0, 0, now.Location())
	if now.Month() < time.August {
		start = start.AddDate(-1, 0, 0)
	}
	end := start.AddDate(1, 0, 0).AddDate(0, 0, -1)
	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

// applyEnvDefaults seeds every declared flag from its UNTIS_ env var when the
// variable is set. Precedence is: explicit command-line flag > environment >
// flag default. It must run before flag.Parse.
//
// A flag that was actually given on the command line is left alone. The flag
// package records the parsed names in flag.CommandLine.Args() only after Parse,
// so instead we seed the default and let Parse overwrite it: setting the default
// is harmless when the flag is also passed, because Parse assigns the
// command-line value on top of it.
func applyEnvDefaults() { applyEnvTo(flag.CommandLine) }

// applyEnvTo is applyEnvDefaults against an explicit FlagSet, so the precedence
// rules can be tested without mutating the process-global flag set.
func applyEnvTo(fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		key := "UNTIS_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v, ok := os.LookupEnv(key); ok && v != "" {
			// A malformed value must not take the whole process down: leave the
			// default in place and say so.
			if err := f.Value.Set(v); err != nil {
				log.Printf("config: ignoring %s=%q: %v", key, v, err)
				if rerr := f.Value.Set(f.DefValue); rerr != nil {
					log.Printf("config: cannot restore default for %s: %v", f.Name, rerr)
				}
			}
		}
	})
}
