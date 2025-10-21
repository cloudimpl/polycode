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

	gettingStartedRepo   = "https://github.com/cloudimpl/polycode-getting-started.git"
	defaultGettingBranch = "main"
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

	case "new":
		cmdNew(os.Args[2:])

	case "build":
		cmdBuild(os.Args[2:])

	case "run":
		cmdRun(os.Args[2:])

	case "extract":
		cmdExtract(os.Args[2:])

	// Optional compatibility alias (uncomment if you want to keep it)
	// case "generate":
	// 	fmt.Fprintln(os.Stderr, "warning: 'generate' is deprecated; use 'build' instead")
	// 	cmdBuild(os.Args[2:])

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		printRootUsage()
		os.Exit(2)
	}
}

// ========================= new =========================

// polycode new <project-name> [options]
func cmdNew(args []string) {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		projectName string
		lang        string
		repo        string
		branch      string
	)
	fs.StringVar(&lang, "language", "go", "Template language: go | java | python")
	fs.StringVar(&repo, "repo", gettingStartedRepo, "Getting started repo (advanced)")
	fs.StringVar(&branch, "branch", defaultGettingBranch, "Repo branch (advanced)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage:\n  polycode new <project-name> [options]\n\nOptions:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return
		}
		os.Exit(2)
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "missing required <project-name>")
		fs.Usage()
		os.Exit(2)
	}
	projectName = fs.Arg(0)

	lang = strings.ToLower(strings.TrimSpace(lang))
	switch lang {
	case "go", "java", "python":
	default:
		log.Fatalf("unsupported language %q (use: go | java | python)", lang)
	}

	dest := projectName
	if _, err := os.Stat(dest); err == nil {
		log.Fatalf("destination folder %q already exists", dest)
	}

	// 1) clone into a temp dir
	tmpDir, err := os.MkdirTemp("", "polycode-gs-*")
	if err != nil {
		log.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	log.Printf("Cloning %s (branch %s)...", repo, branch)
	if err := runCmd(".", "git", "clone", "--depth", "1", "--branch", branch, repo, tmpDir); err != nil {
		log.Fatalf("git clone failed: %v", err)
	}

	// 2) copy the language subfolder into projectName
	src := filepath.Join(tmpDir, lang)
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		log.Fatalf("template subfolder not found: %s", src)
	}
	if err := copyTree(src, dest); err != nil {
		log.Fatalf("failed to copy template: %v", err)
	}

	// 3) language-specific tweaks
	switch lang {
	case "go":
		// replace _getting_started in go.mod and .go files
		if err := replaceInFiles(dest, []string{".go", ".mod"}, "_getting_started", projectName); err != nil {
			log.Fatalf("failed to apply replacements: %v", err)
		}
		// run go mod tidy
		if err := runCmd(dest, "go", "mod", "tidy"); err != nil {
			log.Printf("warning: go mod tidy failed: %v", err)
		}
	case "java":
		// add Java-specific replacements if your template needs it
	case "python":
		// add Python-specific replacements if your template needs it
	}

	fmt.Printf("\n✅ Created %q from %s/%s template.\n\n", projectName, filepath.Base(repo), lang)
	fmt.Println("Next steps:")
	fmt.Printf("  cd %s\n", projectName)
	switch lang {
	case "go":
		fmt.Println("  polycode build .")
		fmt.Println("  go run ./app      # or: go build -o appbin ./app && ./appbin")
	case "java":
		fmt.Println("  # TODO: build & run steps for Java template")
	case "python":
		fmt.Println("  # TODO: build & run steps for Python template")
	}
	fmt.Println()
}

// ========================= build =========================

func cmdBuild(args []string) {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		appLanguage string
		outputPath  string
		watchFlag   bool
	)

	fs.StringVar(&appLanguage, "language", "auto", "Application language (supported: go)")
	fs.StringVar(&outputPath, "out", "", "Output path for generated code (default: <app-path>/app)")
	fs.BoolVar(&watchFlag, "watch", false, "Watch <app-path>/services and rebuild on changes")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage:\n  polycode build <app-path> [options]\n\nOptions:\n")
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

	if appLanguage == "" || appLanguage == "auto" {
		appLanguage = detectLanguage(appPath)
		if appLanguage == "" {
			log.Fatalf("unable to detect language for %s — please specify with -language", appPath)
		}
		fmt.Println("Detected language:", appLanguage)
	}

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
		log.Fatalf("failed to build: %v", err)
	}
}

// ========================= run =========================

// polycode run <app-path>
// - Builds into <app-path>/app (like build)
// - For Go: builds binary into .polycode/app and runs it
func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		appLanguage string
	)
	fs.StringVar(&appLanguage, "language", "auto", "Application language (supported: go)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage:\n  polycode run <app-path> [options]\n\nOptions:\n")
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

	// detect language
	if appLanguage == "" || appLanguage == "auto" {
		appLanguage = detectLanguage(appPath)
		if appLanguage == "" {
			log.Fatalf("unable to detect language for %s — please specify with -language", appPath)
		}
		fmt.Println("Detected language:", appLanguage)
	}

	// 1) build
	if err := os.MkdirAll(filepath.Join(appPath, "app"), 0o755); err != nil {
		log.Fatalf("failed to create app folder: %v", err)
	}
	cmdBuild([]string{appPath, "-language", appLanguage})

	// 2) build & run (language-specific)
	switch appLanguage {
	case "go":
		binDir := filepath.Join(appPath, ".polycode")
		_ = os.MkdirAll(binDir, 0o755)
		bin := filepath.Join(binDir, "app")
		// go mod tidy at root (in case builder added deps)
		_ = runCmd(appPath, "go", "mod", "tidy")
		// build the generated app
		if err := runCmd(appPath, "go", "build", "-o", bin, "./app"); err != nil {
			log.Fatalf("build binary failed: %v", err)
		}
		fmt.Printf("▶ Running %s\n\n", bin)
		// exec the app and proxy signals
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cmd := exec.CommandContext(ctx, bin)
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
		if err := cmd.Start(); err != nil {
			log.Fatalf("failed to start: %v", err)
		}
		done := make(chan struct{})
		go func() {
			handleSignals(func() {
				gracefulStop(cmd.Process)
			})
			_ = cmd.Wait()
			close(done)
		}()
		<-done

	case "java":
		log.Fatalf("run: java pipeline not implemented yet")
	case "python":
		log.Fatalf("run: python pipeline not implemented yet")
	default:
		log.Fatalf("run: unsupported language %q", appLanguage)
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

func detectLanguage(appPath string) string {
	files, _ := os.ReadDir(appPath)
	hasGo, hasJava, hasPython := false, false, false

	for _, f := range files {
		name := f.Name()
		switch {
		case name == "go.mod" || strings.HasSuffix(name, ".go"):
			hasGo = true
		case name == "pom.xml" || strings.HasPrefix(name, "build.gradle"):
			hasJava = true
		case name == "pyproject.toml" || name == "setup.py" || strings.HasSuffix(name, ".py"):
			hasPython = true
		}
	}

	// Priority order if multiple found (tune to your project needs)
	switch {
	case hasGo:
		return "go"
	case hasJava:
		return "java"
	case hasPython:
		return "python"
	default:
		return ""
	}
}

// ---------- local file ops / utilities ----------

func runCmd(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		// file
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func replaceInFiles(root string, exts []string, old, new string) error {
	extSet := map[string]struct{}{}
	for _, e := range exts {
		extSet[e] = struct{}{}
	}
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := filepath.Ext(p)
		if _, ok := extSet[ext]; !ok && !(strings.HasSuffix(p, "go.mod") && contains(exts, ".mod")) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		nb := []byte(strings.ReplaceAll(string(b), old, new))
		if !bytes.Equal(b, nb) {
			// atomic-ish write
			tmp := p + ".tmp"
			if err := os.WriteFile(tmp, nb, info.Mode().Perm()); err != nil {
				return err
			}
			if err := os.Rename(tmp, p); err != nil {
				return err
			}
		}
		return nil
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func printRootUsage() {
	fmt.Println(`polycode — project scaffolding, build & extractor

Usage:
  polycode <command> [arguments]

Commands:
  new        Create a new project from the getting-started repo
  build      Build code from an app folder (with optional watch mode)
  run        Build then run the app (Go supported)
  extract    Run a client binary and capture its startup POST payload

Run 'polycode <command> -h' for more details.

Examples:
  # Create a new project
  polycode new myapp -language go
  cd myapp
  polycode build .
  go run ./app

  # Build in-place
  polycode build ./myapp -language go -out ./myapp/app
  polycode build ./myapp -watch

  # Run (build + binary + execute)
  polycode run ./myapp

  # Extract startup metadata from an app binary
  polycode extract ./bin/myclient -out ./meta.json -callback http://localhost:8080/hook -cwd ./sandbox
`)
}
