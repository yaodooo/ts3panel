package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"

	"ts3panel/internal/model"
	"ts3panel/internal/store"
)

const (
	managedLabel  = "ts3panel.managed"
	instanceLabel = "ts3panel.instance"
	containerPref = "ts3-"

	defaultTS3Image  = "teamspeak:latest"
	fallbackTS3Image = "teamspeak/teamspeak3-server:latest"

	defaultMemoryBytes = 512 * 1024 * 1024
	defaultNanoCPUs    = int64(1_000_000_000)
	defaultWaitTimeout = 60 * time.Second
	defaultTokenTries  = 6
	defaultTokenWait   = 5 * time.Second
)

var instanceIDRE = regexp.MustCompile(`^[a-z0-9-]{1,20}$`)

// Errors are matched by the API layer with errors.Is and translated to the
// documented E_* codes. They never contain credentials or tokens.
var (
	ErrInstanceNotFound = errors.New("instance not found")
	ErrInstanceExists   = errors.New("instance already exists")
	ErrSubdomainTaken   = errors.New("subdomain already in use")
	ErrSubdomainInvalid = errors.New("subdomain invalid")
	ErrNameInvalid      = errors.New("instance name invalid")
	ErrLimitReached     = errors.New("instance limit reached")
	ErrContainerTimeout = errors.New("container did not become running")
	ErrNotManaged       = errors.New("refusing to touch a container not managed by ts3panel")
	ErrDocker           = errors.New("docker operation failed")
	ErrContainerMissing = errors.New("container missing")
)

// StateStore is the subset of *store.StateStore the orchestrator needs. All
// writes go through Update so the in-process cache stays coherent (M1-d).
type StateStore interface {
	Snapshot() (store.State, error)
	Update(func(*store.State) error) error
}

// ContainerSpec is the runtime-independent description of one TS3 container.
// Keeping Docker types out of this struct makes the manager unit-testable with
// a fake runtime.
type ContainerSpec struct {
	Name          string
	Image         string
	Env           []string
	Cmd           []string
	Labels        map[string]string
	Binds         []string
	NetworkMode   string
	RestartPolicy string
	Memory        int64
	NanoCPUs      int64
	LogMaxSize    string
	LogMaxFiles   string
}

// ContainerInfo is the subset of docker inspect the panel needs.
type ContainerInfo struct {
	ID       string
	Name     string
	Image    string
	State    string
	ExitCode int
	Labels   map[string]string
}

// Runtime abstracts the Docker Engine. The production implementation is
// dockerRuntime; tests use a fake that records calls.
type Runtime interface {
	Ping(ctx context.Context) error
	EnsureImage(ctx context.Context, ref string) error
	Create(ctx context.Context, spec ContainerSpec) (string, error)
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string, timeout time.Duration) error
	Restart(ctx context.Context, name string, timeout time.Duration) error
	Remove(ctx context.Context, name string, force bool) error
	Inspect(ctx context.Context, name string) (*ContainerInfo, error)
	Logs(ctx context.Context, name string, tail int) (string, error)
	Stats(ctx context.Context, name string) (StatsSample, error)
	Close() error
}

// CreateParams is the validated input from the API layer.
type CreateParams struct {
	ID         string
	Name       string
	Subdomain  string
	ProviderID string
}

// InstanceView is an Instance plus its live container state. AdminToken is
// present only in detail/create responses; list responses blank it in the API.
type InstanceView struct {
	model.Instance
	ContainerState string `json:"container_state"`
	Recoverable    bool   `json:"recoverable,omitempty"`
}

// Options configures a Manager. Zero values fall back to the documented
// defaults so callers only have to pass the store and instance root.
type Options struct {
	Store        StateStore
	Root         string
	DockerSocket string
	Images       []string
	MaxInstances int
	MemoryBytes  int64
	NanoCPUs     int64
	Probe        Probe
	Runtime      Runtime
	Logger       *slog.Logger

	WaitTimeout   time.Duration
	TokenAttempts int
	TokenInterval time.Duration
	// StatsTTL controls the resource-snapshot cache duration (default 10s).
	// StatsSampleGap is the delay between the two CPU counter reads
	// (default 1s); tests shorten both.
	StatsTTL       time.Duration
	StatsSampleGap time.Duration
	// RunningGrace is how long a container must stay running before it is
	// considered healthy (crash loops are otherwise reported as ready).
	RunningGrace time.Duration
	PollInterval time.Duration
	Now          func() time.Time
}

// Manager owns instance lifecycle. Create is serialized so two concurrent
// requests can never persist the same port group.
type Manager struct {
	store   StateStore
	runtime Runtime
	root    string
	images  []string

	maxInstances int
	memory       int64
	nanoCPUs     int64
	probe        Probe
	logger       *slog.Logger
	now          func() time.Time

	waitTimeout   time.Duration
	tokenAttempts int
	tokenInterval time.Duration
	runningGrace  time.Duration
	pollInterval  time.Duration
	statsTTL      time.Duration
	statsGap      time.Duration

	statsMu    sync.Mutex
	statsCache map[string]statsEntry

	createMu  sync.Mutex
	closeOnce sync.Once
}

// New builds a Manager. It does not contact the Docker daemon; connection
// errors surface on the first operation, so the panel can still start and show
// its UI when Docker is temporarily unavailable.
func New(opts Options) (*Manager, error) {
	if opts.Store == nil {
		return nil, errors.New("orchestrator: state store is required")
	}
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		root = "/srv/ts3"
	}
	images := opts.Images
	if len(images) == 0 {
		images = []string{defaultTS3Image, fallbackTS3Image}
	}
	if opts.MaxInstances <= 0 {
		opts.MaxInstances = 20
	}
	if opts.MemoryBytes <= 0 {
		opts.MemoryBytes = defaultMemoryBytes
	}
	if opts.NanoCPUs <= 0 {
		opts.NanoCPUs = defaultNanoCPUs
	}
	if opts.WaitTimeout <= 0 {
		opts.WaitTimeout = defaultWaitTimeout
	}
	if opts.TokenAttempts <= 0 {
		opts.TokenAttempts = defaultTokenTries
	}
	if opts.TokenInterval <= 0 {
		opts.TokenInterval = defaultTokenWait
	}
	if opts.RunningGrace <= 0 {
		opts.RunningGrace = 2 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.StatsTTL <= 0 {
		opts.StatsTTL = 10 * time.Second
	}
	if opts.StatsSampleGap <= 0 {
		opts.StatsSampleGap = time.Second
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	probe := opts.Probe
	if probe == nil {
		probe = bindProbe
	}

	rt := opts.Runtime
	if rt == nil {
		var err error
		rt, err = newDockerRuntime(opts.DockerSocket)
		if err != nil {
			// The panel must still start (and show a useful error) when Docker
			// cannot be reached, e.g. an unsupported development host.
			logger.Warn("docker runtime unavailable", "err", err)
			rt = unavailableRuntime{err: err}
		}
	}
	return &Manager{
		store:         opts.Store,
		runtime:       rt,
		root:          root,
		images:        images,
		maxInstances:  opts.MaxInstances,
		memory:        opts.MemoryBytes,
		nanoCPUs:      opts.NanoCPUs,
		probe:         probe,
		logger:        logger,
		now:           nowFn,
		waitTimeout:   opts.WaitTimeout,
		tokenAttempts: opts.TokenAttempts,
		tokenInterval: opts.TokenInterval,
		runningGrace:  opts.RunningGrace,
		pollInterval:  opts.PollInterval,
		statsTTL:      opts.StatsTTL,
		statsGap:      opts.StatsSampleGap,
		statsCache:    map[string]statsEntry{},
	}, nil
}

// Close releases the Docker client.
func (m *Manager) Close() error {
	var err error
	m.closeOnce.Do(func() {
		err = m.runtime.Close()
	})
	return err
}

// Root returns the instance root directory.
func (m *Manager) Root() string { return m.root }

// ContainerName returns the fixed container name for an instance id.
func ContainerName(id string) string { return containerPref + id }

// Create provisions one instance. It is re-entrant in the sense that every
// side effect is preceded by a duplicate/ownership check; a failed provision is
// rolled back to a failed state and the error explains why.
func (m *Manager) Create(ctx context.Context, params CreateParams) (InstanceView, error) {
	params.ID = strings.TrimSpace(params.ID)
	params.Name = strings.TrimSpace(params.Name)
	params.Subdomain = strings.ToLower(strings.TrimSpace(params.Subdomain))

	if !instanceIDRE.MatchString(params.ID) {
		return InstanceView{}, fmt.Errorf("%w: id must match [a-z0-9-]{1,20}", ErrSubdomainInvalid)
	}
	if params.Name == "" {
		params.Name = params.ID
	}
	if err := validateDisplayName(params.Name); err != nil {
		return InstanceView{}, err
	}
	if params.Subdomain == "" {
		params.Subdomain = params.ID
	}

	m.createMu.Lock()
	defer m.createMu.Unlock()

	if err := m.runtime.Ping(ctx); err != nil {
		return InstanceView{}, fmt.Errorf("%w: docker unavailable: %v", ErrDocker, err)
	}

	st, err := m.store.Snapshot()
	if err != nil {
		return InstanceView{}, err
	}
	if len(st.Instances) >= m.maxInstances {
		return InstanceView{}, fmt.Errorf("%w: max_instances=%d", ErrLimitReached, m.maxInstances)
	}
	for i := range st.Instances {
		if st.Instances[i].ID == params.ID {
			return InstanceView{}, fmt.Errorf("%w: id %q", ErrInstanceExists, params.ID)
		}
		if strings.EqualFold(st.Instances[i].Subdomain, params.Subdomain) {
			return InstanceView{}, fmt.Errorf("%w: %q", ErrSubdomainTaken, params.Subdomain)
		}
		if st.Instances[i].Container == ContainerName(params.ID) {
			return InstanceView{}, fmt.Errorf("%w: container name %q", ErrInstanceExists, ContainerName(params.ID))
		}
	}

	ports, index, err := m.allocatePorts(st)
	if err != nil {
		return InstanceView{}, err
	}

	inst := model.Instance{
		ID:         params.ID,
		Name:       params.Name,
		Subdomain:  params.Subdomain,
		ProviderID: params.ProviderID,
		Ports:      ports,
		Container:  ContainerName(params.ID),
		Status:     model.InstanceStatusPending,
		DNSStatus:  model.DNSStatusNone,
		CreatedAt:  m.now().UTC(),
		Notes:      "",
	}

	// Step 2 of §11.3: persist the pending instance before touching the disk.
	if err := m.store.Update(func(cur *store.State) error {
		if len(cur.Instances) >= m.maxInstances {
			return fmt.Errorf("%w: max_instances=%d", ErrLimitReached, m.maxInstances)
		}
		for i := range cur.Instances {
			if cur.Instances[i].ID == inst.ID {
				return fmt.Errorf("%w: id %q", ErrInstanceExists, inst.ID)
			}
			if strings.EqualFold(cur.Instances[i].Subdomain, inst.Subdomain) {
				return fmt.Errorf("%w: %q", ErrSubdomainTaken, inst.Subdomain)
			}
		}
		cur.Instances = append(cur.Instances, inst)
		if index+1 > cur.NextIndex {
			cur.NextIndex = index + 1
		}
		return nil
	}); err != nil {
		return InstanceView{}, err
	}

	if err := m.provision(ctx, &inst); err != nil {
		m.rollback(ctx, &inst)
		if failed, getErr := m.instance(inst.ID); getErr == nil {
			inst = failed
		}
		return m.view(ctx, inst), err
	}

	inst.Status = model.InstanceStatusReady
	if err := m.saveInstance(inst); err != nil {
		return m.view(ctx, inst), err
	}
	m.logger.Info("instance created", "instance_id", inst.ID, "ports", inst.Ports, "container", inst.Container)
	return m.view(ctx, inst), nil
}

// provision performs steps 3-8 of §11.3: directory, ini, container, wait,
// token. DNS is intentionally skipped in M2 (dns_status stays "none").
func (m *Manager) provision(ctx context.Context, inst *model.Instance) error {
	if err := m.ensureFiles(inst); err != nil {
		return err
	}

	info, err := m.inspectManaged(ctx, inst)
	if err != nil {
		return err
	}
	if info == nil {
		imageRef, err := m.ensureImage(ctx)
		if err != nil {
			return err
		}
		spec := m.containerSpec(*inst, imageRef)
		if _, err := m.runtime.Create(ctx, spec); err != nil {
			return fmt.Errorf("%w: create container: %v", ErrDocker, err)
		}
		if err := m.runtime.Start(ctx, inst.Container); err != nil {
			return fmt.Errorf("%w: start container: %v", ErrDocker, err)
		}
	} else if info.State != "running" {
		if err := m.runtime.Start(ctx, inst.Container); err != nil {
			return fmt.Errorf("%w: start existing container: %v", ErrDocker, err)
		}
	}

	if _, err := m.waitRunning(ctx, inst.Container); err != nil {
		return err
	}

	if inst.AdminToken == "" {
		token, err := m.captureToken(ctx, inst)
		if err != nil {
			// §11.3: failing to capture the token must not roll back a running
			// instance. Log only a reason, never the token or log contents.
			m.logger.Warn("admin token not captured", "instance_id", inst.ID, "err", err)
		} else if token != "" {
			inst.AdminToken = token
			m.logger.Info("admin token captured", "instance_id", inst.ID, "token", MaskToken(token))
		}
	}
	return nil
}

func (m *Manager) allocatePorts(st store.State) (model.Ports, int, error) {
	start := st.NextIndex
	if start < 0 {
		start = 0
	}
	for attempt := 0; attempt < maxPortAttempts; attempt++ {
		ports, index, err := Allocate(start, m.probe)
		if err != nil {
			return model.Ports{}, -1, err
		}
		if !portsInState(st, ports) {
			return ports, index, nil
		}
		start = index + 1
	}
	return model.Ports{}, -1, ErrPortExhausted
}

func portsInState(st store.State, ports model.Ports) bool {
	for _, inst := range st.Instances {
		if inst.Ports == ports {
			return true
		}
	}
	return false
}

func (m *Manager) ensureImage(ctx context.Context) (string, error) {
	var lastErr error
	for _, ref := range m.images {
		if err := m.runtime.EnsureImage(ctx, ref); err == nil {
			return ref, nil
		} else {
			lastErr = err
			m.logger.Warn("image unavailable", "image", ref, "err", err)
		}
	}
	return "", fmt.Errorf("%w: no usable TS3 image: %v", ErrDocker, lastErr)
}

// ensureFiles creates the instance directory and writes ts3server.ini. It is
// idempotent: existing data (sqlite, files/) is never overwritten or deleted.
func (m *Manager) ensureFiles(inst *model.Instance) error {
	dir := m.instanceDir(inst.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("%w: create instance directory: %v", ErrDocker, err)
	}
	ini, err := RenderINI(inst.Ports)
	if err != nil {
		return err
	}
	iniPath := filepath.Join(dir, "ts3server.ini")
	tmp := iniPath + ".tmp"
	if err := os.WriteFile(tmp, ini, 0o644); err != nil {
		return fmt.Errorf("%w: write ini: %v", ErrDocker, err)
	}
	if err := os.Rename(tmp, iniPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: commit ini: %v", ErrDocker, err)
	}
	// Errata E3: the image starts as root and its entrypoint runs
	// `chown -R ts3server /var/ts3server` itself, so the panel must not chown.
	return nil
}

func (m *Manager) containerSpec(inst model.Instance, imageRef string) ContainerSpec {
	return ContainerSpec{
		Name:  inst.Container,
		Image: imageRef,
		Env: []string{
			"TS3SERVER_LICENSE=accept",
			// The official image generates its own ini under /var/run from
			// these variables and appends its own inifile argument; env vars
			// are therefore the only reliable way to pin ports on this image.
			// Measured on teamspeak:latest (3.13.8), 2026-10-21.
			fmt.Sprintf("TS3SERVER_DEFAULT_VOICE_PORT=%d", inst.Ports.Voice),
			fmt.Sprintf("TS3SERVER_FILETRANSFER_PORT=%d", inst.Ports.FileTransfer),
			fmt.Sprintf("TS3SERVER_QUERY_PORT=%d", inst.Ports.Query),
			fmt.Sprintf("TS3SERVER_QUERY_SSH_PORT=%d", inst.Ports.QuerySSH),
			"TS3SERVER_VOICE_IP=0.0.0.0",
			"TS3SERVER_FILETRANSFER_IP=0.0.0.0",
			"TS3SERVER_QUERY_IP=127.0.0.1",
			"TS3SERVER_QUERY_SSH_IP=127.0.0.1",
			"TS3SERVER_QUERY_PROTOCOLS=raw,ssh",
			"TS3SERVER_LOG_PATH=/var/ts3server/logs",
		},
		Cmd:           []string{"ts3server", "inifile=/var/ts3server/ts3server.ini", "license_accepted=1"},
		Labels:        map[string]string{managedLabel: "true", instanceLabel: inst.ID},
		Binds:         []string{m.instanceDir(inst.ID) + ":/var/ts3server"},
		NetworkMode:   "host",
		RestartPolicy: "unless-stopped",
		Memory:        m.memory,
		NanoCPUs:      m.nanoCPUs,
		LogMaxSize:    "10m",
		LogMaxFiles:   "3",
	}
}

func (m *Manager) waitRunning(ctx context.Context, name string) (*ContainerInfo, error) {
	deadline := time.Now().Add(m.waitTimeout)
	var runningSince time.Time
	for {
		info, err := m.runtime.Inspect(ctx, name)
		if err == nil && info != nil {
			switch info.State {
			case "running":
				// A crash-looping TS3 container is "running" for a moment.
				// Require two consecutive observations two seconds apart.
				if runningSince.IsZero() {
					runningSince = time.Now()
				} else if time.Since(runningSince) >= m.runningGrace {
					return info, nil
				}
			case "exited", "dead":
				return info, fmt.Errorf("%w: container exited with code %d", ErrContainerTimeout, info.ExitCode)
			default:
				runningSince = time.Time{}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w within %s", ErrContainerTimeout, m.waitTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(m.pollInterval):
		}
	}
}

func (m *Manager) captureToken(ctx context.Context, inst *model.Instance) (string, error) {
	logs := func(ctx context.Context) (string, error) {
		return m.runtime.Logs(ctx, inst.Container, 300)
	}
	return PollToken(ctx, logs, m.tokenAttempts, m.tokenInterval)
}

// rollback returns the instance to a known failed state: container removed,
// directory removed, status failed. It must never delete anything it does not
// own, so the label check runs before removal.
func (m *Manager) rollback(ctx context.Context, inst *model.Instance) {
	if info, err := m.inspectManaged(ctx, inst); err == nil && info != nil {
		if err := m.runtime.Stop(ctx, inst.Container, 10*time.Second); err != nil && !errors.Is(err, ErrContainerMissing) {
			// Best effort: Remove below still gets a chance.
			m.logger.Warn("rollback stop failed", "instance_id", inst.ID, "err", err)
		}
		if err := m.removeContainer(ctx, inst.Container); err != nil {
			m.logger.Warn("rollback remove failed", "instance_id", inst.ID, "err", err)
		}
	}
	if err := os.RemoveAll(m.instanceDir(inst.ID)); err != nil {
		m.logger.Warn("rollback directory removal failed", "instance_id", inst.ID, "err", err)
	}
	if err := m.store.Update(func(cur *store.State) error {
		for i := range cur.Instances {
			if cur.Instances[i].ID == inst.ID {
				cur.Instances[i].Status = model.InstanceStatusFailed
				cur.Instances[i].AdminToken = ""
			}
		}
		return nil
	}); err != nil {
		m.logger.Error("rollback state update failed", "instance_id", inst.ID, "err", err)
	}
}

// Start starts (or recreates) the instance container.
func (m *Manager) Start(ctx context.Context, id string) (InstanceView, error) {
	inst, err := m.instance(id)
	if err != nil {
		return InstanceView{}, err
	}
	info, err := m.inspectManaged(ctx, &inst)
	if err != nil {
		return m.view(ctx, inst), err
	}
	if info == nil {
		if err := m.ensureFiles(&inst); err != nil {
			return m.view(ctx, inst), err
		}
		imageRef, err := m.ensureImage(ctx)
		if err != nil {
			return m.view(ctx, inst), err
		}
		if _, err := m.runtime.Create(ctx, m.containerSpec(inst, imageRef)); err != nil {
			return m.view(ctx, inst), fmt.Errorf("%w: create container: %v", ErrDocker, err)
		}
		if err := m.runtime.Start(ctx, inst.Container); err != nil {
			return m.view(ctx, inst), fmt.Errorf("%w: start container: %v", ErrDocker, err)
		}
	} else if info.State != "running" {
		if err := m.runtime.Start(ctx, inst.Container); err != nil {
			return m.view(ctx, inst), fmt.Errorf("%w: start container: %v", ErrDocker, err)
		}
	}

	if _, err := m.waitRunning(ctx, inst.Container); err != nil {
		return m.view(ctx, inst), err
	}
	if inst.AdminToken == "" {
		if token, err := m.captureToken(ctx, &inst); err == nil && token != "" {
			inst.AdminToken = token
		}
	}
	inst.Status = model.InstanceStatusReady
	if err := m.saveInstance(inst); err != nil {
		return m.view(ctx, inst), err
	}
	m.forgetStats(inst.Container)
	m.logger.Info("instance started", "instance_id", inst.ID)
	return m.view(ctx, inst), nil
}

// Stop stops the container. A missing container is treated as success so
// repeated stop requests are idempotent.
func (m *Manager) Stop(ctx context.Context, id string) (InstanceView, error) {
	inst, err := m.instance(id)
	if err != nil {
		return InstanceView{}, err
	}
	info, err := m.inspectManaged(ctx, &inst)
	if err != nil {
		return m.view(ctx, inst), err
	}
	if info != nil && (info.State == "running" || info.State == "restarting") {
		if err := m.runtime.Stop(ctx, inst.Container, 10*time.Second); err != nil && !errors.Is(err, ErrContainerMissing) {
			return m.view(ctx, inst), fmt.Errorf("%w: stop container: %v", ErrDocker, err)
		}
	}
	inst.Status = model.InstanceStatusStopped
	if err := m.saveInstance(inst); err != nil {
		return m.view(ctx, inst), err
	}
	m.forgetStats(inst.Container)
	m.logger.Info("instance stopped", "instance_id", inst.ID)
	return m.view(ctx, inst), nil
}

// Restart restarts (or recreates) the instance container.
func (m *Manager) Restart(ctx context.Context, id string) (InstanceView, error) {
	inst, err := m.instance(id)
	if err != nil {
		return InstanceView{}, err
	}
	info, err := m.inspectManaged(ctx, &inst)
	if err != nil {
		return m.view(ctx, inst), err
	}
	if info == nil {
		return m.Start(ctx, id)
	}
	if err := m.runtime.Restart(ctx, inst.Container, 10*time.Second); err != nil {
		return m.view(ctx, inst), fmt.Errorf("%w: restart container: %v", ErrDocker, err)
	}
	if _, err := m.waitRunning(ctx, inst.Container); err != nil {
		return m.view(ctx, inst), err
	}
	inst.Status = model.InstanceStatusReady
	if err := m.saveInstance(inst); err != nil {
		return m.view(ctx, inst), err
	}
	m.forgetStats(inst.Container)
	m.logger.Info("instance restarted", "instance_id", inst.ID)
	return m.view(ctx, inst), nil
}

// Delete removes the container, optionally the data directory, then the state
// entry. The port group is never reused because NextIndex is left untouched.
func (m *Manager) Delete(ctx context.Context, id string, purge bool) error {
	inst, err := m.instance(id)
	if err != nil {
		return err
	}
	info, err := m.inspectManaged(ctx, &inst)
	if err != nil {
		return err
	}
	if info != nil {
		if info.State == "running" || info.State == "restarting" {
			if err := m.runtime.Stop(ctx, inst.Container, 10*time.Second); err != nil && !errors.Is(err, ErrContainerMissing) {
				// A crash-looping container cannot always be stopped cleanly;
				// removal below escalates to force.
				m.logger.Warn("delete stop failed", "instance_id", inst.ID, "err", err)
			}
		}
		if err := m.removeContainer(ctx, inst.Container); err != nil {
			return fmt.Errorf("%w: remove container: %v", ErrDocker, err)
		}
	}

	if purge {
		dir := m.instanceDir(inst.ID)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("%w: remove instance directory: %v", ErrDocker, err)
		}
	}

	if err := m.store.Update(func(cur *store.State) error {
		out := cur.Instances[:0]
		found := false
		for _, item := range cur.Instances {
			if item.ID == id {
				found = true
				continue
			}
			out = append(out, item)
		}
		if !found {
			return ErrInstanceNotFound
		}
		cur.Instances = out
		return nil
	}); err != nil {
		return err
	}
	m.logger.Info("instance deleted", "instance_id", id, "purge", purge)
	m.forgetStats(inst.Container)
	return nil
}

// Get returns one instance with its live container state.
func (m *Manager) Get(ctx context.Context, id string) (InstanceView, error) {
	inst, err := m.instance(id)
	if err != nil {
		return InstanceView{}, err
	}
	view := m.view(ctx, inst)
	return view, nil
}

// List returns all instances with live container states. AdminToken is blanked
// here; only Get/Create responses carry it.
func (m *Manager) List(ctx context.Context) ([]InstanceView, error) {
	st, err := m.store.Snapshot()
	if err != nil {
		return nil, err
	}
	views := make([]InstanceView, 0, len(st.Instances))
	for i := range st.Instances {
		inst := st.Instances[i]
		inst.AdminToken = ""
		views = append(views, m.view(ctx, inst))
	}
	return views, nil
}

// Reconcile inspects instances left in pending/failed by an interrupted create.
// It only annotates state; the recovery actions are the existing Start
// (continue) and Delete (clean up) endpoints.
func (m *Manager) Reconcile(ctx context.Context) error {
	st, err := m.store.Snapshot()
	if err != nil {
		return err
	}
	type change struct {
		id     string
		status string
	}
	var changes []change
	for i := range st.Instances {
		inst := st.Instances[i]
		if inst.Status != model.InstanceStatusPending && inst.Status != model.InstanceStatusFailed {
			continue
		}
		info, err := m.inspectManaged(ctx, &inst)
		if err != nil {
			m.logger.Warn("reconcile inspect failed", "instance_id", inst.ID, "err", err)
			continue
		}
		if info != nil && info.State == "running" {
			status := model.InstanceStatusRunning
			if inst.AdminToken != "" {
				status = model.InstanceStatusReady
			}
			changes = append(changes, change{id: inst.ID, status: status})
			continue
		}
		m.logger.Warn("instance needs recovery", "instance_id", inst.ID, "container_state", stateOf(info))
	}
	if len(changes) == 0 {
		return nil
	}
	return m.store.Update(func(cur *store.State) error {
		for _, c := range changes {
			for i := range cur.Instances {
				if cur.Instances[i].ID == c.id {
					cur.Instances[i].Status = c.status
				}
			}
		}
		return nil
	})
}

func (m *Manager) instance(id string) (model.Instance, error) {
	st, err := m.store.Snapshot()
	if err != nil {
		return model.Instance{}, err
	}
	for i := range st.Instances {
		if st.Instances[i].ID == id {
			return st.Instances[i], nil
		}
	}
	return model.Instance{}, fmt.Errorf("%w: %q", ErrInstanceNotFound, id)
}

func (m *Manager) saveInstance(inst model.Instance) error {
	return m.store.Update(func(cur *store.State) error {
		for i := range cur.Instances {
			if cur.Instances[i].ID == inst.ID {
				cur.Instances[i] = inst
				return nil
			}
		}
		return fmt.Errorf("%w: %q", ErrInstanceNotFound, inst.ID)
	})
}

// removeContainer removes a verified panel container. If a crash-looping
// container refuses a graceful remove, it escalates to force once. A missing
// container is treated as already removed (idempotent delete).
func (m *Manager) removeContainer(ctx context.Context, name string) error {
	err := m.runtime.Remove(ctx, name, false)
	if err == nil || errors.Is(err, ErrContainerMissing) {
		return nil
	}
	if forceErr := m.runtime.Remove(ctx, name, true); forceErr != nil {
		if errors.Is(forceErr, ErrContainerMissing) {
			return nil
		}
		return forceErr
	}
	return nil
}

// inspectManaged returns nil (no error) when the container does not exist. Any
// existing container must carry both labels, otherwise the panel refuses to
// operate on it (iron rule 5).
func (m *Manager) inspectManaged(ctx context.Context, inst *model.Instance) (*ContainerInfo, error) {
	info, err := m.runtime.Inspect(ctx, inst.Container)
	if errors.Is(err, ErrContainerMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: inspect container: %v", ErrDocker, err)
	}
	if info == nil {
		return nil, nil
	}
	if info.Labels[managedLabel] != "true" || info.Labels[instanceLabel] != inst.ID {
		return nil, fmt.Errorf("%w: container %q is missing %s/instance labels", ErrNotManaged, inst.Container, managedLabel)
	}
	return info, nil
}

func (m *Manager) view(ctx context.Context, inst model.Instance) InstanceView {
	info, err := m.runtime.Inspect(ctx, inst.Container)
	state := "unknown"
	if err != nil {
		if errors.Is(err, ErrContainerMissing) {
			state = "missing"
		}
	} else if info != nil {
		state = info.State
	}
	view := InstanceView{Instance: inst, ContainerState: state}
	view.Status = deriveStatus(inst.Status, state)
	view.Recoverable = view.Status == model.InstanceStatusPending || view.Status == model.InstanceStatusFailed || view.Status == model.InstanceStatusMissing
	return view
}

func deriveStatus(stored, containerState string) string {
	switch containerState {
	case "running":
		return model.InstanceStatusReady
	case "exited", "dead":
		// A panel-initiated Stop records "stopped"; a container stopped outside
		// the panel keeps the Docker wording (errata E5).
		if stored == model.InstanceStatusStopped {
			return model.InstanceStatusStopped
		}
		return model.InstanceStatusExited
	case "paused":
		return model.InstanceStatusStopped
	case "created", "restarting":
		if stored == model.InstanceStatusPending {
			return model.InstanceStatusPending
		}
		return model.InstanceStatusRunning
	case "missing":
		if stored == model.InstanceStatusPending || stored == model.InstanceStatusFailed {
			return stored
		}
		return model.InstanceStatusMissing
	default:
		return stored
	}
}

func stateOf(info *ContainerInfo) string {
	if info == nil {
		return "missing"
	}
	return info.State
}

func (m *Manager) instanceDir(id string) string {
	return filepath.Join(m.root, id)
}

func validateDisplayName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name is empty", ErrNameInvalid)
	}
	if len([]rune(name)) > 40 {
		return fmt.Errorf("%w: name is longer than 40 characters", ErrNameInvalid)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: name contains control characters", ErrNameInvalid)
		}
	}
	return nil
}

// unavailableRuntime stands in for Docker when the client cannot even be
// constructed (for example an unsupported development host). Every operation
// fails with E_DOCKER instead of crashing the panel.
type unavailableRuntime struct{ err error }

func (u unavailableRuntime) Ping(context.Context) error { return u.err }
func (u unavailableRuntime) EnsureImage(context.Context, string) error {
	return u.err
}
func (u unavailableRuntime) Create(context.Context, ContainerSpec) (string, error) {
	return "", u.err
}
func (u unavailableRuntime) Start(context.Context, string) error { return u.err }
func (u unavailableRuntime) Stop(context.Context, string, time.Duration) error {
	return u.err
}
func (u unavailableRuntime) Restart(context.Context, string, time.Duration) error {
	return u.err
}
func (u unavailableRuntime) Remove(context.Context, string, bool) error { return u.err }
func (u unavailableRuntime) Inspect(context.Context, string) (*ContainerInfo, error) {
	return nil, u.err
}
func (u unavailableRuntime) Logs(context.Context, string, int) (string, error) { return "", u.err }
func (u unavailableRuntime) Stats(context.Context, string) (StatsSample, error) {
	return StatsSample{}, u.err
}
func (u unavailableRuntime) Close() error { return nil }

// dockerRuntime is the Docker Engine implementation of Runtime.
type dockerRuntime struct {
	cli *client.Client
}

func newDockerRuntime(socket string) (*dockerRuntime, error) {
	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if strings.TrimSpace(socket) != "" {
		opts = append(opts, client.WithHost(socket))
	} else {
		opts = append(opts, client.FromEnv)
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &dockerRuntime{cli: cli}, nil
}

func (d *dockerRuntime) Close() error { return d.cli.Close() }

func (d *dockerRuntime) Ping(ctx context.Context) error {
	_, err := d.cli.Ping(ctx)
	return err
}

func (d *dockerRuntime) EnsureImage(ctx context.Context, ref string) error {
	if _, _, err := d.cli.ImageInspectWithRaw(ctx, ref); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	reader, err := d.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer reader.Close()
	// Docker returns HTTP 200 for a failed pull and reports the error inside the
	// progress stream, so the stream must be drained and inspected.
	dec := json.NewDecoder(reader)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail *struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
		if msg.ErrorDetail != nil && msg.ErrorDetail.Message != "" {
			return errors.New(msg.ErrorDetail.Message)
		}
	}
}

func (d *dockerRuntime) Create(ctx context.Context, spec ContainerSpec) (string, error) {
	restart := container.RestartPolicy{Name: container.RestartPolicyDisabled}
	if spec.RestartPolicy != "" {
		restart.Name = container.RestartPolicyMode(spec.RestartPolicy)
	}
	mounts := make([]mount.Mount, 0, len(spec.Binds))
	for _, bind := range spec.Binds {
		parts := strings.SplitN(bind, ":", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("invalid bind mount %q", bind)
		}
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: parts[0], Target: parts[1]})
	}
	cfg := &container.Config{
		Image:  spec.Image,
		Env:    spec.Env,
		Cmd:    spec.Cmd,
		Labels: spec.Labels,
	}
	hostCfg := &container.HostConfig{
		NetworkMode:   container.NetworkMode(spec.NetworkMode),
		RestartPolicy: restart,
		Resources: container.Resources{
			Memory:   spec.Memory,
			NanoCPUs: spec.NanoCPUs,
		},
		LogConfig: container.LogConfig{
			Type: "json-file",
			Config: map[string]string{
				"max-size": spec.LogMaxSize,
				"max-file": spec.LogMaxFiles,
			},
		},
		Mounts: mounts,
	}
	resp, err := d.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.Name)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (d *dockerRuntime) Start(ctx context.Context, name string) error {
	return d.cli.ContainerStart(ctx, name, container.StartOptions{})
}

func (d *dockerRuntime) Stop(ctx context.Context, name string, timeout time.Duration) error {
	seconds := int(timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	err := d.cli.ContainerStop(ctx, name, container.StopOptions{Timeout: &seconds})
	if errdefs.IsNotFound(err) {
		return ErrContainerMissing
	}
	return err
}

func (d *dockerRuntime) Restart(ctx context.Context, name string, timeout time.Duration) error {
	seconds := int(timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	err := d.cli.ContainerRestart(ctx, name, container.StopOptions{Timeout: &seconds})
	if errdefs.IsNotFound(err) {
		return ErrContainerMissing
	}
	return err
}

func (d *dockerRuntime) Remove(ctx context.Context, name string, force bool) error {
	err := d.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: force})
	if errdefs.IsNotFound(err) {
		return ErrContainerMissing
	}
	return err
}

func (d *dockerRuntime) Inspect(ctx context.Context, name string) (*ContainerInfo, error) {
	resp, err := d.cli.ContainerInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, ErrContainerMissing
		}
		return nil, err
	}
	info := &ContainerInfo{
		ID:    resp.ID,
		Name:  strings.TrimPrefix(resp.Name, "/"),
		State: "unknown",
	}
	if resp.State != nil {
		info.State = resp.State.Status
		info.ExitCode = resp.State.ExitCode
	}
	if resp.Config != nil {
		info.Image = resp.Config.Image
		info.Labels = resp.Config.Labels
	}
	return info, nil
}

func (d *dockerRuntime) Logs(ctx context.Context, name string, tail int) (string, error) {
	rc, err := d.cli.ContainerLogs(ctx, name, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(tail),
	})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return "", ErrContainerMissing
		}
		return "", err
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, bytes.NewReader(raw)); err != nil {
		// TTY containers stream plain text instead of the multiplexed format.
		return string(raw), nil
	}
	return stdout.String() + stderr.String(), nil
}
