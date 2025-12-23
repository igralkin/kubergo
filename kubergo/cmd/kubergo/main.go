package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

const windowSize = 50

var anomalyThreshold = getenvFloat("ANOMALY_THRESHOLD", 2.0)

type Metric struct {
	Timestamp int64   `json:"timestamp"` // unix seconds
	CPU       float64 `json:"cpu"`       // условно 0..100
	RPS       float64 `json:"rps"`       // запросы/сек
}

type AnalyzeResult struct {
	Window         int       `json:"window"`
	Count          int       `json:"count"`
	RPSRollingAvg  float64   `json:"rps_rolling_avg"`
	RPSMean        float64   `json:"rps_mean"`
	RPSStdDev      float64   `json:"rps_stddev"`
	LastRPS        float64   `json:"last_rps"`
	LastZScore     float64   `json:"last_zscore"`
	IsAnomaly      bool      `json:"is_anomaly"`
	AnomalyThresh  float64   `json:"anomaly_threshold"`
	LastTimestamp  int64     `json:"last_timestamp"`
	RecentRPS      []float64 `json:"recent_rps,omitempty"` // для дебага
	RecentCPU      []float64 `json:"recent_cpu,omitempty"` // для дебага
	ComputeLatency string    `json:"compute_latency"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type App struct {
	startedAt time.Time

	// request counter
	mu              sync.Mutex
	reqCounter      int64
	reqTotal        prometheus.Counter
	metricsAccepted prometheus.Counter
	metricsDropped  prometheus.Counter
	anomalies       prometheus.Counter
	reqLatency      prometheus.Histogram

	// pipeline
	metricsCh chan Metric

	// shared state produced by analyzer goroutine
	stateMu sync.RWMutex
	result  AnalyzeResult
	rdb     *redis.Client
}

func main() {
	port := getenv("PORT", "8080")

	app := NewApp()
	app.startAnalyzer()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", app.handleHealthz)

	// основной API
	mux.HandleFunc("/api/v1/metrics", app.withMiddlewares(app.handleMetrics)) // POST
	mux.HandleFunc("/api/v1/analyze", app.withMiddlewares(app.handleAnalyze)) // GET
	mux.HandleFunc("/api/v1/stats", app.withMiddlewares(app.handleStats))     // GET
	mux.HandleFunc("/api/v1/redis/ping", app.withMiddlewares(app.handleRedisPing))

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("kubergo listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = srv.Shutdown(shutdownCtx)
	log.Printf("bye")
}

func NewApp() *App {
	addr := getenv("REDIS_ADDR", "localhost:6379")

	bufSize := getenvInt("METRICS_CH_SIZE", 4096)

	a := &App{
		startedAt: time.Now().UTC(),
		metricsCh: make(chan Metric, bufSize),
		result: AnalyzeResult{
			Window:        windowSize,
			AnomalyThresh: anomalyThreshold,
		},
	}

	a.rdb = redis.NewClient(&redis.Options{
		Addr: addr,
	})

	a.reqTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kubergo_http_requests_total",
		Help: "Total number of HTTP requests handled by kubergo.",
	})
	prometheus.MustRegister(a.reqTotal)

	a.reqLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "kubergo_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	})
	prometheus.MustRegister(a.reqLatency)

	a.metricsAccepted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kubergo_metrics_accepted_total",
		Help: "Total number of accepted metrics events.",
	})
	prometheus.MustRegister(a.metricsAccepted)

	a.metricsDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kubergo_metrics_dropped_total",
		Help: "Total number of dropped metrics events (buffer full).",
	})
	prometheus.MustRegister(a.metricsDropped)

	a.anomalies = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kubergo_anomalies_total",
		Help: "Total number of detected anomalies (z-score > threshold).",
	})
	prometheus.MustRegister(a.anomalies)

	return a
}

func (a *App) startAnalyzer() {
	go func() {
		// скользящее окно по RPS и CPU
		var rpsBuf ring
		var cpuBuf ring
		rpsBuf.init(windowSize)
		cpuBuf.init(windowSize)

		for m := range a.metricsCh {
			start := time.Now()
			// test helper: artificial analyzer slowness
			if d := getenvInt("ANALYZER_DELAY_MS", 0); d > 0 {
				time.Sleep(time.Duration(d) * time.Millisecond)
			}

			// обновляем окна
			rpsBuf.push(m.RPS)
			cpuBuf.push(m.CPU)

			// анализируем по RPS
			count := rpsBuf.len()
			last := rpsBuf.last()

			mean, std := meanStd(rpsBuf.values())
			roll := mean // rolling average по окну

			z := 0.0
			if std > 0 {
				z = math.Abs((last - mean) / std)
			}
			isAnomaly := z > anomalyThreshold

			if isAnomaly && a.anomalies != nil {
				a.anomalies.Inc()
			}

			res := AnalyzeResult{
				Window:         windowSize,
				Count:          count,
				RPSRollingAvg:  roll,
				RPSMean:        mean,
				RPSStdDev:      std,
				LastRPS:        last,
				LastZScore:     z,
				IsAnomaly:      isAnomaly,
				AnomalyThresh:  anomalyThreshold,
				LastTimestamp:  m.Timestamp,
				RecentRPS:      rpsBuf.copyValues(),
				RecentCPU:      cpuBuf.copyValues(),
				ComputeLatency: time.Since(start).String(),
			}

			// cache analysis result in Redis (best-effort)
			if a.rdb != nil {
				b, _ := json.Marshal(res)
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				_ = a.rdb.Set(ctx, "kubergo:analyze:last", b, 0).Err()
				cancel()
			}

			// publish result in memory
			a.stateMu.Lock()
			a.result = res
			a.stateMu.Unlock()
		}
	}()
}

func (a *App) withMiddlewares(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid := requestID()
		start := time.Now()

		w.Header().Set("X-Request-ID", rid)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		a.mu.Lock()
		a.reqCounter++
		if a.reqTotal != nil {
			a.reqTotal.Inc()
		}
		a.mu.Unlock()

		defer func() {
			if a.reqLatency != nil {
				a.reqLatency.Observe(time.Since(start).Seconds())
			}
			log.Printf("rid=%s method=%s path=%s remote=%s dur=%s",
				rid, r.Method, r.URL.Path, r.RemoteAddr, time.Since(start))
		}()

		next(w, r)
	}
}

func (a *App) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprint(w, "ok\n")
}

func (a *App) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET supported")
		return
	}

	a.mu.Lock()
	reqs := a.reqCounter
	a.mu.Unlock()

	a.stateMu.RLock()
	cnt := a.result.Count
	a.stateMu.RUnlock()

	out := map[string]any{
		"uptime_sec": int64(time.Since(a.startedAt).Seconds()),
		"requests":   reqs,
		"window":     windowSize,
		"received":   cnt,
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST supported")
		return
	}

	var m Metric
	if err := decodeJSON(r, &m); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// минимальная валидация
	if m.Timestamp == 0 {
		m.Timestamp = time.Now().Unix()
	}
	if m.RPS < 0 || math.IsNaN(m.RPS) || math.IsInf(m.RPS, 0) {
		writeErr(w, http.StatusBadRequest, "validation_error", "rps must be finite and >= 0")
		return
	}
	if m.CPU < 0 || m.CPU > 100 || math.IsNaN(m.CPU) || math.IsInf(m.CPU, 0) {
		writeErr(w, http.StatusBadRequest, "validation_error", "cpu must be finite in range 0..100")
		return
	}
	// cache metric in Redis (best-effort)
	if a.rdb != nil {
		b, _ := json.Marshal(m)
		ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		_ = a.rdb.LPush(ctx, "kubergo:metrics", b).Err()
		_ = a.rdb.LTrim(ctx, "kubergo:metrics", 0, windowSize-1).Err()
		cancel()
	}
	// push в канал, не блокируемся бесконечно
	select {
	case a.metricsCh <- m:
		if a.metricsAccepted != nil {
			a.metricsAccepted.Inc()
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})
	default:
		if a.metricsDropped != nil {
			a.metricsDropped.Inc()
		}
		writeErr(w, http.StatusServiceUnavailable, "overloaded", "metrics buffer full")
	}
}

func (a *App) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET supported")
		return
	}

	// optional: ?debug=0 чтобы не отдавать массивы recent*
	debug := true
	if v := r.URL.Query().Get("debug"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			debug = b
		}
	}

	a.stateMu.RLock()
	res := a.result
	a.stateMu.RUnlock()

	if !debug {
		res.RecentRPS = nil
		res.RecentCPU = nil
	}

	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleRedisPing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET supported")
		return
	}
	if a.rdb == nil {
		writeErr(w, http.StatusServiceUnavailable, "redis_not_configured", "redis client is nil")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Millisecond)
	defer cancel()

	if err := a.rdb.Ping(ctx).Err(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "redis_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "pong"})
}

func decodeJSON(r *http.Request, out any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, APIError{Code: code, Message: msg})
}

func getenv(k, def string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	return v
}

func requestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

/* --------- math helpers + ring buffer --------- */

func meanStd(xs []float64) (mean float64, std float64) {
	n := float64(len(xs))
	if n == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	mean = sum / n

	// population stddev
	vars := 0.0
	for _, x := range xs {
		d := x - mean
		vars += d * d
	}
	vars /= n
	std = math.Sqrt(vars)
	return mean, std
}

type ring struct {
	buf   []float64
	size  int
	count int
	head  int
}

func (r *ring) init(size int) {
	r.size = size
	r.buf = make([]float64, size)
	r.count = 0
	r.head = 0
}

func (r *ring) push(v float64) {
	r.buf[r.head] = v
	r.head = (r.head + 1) % r.size
	if r.count < r.size {
		r.count++
	}
}

func (r *ring) len() int { return r.count }

func (r *ring) last() float64 {
	if r.count == 0 {
		return 0
	}
	// last written is at head-1
	i := r.head - 1
	if i < 0 {
		i = r.size - 1
	}
	return r.buf[i]
}

func (r *ring) values() []float64 {
	out := make([]float64, 0, r.count)
	start := r.head - r.count
	if start < 0 {
		start += r.size
	}
	for i := 0; i < r.count; i++ {
		out = append(out, r.buf[(start+i)%r.size])
	}
	return out
}

func (r *ring) copyValues() []float64 {
	v := r.values()
	out := make([]float64, len(v))
	copy(out, v)
	return out
}

func getenvInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func getenvFloat(k string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}
