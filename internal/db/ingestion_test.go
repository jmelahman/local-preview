package db

import (
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

var jobTestNow = time.Unix(1_800_000_000, 0).UTC()

func jobTestRepo(t *testing.T, s *Store) Repo {
	t.Helper()
	r, err := s.CreateRepo("demo", "/src", "/bare", RepoReady)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func jobTestSpec(repoID int64, id, kind string) JobSpec {
	return JobSpec{
		ID: id, RepoID: repoID, RequestKey: "request-" + id,
		Fingerprint: "fingerprint-" + id, Kind: kind, SpecJSON: `{"repo":"demo"}`,
		ArchiveKey: "staging/" + id, ArchiveSHA256: "digest", ArchiveBytes: 42,
		ExpiresAt: jobTestNow.Add(time.Hour).Unix(),
	}
}

func TestReserveJobConcurrentAdmission(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	const capacity = 3
	results := make(chan error, 30)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			<-start
			_, created, err := s.ReserveJob(jobTestSpec(repo.ID, strconv.Itoa(i), JobUpload), capacity, jobTestNow)
			if err == nil && !created {
				err = errors.New("unique request was not created")
			}
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	admitted := 0
	for err := range results {
		if err == nil {
			admitted++
		} else if !errors.Is(err, ErrJobFull) {
			t.Fatal(err)
		}
	}
	if admitted != capacity {
		t.Fatalf("admitted %d jobs, want %d", admitted, capacity)
	}
	stats, err := s.JobStats(jobTestNow)
	if err != nil || stats.Uploading != capacity {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
}

func TestReserveJobIdempotency(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	spec := jobTestSpec(repo.ID, "first", JobUpload)
	first, created, err := s.ReserveJob(spec, 1, jobTestNow)
	if err != nil || !created {
		t.Fatalf("reserve = %+v, %v, %v", first, created, err)
	}
	spec.ID = "retry-id"
	second, created, err := s.ReserveJob(spec, 1, jobTestNow)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("retry = %+v, %v, %v", second, created, err)
	}
	lookup, err := s.GetJobByRequest(repo.ID, spec.RequestKey)
	if err != nil || lookup.ID != first.ID {
		t.Fatalf("lookup = %+v, %v", lookup, err)
	}
	spec.Fingerprint = "different-inputs"
	if _, _, err := s.ReserveJob(spec, 1, jobTestNow); !errors.Is(err, ErrJobConflict) {
		t.Fatalf("changed request inputs: %v", err)
	}
}

func TestJobConcurrentClaimsAreExclusive(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "job", JobBuild), 1, jobTestNow); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 20)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			_, err := s.ClaimJob("worker-"+strconv.Itoa(i), "token-"+strconv.Itoa(i), jobTestNow, time.Minute, 3)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	claimed := 0
	for err := range results {
		if err == nil {
			claimed++
		} else if !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	if claimed != 1 {
		t.Fatalf("%d workers claimed one job", claimed)
	}
}

func TestJobLeaseRecoveryFencesStaleWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := jobTestRepo(t, s)
	if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "job", JobBuild), 2, jobTestNow); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimJob("worker-a", "lease-a", jobTestNow, time.Minute, 3)
	if err != nil || first.Attempts != 1 {
		t.Fatalf("first lease = %+v, %v", first, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	later := jobTestNow.Add(time.Minute)
	if err := s.RenewJob(first.ID, "lease-a", later, time.Minute); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("expired lease renewal: %v", err)
	}
	second, err := s.ClaimJob("worker-b", "lease-b", later, time.Minute, 3)
	if err != nil || second.ID != first.ID || second.Attempts != 2 {
		t.Fatalf("reclaimed lease = %+v, %v", second, err)
	}
	if err := s.BeginPublishJob(first.ID, "lease-a", `{"hash":"stale"}`, later); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("stale completion accepted: %v", err)
	}
	if err := s.FailJobAttempt(first.ID, "lease-a", "stale failure", false, later, 3); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("stale failure accepted: %v", err)
	}
	if err := s.RenewJob(first.ID, "lease-b", later.Add(30*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginPublishJob(first.ID, "lease-b", `{"hash":"current"}`, later.Add(45*time.Second)); err != nil {
		t.Fatal(err)
	}
	j, err := s.GetJob(first.ID)
	if err != nil || j.ResultJSON != `{"hash":"current"}` || j.Status != JobPublishing {
		t.Fatalf("winning completion = %+v, %v", j, err)
	}
}

func TestPublishingSurvivesRestartAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := jobTestRepo(t, s)
	if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "job", JobBuild), 1, jobTestNow); err != nil {
		t.Fatal(err)
	}
	j, err := s.ClaimJob("worker", "lease", jobTestNow, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BeginPublishJob(j.ID, "lease", `{"object":"attempt-output"}`, jobTestNow); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	later := jobTestNow.Add(2 * time.Hour)
	if _, err := s.ClaimJob("replacement", "new-lease", later, time.Minute, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("publishing job was reclaimed: %v", err)
	}
	jobs, err := s.ListPublishingJobs()
	if err != nil || len(jobs) != 1 || jobs[0].ResultJSON != `{"object":"attempt-output"}` {
		t.Fatalf("publishing recovery = %+v, %v", jobs, err)
	}
	if err := s.CancelJob(j.ID, "cancel too late", later); !errors.Is(err, ErrJobNotReady) {
		t.Fatalf("publishing cancellation: %v", err)
	}
	if err := s.FinishPublishJob(j.ID, later); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishPublishJob(j.ID, later); err != nil {
		t.Fatalf("idempotent finish: %v", err)
	}
	j, err = s.GetJob(j.ID)
	if err != nil || j.Status != JobDone || j.LeaseToken != "" {
		t.Fatalf("completed job = %+v, %v", j, err)
	}
	if retried, err := s.QueueJob(j.ID, later); err != nil || retried.Status != JobDone {
		t.Fatalf("submission retry after completion = %+v, %v", retried, err)
	}
}

func TestJobRetryCancellationAndExpiry(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "retry", JobBuild), 1, jobTestNow); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimJob("worker", "first", jobTestNow, time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FailJobAttempt(first.ID, "first", "temporary", true, jobTestNow, 2); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimJob("worker", "second", jobTestNow, time.Minute, 2)
	if err != nil || second.Attempts != 2 {
		t.Fatalf("retry = %+v, %v", second, err)
	}
	if err := s.FailJobAttempt(second.ID, "second", "failed again", true, jobTestNow, 2); err != nil {
		t.Fatal(err)
	}
	j, err := s.GetJob(first.ID)
	if err != nil || j.Status != JobFailed || j.Error != "failed again" {
		t.Fatalf("exhausted retries = %+v, %v", j, err)
	}
	if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "cancel", JobBuild), 1, jobTestNow); err != nil {
		t.Fatal(err)
	}
	j, err = s.ClaimJob("worker", "cancel-token", jobTestNow, time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelJob(j.ID, "superseded", jobTestNow); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginPublishJob(j.ID, "cancel-token", `{}`, jobTestNow); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("cancelled job completed: %v", err)
	}
	spec := jobTestSpec(repo.ID, "expire", JobUpload)
	spec.ExpiresAt = jobTestNow.Add(time.Second).Unix()
	if _, _, err := s.ReserveJob(spec, 1, jobTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueJob(spec.ID, jobTestNow.Add(time.Second)); !errors.Is(err, ErrJobNotReady) {
		t.Fatalf("expired archive submitted: %v", err)
	}
	if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "after-expiry", JobBuild), 1, jobTestNow.Add(time.Second)); err != nil {
		t.Fatalf("expired reservation held capacity: %v", err)
	}
	j, err = s.GetJob(spec.ID)
	if err != nil || j.Status != JobFailed || j.Error != "job expired" {
		t.Fatalf("expired job = %+v, %v", j, err)
	}
}

func TestPublishingCanFailOrRecordNormalizedResult(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			s := newTestStore(t)
			repo := jobTestRepo(t, s)
			if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "publish", JobBuild), 1, jobTestNow); err != nil {
				t.Fatal(err)
			}
			j, err := s.ClaimJob("worker", "lease", jobTestNow, time.Minute, 2)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.BeginPublishJob(j.ID, "lease", `{"published":false}`, jobTestNow); err != nil {
				t.Fatal(err)
			}
			if err := s.FailJobAttempt(j.ID, "lease", "worker cannot fail publication", false, jobTestNow, 2); !errors.Is(err, ErrJobLeaseLost) {
				t.Fatalf("worker changed publishing job: %v", err)
			}
			if fail {
				if err := s.FailPublishingJob(j.ID, "promotion failed", jobTestNow); err != nil {
					t.Fatal(err)
				}
				if err := s.FinishPublishJobWithResult(j.ID, `{"published":true}`, jobTestNow); !errors.Is(err, ErrJobNotReady) {
					t.Fatalf("failed publication completed: %v", err)
				}
			} else {
				if err := s.FinishPublishJobWithResult(j.ID, `{"published":true}`, jobTestNow); err != nil {
					t.Fatal(err)
				}
				if err := s.FinishPublishJobWithResult(j.ID, `{"published":false}`, jobTestNow); err != nil {
					t.Fatalf("repeated completion failed: %v", err)
				}
			}
			j, err = s.GetJob(j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if j.Status != JobFailed || j.Error != "promotion failed" {
					t.Fatalf("failed publication = %+v", j)
				}
			} else if j.Status != JobDone || j.ResultJSON != `{"published":true}` {
				t.Fatalf("completed publication = %+v", j)
			}
			if j.LeaseToken != "" || j.LeaseUntil != 0 || j.WorkerID != "" {
				t.Fatalf("terminal job retains lease: %+v", j)
			}
		})
	}
}

func TestJobUploadSubmissionAndStats(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "upload", JobUpload), 2, jobTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob("worker", "token", jobTestNow, time.Minute, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unsubmitted archive claimed: %v", err)
	}
	for range 2 {
		if _, err := s.QueueJob("upload", jobTestNow.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := s.JobStats(jobTestNow.Add(10 * time.Second))
	if err != nil || stats.Queued != 1 || stats.OldestQueuedAgeSeconds != 10 || stats.Uploading != 0 {
		t.Fatalf("queued stats = %+v, %v", stats, err)
	}
	if _, err := s.ClaimJob("worker", "token", jobTestNow.Add(10*time.Second), time.Minute, 3); err != nil {
		t.Fatal(err)
	}
	stats, err = s.JobStats(jobTestNow.Add(20 * time.Second))
	if err != nil || stats.Active != 1 || stats.Queued != 0 {
		t.Fatalf("active stats = %+v, %v", stats, err)
	}
	stats, err = s.JobStats(jobTestNow.Add(70 * time.Second))
	if err != nil || stats.Active != 0 || stats.Queued != 1 {
		t.Fatalf("expired lease stats = %+v, %v", stats, err)
	}
	if _, err := s.ClaimJob("worker", "token-2", jobTestNow.Add(70*time.Second), time.Minute, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exhausted lease reclaimed: %v", err)
	}
	j, err := s.GetJob("upload")
	if err != nil || j.Status != JobFailed || j.Error != "attempt limit reached" {
		t.Fatalf("expired final attempt = %+v, %v", j, err)
	}
}

func TestDeleteTerminalJobsKeepsActiveJobsAndHonorsCutoff(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	cutoff := jobTestNow.Add(10 * time.Second)
	for _, id := range []string{"done-old", "failed-old", "publishing-old", "done-cutoff", "queued-old", "uploading-old"} {
		kind := JobBuild
		if id == "uploading-old" {
			kind = JobUpload
		}
		if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, id, kind), 10, jobTestNow); err != nil {
			t.Fatal(err)
		}
		if id == "failed-old" {
			if err := s.CancelJob(id, "test failure", jobTestNow); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if id == "queued-old" || id == "uploading-old" {
			continue
		}
		j, err := s.ClaimJob("worker", "token-"+id, jobTestNow, time.Minute, 3)
		if err != nil || j.ID != id {
			t.Fatalf("claim %s = %+v, %v", id, j, err)
		}
		if err := s.BeginPublishJob(id, j.LeaseToken, `{}`, jobTestNow); err != nil {
			t.Fatal(err)
		}
		if id == "publishing-old" {
			continue
		}
		finished := jobTestNow
		if id == "done-cutoff" {
			finished = cutoff
		}
		if err := s.FinishPublishJob(id, finished); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DeleteTerminalJobs(cutoff, 0); err == nil {
		t.Fatal("invalid cleanup limit accepted")
	}
	for _, limit := range []int{1, 10} {
		deleted, err := s.DeleteTerminalJobs(cutoff, limit)
		if err != nil || deleted != 1 {
			t.Fatalf("cleanup limit %d deleted %d, %v", limit, deleted, err)
		}
	}
	for _, id := range []string{"done-old", "failed-old"} {
		if _, err := s.GetJob(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old terminal job %s retained: %v", id, err)
		}
	}
	if deleted, err := s.DeleteTerminalJobs(cutoff, 10); err != nil || deleted != 0 {
		t.Fatalf("cutoff was inclusive: %d, %v", deleted, err)
	}
	if deleted, err := s.DeleteTerminalJobs(cutoff.Add(time.Hour), 10); err != nil || deleted != 1 {
		t.Fatalf("later cleanup = %d, %v", deleted, err)
	}
	for _, id := range []string{"publishing-old", "queued-old", "uploading-old"} {
		if _, err := s.GetJob(id); err != nil {
			t.Fatalf("cleanup deleted active job %s: %v", id, err)
		}
	}
}

func TestJobOrderUsesDurableInsertionOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := jobTestRepo(t, s)
	for _, id := range []string{"z-created-first", "a-created-second"} {
		if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, id, JobBuild), 3, jobTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tt := range []struct {
		id, than string
		want     bool
	}{
		{"a-created-second", "z-created-first", true},
		{"z-created-first", "a-created-second", false},
		{"a-created-second", "a-created-second", false},
		{"missing", "a-created-second", false},
	} {
		got, err := s.IsJobNewer(tt.id, tt.than)
		if err != nil || got != tt.want {
			t.Fatalf("newer(%s,%s)=%v, %v", tt.id, tt.than, got, err)
		}
	}
}
