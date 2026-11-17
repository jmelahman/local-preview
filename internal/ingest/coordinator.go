package ingest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/s3store"
)

var ErrValidation = errors.New("invalid ingestion request")

const (
	LeaseDuration   = time.Minute
	JobTimeout      = 25 * time.Minute
	MaxAttempts     = 3
	MaxMessageBytes = 4 << 20
)

type Request struct {
	RequestKey    string `json:"request_key"`
	Ref           string `json:"ref"`
	Side          string `json:"side"`
	Name          string `json:"name,omitempty"`
	Overwrite     bool   `json:"overwrite"`
	Deploy        bool   `json:"deploy"`
	ArchiveSize   int64  `json:"archive_size"`
	ArchiveSHA256 string `json:"archive_sha256"`
}

type Snapshot struct {
	ParentID  string              `json:"parent_id,omitempty"`
	Execution build.ExecutionSpec `json:"execution"`
	CreatedBy string              `json:"created_by"`
}

type Completion struct {
	Execution build.ExecutionResult `json:"execution"`
	Outputs   []s3store.Output      `json:"outputs"`
}

type Result struct {
	Upload *build.UploadResult
	Deploy *db.DeployRow
}

type Coordinator struct {
	Store            *db.Store
	Tier             *s3store.Tier
	Queue            *build.Queue
	ManifestRefs     []build.ManifestRef
	LocalManifestDir string
	MaxPending       int
	MaxUploadBytes   int64
	Report           func(context.Context, db.JobStats) error
}

func (c *Coordinator) snapshot(repo db.Repo, ref, createdBy string) (Snapshot, error) {
	snapshot := Snapshot{Execution: build.ExecutionSpec{RepoName: repo.Name, Source: repo.Source, ProjectDir: repo.ProjectDir, Ref: ref, ManifestRefs: c.ManifestRefs}, CreatedBy: createdBy}
	if c.LocalManifestDir != "" {
		file, err := os.Open(filepath.Join(c.LocalManifestDir, repo.Name+".toml"))
		if err == nil {
			snapshot.Execution.LocalManifest, err = io.ReadAll(io.LimitReader(file, (1<<20)+1))
			file.Close()
			if err != nil || len(snapshot.Execution.LocalManifest) > 1<<20 {
				return Snapshot{}, errors.New("local manifest exceeds the 1 MiB limit or cannot be read")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, err
		}
	}
	return snapshot, nil
}

func (c *Coordinator) ReserveBuild(ctx context.Context, repoName, ref string, rebuild bool, createdBy string) (db.Job, error) {
	if ref == "" || len(ref) > 2048 {
		return db.Job{}, fmt.Errorf("%w: a ref is required", ErrValidation)
	}
	repo, err := c.Store.GetRepoByName(repoName)
	if err != nil {
		return db.Job{}, err
	}
	snapshot, err := c.snapshot(repo, ref, createdBy)
	if err != nil {
		return db.Job{}, err
	}
	snapshot.Execution.Deploy = true
	snapshot.Execution.Rebuild = rebuild
	data, err := json.Marshal(snapshot)
	if err != nil {
		return db.Job{}, err
	}
	fingerprint := sha256.Sum256(data)
	id, err := randomID()
	if err != nil {
		return db.Job{}, err
	}
	key := "build:" + id
	if hexString(ref, 40) && !rebuild {
		key = "build:" + hex.EncodeToString(fingerprint[:])
		if prior, err := c.Store.GetJobByRequest(repo.ID, key); err == nil {
			if prior.Status == db.JobDone {
				if deploy, err := c.Store.GetDeployBySHA(repo.ID, ref); err == nil && deploy.Status == db.DeployReady && deploy.ProjectDir == repo.ProjectDir {
					return prior, nil
				}
			} else if prior.Status != db.JobFailed && (prior.Status == db.JobPublishing || prior.ExpiresAt > time.Now().Unix()) {
				return prior, nil
			}
			key += "-" + id
		} else if !errors.Is(err, db.ErrNotFound) {
			return db.Job{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return db.Job{}, err
	}
	now := time.Now()
	job, _, err := c.Store.ReserveJob(db.JobSpec{ID: id, RepoID: repo.ID, RequestKey: key, Fingerprint: hex.EncodeToString(fingerprint[:]), Kind: db.JobBuild, SpecJSON: string(data), ExpiresAt: now.Add(JobTimeout).Unix()}, c.MaxPending, now)
	return job, err
}

func (c *Coordinator) Reserve(ctx context.Context, repoName string, req Request, createdBy string) (db.Job, s3store.StagedUpload, error) {
	if len(req.RequestKey) < 16 || len(req.RequestKey) > 128 || req.Ref == "" || len(req.Ref) > 2048 {
		return db.Job{}, s3store.StagedUpload{}, fmt.Errorf("%w: request key and ref are required", ErrValidation)
	}
	if req.Side != build.SideFrontend && req.Side != build.SideBackend && req.Side != build.SideArtifact {
		return db.Job{}, s3store.StagedUpload{}, fmt.Errorf("%w: invalid upload side", ErrValidation)
	}
	if (req.Side == build.SideArtifact && req.Name == "") || len(req.Name) > 128 || (req.Side != build.SideArtifact && req.Name != "") {
		return db.Job{}, s3store.StagedUpload{}, fmt.Errorf("%w: invalid artifact name", ErrValidation)
	}
	if req.ArchiveSize <= 0 || (c.MaxUploadBytes > 0 && req.ArchiveSize > c.MaxUploadBytes) || !hexString(req.ArchiveSHA256, 64) {
		return db.Job{}, s3store.StagedUpload{}, fmt.Errorf("%w: invalid archive size or SHA-256", ErrValidation)
	}
	repo, err := c.Store.GetRepoByName(repoName)
	if err != nil {
		return db.Job{}, s3store.StagedUpload{}, err
	}
	snapshot, err := c.snapshot(repo, req.Ref, createdBy)
	if err != nil {
		return db.Job{}, s3store.StagedUpload{}, err
	}
	snapshot.Execution.Side = req.Side
	snapshot.Execution.Name = req.Name
	snapshot.Execution.Overwrite = req.Overwrite
	snapshot.Execution.Deploy = req.Deploy
	data, err := json.Marshal(snapshot)
	if err != nil {
		return db.Job{}, s3store.StagedUpload{}, err
	}
	fingerprintData, _ := json.Marshal(struct {
		Request  Request
		Snapshot Snapshot
	}{req, snapshot})
	fingerprint := sha256.Sum256(fingerprintData)
	id, err := randomID()
	if err != nil {
		return db.Job{}, s3store.StagedUpload{}, err
	}
	now := time.Now()
	job, _, err := c.Store.ReserveJob(db.JobSpec{ID: id, RepoID: repo.ID, RequestKey: req.RequestKey, Fingerprint: hex.EncodeToString(fingerprint[:]), Kind: db.JobUpload, SpecJSON: string(data), ArchiveKey: s3store.StagingPrefix + id + "/input.tar.gz", ArchiveSHA256: req.ArchiveSHA256, ArchiveBytes: req.ArchiveSize, ExpiresAt: now.Add(JobTimeout).Unix()}, c.MaxPending, now)
	if err != nil {
		return db.Job{}, s3store.StagedUpload{}, err
	}
	var signed s3store.StagedUpload
	if job.Status == db.JobUploading {
		signed, err = c.Tier.PresignUpload(ctx, job.ID, job.ArchiveBytes, job.ArchiveSHA256, 10*time.Minute)
	}
	return job, signed, err
}

func (c *Coordinator) Submit(ctx context.Context, id string) (db.Job, error) {
	job, err := c.Store.GetJob(id)
	if err != nil || job.Status != db.JobUploading {
		return job, err
	}
	object, err := c.Tier.StatUpload(ctx, id)
	if err != nil {
		return db.Job{}, fmt.Errorf("%w: staged archive is not available", ErrValidation)
	}
	if object.Size != job.ArchiveBytes || object.SHA256 != job.ArchiveSHA256 || object.Key != job.ArchiveKey {
		return db.Job{}, fmt.Errorf("%w: staged archive does not match its reservation", ErrValidation)
	}
	return c.Store.QueueJob(id, time.Now())
}

func (c *Coordinator) Get(id string) (db.Job, error) {
	job, err := c.Store.GetJob(id)
	if err == nil && job.ExpiresAt <= time.Now().Unix() && (job.Status == db.JobUploading || job.Status == db.JobQueued || job.Status == db.JobLeased) {
		if err := c.Store.CancelJob(id, "job expired", time.Now()); err != nil {
			return db.Job{}, err
		}
		return c.Store.GetJob(id)
	}
	return job, err
}

func (c *Coordinator) Cancel(id string) error {
	return c.Store.CancelJob(id, "cancelled by caller", time.Now())
}

func (c *Coordinator) Result(_ context.Context, job db.Job) (Result, error) {
	var completion Completion
	if err := json.Unmarshal([]byte(job.ResultJSON), &completion); err != nil {
		return Result{}, err
	}
	result := Result{Upload: completion.Execution.Upload}
	if completion.Execution.Deploy != nil {
		deploy, err := c.Store.GetDeployBySHA(job.RepoID, completion.Execution.Deploy.SHA)
		if err != nil {
			return Result{}, err
		}
		row, err := c.Store.GetDeployByID(deploy.ID)
		if err != nil {
			return Result{}, err
		}
		result.Deploy = &row
	}
	return result, nil
}

func (c *Coordinator) Complete(id, token string, completion Completion) error {
	job, err := c.Store.GetJob(id)
	if err != nil {
		return err
	}
	if err := validateCompletion(job, token, completion); err != nil {
		return err
	}
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(job.SpecJSON), &snapshot); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	present := make(map[string]bool)
	for _, output := range completion.Outputs {
		present[output.Side+":"+output.Hash] = true
	}
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for identity := range resultArtifacts(completion.Execution) {
		if present[identity] {
			continue
		}
		side, hash, _ := strings.Cut(identity, ":")
		group.Go(func() error {
			_, found, err := c.Tier.Stat(ctx, snapshot.Execution.RepoName, side, hash)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: worker did not provide a required artifact", ErrValidation)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	data, err := json.Marshal(completion)
	if err != nil {
		return err
	}
	return c.Store.BeginPublishJob(id, token, string(data), time.Now())
}

func (c *Coordinator) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	if c.Report != nil {
		go c.reportMetrics(ctx)
	}
	lastRepair := time.Time{}
	lastCleanup := time.Time{}
	for {
		jobs, err := c.Store.ListPublishingJobs()
		if err != nil {
			log.Printf("ingestion: list publishing jobs: %v", err)
		}
		for _, job := range jobs {
			publishCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			err := c.publish(publishCtx, job)
			cancel()
			if err != nil {
				log.Printf("ingestion: publish job %s: %v", job.ID, err)
				if time.Since(time.Unix(job.UpdatedAt, 0)) > 10*time.Minute {
					if failErr := c.Store.FailPublishingJob(job.ID, "artifact publication could not finish", time.Now()); failErr != nil {
						log.Printf("ingestion: fail publication %s: %v", job.ID, failErr)
					}
				}
			}
		}
		if time.Since(lastRepair) >= 30*time.Second {
			if ids, err := c.Store.ListUnfinishedDeployIDs(); err == nil {
				for _, id := range ids {
					row, err := c.Store.GetDeployByID(id)
					if err != nil || row.Status != db.DeployReady {
						continue
					}
					if err := c.ScheduleArtifactsForDeploy(ctx, row); err != nil {
						log.Printf("ingestion: resume artifacts for deploy %d: %v", id, err)
						break
					}
				}
			}
			lastRepair = time.Now()
		}
		if time.Since(lastCleanup) >= time.Minute {
			if _, err := c.Store.DeleteTerminalJobs(time.Now().Add(-7*24*time.Hour), 100); err != nil {
				log.Printf("ingestion: remove old jobs: %v", err)
			}
			lastCleanup = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Coordinator) reportMetrics(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if stats, err := c.Store.JobStats(time.Now()); err == nil {
			reportCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := c.Report(reportCtx, stats); err != nil {
				log.Printf("ingestion: report queue: %v", err)
			}
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Coordinator) publish(ctx context.Context, job db.Job) error {
	var completion Completion
	if err := json.Unmarshal([]byte(job.ResultJSON), &completion); err != nil {
		return err
	}
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(job.SpecJSON), &snapshot); err != nil {
		return err
	}
	repo, err := c.Store.GetRepoByName(snapshot.Execution.RepoName)
	if err != nil {
		return err
	}
	if repo.ID != job.RepoID || repo.Source != snapshot.Execution.Source || repo.ProjectDir != snapshot.Execution.ProjectDir {
		return c.Store.FailPublishingJob(job.ID, "repository configuration changed; submit a new job", time.Now())
	}
	obsolete, err := c.publicationSuperseded(job, snapshot, completion.Execution)
	if err != nil {
		return err
	}
	if obsolete {
		if completion.Execution.Upload != nil {
			completion.Execution.Upload.Published = false
		}
		data, err := json.Marshal(completion)
		if err != nil {
			return err
		}
		return c.Store.FinishPublishJobWithResult(job.ID, string(data), time.Now())
	}
	for _, output := range completion.Outputs {
		overwrite := snapshot.Execution.Rebuild
		if upload := completion.Execution.Upload; upload != nil {
			side := map[string]string{build.SideFrontend: "fe", build.SideBackend: "be", build.SideArtifact: "dl"}[upload.Side]
			overwrite = overwrite || (snapshot.Execution.Overwrite && output.Side == side && output.Hash == upload.Hash)
		}
		published, err := c.Tier.Promote(ctx, output, overwrite)
		if err != nil {
			return err
		}
		if upload := completion.Execution.Upload; upload != nil && output.Hash == upload.Hash {
			upload.Published = published
		}
	}
	if deployed := completion.Execution.Deploy; deployed != nil {
		deployed.JobID = job.ID
		deployed.Rebuild = snapshot.Execution.Rebuild
		row, err := c.Queue.CompleteExecution(ctx, repo, *deployed, snapshot.CreatedBy)
		if err != nil {
			return err
		}
		if err := c.scheduleArtifacts(snapshot, row); err != nil {
			if !errors.Is(err, db.ErrJobFull) {
				return err
			}
			log.Printf("ingestion: deferred artifact scheduling for deploy %d: queue full", row.ID)
		}
	}
	if artifacts := completion.Execution.Artifacts; artifacts != nil {
		if err := c.Queue.CompleteArtifactExecution(repo, *artifacts); err != nil {
			return err
		}
	}
	data, err := json.Marshal(completion)
	if err != nil {
		return err
	}
	if err := c.Store.FinishPublishJobWithResult(job.ID, string(data), time.Now()); err != nil {
		return err
	}
	if err := c.Tier.DeleteUpload(ctx, job.ID); err != nil {
		log.Printf("ingestion: remove input %s: %v", job.ID, err)
	}
	return nil
}

func (c *Coordinator) publicationSuperseded(job db.Job, snapshot Snapshot, result build.ExecutionResult) (bool, error) {
	sha := snapshot.Execution.Ref
	if !snapshot.Execution.ArtifactsOnly {
		if result.Deploy == nil {
			return false, nil
		}
		sha = result.Deploy.SHA
	}
	current, err := c.Store.GetDeployBySHA(job.RepoID, sha)
	if errors.Is(err, db.ErrNotFound) {
		return snapshot.Execution.ArtifactsOnly, nil
	}
	if err != nil {
		return false, err
	}
	if snapshot.Execution.ArtifactsOnly {
		return current.Status != db.DeployReady || current.IngestionJobID != snapshot.ParentID, nil
	}
	return c.Store.IsJobNewer(current.IngestionJobID, job.ID)
}

func validateCompletion(job db.Job, token string, completion Completion) error {
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(job.SpecJSON), &snapshot); err != nil {
		return err
	}
	if token == "" || token != job.LeaseToken || job.Status != db.JobLeased {
		return db.ErrJobLeaseLost
	}
	if len(completion.Outputs) > 130 || !validUploadResult(snapshot.Execution, completion.Execution.Upload) ||
		!validDeployResult(snapshot.Execution, completion.Execution) || !validArtifactResult(snapshot.Execution, completion.Execution.Artifacts) {
		return fmt.Errorf("%w: invalid worker result", ErrValidation)
	}
	artifacts := resultArtifacts(completion.Execution)
	for _, output := range completion.Outputs {
		if output.Repo != snapshot.Execution.RepoName || !hexString(output.Hash, 64) ||
			!strings.HasPrefix(output.Key, s3store.StagingPrefix+job.ID+"/"+token+"/") || !artifacts[output.Side+":"+output.Hash] {
			return fmt.Errorf("%w: invalid worker output", ErrValidation)
		}
	}
	return nil
}

func validUploadResult(spec build.ExecutionSpec, upload *build.UploadResult) bool {
	if (spec.Side != "") != (upload != nil) {
		return false
	}
	if upload == nil {
		return true
	}
	return hexString(upload.SHA, 40) && hexString(upload.Hash, 64) && upload.Side == spec.Side && upload.Name == spec.Name &&
		(!hexString(spec.Ref, 40) || upload.SHA == spec.Ref)
}

func validArtifactResult(spec build.ExecutionSpec, artifacts *build.ExecutedArtifacts) bool {
	if spec.ArtifactsOnly != (artifacts != nil) {
		return false
	}
	if artifacts == nil {
		return true
	}
	if artifacts.SHA != spec.Ref || !hexString(artifacts.SHA, 40) || len(artifacts.Artifacts) > 128 ||
		len(artifacts.Artifacts) != len(spec.ExpectedArtifacts) {
		return false
	}
	for name, artifact := range artifacts.Artifacts {
		expected, ok := spec.ExpectedArtifacts[name]
		if !ok || expected.Hash != artifact.Hash || (artifact.Status != db.ArtifactReady && artifact.Status != db.ArtifactFailed) {
			return false
		}
	}
	total := 0
	for name, content := range artifacts.Logs {
		if _, ok := artifacts.Artifacts[name]; !ok {
			return false
		}
		total += len(content)
	}
	return total <= 512<<10
}

func validDeployResult(spec build.ExecutionSpec, result build.ExecutionResult) bool {
	deployed := result.Deploy
	if spec.Deploy != (deployed != nil) {
		return false
	}
	if deployed == nil {
		return true
	}
	if !hexString(deployed.SHA, 40) || (hexString(spec.Ref, 40) && deployed.SHA != spec.Ref) ||
		(result.Upload != nil && deployed.SHA != result.Upload.SHA) || deployed.ProjectDir != spec.ProjectDir ||
		len(deployed.Ancestors) > 1000 || len(deployed.Artifacts) > 128 {
		return false
	}
	for _, hash := range []string{deployed.FrontendHash, deployed.BackendHash} {
		if hash != "" && !hexString(hash, 64) {
			return false
		}
	}
	for _, sha := range deployed.Ancestors {
		if !hexString(sha, 40) {
			return false
		}
	}
	for _, artifact := range deployed.Artifacts {
		if !hexString(artifact.Hash, 64) {
			return false
		}
	}
	for kind, content := range deployed.Logs {
		if (kind != "fe" && kind != "be") || len(content) > 256<<10 {
			return false
		}
	}
	return true
}

func (c *Coordinator) ScheduleArtifactsForDeploy(ctx context.Context, row db.DeployRow) error {
	repo, err := c.Store.GetRepoByName(row.RepoName)
	if err != nil {
		return err
	}
	var snapshot Snapshot
	if row.IngestionJobID != "" {
		parent, err := c.Store.GetJob(row.IngestionJobID)
		if err != nil {
			return err
		}
		if parent.RepoID != row.RepoID {
			return errors.New("artifact parent belongs to another repository")
		}
		if err := json.Unmarshal([]byte(parent.SpecJSON), &snapshot); err != nil {
			return err
		}
	} else {
		snapshot, err = c.snapshot(repo, row.SHA, row.CreatedBy)
		if err != nil {
			return err
		}
	}
	snapshot.Execution.ProjectDir = row.ProjectDir
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.scheduleArtifacts(snapshot, row)
}

func (c *Coordinator) scheduleArtifacts(snapshot Snapshot, row db.DeployRow) error {
	pending := make(map[string]db.ArtifactRef)
	expected := make(map[string]db.ArtifactRef, len(row.Artifacts))
	for name, artifact := range row.Artifacts {
		expected[name] = db.ArtifactRef{Hash: artifact.Hash, Status: db.ArtifactBuilding}
		if artifact.Status == db.ArtifactBuilding {
			pending[name] = artifact
		}
	}
	if len(pending) == 0 {
		return nil
	}
	snapshot.ParentID = row.IngestionJobID
	snapshot.CreatedBy = ""
	snapshot.Execution.Overwrite = false
	snapshot.Execution.Ref = row.SHA
	snapshot.Execution.Side = ""
	snapshot.Execution.Name = ""
	snapshot.Execution.Deploy = false
	snapshot.Execution.ArtifactsOnly = true
	snapshot.Execution.ExpectedArtifacts = expected
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	id, err := randomID()
	if err != nil {
		return err
	}
	now := time.Now()
	job, _, err := c.Store.ReserveJob(db.JobSpec{ID: id, RepoID: row.RepoID, RequestKey: "artifacts:" + row.SHA + ":" + hex.EncodeToString(digest[:8]), Fingerprint: hex.EncodeToString(digest[:]), Kind: db.JobBuild, SpecJSON: string(data), ExpiresAt: now.Add(JobTimeout).Unix()}, c.MaxPending+1, now)
	if err == nil && job.Status == db.JobFailed {
		for name, artifact := range pending {
			artifact.Status = db.ArtifactFailed
			artifact.Error = job.Error
			pending[name] = artifact
		}
		repo, getErr := c.Store.GetRepoByName(row.RepoName)
		if getErr != nil {
			return getErr
		}
		return c.Queue.CompleteArtifactExecution(repo, build.ExecutedArtifacts{SHA: row.SHA, Artifacts: pending})
	}
	return err
}

func resultArtifacts(result build.ExecutionResult) map[string]bool {
	refs := make(map[string]bool)
	if upload := result.Upload; upload != nil {
		side := map[string]string{build.SideFrontend: "fe", build.SideBackend: "be", build.SideArtifact: "dl"}[upload.Side]
		refs[side+":"+upload.Hash] = true
	}
	if deploy := result.Deploy; deploy != nil {
		if deploy.FrontendHash != "" {
			refs["fe:"+deploy.FrontendHash] = true
		}
		if deploy.BackendHash != "" {
			refs["be:"+deploy.BackendHash] = true
		}
		for _, artifact := range deploy.Artifacts {
			if artifact.Status == db.ArtifactReady {
				refs["dl:"+artifact.Hash] = true
			}
		}
	}
	if result.Artifacts != nil {
		for _, artifact := range result.Artifacts.Artifacts {
			if artifact.Status == db.ArtifactReady {
				refs["dl:"+artifact.Hash] = true
			}
		}
	}
	return refs
}

func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func hexString(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
