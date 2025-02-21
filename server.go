package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sys/unix"
)

var (
	meter  metric.Meter
	tracer trace.Tracer
	logger *zap.Logger
)

type Epoll struct {
	fd          int
	connections map[int]net.Conn
	clients     map[string]net.Conn
	lock        *sync.RWMutex
}

func NewEpoll() (*Epoll, error) {
	fd, err := unix.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	return &Epoll{
		fd:          fd,
		connections: make(map[int]net.Conn),
		clients:     make(map[string]net.Conn),
		lock:        &sync.RWMutex{},
	}, nil
}

func (e *Epoll) Add(conn net.Conn, clientID string) error {
	ctx, span := tracer.Start(context.Background(), "EpollAdd")
	defer span.End()

	fd := getFD(conn)
	err := unix.EpollCtl(e.fd, syscall.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
		Events: unix.POLLIN | unix.POLLHUP,
		Fd:     int32(fd),
	})
	if err != nil {
		counter, err := meter.Int64Counter("websocket_errors_total")
		if err == nil {
			counter.Add(ctx, 1)
		}
		logger.Error("Failed to add connection to epoll", zap.Error(err), zap.String("client_id", clientID))
		return err
	}
	e.lock.Lock()
	e.connections[fd] = conn
	e.clients[clientID] = conn
	e.lock.Unlock()
	counter, err := meter.Int64UpDownCounter("websocket_active_connections")
	if err == nil {
		counter.Add(ctx, 1)
	}
	logger.Info("Client connected", zap.String("client_id", clientID), zap.Int("total_clients", len(e.clients)))
	return nil
}

func (e *Epoll) Remove(conn net.Conn, clientID string) error {
	ctx, span := tracer.Start(context.Background(), "EpollRemove")
	defer span.End()

	fd := getFD(conn)
	e.lock.Lock()
	if _, exists := e.connections[fd]; !exists {
		// 이미 제거된 경우 무시
		e.lock.Unlock()
		logger.Debug("Connection already removed from epoll", zap.String("client_id", clientID), zap.Int("fd", fd))
		return nil
	}
	err := unix.EpollCtl(e.fd, syscall.EPOLL_CTL_DEL, fd, nil)
	delete(e.connections, fd)
	delete(e.clients, clientID)
	e.lock.Unlock()

	if err != nil {
		// EBADF 또는 ENOENT는 클라이언트 연결 종료로 인한 정상 상황으로 간주
		if err == unix.EBADF || err == unix.ENOENT {
			logger.Debug("Connection already closed or removed from epoll", zap.String("client_id", clientID), zap.Error(err))
		} else {
			counter, err := meter.Int64Counter("websocket_errors_total")
			if err == nil {
				counter.Add(ctx, 1)
			}
			logger.Warn("Failed to remove connection from epoll", zap.Error(err), zap.String("client_id", clientID))
		}
	} else {
		counter, err := meter.Int64UpDownCounter("websocket_active_connections")
		if err == nil {
			counter.Add(ctx, -1)
		}
		logger.Info("Client disconnected", zap.String("client_id", clientID), zap.Int("total_clients", len(e.clients)))
	}
	return nil
}

func (e *Epoll) Wait() ([]net.Conn, error) {
	events := make([]unix.EpollEvent, 100)
	n, err := unix.EpollWait(e.fd, events, -1)
	if err != nil {
		counter, err := meter.Int64Counter("websocket_errors_total")
		if err == nil {
			counter.Add(context.Background(), 1)
		}
		logger.Error("Epoll wait error", zap.Error(err))
		return nil, err
	}
	e.lock.RLock()
	defer e.lock.RUnlock()
	var conns []net.Conn
	for i := 0; i < n; i++ {
		conn := e.connections[int(events[i].Fd)]
		conns = append(conns, conn)
	}
	return conns, nil
}

func getFD(conn net.Conn) int {
	tcpConn := conn.(*net.TCPConn)
	f, _ := tcpConn.File()
	return int(f.Fd())
}

func monitorMemory(ctx context.Context) {
	for {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		gauge, err := meter.Int64Gauge("websocket_memory_usage_bytes")
		if err == nil {
			gauge.Record(ctx, int64(m.Alloc))
		}
		time.Sleep(5 * time.Second)
	}
}

func initTelemetry() (func(), error) {
	ctx := context.Background()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("websocket-server"),
		),
	)
	if err != nil {
		return nil, err
	}

	// traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure(), otlptracegrpc.WithEndpoint("localhost:4317"))
	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	promExporter, err := prometheus.New()
	if err != nil {
		return nil, err
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExporter),
	)
	otel.SetMeterProvider(meterProvider)

	// config := zap.NewProductionConfig()
	config := zap.NewDevelopmentConfig()
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	// config.OutputPaths = []string{"./server.log"}
	logger, err = config.Build()
	if err != nil {
		return nil, err
	}

	tracer = otel.Tracer("websocket-server")
	meter = otel.Meter("websocket-server")

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tp.Shutdown(ctx)
		meterProvider.Shutdown(ctx)
		logger.Sync()
	}
	return shutdown, nil
}

func main() {
	shutdown, err := initTelemetry()
	if err != nil {
		logger.Fatal("Failed to initialize telemetry", zap.Error(err))
	}
	defer shutdown()

	epoll, err := NewEpoll()
	if err != nil {
		logger.Fatal("Failed to create epoll", zap.Error(err))
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "HandleWebSocket")
		defer span.End()

		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			counter, err := meter.Int64Counter("websocket_errors_total")
			if err == nil {
				counter.Add(ctx, 1)
			}
			logger.Error("WebSocket upgrade error", zap.Error(err))
			span.RecordError(err)
			return
		}
		clientID := r.URL.Query().Get("client_id")
		if clientID == "" {
			conn.Close()
			logger.Warn("Missing client_id in WebSocket request")
			return
		}
		span.SetAttributes(attribute.String("client_id", clientID))

		if err := epoll.Add(conn, clientID); err != nil {
			conn.Close()
			return
		}
		go func(c net.Conn, cid string) {
			defer func() {
				if err := epoll.Remove(c, cid); err != nil {
					logger.Error("Failed to remove client", zap.String("client_id", cid), zap.Error(err))
				}
			}()
			for {
				_, err := wsutil.ReadClientText(c)
				if err != nil {
					logger.Info("Client read error, closing", zap.String("client_id", cid), zap.Error(err))
					return
				}
			}
		}(conn, clientID)
	})

	mux.HandleFunc("/push", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "HandlePush")
		defer span.End()

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			logger.Warn("Invalid method for /push", zap.String("method", r.Method))
			return
		}

		start := time.Now()
		clientID := r.URL.Query().Get("client_id")
		if clientID == "" {
			http.Error(w, "Missing client_id", http.StatusBadRequest)
			logger.Warn("Missing client_id in push request")
			return
		}

		message, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
			logger.Error("Failed to read push request body", zap.Error(err))
			span.RecordError(err)
			return
		}
		defer r.Body.Close()

		span.SetAttributes(attribute.String("client_id", clientID), attribute.String("message", string(message)))

		epoll.lock.RLock()
		conn, exists := epoll.clients[clientID]
		epoll.lock.RUnlock()
		if !exists {
			http.Error(w, "Client not found", http.StatusNotFound)
			logger.Warn("Client not found", zap.String("client_id", clientID))
			return
		}
		err = wsutil.WriteServerText(conn, message)
		if err != nil {
			counter, err := meter.Int64Counter("websocket_errors_total")
			if err == nil {
				counter.Add(ctx, 1)
			}
			logger.Error("Failed to send message", zap.Error(err), zap.String("client_id", clientID))
			span.RecordError(err)
			http.Error(w, "Failed to send message", http.StatusInternalServerError)
			return
		}
		histogram, err := meter.Float64Histogram("websocket_push_latency_seconds")
		if err == nil {
			histogram.Record(ctx, time.Since(start).Seconds())
		}
		logger.Info("Push sent", zap.String("client_id", clientID))
		w.Write([]byte("Push sent"))
	})

	mux.Handle("/metrics", promhttp.Handler())

	go monitorMemory(context.Background())

	go func() {
		for {
			_, err := epoll.Wait()
			if err != nil {
				logger.Error("Epoll wait error", zap.Error(err))
			}
		}
	}()

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	logger.Info("Server starting on :8080")
	logger.Fatal("Server failed", zap.Error(server.ListenAndServe()))
}
