package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	JobUpload = "upload"
	JobBuild  = "build"

	JobUploading  = "uploading"
	JobQueued     = "queued"
	JobLeased     = "leased"
	JobPublishing = "publishing"
	JobDone       = "done"
	JobFailed     = "failed"
)

var (
	ErrJobFull      = errors.New("ingestion queue is full")
	ErrJobConflict  = errors.New("ingestion request key already has different inputs")
	ErrJobLeaseLost = errors.New("ingestion job lease is no longer active")
	ErrJobNotReady  = errors.New("ingestion job is not ready for this operation")
)

type JobSpec struct {
	ID            string
	RepoID        int64
	RequestKey    string
	Fingerprint   string
	Kind          string
	SpecJSON      string
	ArchiveKey    string
	ArchiveSHA256 string
	ArchiveBytes  int64
	ExpiresAt     int64
}

type Job struct {
	JobSpec
	Status     string
	Attempts   int
	WorkerID   string
	LeaseToken string
	LeaseUntil int64
	CreatedAt  int64
	UpdatedAt  int64
	ResultJSON string
	Error      string
}

type JobStats struct {
	Uploading              int
	Queued                 int
	Active                 int
	Publishing             int
	OldestQueuedAgeSeconds int64
}

const jobColumns = `id, repo_id, request_key, fingerprint, kind, spec_json, archive_key,
	archive_sha256, archive_bytes, expires_at, status, attempts, worker_id, lease_token,
	lease_until, created_at, updated_at, result_json, error`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.RepoID, &j.RequestKey, &j.Fingerprint, &j.Kind, &j.SpecJSON,
		&j.ArchiveKey, &j.ArchiveSHA256, &j.ArchiveBytes, &j.ExpiresAt, &j.Status,
		&j.Attempts, &j.WorkerID, &j.LeaseToken, &j.LeaseUntil, &j.CreatedAt,
		&j.UpdatedAt, &j.ResultJSON, &j.Error)
	return j, mapNoRows(err)
}

func (s *Store) GetJob(id string) (Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobColumns+` FROM ingestion_jobs WHERE id = ?`, id))
}

func (s *Store) GetJobByRequest(repoID int64, key string) (Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobColumns+` FROM ingestion_jobs WHERE repo_id = ? AND request_key = ?`, repoID, key))
}

func expireJobs(tx *sql.Tx, now int64) error {
	_, err := tx.Exec(`UPDATE ingestion_jobs SET status = 'failed', error = 'job expired',
		worker_id = '', lease_token = '', lease_until = 0, updated_at = ?
		WHERE status IN ('uploading', 'queued', 'leased') AND expires_at <= ?`, now, now)
	return err
}

const UploadReservationTimeout = 10 * time.Minute

func JobRepositoryLimit(maxPending int) int {
	return max(1, (maxPending+3)/4)
}

func (s *Store) ReserveJob(spec JobSpec, maxPending int, now time.Time) (Job, bool, error) {
	return s.reserveJob(spec, maxPending, JobRepositoryLimit(maxPending), now)
}

func (s *Store) ReserveContinuation(spec JobSpec, maxPending int, now time.Time) (Job, bool, error) {
	if maxPending < 1 || spec.Kind != JobBuild {
		return Job{}, false, errors.New("invalid ingestion continuation reservation")
	}
	return s.reserveJob(spec, maxPending+1, JobRepositoryLimit(maxPending)+1, now)
}

func (s *Store) reserveJob(spec JobSpec, maxPending, maxPerRepo int, now time.Time) (Job, bool, error) {
	if spec.ID == "" || spec.RepoID <= 0 || spec.RequestKey == "" || spec.Fingerprint == "" ||
		(spec.Kind != JobUpload && spec.Kind != JobBuild) || !json.Valid([]byte(spec.SpecJSON)) ||
		spec.ArchiveBytes < 0 || spec.ExpiresAt <= now.Unix() || maxPending < 1 {
		return Job{}, false, errors.New("invalid ingestion job reservation")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	if err := expireJobs(tx, now.Unix()); err != nil {
		return Job{}, false, err
	}
	existing, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM ingestion_jobs WHERE repo_id = ? AND request_key = ?`, spec.RepoID, spec.RequestKey))
	if err == nil {
		if existing.Fingerprint != spec.Fingerprint {
			return Job{}, false, ErrJobConflict
		}
		if err := tx.Commit(); err != nil {
			return Job{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Job{}, false, err
	}
	if err := checkJobAdmission(tx, spec.RepoID, maxPending, maxPerRepo, now); err != nil {
		return Job{}, false, err
	}
	status := JobUploading
	if spec.Kind == JobBuild {
		status = JobQueued
	}
	j, err := scanJob(tx.QueryRow(`INSERT INTO ingestion_jobs
		(id, repo_id, request_key, fingerprint, kind, spec_json, archive_key, archive_sha256,
		archive_bytes, expires_at, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING `+jobColumns,
		spec.ID, spec.RepoID, spec.RequestKey, spec.Fingerprint, spec.Kind, spec.SpecJSON,
		spec.ArchiveKey, spec.ArchiveSHA256, spec.ArchiveBytes, spec.ExpiresAt, status, now.Unix(), now.Unix()))
	if err != nil {
		return Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

func checkJobAdmission(tx *sql.Tx, repoID int64, maxPending, maxPerRepo int, now time.Time) error {
	var count, repoCount int
	if err := tx.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE repo_id = ?) FROM ingestion_jobs
  WHERE status NOT IN ('done', 'failed') AND (status != 'uploading' OR created_at > ?)`,
		repoID, now.Add(-UploadReservationTimeout).Unix()).Scan(&count, &repoCount); err != nil {
		return err
	}
	if count >= maxPending || repoCount >= maxPerRepo {
		return ErrJobFull
	}
	return nil
}

func (s *Store) QueueJob(id string, maxPending int, now time.Time) (Job, error) {
	if maxPending < 1 {
		return Job{}, errors.New("job admission limit must be positive")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	if err := expireJobs(tx, now.Unix()); err != nil {
		return Job{}, err
	}
	job, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM ingestion_jobs WHERE id = ?`, id))
	if err != nil {
		return Job{}, err
	}
	if job.Status == JobUploading {
		if job.CreatedAt <= now.Add(-UploadReservationTimeout).Unix() {
			if err := checkJobAdmission(tx, job.RepoID, maxPending, JobRepositoryLimit(maxPending), now); err != nil {
				return Job{}, err
			}
		}
		job, err = scanJob(tx.QueryRow(`UPDATE ingestion_jobs SET status = 'queued', updated_at = ?
   WHERE id = ? RETURNING `+jobColumns, now.Unix(), id))
		if err != nil {
			return Job{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}
	if job.Status == JobFailed {
		return Job{}, ErrJobNotReady
	}
	return job, nil
}

func (s *Store) ClaimJob(workerID, token string, now time.Time, lease time.Duration, maxAttempts int) (Job, error) {
	if workerID == "" || token == "" || lease < time.Second || maxAttempts < 1 {
		return Job{}, errors.New("invalid ingestion job claim")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	if err := expireJobs(tx, now.Unix()); err != nil {
		return Job{}, err
	}
	if _, err := tx.Exec(`UPDATE ingestion_jobs SET status = 'failed', error = 'attempt limit reached',
		worker_id = '', lease_token = '', lease_until = 0, updated_at = ?
		WHERE attempts >= ? AND (status = 'queued' OR (status = 'leased' AND lease_until <= ?))`, now.Unix(), maxAttempts, now.Unix()); err != nil {
		return Job{}, err
	}
	j, err := scanJob(tx.QueryRow(`UPDATE ingestion_jobs SET status = 'leased', worker_id = ?,
		lease_token = ?, lease_until = MIN(?, expires_at), attempts = attempts + 1, updated_at = ?, error = ''
		WHERE id = (SELECT id FROM ingestion_jobs WHERE expires_at > ? AND attempts < ?
		AND (status = 'queued' OR (status = 'leased' AND lease_until <= ?))
		ORDER BY created_at, id LIMIT 1) RETURNING `+jobColumns,
		workerID, token, now.Add(lease).Unix(), now.Unix(), now.Unix(), maxAttempts, now.Unix()))
	if errors.Is(err, ErrNotFound) {
		if err := tx.Commit(); err != nil {
			return Job{}, err
		}
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}
	return j, nil
}

func (s *Store) RenewJob(id, token string, now time.Time, lease time.Duration) error {
	if token == "" || lease < time.Second {
		return ErrJobLeaseLost
	}
	res, err := s.db.Exec(`UPDATE ingestion_jobs SET lease_until = MIN(?, expires_at), updated_at = ?
		WHERE id = ? AND status = 'leased' AND lease_token = ? AND lease_until > ? AND expires_at > ?`,
		now.Add(lease).Unix(), now.Unix(), id, token, now.Unix(), now.Unix())
	return requireJobUpdate(res, err, ErrJobLeaseLost)
}

func (s *Store) BeginPublishJob(id, token, resultJSON string, now time.Time) error {
	if token == "" || !json.Valid([]byte(resultJSON)) {
		return ErrJobLeaseLost
	}
	res, err := s.db.Exec(`UPDATE ingestion_jobs SET status = 'publishing', result_json = ?, updated_at = ?
		WHERE id = ? AND status = 'leased' AND lease_token = ? AND lease_until > ? AND expires_at > ?`,
		resultJSON, now.Unix(), id, token, now.Unix(), now.Unix())
	return requireJobUpdate(res, err, ErrJobLeaseLost)
}

func (s *Store) FinishPublishJob(id string, now time.Time) error {
	return s.finishPublishJob(id, nil, now)
}

func (s *Store) FinishPublishJobWithResult(id, resultJSON string, now time.Time) error {
	if !json.Valid([]byte(resultJSON)) {
		return errors.New("invalid ingestion job result")
	}
	return s.finishPublishJob(id, &resultJSON, now)
}

func (s *Store) finishPublishJob(id string, resultJSON *string, now time.Time) error {
	res, err := s.db.Exec(`UPDATE ingestion_jobs SET status = 'done', worker_id = '', lease_token = '',
		lease_until = 0, result_json = COALESCE(?, result_json), updated_at = ?
		WHERE id = ? AND status = 'publishing'`, resultJSON, now.Unix(), id)
	if err := requireJobUpdate(res, err, ErrJobNotReady); err != nil {
		if errors.Is(err, ErrJobNotReady) {
			j, gerr := s.GetJob(id)
			if gerr == nil && j.Status == JobDone {
				return nil
			}
		}
		return err
	}
	return nil
}

func (s *Store) FailPublishingJob(id, message string, now time.Time) error {
	res, err := s.db.Exec(`UPDATE ingestion_jobs SET status = 'failed', error = ?, worker_id = '',
		lease_token = '', lease_until = 0, updated_at = ? WHERE id = ? AND status = 'publishing'`,
		message, now.Unix(), id)
	return requireJobUpdate(res, err, ErrJobNotReady)
}

func (s *Store) FailJobAttempt(id, token, message string, retry bool, now time.Time, maxAttempts int) error {
	if token == "" {
		return ErrJobLeaseLost
	}
	status := JobFailed
	if retry {
		status = JobQueued
	}
	res, err := s.db.Exec(`UPDATE ingestion_jobs SET status = CASE WHEN attempts < ? THEN ? ELSE 'failed' END,
		error = ?, worker_id = '', lease_token = '', lease_until = 0, updated_at = ?
		WHERE id = ? AND status = 'leased' AND lease_token = ? AND lease_until > ? AND expires_at > ?`,
		maxAttempts, status, message, now.Unix(), id, token, now.Unix(), now.Unix())
	return requireJobUpdate(res, err, ErrJobLeaseLost)
}

func (s *Store) CancelJob(id, message string, now time.Time) error {
	res, err := s.db.Exec(`UPDATE ingestion_jobs SET status = 'failed', error = ?, worker_id = '',
		lease_token = '', lease_until = 0, updated_at = ?
		WHERE id = ? AND status IN ('uploading', 'queued', 'leased')`, message, now.Unix(), id)
	return requireJobUpdate(res, err, ErrJobNotReady)
}

func requireJobUpdate(res sql.Result, err, missing error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return missing
	}
	return nil
}

func (s *Store) ListPublishingJobs() ([]Job, error) {
	rows, err := s.db.Query(`SELECT ` + jobColumns + ` FROM ingestion_jobs WHERE status = 'publishing' ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *Store) DeleteTerminalJobs(before time.Time, limit int) (int64, error) {
	if limit < 1 {
		return 0, errors.New("job cleanup limit must be positive")
	}
	res, err := s.db.Exec(`DELETE FROM ingestion_jobs WHERE id IN
		(SELECT id FROM ingestion_jobs WHERE status IN ('done','failed') AND updated_at < ? ORDER BY updated_at LIMIT ?)`, before.Unix(), limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) JobStats(now time.Time) (JobStats, error) {
	var stats JobStats
	var oldest sql.NullInt64
	err := s.db.QueryRow(`SELECT
		COUNT(*) FILTER (WHERE status = 'uploading' AND expires_at > ? AND created_at > ?),
		COUNT(*) FILTER (WHERE expires_at > ? AND (status = 'queued' OR (status = 'leased' AND lease_until <= ?))),
		COUNT(*) FILTER (WHERE status = 'leased' AND lease_until > ? AND expires_at > ?),
		COUNT(*) FILTER (WHERE status = 'publishing'),
		MIN(CASE WHEN expires_at > ? AND (status = 'queued' OR (status = 'leased' AND lease_until <= ?)) THEN created_at END)
		FROM ingestion_jobs`, now.Unix(), now.Add(-UploadReservationTimeout).Unix(), now.Unix(), now.Unix(), now.Unix(), now.Unix(), now.Unix(), now.Unix()).
		Scan(&stats.Uploading, &stats.Queued, &stats.Active, &stats.Publishing, &oldest)
	if err != nil {
		return JobStats{}, err
	}
	if oldest.Valid {
		stats.OldestQueuedAgeSeconds = max(0, now.Unix()-oldest.Int64)
	}
	return stats, nil
}

func (s *Store) IsJobNewer(id, thanID string) (bool, error) {
	var newer bool
	err := s.db.QueryRow(`SELECT EXISTS (
  SELECT 1 FROM ingestion_jobs current JOIN ingestion_jobs prior ON current.repo_id = prior.repo_id
  WHERE current.id = ? AND prior.id = ? AND current.rowid > prior.rowid
 )`, id, thanID).Scan(&newer)
	return newer, err
}
