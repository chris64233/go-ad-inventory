// Command server 启动广告预算服务的 HTTP 接口。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chris64233/go-ad-inventory/clock"
	"github.com/chris64233/go-ad-inventory/domain"
	"github.com/chris64233/go-ad-inventory/httpapi"
	"github.com/chris64233/go-ad-inventory/store"
)

func main() {
	var (
		addr          = flag.String("addr", ":8080", "HTTP listen address")
		eventLog      = flag.String("event-log", "data/events.jsonl", "event log file path")
		sweepInterval = flag.Duration("sweep-interval", 30*time.Second, "reservation expiry sweep interval")
	)
	flag.Parse()

	st, err := store.NewFileStore(*eventLog)
	if err != nil {
		log.Fatalf("open event store: %v", err)
	}
	defer st.Close()

	svc, err := domain.NewService(clock.System{}, st)
	if err != nil {
		log.Fatalf("load domain service: %v", err)
	}

	// 后台过期扫描：到期凭证释放全部额度。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		t := time.NewTicker(*sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := svc.ExpireSweep(context.Background()); err != nil {
					log.Printf("expire sweep: %v", err)
				} else if n > 0 {
					log.Printf("expire sweep: expired %d reservations", n)
				}
			}
		}
	}()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           httpapi.NewServer(svc).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on %s, event log %s", *addr, *eventLog)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}
