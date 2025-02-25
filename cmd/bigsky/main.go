package main

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bluesky-social/indigo/api"
	libbgs "github.com/bluesky-social/indigo/bgs"
	"github.com/bluesky-social/indigo/carstore"
	"github.com/bluesky-social/indigo/did"
	"github.com/bluesky-social/indigo/events"
	"github.com/bluesky-social/indigo/indexer"
	"github.com/bluesky-social/indigo/notifs"
	"github.com/bluesky-social/indigo/plc"
	"github.com/bluesky-social/indigo/repomgr"
	"github.com/bluesky-social/indigo/util"
	"github.com/bluesky-social/indigo/util/cliutil"
	"github.com/bluesky-social/indigo/xrpc"

	_ "github.com/joho/godotenv/autoload"
	_ "go.uber.org/automaxprocs"

	"github.com/carlmjohnson/versioninfo"
	logging "github.com/ipfs/go-log"
	"github.com/urfave/cli/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"gorm.io/plugin/opentelemetry/tracing"
)

var log = logging.Logger("bigsky")

func init() {
	// control log level using, eg, GOLOG_LOG_LEVEL=debug
	//logging.SetAllLoggers(logging.LevelDebug)
}

func main() {
	if err := run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {

	app := cli.App{
		Name:    "bigsky",
		Usage:   "atproto Relay daemon",
		Version: versioninfo.Short(),
	}

	app.Flags = []cli.Flag{
		&cli.BoolFlag{
			Name: "jaeger",
		},
		&cli.StringFlag{
			Name:    "db-url",
			Usage:   "database connection string for BGS database",
			Value:   "sqlite://./data/bigsky/bgs.sqlite",
			EnvVars: []string{"DATABASE_URL"},
		},
		&cli.StringFlag{
			Name:    "carstore-db-url",
			Usage:   "database connection string for carstore database",
			Value:   "sqlite://./data/bigsky/carstore.sqlite",
			EnvVars: []string{"CARSTORE_DATABASE_URL"},
		},
		&cli.BoolFlag{
			Name: "db-tracing",
		},
		&cli.StringFlag{
			Name:    "data-dir",
			Usage:   "path of directory for CAR files and other data",
			Value:   "data/bigsky",
			EnvVars: []string{"RELAY_DATA_DIR", "DATA_DIR"},
		},
		&cli.StringFlag{
			Name:    "plc-host",
			Usage:   "method, hostname, and port of PLC registry",
			Value:   "https://plc.directory",
			EnvVars: []string{"ATP_PLC_HOST"},
		},
		&cli.BoolFlag{
			Name:  "crawl-insecure-ws",
			Usage: "when connecting to PDS instances, use ws:// instead of wss://",
		},
		&cli.BoolFlag{
			Name:    "spidering",
			Value:   false,
			EnvVars: []string{"RELAY_SPIDERING", "BGS_SPIDERING"},
		},
		&cli.StringFlag{
			Name:  "api-listen",
			Value: ":2470",
		},
		&cli.StringFlag{
			Name:    "metrics-listen",
			Value:   ":2471",
			EnvVars: []string{"RELAY_METRICS_LISTEN", "BGS_METRICS_LISTEN"},
		},
		&cli.StringFlag{
			Name:    "disk-persister-dir",
			Usage:   "set directory for disk persister (implicitly enables disk persister)",
			EnvVars: []string{"RELAY_PERSISTER_DIR"},
		},
		&cli.StringFlag{
			Name:    "admin-key",
			EnvVars: []string{"RELAY_ADMIN_KEY", "BGS_ADMIN_KEY"},
		},
		&cli.StringSliceFlag{
			Name:    "handle-resolver-hosts",
			EnvVars: []string{"HANDLE_RESOLVER_HOSTS"},
		},
		&cli.IntFlag{
			Name:    "max-carstore-connections",
			EnvVars: []string{"MAX_CARSTORE_CONNECTIONS"},
			Value:   40,
		},
		&cli.IntFlag{
			Name:    "max-metadb-connections",
			EnvVars: []string{"MAX_METADB_CONNECTIONS"},
			Value:   40,
		},
		&cli.DurationFlag{
			Name:    "compact-interval",
			EnvVars: []string{"RELAY_COMPACT_INTERVAL", "BGS_COMPACT_INTERVAL"},
			Value:   4 * time.Hour,
			Usage:   "interval between compaction runs, set to 0 to disable scheduled compaction",
		},
		&cli.StringFlag{
			Name:    "resolve-address",
			EnvVars: []string{"RESOLVE_ADDRESS"},
			Value:   "1.1.1.1:53",
		},
		&cli.BoolFlag{
			Name:    "force-dns-udp",
			EnvVars: []string{"FORCE_DNS_UDP"},
		},
		&cli.IntFlag{
			Name:    "max-fetch-concurrency",
			Value:   100,
			EnvVars: []string{"MAX_FETCH_CONCURRENCY"},
		},
		&cli.StringFlag{
			Name:    "env",
			Value:   "dev",
			EnvVars: []string{"ENVIRONMENT"},
			Usage:   "declared hosting environment (prod, qa, etc); used in metrics",
		},
		&cli.StringFlag{
			Name:    "otel-exporter-otlp-endpoint",
			EnvVars: []string{"OTEL_EXPORTER_OTLP_ENDPOINT"},
		},
		&cli.StringFlag{
			Name:    "bsky-social-rate-limit-skip",
			EnvVars: []string{"BSKY_SOCIAL_RATE_LIMIT_SKIP"},
			Usage:   "ratelimit bypass secret token for *.bsky.social domains",
		},
		&cli.IntFlag{
			Name:    "default-repo-limit",
			Value:   100,
			EnvVars: []string{"RELAY_DEFAULT_REPO_LIMIT"},
		},
		&cli.IntFlag{
			Name:    "concurrency-per-pds",
			EnvVars: []string{"RELAY_CONCURRENCY_PER_PDS"},
			Value:   100,
		},
		&cli.IntFlag{
			Name:    "max-queue-per-pds",
			EnvVars: []string{"RELAY_MAX_QUEUE_PER_PDS"},
			Value:   1_000,
		},
		&cli.IntFlag{
			Name:    "did-cache-size",
			EnvVars: []string{"RELAY_DID_CACHE_SIZE"},
			Value:   5_000_000,
		},
		&cli.DurationFlag{
			Name:    "event-playback-ttl",
			Usage:   "time to live for event playback buffering (only applies to disk persister)",
			EnvVars: []string{"RELAY_EVENT_PLAYBACK_TTL"},
			Value:   72 * time.Hour,
		},
	}

	app.Action = runBigsky
	return app.Run(os.Args)
}

func setupOTEL(cctx *cli.Context) error {

	env := cctx.String("env")
	if env == "" {
		env = "dev"
	}
	if cctx.Bool("jaeger") {
		url := "http://localhost:14268/api/traces"
		exp, err := jaeger.New(jaeger.WithCollectorEndpoint(jaeger.WithEndpoint(url)))
		if err != nil {
			return err
		}
		tp := tracesdk.NewTracerProvider(
			// Always be sure to batch in production.
			tracesdk.WithBatcher(exp),
			// Record information about this application in a Resource.
			tracesdk.WithResource(resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceNameKey.String("bgs"),
				attribute.String("env", env),         // DataDog
				attribute.String("environment", env), // Others
				attribute.Int64("ID", 1),
			)),
		)

		otel.SetTracerProvider(tp)
	}

	// Enable OTLP HTTP exporter
	// For relevant environment variables:
	// https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlptrace#readme-environment-variables
	// At a minimum, you need to set
	// OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
	if ep := cctx.String("otel-exporter-otlp-endpoint"); ep != "" {
		log.Infow("setting up trace exporter", "endpoint", ep)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			log.Fatalw("failed to create trace exporter", "error", err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := exp.Shutdown(ctx); err != nil {
				log.Errorw("failed to shutdown trace exporter", "error", err)
			}
		}()

		tp := tracesdk.NewTracerProvider(
			tracesdk.WithBatcher(exp),
			tracesdk.WithResource(resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceNameKey.String("bgs"),
				attribute.String("env", env),         // DataDog
				attribute.String("environment", env), // Others
				attribute.Int64("ID", 1),
			)),
		)
		otel.SetTracerProvider(tp)
	}

	return nil
}

func runBigsky(cctx *cli.Context) error {
	// Trap SIGINT to trigger a shutdown.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	// start observability/tracing (OTEL and jaeger)
	if err := setupOTEL(cctx); err != nil {
		return err
	}

	// ensure data directory exists; won't error if it does
	datadir := cctx.String("data-dir")
	csdir := filepath.Join(datadir, "carstore")
	if err := os.MkdirAll(datadir, os.ModePerm); err != nil {
		return err
	}

	log.Infow("setting up main database")
	dburl := cctx.String("db-url")
	db, err := cliutil.SetupDatabase(dburl, cctx.Int("max-metadb-connections"))
	if err != nil {
		return err
	}

	log.Infow("setting up carstore database")
	csdburl := cctx.String("carstore-db-url")
	csdb, err := cliutil.SetupDatabase(csdburl, cctx.Int("max-carstore-connections"))
	if err != nil {
		return err
	}

	if cctx.Bool("db-tracing") {
		if err := db.Use(tracing.NewPlugin()); err != nil {
			return err
		}
		if err := csdb.Use(tracing.NewPlugin()); err != nil {
			return err
		}
	}

	os.MkdirAll(filepath.Dir(csdir), os.ModePerm)
	cstore, err := carstore.NewCarStore(csdb, csdir)
	if err != nil {
		return err
	}

	mr := did.NewMultiResolver()

	didr := &api.PLCServer{Host: cctx.String("plc-host")}
	mr.AddHandler("plc", didr)

	webr := did.WebResolver{}
	if cctx.Bool("crawl-insecure-ws") {
		webr.Insecure = true
	}
	mr.AddHandler("web", &webr)

	cachedidr := plc.NewCachingDidResolver(mr, time.Hour*24, cctx.Int("did-cache-size"))

	kmgr := indexer.NewKeyManager(cachedidr, nil)

	repoman := repomgr.NewRepoManager(cstore, kmgr)

	var persister events.EventPersistence

	if dpd := cctx.String("disk-persister-dir"); dpd != "" {
		log.Infow("setting up disk persister")

		pOpts := events.DefaultDiskPersistOptions()
		pOpts.Retention = cctx.Duration("event-playback-ttl")
		dp, err := events.NewDiskPersistence(dpd, "", db, pOpts)
		if err != nil {
			return fmt.Errorf("setting up disk persister: %w", err)
		}
		persister = dp
	} else {
		dbp, err := events.NewDbPersistence(db, cstore, nil)
		if err != nil {
			return fmt.Errorf("setting up db event persistence: %w", err)
		}
		persister = dbp
	}

	evtman := events.NewEventManager(persister)

	notifman := &notifs.NullNotifs{}

	rf := indexer.NewRepoFetcher(db, repoman, cctx.Int("max-fetch-concurrency"))

	ix, err := indexer.NewIndexer(db, notifman, evtman, cachedidr, rf, true, cctx.Bool("spidering"), false)
	if err != nil {
		return err
	}

	rlskip := cctx.String("bsky-social-rate-limit-skip")
	ix.ApplyPDSClientSettings = func(c *xrpc.Client) {
		if c.Client == nil {
			c.Client = util.RobustHTTPClient()
		}
		if strings.HasSuffix(c.Host, ".bsky.network") {
			c.Client.Timeout = time.Minute * 30
			if rlskip != "" {
				c.Headers = map[string]string{
					"x-ratelimit-bypass": rlskip,
				}
			}
		} else {
			// Generic PDS timeout
			c.Client.Timeout = time.Minute * 1
		}
	}
	rf.ApplyPDSClientSettings = ix.ApplyPDSClientSettings

	repoman.SetEventHandler(func(ctx context.Context, evt *repomgr.RepoEvent) {
		if err := ix.HandleRepoEvent(ctx, evt); err != nil {
			log.Errorw("failed to handle repo event", "err", err)
		}
	}, false)

	prodHR, err := api.NewProdHandleResolver(100_000, cctx.String("resolve-address"), cctx.Bool("force-dns-udp"))
	if err != nil {
		return fmt.Errorf("failed to set up handle resolver: %w", err)
	}
	if rlskip != "" {
		prodHR.ReqMod = func(req *http.Request, host string) error {
			if strings.HasSuffix(host, ".bsky.social") {
				req.Header.Set("x-ratelimit-bypass", rlskip)
			}
			return nil
		}
	}

	var hr api.HandleResolver = prodHR
	if cctx.StringSlice("handle-resolver-hosts") != nil {
		hr = &api.TestHandleResolver{
			TrialHosts: cctx.StringSlice("handle-resolver-hosts"),
		}
	}

	log.Infow("constructing bgs")
	bgsConfig := libbgs.DefaultBGSConfig()
	bgsConfig.SSL = !cctx.Bool("crawl-insecure-ws")
	bgsConfig.CompactInterval = cctx.Duration("compact-interval")
	bgsConfig.ConcurrencyPerPDS = cctx.Int64("concurrency-per-pds")
	bgsConfig.MaxQueuePerPDS = cctx.Int64("max-queue-per-pds")
	bgsConfig.DefaultRepoLimit = cctx.Int64("default-repo-limit")
	bgs, err := libbgs.NewBGS(db, ix, repoman, evtman, cachedidr, rf, hr, bgsConfig)
	if err != nil {
		return err
	}

	if tok := cctx.String("admin-key"); tok != "" {
		if err := bgs.CreateAdminToken(tok); err != nil {
			return fmt.Errorf("failed to set up admin token: %w", err)
		}
	}

	// set up metrics endpoint
	go func() {
		if err := bgs.StartMetrics(cctx.String("metrics-listen")); err != nil {
			log.Fatalf("failed to start metrics endpoint: %s", err)
		}
	}()

	bgsErr := make(chan error, 1)

	go func() {
		err := bgs.Start(cctx.String("api-listen"))
		bgsErr <- err
	}()

	log.Infow("startup complete")
	select {
	case <-signals:
		log.Info("received shutdown signal")
		errs := bgs.Shutdown()
		for err := range errs {
			log.Errorw("error during BGS shutdown", "err", err)
		}
	case err := <-bgsErr:
		if err != nil {
			log.Errorw("error during BGS startup", "err", err)
		}
		log.Info("shutting down")
		errs := bgs.Shutdown()
		for err := range errs {
			log.Errorw("error during BGS shutdown", "err", err)
		}
	}

	log.Info("shutdown complete")

	return nil
}
