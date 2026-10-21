package model

import "time"

// Instance status values, matching the state.json contract in the spec.
const (
	InstanceStatusPending = "pending"
	InstanceStatusRunning = "running"
	InstanceStatusStopped = "stopped"
	InstanceStatusExited  = "exited"
	InstanceStatusMissing = "missing"
	InstanceStatusFailed  = "failed"
	InstanceStatusReady   = "ready"
)

// DNS status values, matching the state.json contract in the spec.
const (
	DNSStatusNone     = "none"
	DNSStatusPending  = "pending"
	DNSStatusVerified = "verified"
	DNSStatusFailed   = "failed"
)

// User is the single panel administrator. There is deliberately no default
// account: the user is created by the setup wizard.
type User struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

// RecoveryCode stores only a bcrypt hash of a one-time recovery code. The
// plaintext code exists only in the setup response and must never be persisted.
type RecoveryCode struct {
	Hash   string     `json:"hash"`
	Used   bool       `json:"used"`
	UsedAt *time.Time `json:"used_at,omitempty"`
}

// Ports is the four-port group owned by one TS3 instance. Query ports are
// loopback-only and must never appear in the public port checklist.
type Ports struct {
	Voice        int `json:"voice"`
	FileTransfer int `json:"filetransfer"`
	Query        int `json:"query"`
	QuerySSH     int `json:"query_ssh"`
}

// BackupRecord describes one backup archive. Records live at the top level of
// state.json (not inside Instance) so deleting an instance does not destroy the
// information needed to restore it later. The snapshot fields are captured when
// the backup is taken and are only used when the instance no longer exists.
type BackupRecord struct {
	InstanceID string    `json:"instance_id"`
	File       string    `json:"file"`
	SizeBytes  int64     `json:"size_bytes"`
	CreatedAt  time.Time `json:"created_at"`
	Auto       bool      `json:"auto,omitempty"`
	// Snapshot of the instance at backup time, used to recreate a deleted
	// instance with its original id, address and port group.
	Name       string `json:"name,omitempty"`
	Subdomain  string `json:"subdomain,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
	Container  string `json:"container,omitempty"`
	Ports      Ports  `json:"ports,omitempty"`
}

// BackupRunState is the scheduler's persisted state: when the last attempt and
// success happened and when the next run is due. Persisting NextRunAt is what
// keeps a panel restart from re-running a backup that already happened.
type BackupRunState struct {
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	NextRunAt     *time.Time `json:"next_run_at,omitempty"`
}

// DNSInfo records provider-side record IDs so deletion can target exactly the
// records the panel created instead of deleting by name.
type DNSInfo struct {
	ARecordID   string     `json:"a_record_id,omitempty"`
	SRVRecordID string     `json:"srv_record_id,omitempty"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
}

// Instance is one managed TS3 server. AdminToken is sensitive: it is persisted
// in state.json (0600) but must not be returned in list responses or logs.
type Instance struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Subdomain  string    `json:"subdomain"`
	ProviderID string    `json:"provider_id,omitempty"`
	Ports      Ports     `json:"ports"`
	Container  string    `json:"container"`
	Status     string    `json:"status"`
	DNSStatus  string    `json:"dns_status"`
	AdminToken string    `json:"admin_token,omitempty"`
	DNS        DNSInfo   `json:"dns"`
	CreatedAt  time.Time `json:"created_at"`
	Notes      string    `json:"notes"`
}
