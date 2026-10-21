package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ts3panel/internal/model"
	"ts3panel/internal/store"
)

type fakeContainer struct {
	info ContainerInfo
	spec ContainerSpec
}

type fakeRuntime struct {
	mu               sync.Mutex
	images           map[string]bool
	imageErr         map[string]error
	containers       map[string]*fakeContainer
	logs             map[string]string
	startErr         error
	pingErr          error
	removeNeedsForce bool
	created          []ContainerSpec
	stopped          []string
	removed          []string
	nextID           int
	stats            []StatsSample
	statsErr         error
	statsCalls       int
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		images:     map[string]bool{},
		imageErr:   map[string]error{},
		containers: map[string]*fakeContainer{},
		logs:       map[string]string{},
	}
}

func (f *fakeRuntime) Ping(ctx context.Context) error {
	return f.pingErr
}

func (f *fakeRuntime) EnsureImage(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.imageErr[ref]; ok {
		return err
	}
	f.images[ref] = true
	return nil
}

func (f *fakeRuntime) Create(ctx context.Context, spec ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.containers[spec.Name]; exists {
		return "", fmt.Errorf("container %s already exists", spec.Name)
	}
	f.nextID++
	id := fmt.Sprintf("cid-%d", f.nextID)
	f.containers[spec.Name] = &fakeContainer{
		info: ContainerInfo{
			ID:     id,
			Name:   spec.Name,
			Image:  spec.Image,
			State:  "created",
			Labels: spec.Labels,
		},
		spec: spec,
	}
	f.created = append(f.created, spec)
	return id, nil
}

func (f *fakeRuntime) Start(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	if !ok {
		return ErrContainerMissing
	}
	if f.startErr != nil {
		c.info.State = "exited"
		c.info.ExitCode = 1
		return f.startErr
	}
	c.info.State = "running"
	return nil
}

func (f *fakeRuntime) Stop(ctx context.Context, name string, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	if !ok {
		return ErrContainerMissing
	}
	c.info.State = "exited"
	f.stopped = append(f.stopped, name)
	return nil
}

func (f *fakeRuntime) Restart(ctx context.Context, name string, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	if !ok {
		return ErrContainerMissing
	}
	c.info.State = "running"
	return nil
}

func (f *fakeRuntime) Remove(ctx context.Context, name string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[name]; !ok {
		return ErrContainerMissing
	}
	if f.removeNeedsForce && !force {
		return errors.New("container is restarting: stop the container before removing")
	}
	delete(f.containers, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeRuntime) Inspect(ctx context.Context, name string) (*ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	if !ok {
		return nil, ErrContainerMissing
	}
	info := c.info
	info.Labels = map[string]string{}
	for k, v := range c.info.Labels {
		info.Labels[k] = v
	}
	return &info, nil
}

func (f *fakeRuntime) Logs(ctx context.Context, name string, tail int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[name]; !ok {
		return "", ErrContainerMissing
	}
	return f.logs[name], nil
}

// Stats returns the configured samples in order, repeating the last one. The
// second sample is what makes the CPU percentage meaningful.
func (f *fakeRuntime) Stats(ctx context.Context, name string) (StatsSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[name]; !ok {
		return StatsSample{}, ErrContainerMissing
	}
	f.statsCalls++
	if f.statsErr != nil {
		return StatsSample{}, f.statsErr
	}
	if len(f.stats) == 0 {
		return StatsSample{}, errors.New("fake runtime has no stats samples")
	}
	index := f.statsCalls - 1
	if index >= len(f.stats) {
		index = len(f.stats) - 1
	}
	return f.stats[index], nil
}

func (f *fakeRuntime) Close() error { return nil }

func (f *fakeRuntime) container(name string) (*fakeContainer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	return c, ok
}

func newTestManager(t *testing.T, rt *fakeRuntime) (*Manager, *store.StateStore, string) {
	t.Helper()
	base := t.TempDir()
	st, err := store.OpenState(filepath.Join(base, "state.json"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	root := filepath.Join(base, "instances")
	m, err := New(Options{
		Store:         st,
		Root:          root,
		Runtime:       rt,
		Probe:         func(network, address string) (io.Closer, error) { return nopCloser{}, nil },
		WaitTimeout:   2 * time.Second,
		TokenAttempts: 1,
		TokenInterval: time.Millisecond,
		RunningGrace:  5 * time.Millisecond,
		PollInterval:  time.Millisecond,
		MaxInstances:  20,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return m, st, root
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func seedInstance(t *testing.T, st *store.StateStore, inst model.Instance) {
	t.Helper()
	if err := st.Update(func(cur *store.State) error {
		cur.Instances = append(cur.Instances, inst)
		if inst.Ports.Voice-VoicePortBase+1 > cur.NextIndex {
			cur.NextIndex = inst.Ports.Voice - VoicePortBase + 1
		}
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

func TestCreateProvisionsContainerAndToken(t *testing.T) {
	rt := newFakeRuntime()
	rt.logs["ts3-ts1"] = "starting\ntoken=AbCdEfGhIjKlMnOpQrStUv\n"
	m, st, root := newTestManager(t, rt)
	m.tokenAttempts = 1

	view, err := m.Create(context.Background(), CreateParams{
		ID: "ts1", Name: "一队", Subdomain: "ts1.example.com", ProviderID: "manual-main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if view.Status != model.InstanceStatusReady || view.ContainerState != "running" {
		t.Fatalf("view status=%q container=%q", view.Status, view.ContainerState)
	}
	if view.Ports.Voice != 9987 || view.Ports.FileTransfer != 30033 || view.Ports.Query != 10011 || view.Ports.QuerySSH != 10022 {
		t.Fatalf("ports = %+v", view.Ports)
	}
	if view.AdminToken != "AbCdEfGhIjKlMnOpQrStUv" {
		t.Fatalf("admin token = %q", view.AdminToken)
	}

	c, ok := rt.container("ts3-ts1")
	if !ok {
		t.Fatalf("container was not created")
	}
	if c.spec.NetworkMode != "host" || c.spec.RestartPolicy != "unless-stopped" {
		t.Fatalf("spec = %+v", c.spec)
	}
	if c.spec.Labels[managedLabel] != "true" || c.spec.Labels[instanceLabel] != "ts1" {
		t.Fatalf("labels = %+v", c.spec.Labels)
	}
	if c.spec.Image != defaultTS3Image {
		t.Fatalf("image = %q", c.spec.Image)
	}
	if c.spec.Memory != defaultMemoryBytes || c.spec.NanoCPUs != defaultNanoCPUs {
		t.Fatalf("resources = %d/%d", c.spec.Memory, c.spec.NanoCPUs)
	}
	if len(c.spec.Cmd) != 3 || c.spec.Cmd[0] != "ts3server" || c.spec.Cmd[1] != "inifile=/var/ts3server/ts3server.ini" {
		t.Fatalf("cmd = %v", c.spec.Cmd)
	}
	if len(c.spec.Binds) != 1 || c.spec.Binds[0] != filepath.Join(root, "ts1")+":/var/ts3server" {
		t.Fatalf("binds = %v", c.spec.Binds)
	}
	env := strings.Join(c.spec.Env, "\n")
	for _, want := range []string{
		"TS3SERVER_DEFAULT_VOICE_PORT=9987",
		"TS3SERVER_FILETRANSFER_PORT=30033",
		"TS3SERVER_QUERY_PORT=10011",
		"TS3SERVER_QUERY_SSH_PORT=10022",
		"TS3SERVER_QUERY_IP=127.0.0.1",
		"TS3SERVER_QUERY_SSH_IP=127.0.0.1",
		"TS3SERVER_QUERY_PROTOCOLS=raw,ssh",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q: %v", want, c.spec.Env)
		}
	}

	ini, err := os.ReadFile(filepath.Join(root, "ts1", "ts3server.ini"))
	if err != nil {
		t.Fatalf("read ini: %v", err)
	}
	if !strings.Contains(string(ini), "query_ip=127.0.0.1") {
		t.Fatalf("ini missing loopback query:\n%s", ini)
	}

	snap, err := st.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.NextIndex != 1 || len(snap.Instances) != 1 || snap.Instances[0].Status != model.InstanceStatusReady {
		t.Fatalf("state = %+v", snap)
	}
}

func TestCreateRollsBackWhenStartFails(t *testing.T) {
	rt := newFakeRuntime()
	rt.startErr = errors.New("boom")
	m, st, root := newTestManager(t, rt)

	_, err := m.Create(context.Background(), CreateParams{ID: "ts1", Name: "一队", Subdomain: "ts1"})
	if err == nil {
		t.Fatal("Create succeeded, want error")
	}
	if !errors.Is(err, ErrDocker) {
		t.Fatalf("err = %v, want ErrDocker", err)
	}
	if _, ok := rt.container("ts3-ts1"); ok {
		t.Fatal("container was not removed by rollback")
	}
	if _, err := os.Stat(filepath.Join(root, "ts1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory still exists: %v", err)
	}
	snap, _ := st.Snapshot()
	if len(snap.Instances) != 1 || snap.Instances[0].Status != model.InstanceStatusFailed {
		t.Fatalf("state after rollback = %+v", snap.Instances)
	}
	if snap.NextIndex != 1 {
		t.Fatalf("next_index = %d, want 1 (ports are never reused)", snap.NextIndex)
	}
}

func TestCreateFallsBackToSecondImage(t *testing.T) {
	rt := newFakeRuntime()
	rt.imageErr[defaultTS3Image] = errors.New("no such image")
	m, _, _ := newTestManager(t, rt)
	if _, err := m.Create(context.Background(), CreateParams{ID: "ts1", Name: "ts1", Subdomain: "ts1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	c, _ := rt.container("ts3-ts1")
	if c.spec.Image != fallbackTS3Image {
		t.Fatalf("image = %q", c.spec.Image)
	}
}

func TestRefusesUnmanagedContainer(t *testing.T) {
	rt := newFakeRuntime()
	m, st, _ := newTestManager(t, rt)
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone,
	})
	// A container with the right name but no ts3panel labels.
	rt.containers["ts3-ts1"] = &fakeContainer{info: ContainerInfo{Name: "ts3-ts1", State: "running", Labels: map[string]string{"someone": "else"}}}

	if _, err := m.Stop(context.Background(), "ts1"); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("Stop err = %v, want ErrNotManaged", err)
	}
	if len(rt.stopped) != 0 {
		t.Fatalf("unmanaged container was stopped: %v", rt.stopped)
	}
	if err := m.Delete(context.Background(), "ts1", true); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("Delete err = %v, want ErrNotManaged", err)
	}
}

func TestStartRecreatesMissingContainer(t *testing.T) {
	rt := newFakeRuntime()
	m, st, root := newTestManager(t, rt)
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusStopped, DNSStatus: model.DNSStatusNone,
	})

	view, err := m.Start(context.Background(), "ts1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if view.Status != model.InstanceStatusReady || view.ContainerState != "running" {
		t.Fatalf("view = %+v", view)
	}
	if _, err := os.Stat(filepath.Join(root, "ts1", "ts3server.ini")); err != nil {
		t.Fatalf("ini was not recreated: %v", err)
	}
	if _, ok := rt.container("ts3-ts1"); !ok {
		t.Fatal("container was not recreated")
	}
}

func TestDeletePurgesContainerAndData(t *testing.T) {
	rt := newFakeRuntime()
	m, st, root := newTestManager(t, rt)
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone,
	})
	if err := os.MkdirAll(filepath.Join(root, "ts1"), 0o750); err != nil {
		t.Fatal(err)
	}
	rt.containers["ts3-ts1"] = &fakeContainer{info: ContainerInfo{
		Name: "ts3-ts1", State: "running",
		Labels: map[string]string{managedLabel: "true", instanceLabel: "ts1"},
	}}

	if err := m.Delete(context.Background(), "ts1", true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := rt.container("ts3-ts1"); ok {
		t.Fatal("container still exists")
	}
	if _, err := os.Stat(filepath.Join(root, "ts1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data directory still exists: %v", err)
	}
	snap, _ := st.Snapshot()
	if len(snap.Instances) != 0 {
		t.Fatalf("instance still in state: %+v", snap.Instances)
	}
	if snap.NextIndex != 1 {
		t.Fatalf("next_index = %d, want 1", snap.NextIndex)
	}
}

func TestDeleteWithoutPurgeKeepsData(t *testing.T) {
	rt := newFakeRuntime()
	m, st, root := newTestManager(t, rt)
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusStopped, DNSStatus: model.DNSStatusNone,
	})
	dir := filepath.Join(root, "ts1")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), "ts1", false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("data directory was removed: %v", err)
	}
}

func TestReconcileMarksPendingRunningAsReady(t *testing.T) {
	rt := newFakeRuntime()
	m, st, _ := newTestManager(t, rt)
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusPending, DNSStatus: model.DNSStatusNone,
	})
	rt.containers["ts3-ts1"] = &fakeContainer{info: ContainerInfo{
		Name: "ts3-ts1", State: "running",
		Labels: map[string]string{managedLabel: "true", instanceLabel: "ts1"},
	}}

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	snap, _ := st.Snapshot()
	if snap.Instances[0].Status != model.InstanceStatusRunning {
		t.Fatalf("status = %q, want running", snap.Instances[0].Status)
	}
}

func TestCreateHonorsNextIndex(t *testing.T) {
	rt := newFakeRuntime()
	m, st, _ := newTestManager(t, rt)
	seedInstance(t, st, model.Instance{
		ID: "old", Name: "old", Subdomain: "old", Container: "ts3-old",
		Ports: PortsFor(0), Status: model.InstanceStatusStopped, DNSStatus: model.DNSStatusNone,
	})
	if _, err := m.Create(context.Background(), CreateParams{ID: "ts2", Name: "ts2", Subdomain: "ts2"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	snap, _ := st.Snapshot()
	for _, inst := range snap.Instances {
		if inst.ID == "ts2" && inst.Ports.Voice != VoicePortBase+1 {
			t.Fatalf("ts2 ports = %+v", inst.Ports)
		}
	}
}

func TestCreateRejectsDuplicateSubdomain(t *testing.T) {
	rt := newFakeRuntime()
	m, st, _ := newTestManager(t, rt)
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1.example.com", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusStopped, DNSStatus: model.DNSStatusNone,
	})
	_, err := m.Create(context.Background(), CreateParams{ID: "ts9", Name: "ts9", Subdomain: "ts1.example.com"})
	if !errors.Is(err, ErrSubdomainTaken) {
		t.Fatalf("err = %v, want ErrSubdomainTaken", err)
	}
	if len(rt.created) != 0 {
		t.Fatal("container was created for a rejected request")
	}
}

func TestCreateRespectsMaxInstances(t *testing.T) {
	rt := newFakeRuntime()
	m, st, _ := newTestManager(t, rt)
	m.maxInstances = 1
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusStopped, DNSStatus: model.DNSStatusNone,
	})
	if _, err := m.Create(context.Background(), CreateParams{ID: "ts2", Name: "ts2", Subdomain: "ts2"}); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("err = %v, want ErrLimitReached", err)
	}
}

func TestCreateFailsFastWhenDockerUnavailable(t *testing.T) {
	rt := newFakeRuntime()
	rt.pingErr = errors.New("dial unix /var/run/docker.sock: connect: no such file or directory")
	m, st, root := newTestManager(t, rt)

	_, err := m.Create(context.Background(), CreateParams{ID: "ts1", Name: "ts1", Subdomain: "ts1"})
	if !errors.Is(err, ErrDocker) {
		t.Fatalf("err = %v, want ErrDocker", err)
	}
	snap, _ := st.Snapshot()
	if len(snap.Instances) != 0 {
		t.Fatalf("state was modified: %+v", snap.Instances)
	}
	if _, statErr := os.Stat(filepath.Join(root, "ts1")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("directory was created despite docker being unavailable: %v", statErr)
	}
}

func TestDeleteEscalatesToForceForRestartingContainer(t *testing.T) {
	rt := newFakeRuntime()
	rt.removeNeedsForce = true
	m, st, _ := newTestManager(t, rt)
	seedInstance(t, st, model.Instance{
		ID: "ts1", Name: "ts1", Subdomain: "ts1", Container: "ts3-ts1",
		Ports: PortsFor(0), Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone,
	})
	rt.containers["ts3-ts1"] = &fakeContainer{info: ContainerInfo{
		Name: "ts3-ts1", State: "restarting",
		Labels: map[string]string{managedLabel: "true", instanceLabel: "ts1"},
	}}
	if err := m.Delete(context.Background(), "ts1", true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := rt.container("ts3-ts1"); ok {
		t.Fatal("container still exists")
	}
	snap, _ := st.Snapshot()
	if len(snap.Instances) != 0 {
		t.Fatalf("instance still in state")
	}
}

func TestListReportsLiveContainerStates(t *testing.T) {
	rt := newFakeRuntime()
	m, st, _ := newTestManager(t, rt)

	seedInstance(t, st, model.Instance{ID: "a", Name: "a", Subdomain: "a", Container: "ts3-a", Ports: PortsFor(0), Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone})
	seedInstance(t, st, model.Instance{ID: "b", Name: "b", Subdomain: "b", Container: "ts3-b", Ports: PortsFor(1), Status: model.InstanceStatusStopped, DNSStatus: model.DNSStatusNone})
	seedInstance(t, st, model.Instance{ID: "c", Name: "c", Subdomain: "c", Container: "ts3-c", Ports: PortsFor(2), Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone})
	seedInstance(t, st, model.Instance{ID: "d", Name: "d", Subdomain: "d", Container: "ts3-d", Ports: PortsFor(3), Status: model.InstanceStatusReady, DNSStatus: model.DNSStatusNone})

	rt.containers["ts3-a"] = &fakeContainer{info: ContainerInfo{Name: "ts3-a", State: "running"}}
	rt.containers["ts3-b"] = &fakeContainer{info: ContainerInfo{Name: "ts3-b", State: "exited"}}
	rt.containers["ts3-d"] = &fakeContainer{info: ContainerInfo{Name: "ts3-d", State: "exited"}}

	views, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	type want struct {
		status      string
		container   string
		recoverable bool
	}
	expected := map[string]want{
		"a": {model.InstanceStatusReady, "running", false},
		"b": {model.InstanceStatusStopped, "exited", false}, // panel-initiated stop
		"c": {model.InstanceStatusMissing, "missing", true}, // container removed
		"d": {model.InstanceStatusExited, "exited", false},  // stopped outside the panel
	}
	for _, view := range views {
		w, ok := expected[view.ID]
		if !ok {
			t.Fatalf("unexpected instance %q", view.ID)
		}
		if view.Status != w.status || view.ContainerState != w.container || view.Recoverable != w.recoverable {
			t.Fatalf("%s: status=%q state=%q recoverable=%v, want %+v", view.ID, view.Status, view.ContainerState, view.Recoverable, w)
		}
	}
}
