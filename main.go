package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/cloudimpl/polycode/core"
	_go "github.com/cloudimpl/polycode/go"
	"github.com/fsnotify/fsnotify"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	captureAddr = "127.0.0.1:9999"
	capturePath = "/v1/system/app/start"
	timeout     = 60 * time.Second
	grace       = 1500 * time.Millisecond
)

// Language -> Generator
var generators = map[string]core.Generator{
	"go": &_go.Generator{},
}

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 {
		printRootUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "help", "-h", "--help":
		printRootUsage()
		return

	case "generate":
		cmdGenerate(os.Args[2:])

	case "extract":
		cmdExtract(os.Args[2:])

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		printRootUsage()
		os.Exit(2)
	}
}

// ========================= generate =========================

func cmdGenerate(args []string) {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		appLanguage string
		outputPath  string
		watchFlag   bool
	)

	fs.StringVar(&appLanguage, "language", "go", "Application language (supported: go)")
	fs.StringVar(&outputPath, "out", "", "Output path for generated code (default: <app-path>/app)")
	fs.BoolVar(&watchFlag, "watch", false, "Watch <app-path>/services and regenerate on changes")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage:\n  polycode generate <app-path> [options]\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return
		}
		os.Exit(2)
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "missing required <app-path>")
		fs.Usage()
		os.Exit(2)
	}
	appPath := fs.Arg(0)

	// Defaults
	if outputPath == "" {
		outputPath = filepath.Join(appPath, "app")
	}

	// Validate app path
	if st, err := os.Stat(appPath); err != nil || !st.IsDir() {
		log.Fatalf("app path does not exist or is not a directory: %s", appPath)
	}

	// Pick generator
	g := generators[appLanguage]
	if g == nil {
		log.Fatalf("language %q is not supported", appLanguage)
	}

	if watchFlag {
		servicesPath := filepath.Join(appPath, "services")
		log.Printf("watching: %s", servicesPath)
		watch(servicesPath, func(event fsnotify.Event) {
			_ = g.OnChange(appPath, outputPath, event)
		})
		return
	}

	if err := g.Generate(appPath, outputPath); err != nil {
		log.Fatalf("failed to generate code: %v", err)
	}
}

// ========================= extract =========================

func cmdExtract(args []string) {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		out      string
		callback string
		cwd      string
	)

	fs.StringVar(&out, "out", "app.json", "Optional output file to write sanitized captured JSON (required)")
	fs.StringVar(&callback, "callback", "", "Optional HTTP URL to POST {\"data\": <sanitized>} to")
	fs.StringVar(&cwd, "cwd", "", "Optional working directory to run the client in")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage:\n  polycode extract <bin-path> [options]\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return
		}
		os.Exit(2)
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "missing required <bin-path>")
		fs.Usage()
		os.Exit(2)
	}
	client := fs.Arg(0)

	if out == "" {
		fmt.Fprintln(os.Stderr, "missing required -out")
		fs.Usage()
		os.Exit(2)
	}

	runExtractor(client, out, callback, cwd)
}

func runExtractor(client, out, callback, cwd string) {
	// Start capture server
	done := make(chan []byte, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(capturePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		safe := sanitizeBody(body)

		// Print extracted/sanitized JSON before writing
		fmt.Println("=== Extracted JSON (sanitized) ===")
		fmt.Println(string(safe))
		fmt.Println("=== End Extracted JSON ===")

		// Always write to file atomically
		tmp := out + ".tmp"
		if err := os.WriteFile(tmp, safe, 0o600); err != nil {
			http.Error(w, "write error", http.StatusInternalServerError)
			return
		}
		_ = os.Rename(tmp, out)
		fmt.Println("Saved sanitized JSON to:", out)

		// Acknowledge client
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))

		select {
		case done <- safe:
		default:
		}
	})

	server := &http.Server{Addr: captureAddr, Handler: mux}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errorsIsClosed(err) {
			fmt.Fprintln(os.Stderr, "capture server error:", err)
		}
	}()

	// Run client (no args)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, client)
	if cwd != "" {
		if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
		cmd.Dir = cwd
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = dropXX(os.Environ())

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "failed to start client:", err)
		_ = server.Close()
		os.Exit(1)
	}

	// Graceful shutdown on SIGINT/SIGTERM
	go handleSignals(func() {
		gracefulStop(cmd.Process)
		_ = server.Close()
	})

	// Wait for capture or timeout
	select {
	case body := <-done:
		// Optionally POST to callback
		if strings.TrimSpace(callback) != "" {
			fmt.Println("Posting metadata to:", callback)
			postCtx, cancelPost := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancelPost()
			if err := postMetadata(postCtx, callback, body); err != nil {
				fmt.Fprintln(os.Stderr, "callback error:", err)
				gracefulStop(cmd.Process)
				_ = server.Close()
				os.Exit(3)
			}
			fmt.Println("Posted metadata successfully.")
		}

		gracefulStop(cmd.Process)
		shutdown(server)
		os.Exit(0)

	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "timed out waiting for app start request")
		gracefulStop(cmd.Process)
		shutdown(server)
		os.Exit(124)
	}
}

// ========================= helpers =========================

func watch(watchPath string, onChange func(event fsnotify.Event)) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("failed to create watcher: %v", err)
	}
	defer watcher.Close()

	// Handle OS signals for graceful shutdown
	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				// auto-add new directories
				if event.Op&fsnotify.Create == fsnotify.Create {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						if err := watcher.Add(event.Name); err != nil {
							log.Printf("failed to watch new dir %s: %v", event.Name, err)
						}
					}
				}

				if event.Op&fsnotify.Write == fsnotify.Write {
					onChange(event)
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("watcher error: %v", err)
			}
		}
	}()

	// initial walk
	err = filepath.Walk(watchPath, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("walk error on %s: %v", p, err)
			return err
		}
		if info.IsDir() {
			return watcher.Add(p)
		}
		return nil
	})
	if err != nil {
		log.Fatalf("failed to initialize watcher: %v", err)
	}

	// Block forever; exit on signal
	handleSignals(func() {
		watcher.Close()
	})

	<-done
}

func handleSignals(onTerm func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		onTerm()
	}()
}

func sanitizeBody(b []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		// Not a JSON object → keep original bytes
		return b
	}
	delete(m, "appName")
	delete(m, "appEndpoint")
	out, err := json.Marshal(m) // compact
	if err != nil {
		return b
	}
	return out
}

func postMetadata(ctx context.Context, url string, body []byte) error {
	var data json.RawMessage
	if json.Valid(body) {
		data = json.RawMessage(body)
	} else {
		esc, _ := json.Marshal(string(body))
		data = json.RawMessage(esc)
	}

	payload := map[string]any{"data": data}
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	// simple retry loop (3 tries)
	var lastErr error
	for i := 0; i < 3; i++ {
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("http %d", resp.StatusCode)
		}
		time.Sleep(time.Duration(i+1) * 300 * time.Millisecond)
	}
	return lastErr
}

func dropXX(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && strings.EqualFold(kv[:i], "XX") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func gracefulStop(p *os.Process) {
	if p == nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !alive(p.Pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = p.Kill()
	_, _ = p.Wait()
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func errorsIsClosed(err error) bool {
	// treat server closed as non-fatal without importing net.ErrClosed (older Go) or http.ErrServerClosed
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "closed network connection") || strings.Contains(msg, "server closed")
}

func printRootUsage() {
	fmt.Println(`polycode — code generator & extractor

Usage:
  polycode <command> [arguments]

Commands:
  generate   Generate code from an app folder (with optional watch mode)
  extract    Run a client binary and capture its startup POST payload

Run 'polycode <command> -h' for more details.

Examples:
  polycode generate ./myapp -language go -out ./myapp/app
  polycode generate ./myapp -watch

  polycode extract ./bin/myclient
  polycode extract ./bin/myclient -out ./meta.json -callback http://localhost:8080/hook -cwd ./sandbox
`)
}
