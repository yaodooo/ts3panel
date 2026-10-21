package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"ts3panel/internal/backup"
	"ts3panel/internal/model"
	"ts3panel/internal/orchestrator"
	"ts3panel/internal/store"
)

// uploadLimit caps a single uploaded archive. TS3 data directories are small
// (a few hundred MB at most), so 2 GiB leaves room without letting an upload
// fill the disk before the free-space check runs.
const (
	backupUploadLimit = 2 << 30
	backupOpTimeout   = 10 * time.Minute
)

// managerLifecycle adapts the orchestrator to the small interface the backup
// package needs, keeping backup free of Docker and orchestrator types.
type managerLifecycle struct {
	m *orchestrator.Manager
}

func (l managerLifecycle) Running(ctx context.Context, id string) (bool, error) {
	view, err := l.m.Get(ctx, id)
	if err != nil {
		return false, err
	}
	return view.ContainerState == "running" || view.ContainerState == "restarting", nil
}

func (l managerLifecycle) Stop(ctx context.Context, id string) error {
	_, err := l.m.Stop(ctx, id)
	return err
}

func (l managerLifecycle) Start(ctx context.Context, id string) error {
	_, err := l.m.Start(ctx, id)
	return err
}

type backupView struct {
	model.BackupRecord
	Exists bool `json:"exists"`
}

type backupPlanView struct {
	Enabled       bool       `json:"enabled"`
	Mode          string     `json:"mode"`
	DailyAt       string     `json:"daily_at"`
	IntervalHours int        `json:"interval_hours"`
	Retention     int        `json:"retention"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	NextRunAt     *time.Time `json:"next_run_at,omitempty"`
	InstanceCount int        `json:"instance_count"`
}

type backupPlanUpdate struct {
	Enabled       *bool   `json:"enabled"`
	Mode          *string `json:"mode"`
	DailyAt       *string `json:"daily_at"`
	IntervalHours *int    `json:"interval_hours"`
	Retention     *int    `json:"retention"`
}

type restoreRequest struct {
	File string `json:"file"`
}

// writeBackupError translates backup failures to the API contract. All messages
// are Chinese and operator-facing; the underlying error goes to hint.
func writeBackupError(w http.ResponseWriter, err error) {
	var diskErr *backup.DiskSpaceError
	switch {
	case errors.Is(err, backup.ErrInstanceNotFound):
		writeError(w, http.StatusNotFound, "E_INSTANCE_NOT_FOUND", "实例不存在。", "")
	case errors.Is(err, backup.ErrBackupNotFound):
		writeError(w, http.StatusNotFound, "E_BACKUP_NOT_FOUND", "找不到这条备份记录。", "")
	case errors.Is(err, backup.ErrArchiveMissing):
		writeError(w, http.StatusNotFound, "E_BACKUP_NOT_FOUND", "备份文件已不存在，可能已被删除。", "")
	case errors.Is(err, backup.ErrArchiveNoDatabase):
		writeError(w, http.StatusBadRequest, "E_BACKUP_INVALID", "备份包里没有 ts3server.sqlitedb，不是有效的 TS3 数据备份。", "")
	case errors.Is(err, backup.ErrArchiveInvalid), errors.Is(err, backup.ErrArchiveUnsafe):
		writeError(w, http.StatusBadRequest, "E_BACKUP_INVALID", "备份包格式不正确或包含不安全路径。", err.Error())
	case errors.Is(err, backup.ErrInstanceDirMissing):
		writeError(w, http.StatusConflict, "E_INSTANCE_NOT_FOUND", "实例数据目录不存在，无法备份。", err.Error())
	case errors.Is(err, backup.ErrPortConflict):
		writeError(w, http.StatusConflict, "E_PORT_EXHAUSTED", "备份中的端口已被其他实例占用，无法恢复。", err.Error())
	case errors.As(err, &diskErr):
		writeError(w, http.StatusConflict, "E_DISK_SPACE",
			fmt.Sprintf("磁盘空间不足：备份需要约 %s，当前仅剩 %s。",
				formatBytes(diskErr.Needed), formatBytes(diskErr.Free)),
			"未写入任何备份文件，请清理磁盘后重试。")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "E_DOCKER", "备份/恢复操作超时。", "")
	case errors.Is(err, orchestrator.ErrNotManaged):
		writeError(w, http.StatusConflict, "E_DOCKER", "同名容器不属于 ts3panel，拒绝操作。", err.Error())
	case errors.Is(err, orchestrator.ErrDocker), errors.Is(err, orchestrator.ErrContainerTimeout):
		writeError(w, http.StatusInternalServerError, "E_DOCKER", "容器操作失败，备份/恢复未完成。", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", "备份/恢复失败。", err.Error())
	}
}

func formatBytes(n int64) string {
	if n < 0 {
		return "未知"
	}
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// handleCreateBackup takes a manual backup. The container is stopped while the
// archive is written, so this is a long request with an extended deadline.
func (a *API) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	setLongDeadline(w, backupOpTimeout)
	ctx, cancel := context.WithTimeout(r.Context(), backupOpTimeout)
	defer cancel()

	rec, err := a.backups.Create(ctx, r.PathValue("id"), false)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, backupView{BackupRecord: rec, Exists: true})
}

func (a *API) handleListBackups(w http.ResponseWriter, r *http.Request) {
	recs, err := a.backups.List(r.PathValue("id"))
	if err != nil {
		writeBackupError(w, err)
		return
	}
	out := make([]backupView, 0, len(recs))
	for _, rec := range recs {
		_, statErr := os.Stat(filepath.Join(a.backups.BackupDir(), rec.File))
		out = append(out, backupView{BackupRecord: rec, Exists: statErr == nil})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) handleListAllBackups(w http.ResponseWriter, r *http.Request) {
	recs, err := a.backups.All()
	if err != nil {
		writeBackupError(w, err)
		return
	}
	st, err := a.state.Snapshot()
	if err != nil {
		a.internalError(w, "list backups", err)
		return
	}
	instances := make(map[string]bool, len(st.Instances))
	for _, inst := range st.Instances {
		instances[inst.ID] = true
	}
	type allView struct {
		backupView
		InstanceExists bool `json:"instance_exists"`
	}
	out := make([]allView, 0, len(recs))
	for _, rec := range recs {
		_, statErr := os.Stat(filepath.Join(a.backups.BackupDir(), rec.File))
		out = append(out, allView{
			backupView:     backupView{BackupRecord: rec, Exists: statErr == nil},
			InstanceExists: instances[rec.InstanceID],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDownloadBackup streams the archive with http.ServeContent: nothing is
// buffered in memory, so even a large backup downloads with constant memory.
func (a *API) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	file := r.URL.Query().Get("file")
	f, rec, err := a.backups.Open(r.PathValue("id"), file)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": rec.File}))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, rec.File, rec.CreatedAt, f)
}

// handleRestore accepts either an existing archive name (JSON) or an uploaded
// multipart archive. Uploads are imported as a new backup record first, so a
// restore can always be retried from the list.
func (a *API) handleRestore(w http.ResponseWriter, r *http.Request) {
	setLongDeadline(w, backupOpTimeout)
	ctx, cancel := context.WithTimeout(r.Context(), backupOpTimeout)
	defer cancel()
	id := r.PathValue("id")

	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var file string
	if mediaType == "multipart/form-data" {
		if _, err := a.instanceByID(id); err != nil {
			writeError(w, http.StatusBadRequest, "E_BAD_REQUEST",
				"该实例已删除，只能从已有备份列表恢复，不能直接上传。", "")
			return
		}
		uploaded, err := a.saveUploadedArchive(w, r)
		if err != nil {
			return
		}
		defer os.Remove(uploaded)
		rec, err := a.backups.ImportTempFile(ctx, id, uploaded)
		if err != nil {
			writeBackupError(w, err)
			return
		}
		file = rec.File
	} else {
		var req restoreRequest
		if err := decodeJSON(w, r, &req); err != nil {
			a.badRequest(w, "请求格式不正确。", err)
			return
		}
		file = req.File
	}
	if file == "" {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "请选择要恢复的备份文件。", "")
		return
	}

	if _, err := a.backups.Restore(ctx, id, file); err != nil {
		writeBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file": file})
}

// saveUploadedArchive streams the multipart file to a temp file next to the
// backups. The panel never buffers the whole archive in memory.
func (a *API) saveUploadedArchive(w http.ResponseWriter, r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, backupUploadLimit)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "E_BAD_REQUEST", "上传文件过大或表单格式不正确。", "")
		return "", err
	}
	src, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "没有找到上传的备份文件。", "")
		return "", err
	}
	defer src.Close()

	dir := a.backups.BackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		a.internalError(w, "create backup directory", err)
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*.tar.gz")
	if err != nil {
		a.internalError(w, "create upload temp file", err)
		return "", err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		a.internalError(w, "secure upload temp file", err)
		return "", err
	}
	_, copyErr := io.Copy(tmp, src)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(name)
		err := copyErr
		if err == nil {
			err = syncErr
		}
		if err == nil {
			err = closeErr
		}
		a.internalError(w, "save uploaded archive", err)
		return "", err
	}
	return name, nil
}

func (a *API) handleGetBackupPlan(w http.ResponseWriter, r *http.Request) {
	cfg := a.config.Snapshot()
	st, err := a.state.Snapshot()
	if err != nil {
		a.internalError(w, "read backup plan", err)
		return
	}
	writeJSON(w, http.StatusOK, backupPlanView{
		Enabled:       cfg.Backup.Enabled,
		Mode:          cfg.Backup.Mode,
		DailyAt:       cfg.Backup.DailyAt,
		IntervalHours: cfg.Backup.IntervalHours,
		Retention:     cfg.Backup.Retention,
		LastAttemptAt: st.Backup.LastAttemptAt,
		LastSuccessAt: st.Backup.LastSuccessAt,
		LastError:     st.Backup.LastError,
		NextRunAt:     st.Backup.NextRunAt,
		InstanceCount: len(st.Instances),
	})
}

func (a *API) handleUpdateBackupPlan(w http.ResponseWriter, r *http.Request) {
	var req backupPlanUpdate
	if err := decodeJSON(w, r, &req); err != nil {
		a.badRequest(w, "请求格式不正确。", err)
		return
	}
	err := a.config.Update(func(cfg *store.Config) error {
		if req.Enabled != nil {
			cfg.Backup.Enabled = *req.Enabled
		}
		if req.Mode != nil {
			cfg.Backup.Mode = *req.Mode
		}
		if req.DailyAt != nil {
			cfg.Backup.DailyAt = *req.DailyAt
		}
		if req.IntervalHours != nil {
			cfg.Backup.IntervalHours = *req.IntervalHours
		}
		if req.Retention != nil {
			cfg.Backup.Retention = *req.Retention
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_BAD_REQUEST", "备份计划参数不正确。", err.Error())
		return
	}
	if err := a.scheduler.Reschedule(); err != nil {
		a.internalError(w, "reschedule backups", err)
		return
	}
	a.handleGetBackupPlan(w, r)
}

func setLongDeadline(w http.ResponseWriter, d time.Duration) {
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(d + time.Minute))
	}
}
