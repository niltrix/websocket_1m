package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
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
)

var (
	meter  metric.Meter
	tracer trace.Tracer
	logger *zap.Logger
)

func initTelemetry() (func(), error) {
	ctx := context.Background()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("websocket-client"),
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
	// config.OutputPaths = []string{"./client.log"}
	logger, err = config.Build()
	if err != nil {
		return nil, err
	}

	tracer = otel.Tracer("websocket-client")
	meter = otel.Meter("websocket-client")

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tp.Shutdown(ctx)
		meterProvider.Shutdown(ctx)
		logger.Sync()
	}
	return shutdown, nil
}

func clientTask(clientID string, serverURI string, wg *sync.WaitGroup) {
	defer wg.Done()

	ctx, span := tracer.Start(context.Background(), "ClientTask", trace.WithAttributes(attribute.String("client_id", clientID)))
	defer span.End()

	startTime := time.Now()
	u, err := url.Parse(serverURI)
	if err != nil {
		logger.Error("Failed to parse URI", zap.String("client_id", clientID), zap.Error(err))
		span.RecordError(err)
		return
	}
	q := u.Query()
	q.Set("client_id", clientID)
	u.RawQuery = q.Encode()

	conn, _, _, err := ws.Dial(ctx, u.String())
	if err != nil {
		logger.Error("Connection failed", zap.String("client_id", clientID), zap.Error(err))
		span.RecordError(err)
		return
	}
	defer conn.Close()

	histogram, err := meter.Float64Histogram("websocket_client_connection_time_seconds")
	if err == nil {
		histogram.Record(ctx, time.Since(startTime).Seconds())
	}
	counter, err := meter.Int64Counter("websocket_client_connection_success_total")
	if err == nil {
		counter.Add(ctx, 1)
	}
	upDownCounter, err := meter.Int64UpDownCounter("websocket_client_active_clients")
	if err == nil {
		upDownCounter.Add(ctx, 1)
		defer func() {
			if upDownCounter, err := meter.Int64UpDownCounter("websocket_client_active_clients"); err == nil {
				upDownCounter.Add(ctx, -1)
			}
		}()
	}

	logger.Info("Connected", zap.String("client_id", clientID))

	for {
		msg, err := wsutil.ReadServerText(conn)
		if err != nil {
			logger.Info("Disconnected", zap.String("client_id", clientID), zap.Error(err))
			return
		}
		recvTime := time.Now()
		logger.Info("Received message", zap.String("client_id", clientID), zap.String("message", string(msg)))
		histogram, err := meter.Float64Histogram("websocket_client_message_latency_seconds")
		if err == nil {
			histogram.Record(ctx, time.Since(recvTime).Seconds())
		}
	}
}

func pushMessage(ctx context.Context, httpURI, clientID, message string) error {
	_, span := tracer.Start(ctx, "PushMessage")
	defer span.End()

	u, err := url.Parse(httpURI + "/push")
	if err != nil {
		logger.Error("Failed to parse push URI", zap.Error(err))
		span.RecordError(err)
		return err
	}
	q := u.Query()
	q.Set("client_id", clientID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewBuffer([]byte(message)))
	if err != nil {
		logger.Error("Failed to create push request", zap.Error(err))
		span.RecordError(err)
		return err
	}
	req.Header.Set("Content-Type", "text/plain")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		logger.Error("Push request failed", zap.String("client_id", clientID), zap.Error(err))
		span.RecordError(err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.Error("Push request returned non-OK status", zap.String("client_id", clientID), zap.Int("status_code", resp.StatusCode))
		return fmt.Errorf("push failed with status: %d", resp.StatusCode)
	}

	logger.Info("Pushed", zap.String("client_id", clientID), zap.String("status", resp.Status))
	return nil
}

func main() {
	shutdown, err := initTelemetry()
	if err != nil {
		logger.Fatal("Failed to initialize telemetry", zap.Error(err))
	}
	defer shutdown()

	// serverURI := "ws://localhost:8080/ws"
	// httpURI := "http://localhost:8080"
	// numClients := 10

	ws := flag.String("ws", "ws://localhost:8080/ws", "URI of the server to connect to")
	// uri := flag.String("http", "http://localhost:8080", "URI of the server to connect to")
	conn := flag.Int("conn", 10, "Number of clients to simulate")

	flag.Parse()

	serverURI := *ws
	// httpURI := *uri
	numClients := *conn

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Fatal(http.ListenAndServe(":9090", mux))
	}()

	var wg sync.WaitGroup
	startTime := time.Now()

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		clientID := fmt.Sprintf("client_%d", i)
		go clientTask(clientID, serverURI, &wg)
		if i%1000 == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// time.Sleep(60 * time.Second)
	//
	// for i := 0; i < 10; i++ {
	// 	randomClient := fmt.Sprintf("client_%d", rand.Intn(numClients))
	// 	message := fmt.Sprintf("Test push at %v with a potentially very long message to test POST body handling", time.Now())
	// 	if err := pushMessage(context.Background(), httpURI, randomClient, message); err != nil {
	// 		logger.Error("Push failed", zap.Error(err))
	// 	}
	// 	time.Sleep(1 * time.Second)
	// }

	logger.Info("Total time for setup", zap.Float64("seconds", time.Since(startTime).Seconds()))
	wg.Wait()
}
