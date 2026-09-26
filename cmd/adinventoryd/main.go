// Command adinventoryd 启动广告预算预占/核销 HTTP 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	adinventory "github.com/chris64233/go-ad-inventory"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dataDir := flag.String("data-dir", "./data", "directory for the WAL file")
	ttl := flag.Duration("reservation-ttl", adinventory.DefaultReservationTTL, "reservation validity duration")
	sweepInterval := flag.Duration("sweep-interval", 30*time.Second, "expired reservation sweep interval")
	noFsync := flag.Bool("no-fsync", false, "skip fsync on every WAL append (unsafe, tests only)")
	flag.Parse()

	store, err := adinventory.OpenFileStore(*dataDir, !*noFsync, func(adinventory.Event) error {
		// 打开时只需推进内部序号；状态装载由 NewService 的重放完成。
		return nil
	})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	svc, err := adinventory.NewService(store, adinventory.SystemClock{}, *ttl)
	if err != nil {
		log.Fatalf("init service: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 后台定期过期扫描；所有读接口内部也会惰性过期。
	go func() {
		t := time.NewTicker(*sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := svc.SweepExpired(); n > 0 {
					log.Printf("expired %d reservations", n)
				}
			}
		}
	}()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           adinventory.NewServer(svc),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("adinventoryd listening on %s (data: %s)", *addr, *dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if err := store.Close(); err != nil {
		log.Printf("close store: %v", err)
		os.Exit(1)
	}
}
