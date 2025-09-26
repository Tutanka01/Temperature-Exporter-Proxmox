package main

import (
    "bufio"
    "errors"
    "flag"
    "fmt"
    "log"
    "net/http"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "time"
    "context"
    "os/signal"
    "syscall"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

// These variables are intended to be set at build time via -ldflags
var (
    version = "dev"
    commit  = "none"
    date    = "unknown"
)

// sensorReading represents a single sensor with an optional label name (e.g., CPU, GPU, etc.)
type sensorReading struct {
    chip   string // hwmon chip directory name
    name   string // sensor name from name file when available
    label  string // content of temp*_label when present
    path   string // path to temp*_input
    factor float64 // multiplier (usually 0.001) to convert millidegree C to degree C
}

// collector implements prometheus.Collector
type collector struct {
    basePath   string
    sensors    *prometheus.GaugeVec
    scrapeTime prometheus.Gauge
}

func newCollector(basePath string, namespace string) *collector {
    labels := []string{"chip", "sensor", "label"}
    return &collector{
        basePath: basePath,
        sensors: prometheus.NewGaugeVec(prometheus.GaugeOpts{
            Namespace: namespace,
            Name:      "temperature_celsius",
            Help:      "Température en degrés Celsius lue depuis /sys/class/hwmon.*",
        }, labels),
        scrapeTime: prometheus.NewGauge(prometheus.GaugeOpts{
            Namespace: namespace,
            Name:      "scrape_duration_seconds",
            Help:      "Durée de la dernière collecte des températures.",
        }),
    }
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
    c.sensors.Describe(ch)
    c.scrapeTime.Describe(ch)
}

func readFirstLine(path string) (string, error) {
    f, err := os.Open(path)
    if err != nil {
        return "", err
    }
    defer f.Close()
    s := bufio.NewScanner(f)
    if s.Scan() {
        return strings.TrimSpace(s.Text()), nil
    }
    if err := s.Err(); err != nil {
        return "", err
    }
    return "", errors.New("empty file")
}

// discoverSensors scans basePath (default /sys/class/hwmon) to find temp*_input files and their labels.
func discoverSensors(basePath string) ([]sensorReading, error) {
    var sensors []sensorReading
    // iterate hwmon devices
    entries, err := os.ReadDir(basePath)
    if err != nil {
        return sensors, err
    }
    for _, e := range entries {
        if !e.IsDir() {
            continue
        }
        chipDir := filepath.Join(basePath, e.Name())
        // try to obtain a human friendly chip name
        chipName := e.Name()
        if n, err := readFirstLine(filepath.Join(chipDir, "name")); err == nil && n != "" {
            chipName = n
        }

        // list files to find temp*_input
        files, err := os.ReadDir(chipDir)
        if err != nil {
            // ignore unreadable chips, continue
            continue
        }
        for _, f := range files {
            fname := f.Name()
            if !strings.HasPrefix(fname, "temp") || !strings.HasSuffix(fname, "_input") {
                continue
            }
            // extract index between temp and _input
            idx := strings.TrimSuffix(strings.TrimPrefix(fname, "temp"), "_input")
            label := ""
            // prefer temp{idx}_label when available
            if l, err := readFirstLine(filepath.Join(chipDir, fmt.Sprintf("temp%v_label", idx))); err == nil {
                label = l
            } else if tname, err := readFirstLine(filepath.Join(chipDir, fmt.Sprintf("temp%v_type", idx))); err == nil {
                // fallback to type (like Tctl, Tdie)
                label = tname
            }
            // sensor name from chip name
            sensorName := chipName
            sensors = append(sensors, sensorReading{
                chip:   chipName,
                name:   sensorName,
                label:  label,
                path:   filepath.Join(chipDir, fname),
                factor: 0.001, // default millidegree to degree
            })
        }
    }
    return sensors, nil
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
    start := time.Now()
    // for robustness, re-discover each scrape to account for hotplug; for large systems we could cache with ttl
    sensors, err := discoverSensors(c.basePath)
    if err != nil {
        log.Printf("discoverSensors error: %v", err)
    }
    // reset gaugevec by recreating a new one each collection is heavy; instead, we use DeletePartialMatch before setting new
    c.sensors.Reset()

    for _, s := range sensors {
        raw, err := readFirstLine(s.path)
        if err != nil {
            // ignore missing/permission issues gracefully
            continue
        }
        // Some drivers expose value in millidegree; tolerate empty/non-number
        v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
        if err != nil {
            continue
        }
        tempC := v * s.factor
        c.sensors.WithLabelValues(s.chip, s.name, s.label).Set(tempC)
    }

    // export metrics
    c.sensors.Collect(ch)
    c.scrapeTime.Set(time.Since(start).Seconds())
    c.scrapeTime.Collect(ch)
}

func main() {
    var (
        listenAddr = flag.String("listen", ":9102", "Adresse d'écoute HTTP, ex : :9102")
        metricsPath = flag.String("path", "/metrics", "Chemin HTTP pour exposer les métriques")
        basePath    = flag.String("hwmon", "/sys/class/hwmon", "Chemin de base vers les capteurs hwmon")
        namespace   = flag.String("namespace", "temp_exporter", "Préfixe des métriques Prometheus")
        timeout     = flag.Duration("read-timeout", 5*time.Second, "Timeout lecture HTTP")
        writeTO     = flag.Duration("write-timeout", 10*time.Second, "Timeout écriture HTTP")
        readHdrTO   = flag.Duration("read-header-timeout", 5*time.Second, "Timeout lecture des en-têtes HTTP")
        idleTO      = flag.Duration("idle-timeout", 30*time.Second, "Timeout idle HTTP")
    )
    flag.Parse()

    c := newCollector(*basePath, *namespace)
    reg := prometheus.NewRegistry()
    reg.MustRegister(c)

    mux := http.NewServeMux()
    mux.Handle(*metricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
    mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("ok"))
    })

    srv := &http.Server{
        Addr:              *listenAddr,
        Handler:           mux,
        ReadTimeout:       *timeout,
        WriteTimeout:      *writeTO,
        ReadHeaderTimeout: *readHdrTO,
        IdleTimeout:       *idleTO,
    }

    log.Printf("Starting temperature exporter %s (commit %s, built %s) on %s%s (hwmon path: %s)", version, commit, date, *listenAddr, *metricsPath, *basePath)

    // Start server in background
    errCh := make(chan error, 1)
    go func() {
        if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            errCh <- err
        }
        close(errCh)
    }()

    // Handle termination signals for graceful shutdown
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
    select {
    case sig := <-sigCh:
        log.Printf("Received signal %s, shutting down...", sig)
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        if err := srv.Shutdown(ctx); err != nil {
            log.Printf("HTTP server Shutdown: %v", err)
        }
    case err := <-errCh:
        if err != nil {
            log.Fatalf("server error: %v", err)
        }
    }
}
