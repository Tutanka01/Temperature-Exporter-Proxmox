package main

import (
    "bufio"
    "context"
    "encoding/json"
    "errors"
    "flag"
    "fmt"
    "log"
    "net/http"
    "os"
    "os/signal"
    "os/exec"
    "path/filepath"
    "regexp"
    "strconv"
    "strings"
    "syscall"
    "time"

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
    thermalPath string
    enableHwmon bool
    enableThermal bool
    enableSensorsCli bool
    sensorsCliPath string
    sensorsTimeout time.Duration
    sensors    *prometheus.GaugeVec
    scrapeTime prometheus.Gauge
}

var sensorsCliWarned bool

func newCollector(basePath string, thermalPath string, enableHwmon, enableThermal bool, enableSensorsCli bool, sensorsCliPath string, sensorsTimeout time.Duration, namespace string) *collector {
    labels := []string{"chip", "sensor", "label"}
    return &collector{
        basePath: basePath,
        thermalPath: thermalPath,
        enableHwmon: enableHwmon,
        enableThermal: enableThermal,
        enableSensorsCli: enableSensorsCli,
        sensorsCliPath: sensorsCliPath,
        sensorsTimeout: sensorsTimeout,
        sensors: prometheus.NewGaugeVec(prometheus.GaugeOpts{
            Namespace: namespace,
            Name:      "temperature_celsius",
            Help:      "Température en degrés Celsius lue depuis les capteurs système (hwmon, thermal, lm-sensors).",
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

// discoverThermalSensors scans /sys/class/thermal for thermal_zone*/temp
func discoverThermalSensors(thermalBase string) ([]sensorReading, error) {
    var sensors []sensorReading
    entries, err := os.ReadDir(thermalBase)
    if err != nil {
        return sensors, err
    }
    for _, e := range entries {
        if !e.IsDir() || !strings.HasPrefix(e.Name(), "thermal_zone") {
            continue
        }
        zoneDir := filepath.Join(thermalBase, e.Name())
        ttype, _ := readFirstLine(filepath.Join(zoneDir, "type"))
        // Some systems have trip points; we only read current temp
        tempPath := filepath.Join(zoneDir, "temp")
        if _, err := os.Stat(tempPath); err == nil {
            sensors = append(sensors, sensorReading{
                chip:   "thermal",
                name:   ttype,
                label:  e.Name(),
                path:   tempPath,
                factor: 0.001,
            })
        }
    }
    return sensors, nil
}

type cliReading struct {
    chip  string
    name  string
    label string
    value float64
}

// discoverSensorsCLI runs `sensors -j` and parses temperatures generically.
func discoverSensorsCLI(bin string, timeout time.Duration) ([]cliReading, error) {
    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()
    cmd := exec.CommandContext(ctx, bin, "-j")
    out, err := cmd.Output()
    if err != nil {
        return nil, err
    }
    var root map[string]interface{}
    if err := json.Unmarshal(out, &root); err != nil {
        return nil, err
    }
    var res []cliReading
    // regex to match tempN_input
    re := regexp.MustCompile(`^temp(\d+)_input$`)
    // walk the structure
    for chip, v := range root {
        m, ok := v.(map[string]interface{})
        if !ok {
            continue
        }
        // second-level: sections like Tctl, Composite, etc.
        for section, sv := range m {
            sm, ok := sv.(map[string]interface{})
            if !ok {
                continue
            }
            // find keys tempN_input
            for k, val := range sm {
                match := re.FindStringSubmatch(k)
                if match == nil {
                    continue
                }
                // parse value as float64
                var f float64
                switch tv := val.(type) {
                case float64:
                    f = tv
                case int:
                    f = float64(tv)
                case json.Number:
                    ff, _ := tv.Float64()
                    f = ff
                default:
                    continue
                }
                // find optional label in same section
                idx := match[1]
                labelKey := fmt.Sprintf("temp%v_label", idx)
                label := ""
                if lv, ok := sm[labelKey]; ok {
                    if s, ok := lv.(string); ok {
                        label = s
                    }
                }
                res = append(res, cliReading{
                    chip:  chip,
                    name:  section,
                    label: label,
                    value: f, // already in degree C
                })
            }
        }
    }
    return res, nil
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
    start := time.Now()
    // for robustness, re-discover each scrape to account for hotplug; for large systems we could cache with ttl
    var sensors []sensorReading
    if c.enableHwmon {
        if s, err := discoverSensors(c.basePath); err == nil {
            sensors = append(sensors, s...)
        } else {
            log.Printf("discoverSensors error: %v", err)
        }
    }
    if c.enableThermal {
        if s, err := discoverThermalSensors(c.thermalPath); err == nil {
            sensors = append(sensors, s...)
        } else {
            log.Printf("discoverThermalSensors error: %v", err)
        }
    }
    // reset gaugevec by recreating a new one each collection is heavy; instead, we use Reset before setting new
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

    // Also collect via sensors -j if enabled
    if c.enableSensorsCli {
        if readings, err := discoverSensorsCLI(c.sensorsCliPath, c.sensorsTimeout); err == nil {
            for _, r := range readings {
                c.sensors.WithLabelValues(r.chip, r.name, r.label).Set(r.value)
            }
        } else {
            if !sensorsCliWarned {
                log.Printf("discoverSensorsCLI error: %v (désactivez -enable-sensors-cli ou installez lm-sensors)", err)
                sensorsCliWarned = true
            }
        }
    }

    // export metrics
    c.sensors.Collect(ch)
    c.scrapeTime.Set(time.Since(start).Seconds())
    c.scrapeTime.Collect(ch)
}

// loggingResponseWriter wraps http.ResponseWriter to capture status code
type loggingResponseWriter struct {
    http.ResponseWriter
    status int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
    lrw.status = code
    lrw.ResponseWriter.WriteHeader(code)
}

func withRequestLogging(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        lrw := &loggingResponseWriter{ResponseWriter: w, status: 200}
        next.ServeHTTP(lrw, r)
        dur := time.Since(start)
        log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, lrw.status, dur)
    })
}

func main() {
    var (
        listenAddr = flag.String("listen", ":9102", "Adresse d'écoute HTTP, ex : :9102")
        metricsPath = flag.String("path", "/metrics", "Chemin HTTP pour exposer les métriques")
        basePath    = flag.String("hwmon", "/sys/class/hwmon", "Chemin de base vers les capteurs hwmon")
        thermalPath = flag.String("thermal", "/sys/class/thermal", "Chemin de base vers les zones thermiques (thermal zones)")
        enableHwmon = flag.Bool("enable-hwmon", true, "Activer la lecture via hwmon (/sys/class/hwmon)")
        enableThermal = flag.Bool("enable-thermal", true, "Activer la lecture via thermal zones (/sys/class/thermal)")
    enableSensorsCli = flag.Bool("enable-sensors-cli", true, "Activer la lecture via 'sensors -j' (nécessite lm-sensors)")
        sensorsCliPath = flag.String("sensors-cli-path", "sensors", "Chemin de la commande 'sensors'")
        sensorsTimeout = flag.Duration("sensors-timeout", 2*time.Second, "Timeout pour l'exécution de 'sensors -j'")
        namespace   = flag.String("namespace", "temp_exporter", "Préfixe des métriques Prometheus")
        timeout     = flag.Duration("read-timeout", 5*time.Second, "Timeout lecture HTTP")
        writeTO     = flag.Duration("write-timeout", 10*time.Second, "Timeout écriture HTTP")
        readHdrTO   = flag.Duration("read-header-timeout", 5*time.Second, "Timeout lecture des en-têtes HTTP")
        idleTO      = flag.Duration("idle-timeout", 30*time.Second, "Timeout idle HTTP")
        logRequests = flag.Bool("log-requests", false, "Journaliser les requêtes HTTP (méthode, chemin, statut, durée)")
    )
    flag.Parse()

    c := newCollector(*basePath, *thermalPath, *enableHwmon, *enableThermal, *enableSensorsCli, *sensorsCliPath, *sensorsTimeout, *namespace)
    reg := prometheus.NewRegistry()
    reg.MustRegister(c)

    mux := http.NewServeMux()
    mux.Handle(*metricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
    mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("ok"))
    })
    // Root helper to avoid 404 confusion in browsers
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/" { // keep other paths as 404 to not confuse scraping
            http.NotFound(w, r)
            return
        }
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        _, _ = fmt.Fprintf(w, "Temperature Exporter\nMetrics: %s\nHealth: /healthz\n", *metricsPath)
    })

    var handler http.Handler = mux
    if *logRequests {
        handler = withRequestLogging(mux)
    }

    srv := &http.Server{
        Addr:              *listenAddr,
        Handler:           handler,
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
