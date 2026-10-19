// Command pingvin-share-x runs Pingvin Share X from one file. The file carries Node.js and the
// built frontend and backend. On first start, or after an upgrade, it unpacks them next to
// itself. It then runs the database migrations and seed, starts the backend and the frontend,
// and serves both on one port. This is the same layout as the Docker image.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// payload is the Node.js runtime and the built application. CI writes it before the build.
//
//go:embed payload.tar.gz
var payload []byte

// Version is set at build time.
var Version = "dev"

const (
	markerName   = ".payload-sha256"
	backendPort  = "8080"
	frontendPort = "3333"
)

func main() {
	listen := flag.String("listen", envOr("PINGVIN_LISTEN", ":3000"), "address for the public HTTP port")
	home := flag.String("home", os.Getenv("PINGVIN_HOME"), "directory for the app and the data (default: the directory of this file)")
	showVersion := flag.Bool("version", false, "print the version and the payload checksum, then exit")
	flag.Parse()

	sum := sha256.Sum256(payload)
	if *showVersion {
		fmt.Printf("pingvin-share-x %s payload %s\n", Version, hex.EncodeToString(sum[:]))
		return
	}
	if len(payload) == 0 {
		log.Fatal("this build has no payload; build it with the release workflow")
	}
	base, err := resolveHome(*home)
	if err != nil {
		log.Fatal(err)
	}
	if err := run(base, *listen, hex.EncodeToString(sum[:])); err != nil {
		log.Fatal(err)
	}
}

func run(base, listen, payloadSum string) error {
	app := filepath.Join(base, "app")
	data := filepath.Join(base, "data")
	if err := unpackIfNeeded(app, data, payloadSum); err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	env := childEnv(base, data)
	node := filepath.Join(app, "node", "bin", "node")
	backendDir := filepath.Join(app, "backend")
	frontendDir := filepath.Join(app, "frontend")

	if out, err := runOnce(backendDir, env, node, "node_modules/prisma/build/index.js", "migrate", "deploy"); err != nil {
		return fmt.Errorf("database migration failed: %w\n%s", err, out)
	}
	if out, err := runOnce(backendDir, env, node, "dist/prisma/seed/config.seed.js"); err != nil {
		return fmt.Errorf("database seed failed: %w\n%s", err, out)
	}

	backend, err := start("backend", backendDir, env, node, "dist/src/main.js")
	if err != nil {
		return err
	}
	frontendEnv := append(append([]string{}, env...), "PORT="+frontendPort, "HOSTNAME=127.0.0.1", "API_URL=http://127.0.0.1:"+backendPort)
	frontend, err := start("frontend", frontendDir, frontendEnv, node, "server.js")
	if err != nil {
		backend.stop()
		return err
	}

	if err := waitHealthy("http://127.0.0.1:"+backendPort+"/api/health", 120*time.Second); err != nil {
		backend.stop()
		frontend.stop()
		return err
	}

	srv := &http.Server{Addr: listen, Handler: router(
		mustURL("http://127.0.0.1:"+backendPort), mustURL("http://127.0.0.1:"+frontendPort))}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Printf("pingvin-share-x listening on %s", listen)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var reason error
	select {
	case <-ctx.Done():
		log.Print("stopping")
	case err := <-serveErr:
		reason = err
	case <-backend.done:
		reason = fmt.Errorf("backend exited: %v", backend.err)
	case <-frontend.done:
		reason = fmt.Errorf("frontend exited: %v", frontend.err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	backend.stop()
	frontend.stop()
	if reason != nil && !errors.Is(reason, http.ErrServerClosed) {
		return reason
	}
	return nil
}

// resolveHome returns the install directory: -home or PINGVIN_HOME, else the directory of this file.
func resolveHome(flagValue string) (string, error) {
	if flagValue != "" {
		return filepath.Abs(flagValue)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

// unpackIfNeeded unpacks the payload into app when app was made from a different payload. The data
// directory is kept. The uploaded logos in data/images are kept too, and the default logos are
// copied in only where no file exists yet.
func unpackIfNeeded(app, data, payloadSum string) error {
	marker := filepath.Join(app, markerName)
	if b, err := os.ReadFile(marker); err == nil && string(b) == payloadSum {
		return nil
	}
	if err := os.RemoveAll(app); err != nil {
		return err
	}
	if err := os.MkdirAll(app, 0o755); err != nil {
		return err
	}
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer gz.Close()
	if err := extractTar(gz, app); err != nil {
		return err
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	appImages := filepath.Join(app, "frontend", "public", "img")
	dataImages := filepath.Join(data, "images")
	if _, err := os.Stat(dataImages); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(appImages, dataImages); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if err := copyMissing(appImages, dataImages); err != nil {
		return err
	}
	if err := os.RemoveAll(appImages); err != nil {
		return err
	}
	if err := os.MkdirAll(dataImages, 0o755); err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Dir(appImages), dataImages)
	if err != nil {
		return err
	}
	if err := os.Symlink(rel, appImages); err != nil {
		return err
	}
	return os.WriteFile(marker, []byte(payloadSum), 0o644)
}

// childEnv is the environment for the Node processes. It matches what the Docker image sets, with
// absolute paths and without the container-only NODE_ENV value.
func childEnv(base, data string) []string {
	env := os.Environ()
	return append(env,
		"NODE_ENV=production",
		"BACKEND_PORT="+backendPort,
		"DATA_DIRECTORY="+data,
		"DATABASE_URL=file:"+filepath.Join(data, "pingvin-share.db")+"?connection_limit=1",
		"CONFIG_FILE="+filepath.Join(base, "config.yaml"),
	)
}

func runOnce(dir string, env []string, node string, args ...string) (string, error) {
	cmd := exec.Command(node, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// proc is a supervised child process.
type proc struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func start(name, dir string, env []string, node string, args ...string) (*proc, error) {
	cmd := exec.Command(node, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = &prefixWriter{prefix: "[" + name + "] ", w: os.Stdout}
	cmd.Stderr = &prefixWriter{prefix: "[" + name + "] ", w: os.Stderr}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	p := &proc{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// stop sends SIGTERM to the process group and waits, then kills what is left.
func (p *proc) stop() {
	if p == nil || p.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

// prefixWriter adds a name in front of each line from a child process.
type prefixWriter struct {
	prefix string
	w      io.Writer
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.buf[:i])
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

// router sends /api to the backend and everything else to the frontend, as the Caddy config does.
func router(backend, frontend *url.URL) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", httputil.NewSingleHostReverseProxy(backend))
	mux.Handle("/", httputil.NewSingleHostReverseProxy(frontend))
	return mux
}

func waitHealthy(healthURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("backend did not become healthy at %s within %s", healthURL, timeout)
}

func mustURL(raw string) *url.URL {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		panic(err)
	}
	return u
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
