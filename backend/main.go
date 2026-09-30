package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed dist
var assets embed.FS

func main() {
	executable, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	dataDir := flag.String("data-dir", filepath.Join(filepath.Dir(executable), "data"), "Application data directory")
	sqliteUploadMaxMB := flag.Int64("sqlite-upload-max-mb", 256, "Maximum SQLite upload size in MiB")
	flag.Parse()
	store, err := openStore(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer store.close()
	app := newApplication(store)
	if *sqliteUploadMaxMB < 1 || *sqliteUploadMaxMB > 4096 {
		log.Fatal("sqlite-upload-max-mb must be between 1 and 4096")
	}
	app.sqliteUploadLimit = *sqliteUploadMaxMB << 20
	root, err := fs.Sub(assets, "dist")
	if err != nil {
		log.Fatal(err)
	}
	files := http.FileServer(http.FS(root))
	server := &http.Server{
		Addr:              *addr,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      75 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		Handler:           protectLocalHost(*addr, app.handler(files)),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("GoSQL: http://%s", *addr)
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	if err := serve(ctx, server, listener); err != nil {
		log.Fatal(err)
	}
}

// Shutdown menunggu request aktif; deadline mencegah proses tertahan tanpa batas.
func serve(ctx context.Context, server *http.Server, listener net.Listener) error {
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			<-finished
			return err
		}
		<-finished
		return nil
	}
}
