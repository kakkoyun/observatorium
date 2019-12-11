package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/observatorium/observatorium/internal"
	"github.com/observatorium/observatorium/internal/proxy"
	"github.com/observatorium/observatorium/internal/server"

	"github.com/go-kit/kit/log/level"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
	"go.opentelemetry.io/otel/api/global"
	"go.uber.org/automaxprocs/maxprocs"
)

type options struct {
	debugMutexProfileFraction int
	debugBlockProfileRate     int

	proxyBufferSizeBytes int
	proxyBufferCount     int

	listen                  string
	gracePeriod             string
	debugName               string
	logLevel                string
	logFormat               string
	metricsQueryEndpoint    string
	metricsWriteEndpoint    string
	traceExporter           string
	traceExporterEndpoint   string
	traceSamplerProbability float64
}

func main() {
	opts := options{}

	flag.StringVar(&opts.listen, "listen", ":8080", "The address on which internal server runs.")
	flag.StringVar(&opts.gracePeriod, "grace-period", "5s", "The time to wait after an OS interrupt received.")
	flag.StringVar(&opts.debugName, "debug.name", "observatorium", "The name to add as prefix to log lines.")
	flag.IntVar(&opts.debugMutexProfileFraction, "debug.mutex-profile-fraction", 10,
		"The parameter which controls the fraction of mutex contention events that are reported in the mutex profile.")
	flag.IntVar(&opts.debugBlockProfileRate, "debug.block-profile-rate", 10,
		"The parameter controls the fraction of goroutine blocking events that are reported in the blocking profile.")
	flag.StringVar(&opts.logLevel, "log.level", "info", "The log filtering level. Options: 'error', 'warn', 'info', 'debug'.")
	flag.StringVar(&opts.logFormat, "log.format", internal.LogFormatLogfmt, "The log format to use. Options: 'logfmt', 'json'.")
	flag.StringVar(&opts.metricsQueryEndpoint, "metrics.query.endpoint", "", "The endpoint against which to query for metrics.")
	flag.StringVar(&opts.metricsWriteEndpoint, "metrics.write.endpoint", "",
		"The endpoint against which to make write requests for metrics.")
	flag.IntVar(&opts.proxyBufferCount, "proxy.buffer-count", proxy.DefaultBufferCount,
		"Maximum number of of reusable buffer used for copying HTTP reverse proxy responses.")
	flag.IntVar(&opts.proxyBufferSizeBytes, "proxy.buffer-size-bytes", proxy.DefaultBufferSizeBytes,
		"Size (bytes) of reusable buffer used for copying HTTP reverse proxy responses.")
	flag.StringVar(&opts.traceExporter, "trace.exporter", internal.ExporterJaeger, "The trace exporter to use. Options: 'stdout', 'jaeger'.")
	flag.StringVar(&opts.traceExporterEndpoint, "trace.exporter-endpoint", internal.ExporterJaeger,
		"The trace endpoint which to send trace spans.")
	flag.Float64Var(&opts.traceSamplerProbability, "trace.sampler-probability", 0.1, "The trace sampler probability to use.")
	flag.Parse()

	debug := os.Getenv("DEBUG") != ""

	if debug {
		runtime.SetMutexProfileFraction(opts.debugMutexProfileFraction)
		runtime.SetBlockProfileRate(opts.debugBlockProfileRate)
	}

	logger := internal.NewLogger(opts.logLevel, opts.logFormat, opts.debugName)
	defer level.Info(logger).Log("msg", "exiting")

	tr := internal.NewTracer(opts.traceExporter, opts.traceExporterEndpoint, opts.traceSamplerProbability)
	defer tr.Close()

	global.SetTraceProvider(tr.Provider)

	metricsQueryEndpoint, err := url.ParseRequestURI(opts.metricsQueryEndpoint)
	if err != nil {
		level.Error(logger).Log("msg", "--metrics.query.endpoint is invalid", "err", err)
		return
	}

	metricsWriteEndpoint, err := url.ParseRequestURI(opts.metricsWriteEndpoint)
	if err != nil {
		level.Error(logger).Log("msg", "--metrics.write.endpoint is invalid", "err", err)
		return
	}

	gracePeriod, err := time.ParseDuration(opts.gracePeriod)
	if err != nil {
		level.Error(logger).Log("msg", "--grace-period is invalid", "err", err)
		return
	}

	loggerAdapter := func(template string, args ...interface{}) {
		level.Debug(logger).Log("msg", fmt.Sprintf(template, args))
	}

	// Running in container with limits but with empty/wrong value of GOMAXPROCS env var could lead to throttling by cpu
	// maxprocs will automate adjustment by using cgroups info about cpu limit if it set as value for runtime.GOMAXPROCS
	undo, err := maxprocs.Set(maxprocs.Logger(loggerAdapter))
	if err != nil {
		level.Error(logger).Log("msg", "failed to set GOMAXPROCS:", "err", err)
	}

	defer undo()

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		version.NewCollector("observatorium"),
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	var g run.Group
	{
		// Signal channels must be buffered.
		sig := make(chan os.Signal, 1)
		g.Add(func() error {
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			<-sig
			return nil
		}, func(_ error) {
			level.Info(logger).Log("msg", "caught interrupt")
			close(sig)
		})
	}
	{
		srv := server.New(
			logger,
			reg,
			server.WithListen(opts.listen),
			server.WithGracePeriod(gracePeriod),
			server.WithProfile(os.Getenv("PROFILE") != ""),
			server.WithMetricQueryEndpoint(metricsQueryEndpoint),
			server.WithMetricWriteEndpoint(metricsWriteEndpoint),
			server.WithProxyOptions(
				proxy.WithBufferCount(opts.proxyBufferCount),
				proxy.WithBufferSizeBytes(opts.proxyBufferSizeBytes),
			),
		)
		g.Add(srv.ListenAndServe, srv.Shutdown)
	}

	level.Info(logger).Log("msg", "starting observatorium")

	if err := g.Run(); err != nil {
		level.Error(logger).Log("msg", "observatorium failed", "err", err)
		os.Exit(1)
	}

	os.Exit(0)
}
