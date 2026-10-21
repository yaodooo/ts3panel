package api

import (
	"context"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"ts3panel/internal/backup"
	"ts3panel/internal/dns"
	"ts3panel/internal/orchestrator"
	"ts3panel/internal/store"
	"ts3panel/web"
)

// Options supplies the already-opened stores to the API. Configuration and
// state are loaded before the listener starts so permission errors fail fast.
type Options struct {
	Config  *store.ConfigStore
	State   *store.StateStore
	Version string
	// Manager may be injected by tests; production builds create one from the
	// config's Docker socket and instance root.
	Manager *orchestrator.Manager

	// DNS seams. Production builds use the defaults; tests inject a fake
	// provider factory and a short verification window.
	HTTPClient       *http.Client
	ProviderFactory  func(store.ProviderConfig, map[string]string) (dns.Provider, error)
	Verifier         *dns.Verifier
	DNSVerifyTimeout time.Duration
	DNSPollInterval  time.Duration
	PublicIPDetector func(context.Context) (string, error)
}

// API holds process-local services. Sessions and rate-limit counters are
// intentionally in memory; they reset on restart.
type API struct {
	config    *store.ConfigStore
	state     *store.StateStore
	cipher    *store.CredentialCipher
	version   string
	instances *orchestrator.Manager
	backups   *backup.Manager
	scheduler *backup.Scheduler

	sessions       *sessionManager
	loginLimiter   *attemptLimiter
	forgotLimiter  *attemptLimiter
	setupMu        sync.Mutex
	dummyHash      []byte
	listenLoopback bool

	dnsHTTP          *http.Client
	providerFactory  func(store.ProviderConfig, map[string]string) (dns.Provider, error)
	verifier         *dns.Verifier
	publicIPDetector func(context.Context) (string, error)
	publicIPMu       sync.Mutex
	publicIPCached   string
	dnsLocks         sync.Map
	voiceProbe       voicePortProbe
	probeCache       *voiceProbeCache
}

func New(opts Options) (*API, error) {
	if opts.Config == nil {
		return nil, errors.New("api: config store is required")
	}
	if opts.State == nil {
		return nil, errors.New("api: state store is required")
	}
	cfg := opts.Config.Snapshot()
	cipher, err := store.NewCredentialCipher(cfg.MasterKey)
	if err != nil {
		return nil, err
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte("ts3panel-dummy-password-not-a-real-account"), bcryptCost)
	if err != nil {
		return nil, err
	}
	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = "dev"
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	verifier := opts.Verifier
	if verifier == nil {
		verifier = dns.NewVerifier()
		if opts.DNSVerifyTimeout > 0 {
			verifier.Timeout = opts.DNSVerifyTimeout
		}
		if opts.DNSPollInterval > 0 {
			verifier.Interval = opts.DNSPollInterval
		}
	}

	manager := opts.Manager
	if manager == nil {
		manager, err = orchestrator.New(orchestrator.Options{
			Store:        opts.State,
			Root:         cfg.InstanceRoot,
			DockerSocket: cfg.Docker.Socket,
			MaxInstances: cfg.Limits.MaxInstances,
			MemoryBytes:  parseMemoryLimit(cfg.Limits.TS3Memory),
			NanoCPUs:     parseCPULimit(cfg.Limits.TS3CPUs),
		})
		if err != nil {
			return nil, err
		}
	}

	a := &API{
		config:         opts.Config,
		state:          opts.State,
		cipher:         cipher,
		version:        version,
		instances:      manager,
		sessions:       newSessionManager(),
		loginLimiter:   newAttemptLimiter(5, 15*time.Minute, 15*time.Minute),
		forgotLimiter:  newAttemptLimiter(10, 15*time.Minute, 15*time.Minute),
		dummyHash:      dummy,
		listenLoopback: isLoopbackListen(cfg.Listen),

		dnsHTTP:          httpClient,
		providerFactory:  opts.ProviderFactory,
		verifier:         verifier,
		publicIPDetector: opts.PublicIPDetector,
		voiceProbe:       newVoicePortProbe(),
		probeCache:       newVoiceProbeCache(),
	}

	backupMgr, err := backup.New(backup.Options{
		Store:     opts.State,
		Root:      manager.Root(),
		Lifecycle: managerLifecycle{m: manager},
	})
	if err != nil {
		return nil, err
	}
	scheduler, err := backup.NewScheduler(backup.SchedulerOptions{
		Config:  opts.Config,
		State:   opts.State,
		Manager: backupMgr,
	})
	if err != nil {
		return nil, err
	}
	a.backups = backupMgr
	a.scheduler = scheduler
	a.scheduler.Start()

	// §11.3 / M2-7: annotate instances left pending by an interrupted create so
	// the UI can offer "continue" (start) or "clean up" (delete). Docker being
	// unavailable must not prevent the panel from starting.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.instances.Reconcile(ctx); err != nil {
			slog.Warn("startup reconcile failed", "err", err)
		}
	}()

	return a, nil
}

// Close stops the backup scheduler and releases the Docker client.
func (a *API) Close() error {
	if a.scheduler != nil {
		a.scheduler.Stop()
	}
	if a.instances != nil {
		return a.instances.Close()
	}
	return nil
}

// Handler builds the complete middleware chain. Order matters:
// security headers -> access log -> panic recovery -> setup gate -> CSRF -> mux.
// The setup gate is outside CSRF so a first-run browser request is redirected
// to /setup even before any API header is involved.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", a.handleHealth)
	mux.HandleFunc("GET /api/setup/state", a.handleSetupState)
	mux.HandleFunc("POST /api/setup", a.handleSetup)
	mux.HandleFunc("POST /api/setup/skip-dns", a.handleSetupSkipDNS)

	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
	mux.HandleFunc("GET /api/session", a.handleSession)
	mux.HandleFunc("POST /api/password/forgot", a.handleForgotPassword)

	mux.HandleFunc("GET /api/instances", a.requireAuth(a.handleListInstances))
	mux.HandleFunc("POST /api/instances", a.requireAuth(a.handleCreateInstance))
	mux.HandleFunc("GET /api/instances/{id}", a.requireAuth(a.handleGetInstance))
	mux.HandleFunc("POST /api/instances/{id}/start", a.requireAuth(a.handleStartInstance))
	mux.HandleFunc("POST /api/instances/{id}/stop", a.requireAuth(a.handleStopInstance))
	mux.HandleFunc("POST /api/instances/{id}/restart", a.requireAuth(a.handleRestartInstance))
	mux.HandleFunc("DELETE /api/instances/{id}", a.requireAuth(a.handleDeleteInstance))

	mux.HandleFunc("POST /api/instances/{id}/backup", a.requireAuth(a.handleCreateBackup))
	mux.HandleFunc("GET /api/instances/{id}/backup", a.requireAuth(a.handleDownloadBackup))
	mux.HandleFunc("GET /api/instances/{id}/backups", a.requireAuth(a.handleListBackups))
	mux.HandleFunc("POST /api/instances/{id}/restore", a.requireAuth(a.handleRestore))
	mux.HandleFunc("GET /api/backups", a.requireAuth(a.handleListAllBackups))
	mux.HandleFunc("GET /api/backup/plan", a.requireAuth(a.handleGetBackupPlan))
	mux.HandleFunc("PUT /api/backup/plan", a.requireAuth(a.handleUpdateBackupPlan))

	mux.HandleFunc("POST /api/instances/{id}/domain", a.requireAuth(a.handleBindDomain))
	mux.HandleFunc("DELETE /api/instances/{id}/domain", a.requireAuth(a.handleUnbindDomain))
	mux.HandleFunc("POST /api/instances/{id}/domain/verify", a.requireAuth(a.handleVerifyDomain))
	mux.HandleFunc("GET /api/instances/{id}/domain/manual", a.requireAuth(a.handleManualDomain))
	mux.HandleFunc("GET /api/domain/precheck", a.requireAuth(a.handleDomainPrecheck))

	mux.HandleFunc("GET /api/settings", a.requireAuth(a.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", a.requireAuth(a.handleUpdateSettings))

	mux.HandleFunc("GET /api/ports", a.requireAuth(a.handlePorts))
	mux.HandleFunc("GET /api/ports/check", a.requireAuth(a.handlePortsCheck))

	mux.HandleFunc("GET /api/providers", a.requireAuth(a.handleListProviders))
	mux.HandleFunc("POST /api/providers", a.requireAuth(a.handleCreateProvider))
	mux.HandleFunc("POST /api/providers/test", a.requireAuth(a.handleTestProvider))
	mux.HandleFunc("POST /api/providers/{id}", a.requireAuth(a.handleUpdateProvider))
	mux.HandleFunc("PUT /api/providers/{id}", a.requireAuth(a.handleUpdateProvider))
	mux.HandleFunc("DELETE /api/providers/{id}", a.requireAuth(a.handleDeleteProvider))

	mux.HandleFunc("/api/", a.handleAPINotFound)
	// Keep the web handler method-neutral: a "GET /" pattern cannot coexist with
	// the method-neutral "/api/" catch-all on Go's ServeMux.
	mux.HandleFunc("/", a.handleWeb)

	var h http.Handler = mux
	h = a.csrf(h)
	h = a.setupGate(h)
	h = a.recoverPanic(h)
	h = a.accessLog(h)
	h = a.securityHeaders(h)
	return h
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": a.version})
}

func (a *API) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "E_NOT_FOUND", "接口不存在。", "")
}

func (a *API) handleWeb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "E_METHOD_NOT_ALLOWED", "请求方法不允许。", "")
		return
	}
	switch r.URL.Path {
	case "/":
		a.serveWebFile(w, r, "index.html")
	case "/index.html":
		a.serveWebFile(w, r, "index.html")
	case "/setup", "/setup.html":
		a.serveWebFile(w, r, "setup.html")
	case "/app.js":
		a.serveWebFile(w, r, "app.js")
	case "/style.css":
		a.serveWebFile(w, r, "style.css")
	case "/i18n/zh-CN.json":
		a.serveWebFile(w, r, "i18n/zh-CN.json")
	case "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (a *API) serveWebFile(w http.ResponseWriter, r *http.Request, name string) {
	data, err := web.FS.ReadFile(name)
	if err != nil {
		slog.Error("read embedded web asset", "asset", name, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	contentType := mime.TypeByExtension(filepath.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	if strings.HasSuffix(name, ".html") {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		if _, err := w.Write(data); err != nil {
			slog.Error("write web asset", "asset", name, "err", err)
		}
	}
}
