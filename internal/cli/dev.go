package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"nebulous/internal/manifest"
	"nebulous/internal/queryexec"
)

type reloadHub struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
}

type devGateway struct {
	root         string
	manifestPath string
	state        localState
	tenant       string
	appDir       string
	proxy        *httputil.ReverseProxy
	hub          *reloadHub
	host         string
}

func runAppDev(ctx context.Context, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	if jsonOutput {
		return fmt.Errorf("neb app dev is a long-running command and does not support --json")
	}
	var childArgs []string
	for index, value := range args {
		if value == "--" {
			childArgs, args = append([]string(nil), args[index+1:]...), args[:index]
			break
		}
	}
	listen, args, err := takeValueFlag(args, "--listen")
	if err != nil {
		return err
	}
	proxyURL, args, err := takeValueFlag(args, "--proxy")
	if err != nil {
		return err
	}
	if len(args) != 0 {
		return fmt.Errorf("usage: neb app dev [--listen 127.0.0.1:8787] [--proxy <url>] [-- command...]")
	}
	if listen == "" {
		listen = "127.0.0.1:8787"
	}
	if host, _, err := net.SplitHostPort(listen); err != nil || !isLoopbackHost(host) {
		return fmt.Errorf("--listen must be a loopback host and port")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	loaded, err := manifest.Load(filepath.Join(root, manifest.FileName))
	if err != nil {
		return err
	}
	state, err := loadState()
	if err != nil {
		return err
	}
	tenant, err := resolveTenant(ctx, state, "")
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	gateway := &devGateway{root: root, manifestPath: filepath.Join(root, manifest.FileName), state: state,
		tenant: tenant, appDir: filepath.Join(root, loaded.App.Dir), hub: &reloadHub{clients: map[chan struct{}]struct{}{}},
		host: listener.Addr().String()}
	if proxyURL != "" {
		target, err := url.Parse(proxyURL)
		if err != nil || target.Scheme == "" || target.Host == "" {
			return fmt.Errorf("--proxy must be an absolute URL")
		}
		gateway.proxy = httputil.NewSingleHostReverseProxy(target)
	}

	devCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var child *exec.Cmd
	childDone := make(chan error, 1)
	if len(childArgs) > 0 {
		child = exec.Command(childArgs[0], childArgs[1:]...)
		child.Dir, child.Env, child.Stdout, child.Stderr = root, os.Environ(), stdout, stderr
		if err := child.Start(); err != nil {
			return fmt.Errorf("start frontend command: %w", err)
		}
		go func() { childDone <- child.Wait() }()
	}
	if gateway.proxy == nil {
		watcher, err := watchDevFiles(devCtx, root, loaded, gateway.hub)
		if err != nil {
			stopChild(child, childDone)
			return err
		}
		defer watcher.Close()
	}
	server := &http.Server{Handler: gateway, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	address := "http://" + listener.Addr().String()
	fmt.Fprintf(stdout, "App: %s\nTenant: %s\nQueries reload on every request.\n", address, tenant)
	var result error
	select {
	case result = <-serverDone:
		if errors.Is(result, http.ErrServerClosed) {
			result = nil
		}
	case result = <-childDone:
		if result != nil {
			result = fmt.Errorf("frontend command stopped: %w", result)
		}
	case <-ctx.Done():
	}
	cancel()
	shutdown, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = server.Shutdown(shutdown)
	shutdownCancel()
	stopChild(child, childDone)
	return result
}

func (g *devGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Host, g.host) {
		http.Error(w, "misdirected development request", http.StatusMisdirectedRequest)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+g.host {
		http.Error(w, "development origin rejected", http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/runtime/v1/queries/") {
		g.query(w, r)
		return
	}
	if g.proxy != nil {
		g.proxy.ServeHTTP(w, r)
		return
	}
	if r.URL.Path == "/__neb/reload" {
		g.reload(w, r)
		return
	}
	g.static(w, r)
}

func (g *devGateway) query(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/runtime/v1/queries/")
	loaded, err := manifest.Load(g.manifestPath)
	if err != nil {
		writeDevError(w, http.StatusUnprocessableEntity, err)
		return
	}
	var selected *manifest.Query
	for index := range loaded.Queries {
		if loaded.Queries[index].Name == name {
			selected = &loaded.Queries[index]
			break
		}
	}
	if selected == nil {
		writeDevError(w, http.StatusNotFound, fmt.Errorf("query %q is not declared", name))
		return
	}
	var input struct {
		Parameters map[string]any `json:"parameters"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		writeDevError(w, http.StatusBadRequest, fmt.Errorf("body must contain parameters"))
		return
	}
	sqlText, err := os.ReadFile(filepath.Join(g.root, filepath.Clean(selected.File)))
	if err != nil {
		writeDevError(w, http.StatusUnprocessableEntity, err)
		return
	}
	_, types, err := queryexec.Validate(string(sqlText), input.Parameters)
	if err != nil || !reflect.DeepEqual(types, selected.Parameters) {
		writeDevError(w, http.StatusUnprocessableEntity, fmt.Errorf("query parameters do not match the manifest contract"))
		return
	}
	body, _ := json.Marshal(map[string]any{"sql": string(sqlText), "parameters": input.Parameters})
	payload, status, err := gatewayRequest(r.Context(), g.state, "/v1/tenants/"+url.PathEscape(g.tenant)+"/queries/execute", body)
	if err != nil {
		writeDevError(w, http.StatusBadGateway, err)
		return
	}
	if status < 200 || status >= 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(payload)
		return
	}
	var response struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(payload, &response) != nil || len(response.Result) == 0 {
		writeDevError(w, http.StatusBadGateway, fmt.Errorf("platform returned an invalid query result"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(response.Result)
}

func (g *devGateway) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	path := filepath.Clean("/" + r.URL.Path)
	if path == "/" {
		path = "/index.html"
	}
	file, err := http.Dir(g.appDir).Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	if strings.EqualFold(filepath.Ext(info.Name()), ".html") {
		contents, err := io.ReadAll(io.LimitReader(file, 2<<20))
		if err != nil {
			http.Error(w, "read development asset", http.StatusInternalServerError)
			return
		}
		script := `<script>new EventSource('/__neb/reload').onmessage=()=>location.reload()</script>`
		contents = bytes.Replace(contents, []byte("</body>"), []byte(script+"</body>"), 1)
		if !bytes.Contains(contents, []byte(script)) {
			contents = append(contents, script...)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", fmt.Sprint(len(contents)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(contents)
		}
		return
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(info.Name())))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (g *devGateway) reload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	updates := make(chan struct{}, 1)
	g.hub.mu.Lock()
	g.hub.clients[updates] = struct{}{}
	g.hub.mu.Unlock()
	defer func() {
		g.hub.mu.Lock()
		delete(g.hub.clients, updates)
		g.hub.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	select {
	case <-updates:
		_, _ = io.WriteString(w, "data: reload\n\n")
		flusher.Flush()
	case <-r.Context().Done():
	}
}

func (h *reloadHub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for client := range h.clients {
		select {
		case client <- struct{}{}:
		default:
		}
	}
}

func watchDevFiles(ctx context.Context, root string, loaded manifest.Manifest, hub *reloadHub) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{filepath.Join(root, loaded.App.Dir): true, root: true}
	for _, query := range loaded.Queries {
		paths[filepath.Join(root, filepath.Dir(query.File))] = true
	}
	for path := range paths {
		if err := addWatchTree(watcher, path); err != nil {
			watcher.Close()
			return nil, err
		}
	}
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Create != 0 {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						_ = addWatchTree(watcher, event.Name)
					}
				}
				hub.broadcast()
			case <-watcher.Errors:
			case <-ctx.Done():
				return
			}
		}
	}()
	return watcher, nil
}

func addWatchTree(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if path != root && (name == ".git" || name == "node_modules" || name == ".nebulous") {
			return filepath.SkipDir
		}
		return watcher.Add(path)
	})
}

func gatewayRequest(ctx context.Context, state localState, path string, body []byte) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(state.APIURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+state.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := apiClient.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 11<<20))
	return payload, response.StatusCode, err
}

func writeDevError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func stopChild(command *exec.Cmd, done <-chan error) {
	if command == nil || command.ProcessState != nil {
		return
	}
	_ = command.Process.Signal(os.Interrupt)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
	}
}
