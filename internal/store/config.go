package store

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

const (
	// DefaultConfigPath is the deployment path from the specification.
	DefaultConfigPath = "/etc/ts3panel/config.toml"
	// DefaultStatePath is the deployment state path from the specification.
	DefaultStatePath = "/var/lib/ts3panel/state.json"
)

var (
	backupTimeRE  = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	providerTypes = map[string]bool{
		"cloudflare": true,
		"aliyun":     true,
		"dnspod":     true,
		"manual":     true,
	}
)

// ProviderConfig is one DNS provider account. CredsEnc is an AES-GCM ciphertext
// produced with the config's master_key; plaintext credentials are never stored
// in config.toml or state.json.
type ProviderConfig struct {
	ID       string `toml:"id"`
	Type     string `toml:"type"`
	Label    string `toml:"label"`
	Zone     string `toml:"zone"`
	ZoneID   string `toml:"zone_id"`
	CredsEnc string `toml:"creds_enc"`
	Enabled  bool   `toml:"enabled"`
	Default  bool   `toml:"default"`
}

type CloudflareConfig struct {
	PublicIP string `toml:"public_ip"`
}

type DockerConfig struct {
	Socket string `toml:"socket"`
}

type LimitsConfig struct {
	MaxInstances int    `toml:"max_instances"`
	TS3Memory    string `toml:"ts3_memory"`
	TS3CPUs      string `toml:"ts3_cpus"`
	RecordTTL    int    `toml:"record_ttl"`
}

type BackupConfig struct {
	Enabled       bool   `toml:"enabled"`
	Mode          string `toml:"mode"`
	DailyAt       string `toml:"daily_at"`
	IntervalHours int    `toml:"interval_hours"`
	Retention     int    `toml:"retention"`
}

// PortsConfig controls the optional third-party reachability check. It is off
// by default: enabling it makes the panel contact check-host.net.
type PortsConfig struct {
	ExternalCheck     bool `toml:"external_check"`
	CheckCacheSeconds int  `toml:"check_cache_seconds"`
}

// Config mirrors config.toml. The [cloudflare] table is kept for compatibility
// with the v0.2 layout; new provider entries live under [[providers]].
type Config struct {
	Listen        string           `toml:"listen"`
	InstanceRoot  string           `toml:"instance_root"`
	SessionSecret string           `toml:"session_secret"`
	SetupToken    string           `toml:"setup_token"`
	MasterKey     string           `toml:"master_key"`
	Providers     []ProviderConfig `toml:"providers"`
	Cloudflare    CloudflareConfig `toml:"cloudflare"`
	Docker        DockerConfig     `toml:"docker"`
	Limits        LimitsConfig     `toml:"limits"`
	Backup        BackupConfig     `toml:"backup"`
	Ports         PortsConfig      `toml:"ports"`
}

// ConfigStore serializes config reads and writes. The config is tiny and mostly
// immutable at runtime, so an in-memory copy is kept and replaced atomically
// after every successful disk write.
type ConfigStore struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

const configTemplate = `# ts3panel configuration
# Owned by root:root with mode 0600: it contains the session secret and
# master key. The parent directory is 0700 and must be writable for atomic
# provider/config saves.
# Provider credentials under [[providers]] are AES-GCM encrypted with master_key.

listen = "127.0.0.1:8899"
instance_root = "/srv/ts3"
session_secret = "__SESSION_SECRET__"
setup_token = ""
master_key = "__MASTER_KEY__"

# Provider credentials are managed by the setup wizard. Example:
# [[providers]]
# id = "cf-main"
# type = "cloudflare"        # cloudflare | aliyun | dnspod | manual
# label = "我的 Cloudflare"
# zone = "example.com"
# zone_id = ""
# creds_enc = ""
# enabled = true
# default = true

[cloudflare]                  # 兼容旧配置，公网 IP 暂存于此
public_ip = ""

[docker]
socket = "unix:///var/run/docker.sock"

[limits]
max_instances = 20
ts3_memory = "512m"
ts3_cpus = "1.0"
record_ttl = 300

[backup]
enabled = false
mode = "daily"
daily_at = "04:30"
interval_hours = 24
retention = 7

[ports]
# 外网连通性检测会请求第三方服务 check-host.net；默认关闭。
external_check = false
check_cache_seconds = 60
`

// LoadConfig reads config.toml, creating it from the documented template when
// missing. On Linux it enforces mode 0600; legacy deployments running as the
// ts3panel account also keep ownership of the file, while the supported
// root-run deployment leaves it root:root. The returned bool reports whether a
// new file was created.
func LoadConfig(path string) (*ConfigStore, bool, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigPath
	}
	path = filepath.Clean(path)

	created := false
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		err := generateConfigFile(path)
		switch {
		case err == nil:
			created = true
		case errors.Is(err, os.ErrExist):
			// Another process won the race; load its file below.
		default:
			return nil, false, err
		}
	} else if err != nil {
		return nil, false, fmt.Errorf("stat config: %w", err)
	}

	cfg, err := readConfigFile(path)
	if err != nil {
		return nil, created, err
	}
	applyConfigDefaults(&cfg)
	changed, err := ensureConfigSecrets(&cfg)
	if err != nil {
		return nil, created, err
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, created, err
	}

	if changed {
		if err := writeConfigFile(path, cfg); err != nil {
			return nil, created, fmt.Errorf("persist generated secrets: %w", err)
		}
	}
	if err := enforceConfigPermissions(path); err != nil {
		return nil, created, err
	}

	return &ConfigStore{
		path: path,
		cfg:  cloneConfig(cfg),
	}, created, nil
}

func generateConfigFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	sessionSecret, err := GenerateHexSecret(64)
	if err != nil {
		return err
	}
	masterKey, err := GenerateHexSecret(32)
	if err != nil {
		return err
	}
	content := strings.NewReplacer(
		"__SESSION_SECRET__", sessionSecret,
		"__MASTER_KEY__", masterKey,
	).Replace(configTemplate)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString(content)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write config: %w", writeErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync config: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close config: %w", closeErr)
	}
	return nil
}

func readConfigFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, errors.New("config.toml is empty")
	}
	var cfg Config
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config.toml: %w", err)
	}
	return cfg, nil
}

func writeConfigFile(path string, cfg Config) error {
	var buf bytes.Buffer
	buf.WriteString("# ts3panel configuration\n")
	buf.WriteString("# Owned by root:root, mode 0600. Provider credentials are stored encrypted.\n\n")
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return fmt.Errorf("encode config.toml: %w", err)
	}
	if err := writeFileAtomic(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
	}
	if err := enforceConfigPermissions(path); err != nil {
		return err
	}
	return nil
}

// enforceConfigPermissions keeps the service able to rewrite provider settings:
// config.toml must remain 0600. The supported root deployment keeps root:root;
// a legacy non-root service account claims ownership of its own config.
// Atomic saves create a temp file in the same directory, so the directory must
// also allow the service to write (installer: /etc/ts3panel 0700).
func enforceConfigPermissions(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("config.toml must be 0600: %w", err)
	}
	if err := enforceConfigOwner(path); err != nil {
		return err
	}
	return nil
}

func applyConfigDefaults(cfg *Config) {
	if strings.TrimSpace(cfg.Listen) == "" {
		cfg.Listen = "127.0.0.1:8899"
	}
	if strings.TrimSpace(cfg.InstanceRoot) == "" {
		cfg.InstanceRoot = "/srv/ts3"
	}
	if strings.TrimSpace(cfg.Docker.Socket) == "" {
		cfg.Docker.Socket = "unix:///var/run/docker.sock"
	}
	if cfg.Limits.MaxInstances == 0 {
		cfg.Limits.MaxInstances = 20
	}
	if strings.TrimSpace(cfg.Limits.TS3Memory) == "" {
		cfg.Limits.TS3Memory = "512m"
	}
	if strings.TrimSpace(cfg.Limits.TS3CPUs) == "" {
		cfg.Limits.TS3CPUs = "1.0"
	}
	if cfg.Limits.RecordTTL == 0 {
		cfg.Limits.RecordTTL = 300
	}
	if strings.TrimSpace(cfg.Backup.Mode) == "" {
		cfg.Backup.Mode = "daily"
	}
	if strings.TrimSpace(cfg.Backup.DailyAt) == "" {
		cfg.Backup.DailyAt = "04:30"
	}
	if cfg.Backup.IntervalHours == 0 {
		cfg.Backup.IntervalHours = 24
	}
	if cfg.Backup.Retention == 0 {
		cfg.Backup.Retention = 7
	}
	if cfg.Ports.CheckCacheSeconds == 0 {
		cfg.Ports.CheckCacheSeconds = 60
	}
}

func ensureConfigSecrets(cfg *Config) (bool, error) {
	changed := false
	if strings.TrimSpace(cfg.SessionSecret) == "" {
		secret, err := GenerateHexSecret(64)
		if err != nil {
			return false, err
		}
		cfg.SessionSecret = secret
		changed = true
	}
	if strings.TrimSpace(cfg.MasterKey) == "" {
		if len(cfg.Providers) > 0 {
			return false, errors.New("master_key is missing but providers exist; refusing to generate a new key")
		}
		key, err := GenerateHexSecret(32)
		if err != nil {
			return false, err
		}
		cfg.MasterKey = key
		changed = true
	}
	return changed, nil
}

func validateConfig(cfg *Config) error {
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return fmt.Errorf("listen must be host:port: %w", err)
	}
	_, portText, _ := net.SplitHostPort(cfg.Listen)
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("listen port must be between 1 and 65535")
	}
	// instance_root is a Linux host path. Accept both native absolute paths and
	// POSIX absolute paths so Windows developer builds can validate the
	// documented default (/srv/ts3).
	if !filepath.IsAbs(cfg.InstanceRoot) && !path.IsAbs(cfg.InstanceRoot) {
		return errors.New("instance_root must be an absolute path")
	}
	if err := validateHexSecret("session_secret", cfg.SessionSecret, 64); err != nil {
		return err
	}
	if err := validateHexSecret("master_key", cfg.MasterKey, 32); err != nil {
		return err
	}
	if cfg.Limits.MaxInstances < 1 {
		return errors.New("limits.max_instances must be positive")
	}
	if cfg.Limits.RecordTTL < 1 {
		return errors.New("limits.record_ttl must be positive")
	}
	if cfg.Backup.Mode != "daily" && cfg.Backup.Mode != "interval" {
		return errors.New("backup.mode must be daily or interval")
	}
	if !backupTimeRE.MatchString(cfg.Backup.DailyAt) {
		return errors.New("backup.daily_at must be HH:MM")
	}
	if cfg.Backup.IntervalHours < 1 {
		return errors.New("backup.interval_hours must be positive")
	}
	if cfg.Backup.Retention < 1 {
		return errors.New("backup.retention must be positive")
	}
	if cfg.Ports.CheckCacheSeconds < 10 || cfg.Ports.CheckCacheSeconds > 3600 {
		return errors.New("ports.check_cache_seconds must be between 10 and 3600")
	}
	if cfg.Cloudflare.PublicIP != "" {
		ip := net.ParseIP(cfg.Cloudflare.PublicIP)
		if ip == nil || ip.To4() == nil {
			return errors.New("cloudflare.public_ip must be a valid IPv4 address")
		}
	}
	seen := make(map[string]bool, len(cfg.Providers))
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		p.Type = strings.ToLower(strings.TrimSpace(p.Type))
		p.Zone = NormalizeDomain(p.Zone)
		if strings.TrimSpace(p.ID) == "" {
			return errors.New("provider id must not be empty")
		}
		if seen[p.ID] {
			return fmt.Errorf("duplicate provider id %q", p.ID)
		}
		seen[p.ID] = true
		if !providerTypes[p.Type] {
			return fmt.Errorf("provider %q has unsupported type %q", p.ID, p.Type)
		}
		if !ValidDomain(p.Zone) {
			return fmt.Errorf("provider %q has invalid zone", p.ID)
		}
		if p.CredsEnc != "" {
			if _, err := base64.StdEncoding.DecodeString(p.CredsEnc); err != nil {
				return fmt.Errorf("provider %q has malformed creds_enc", p.ID)
			}
		}
	}
	return nil
}

func validateHexSecret(name, value string, wantBytes int) error {
	raw, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s must be hex encoded", name)
	}
	if len(raw) != wantBytes {
		return fmt.Errorf("%s must decode to %d bytes", name, wantBytes)
	}
	return nil
}

// Snapshot returns a deep copy so callers cannot mutate the store's state.
func (c *ConfigStore) Snapshot() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneConfig(c.cfg)
}

// Save validates and atomically replaces config.toml, re-applies 0600 mode and
// ts3panel:ts3panel ownership, then updates the in-memory copy. On any error the
// previous file and memory state are kept.
func (c *ConfigStore) Save(cfg Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Clone first: validation normalizes provider fields in place, and callers
	// may retain this value for rollback.
	cfg = cloneConfig(cfg)
	applyConfigDefaults(&cfg)
	if err := validateConfig(&cfg); err != nil {
		return err
	}
	if err := writeConfigFile(c.path, cfg); err != nil {
		return err
	}
	c.cfg = cloneConfig(cfg)
	return nil
}

// Update applies fn to a copy of the config and persists the result atomically.
func (c *ConfigStore) Update(fn func(*Config) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	next := cloneConfig(c.cfg)
	if err := fn(&next); err != nil {
		return err
	}
	applyConfigDefaults(&next)
	if err := validateConfig(&next); err != nil {
		return err
	}
	if err := writeConfigFile(c.path, next); err != nil {
		return err
	}
	c.cfg = cloneConfig(next)
	return nil
}

func (c *ConfigStore) Path() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.path
}

func cloneConfig(cfg Config) Config {
	out := cfg
	if len(cfg.Providers) > 0 {
		out.Providers = append([]ProviderConfig(nil), cfg.Providers...)
	}
	return out
}

// NormalizeDomain lowercases a root domain and strips one trailing dot.
func NormalizeDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	return strings.TrimSuffix(domain, ".")
}

var domainLabelRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidDomain performs structural validation only; it does not prove ownership
// or public resolvability.
func ValidDomain(domain string) bool {
	domain = NormalizeDomain(domain)
	if len(domain) == 0 || len(domain) > 253 {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !domainLabelRE.MatchString(label) {
			return false
		}
	}
	return true
}

// FindProviderIndex returns the index of the provider matching type and zone.
func (cfg *Config) FindProviderIndex(typ, zone string) int {
	typ = strings.ToLower(strings.TrimSpace(typ))
	zone = NormalizeDomain(zone)
	for i := range cfg.Providers {
		if cfg.Providers[i].Type == typ && cfg.Providers[i].Zone == zone {
			return i
		}
	}
	return -1
}

// UpsertProvider creates or updates the provider for its type+zone pair and
// returns its stable ID. It is idempotent so a retried setup cannot create
// duplicate provider entries.
func (cfg *Config) UpsertProvider(p ProviderConfig) string {
	p.Type = strings.ToLower(strings.TrimSpace(p.Type))
	p.Zone = NormalizeDomain(p.Zone)
	idx := cfg.FindProviderIndex(p.Type, p.Zone)
	if idx >= 0 {
		existing := &cfg.Providers[idx]
		p.ID = existing.ID
		if p.ZoneID == "" {
			p.ZoneID = existing.ZoneID
		}
		if p.CredsEnc == "" {
			p.CredsEnc = existing.CredsEnc
		}
		p.Enabled = true
		p.Default = true
		existing.Label = p.Label
		existing.Zone = p.Zone
		existing.ZoneID = p.ZoneID
		existing.CredsEnc = p.CredsEnc
		existing.Enabled = p.Enabled
		existing.Default = p.Default
		for i := range cfg.Providers {
			if i != idx {
				cfg.Providers[i].Default = false
			}
		}
		return p.ID
	}

	p.ID = nextProviderID(cfg, p.Type)
	p.Enabled = true
	p.Default = true
	for i := range cfg.Providers {
		cfg.Providers[i].Default = false
	}
	cfg.Providers = append(cfg.Providers, p)
	return p.ID
}

// NextProviderID returns an unused, human-readable provider ID for typ
// (for example "cf-main", then "cf-2"). It is exported so the settings API can
// create providers without duplicating the naming rule.
func (cfg *Config) NextProviderID(typ string) string {
	return nextProviderID(cfg, typ)
}

func nextProviderID(cfg *Config, typ string) string {
	prefix := map[string]string{
		"cloudflare": "cf",
		"aliyun":     "aliyun",
		"dnspod":     "dnspod",
		"manual":     "manual",
	}[typ]
	if prefix == "" {
		prefix = "provider"
	}
	used := make(map[string]bool, len(cfg.Providers))
	for _, p := range cfg.Providers {
		used[p.ID] = true
	}
	candidate := prefix + "-main"
	if !used[candidate] {
		return candidate
	}
	for n := 2; ; n++ {
		candidate = fmt.Sprintf("%s-%d", prefix, n)
		if !used[candidate] {
			return candidate
		}
	}
}
