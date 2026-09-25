// Command server runs the protobuf compatibility registry: a pure backend
// ConnectRPC service backed by PostgreSQL. There is deliberately no admin
// UI; the API is the product.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"protocompat/internal/registry"
)

func main() {
	// STORE=memory runs the service without PostgreSQL (local smoke tests
	// only; nothing is persisted). Default is the PostgreSQL store.
	var store registry.Store
	if os.Getenv("STORE") == "memory" {
		log.Printf("STORE=memory: using in-memory store, data is not persisted")
		store = registry.NewMemStore()
	} else {
		store = mustPGStore()
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	svc := registry.NewService(store)
	pattern, handler := svc.Handler()

	mux := http.NewServeMux()
	mux.Handle(pattern, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    addr,
		Handler: h2c.NewHandler(mux, &http2.Server{}),
	}

	go func() {
		log.Printf("registry service listening on %s (connect+json)", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Printf("stopped")
}

func mustPGStore() registry.Store {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required (e.g. postgres://postgres:postgres@localhost:5432/registry?sslmode=disable), or set STORE=memory for a non-persistent smoke run")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping database: %v", err)
	}
	if _, err := db.ExecContext(ctx, registry.SchemaSQL); err != nil {
		log.Fatalf("apply schema: %v", err)
	}
	log.Printf("database ready, schema applied")
	return registry.NewPGStore(db)
}
