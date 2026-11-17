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
			_, created, err := s.ReserveJob(jobTestSpec(repo.ID, strconv.Itoa(i), JobUpload), capacity*4, jobTestNow)
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
	if retried, err := s.QueueJob(j.ID, 1, later); err != nil || retried.Status != JobDone {
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
	if _, err := s.QueueJob(spec.ID, 1, jobTestNow.Add(time.Second)); !errors.Is(err, ErrJobNotReady) {
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
		if _, err := s.QueueJob("upload", 1, jobTestNow.Add(time.Second)); err != nil {
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
		if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, id, JobBuild), 8, jobTestNow); err != nil {
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

func TestReserveJobConcurrentRepositoryFairness(t *testing.T) {
	s := newTestStore(t)
	first := jobTestRepo(t, s)
	second, err := s.CreateRepo("other", "/other", "/other-bare", RepoReady)
	if err != nil {
		t.Fatal(err)
	}
	const global = 8
	admitted := make(chan int64, 60)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, repo := range []Repo{first, second} {
		for i := range 30 {
			wg.Go(func() {
				<-start
				_, created, err := s.ReserveJob(jobTestSpec(repo.ID, repo.Name+"-"+strconv.Itoa(i), JobUpload), global, jobTestNow)
				if err == nil && created {
					admitted <- repo.ID
				} else if !errors.Is(err, ErrJobFull) {
					t.Errorf("reserve: created=%v error=%v", created, err)
				}
			})
		}
	}
	close(start)
	wg.Wait()
	close(admitted)
	counts := map[int64]int{}
	for repoID := range admitted {
		counts[repoID]++
	}
	if counts[first.ID] != 2 || counts[second.ID] != 2 {
		t.Fatalf("repository admission counts=%v; want two each", counts)
	}
}

func TestUnsubmittedUploadExpiresOnlyAtHardDeadline(t *testing.T) {
	for _, age := range []time.Duration{25*time.Minute - time.Second, 25 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			s := newTestStore(t)
			repo := jobTestRepo(t, s)
			spec := jobTestSpec(repo.ID, "upload", JobUpload)
			spec.ExpiresAt = jobTestNow.Add(25 * time.Minute).Unix()
			if _, _, err := s.ReserveJob(spec, 1, jobTestNow); err != nil {
				t.Fatal(err)
			}
			job, err := s.QueueJob(spec.ID, 1, jobTestNow.Add(age))
			if age < 25*time.Minute {
				if err != nil || job.Status != JobQueued {
					t.Fatalf("valid late upload rejected: %+v error=%v", job, err)
				}
				return
			}
			if !errors.Is(err, ErrJobNotReady) {
				t.Fatalf("hard-expired upload submitted: %v", err)
			}
			old, err := s.GetJob(spec.ID)
			if err != nil || old.Status != JobFailed {
				t.Fatalf("hard-expired reservation=%+v error=%v", old, err)
			}
		})
	}
}

func TestContinuationHasOneExplicitAdmissionSlot(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	parent := jobTestSpec(repo.ID, "parent", JobBuild)
	if _, _, err := s.ReserveJob(parent, 1, jobTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob("worker", "token", jobTestNow, time.Minute, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginPublishJob("parent", "token", `{}`, jobTestNow); err != nil {
		t.Fatal(err)
	}
	child := jobTestSpec(repo.ID, "child", JobBuild)
	if _, _, err := s.ReserveJob(child, 1, jobTestNow); !errors.Is(err, ErrJobFull) {
		t.Fatalf("normal reservation bypassed limit: %v", err)
	}
	if _, created, err := s.ReserveContinuation(child, 1, jobTestNow); err != nil || !created {
		t.Fatalf("continuation admission: created=%v error=%v", created, err)
	}
	if _, _, err := s.ReserveContinuation(jobTestSpec(repo.ID, "extra-child", JobBuild), 1, jobTestNow); !errors.Is(err, ErrJobFull) {
		t.Fatalf("second extra slot admitted: %v", err)
	}
	if _, created, err := s.ReserveContinuation(child, 1, jobTestNow); err != nil || created {
		t.Fatalf("idempotent continuation: created=%v error=%v", created, err)
	}
	if err := s.FinishPublishJob("parent", jobTestNow); err != nil {
		t.Fatal(err)
	}
	stats, err := s.JobStats(jobTestNow)
	if err != nil || stats.Queued != 1 || stats.Publishing != 0 {
		t.Fatalf("completed parent state: %+v error=%v", stats, err)
	}
	if _, _, err := s.ReserveContinuation(jobTestSpec(repo.ID, "upload-not-child", JobUpload), 1, jobTestNow); err == nil {
		t.Fatal("upload accepted as internal continuation")
	}
}

func TestSubmittedUploadKeepsExecutionDeadline(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	spec := jobTestSpec(repo.ID, "upload", JobUpload)
	spec.ExpiresAt = jobTestNow.Add(25 * time.Minute).Unix()
	if _, _, err := s.ReserveJob(spec, 1, jobTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueJob(spec.ID, 1, jobTestNow.Add(9*time.Minute)); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob("worker", "token", jobTestNow.Add(10*time.Minute), time.Minute, 3)
	if err != nil || job.ID != spec.ID || job.ExpiresAt != spec.ExpiresAt {
		t.Fatalf("submitted upload deadline: %+v error=%v", job, err)
	}
	if _, err := s.ClaimJob("worker", "new-token", jobTestNow.Add(25*time.Minute), time.Minute, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job outlived execution deadline: %v", err)
	}
	job, err = s.GetJob(spec.ID)
	if err != nil || job.Status != JobFailed {
		t.Fatalf("expired execution: %+v error=%v", job, err)
	}
}

func TestUploadingExpiryReleasesRepositoryAdmission(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "abandoned", JobUpload), 1, jobTestNow); err != nil {
		t.Fatal(err)
	}
	replacement := jobTestSpec(repo.ID, "replacement", JobBuild)
	if _, _, err := s.ReserveJob(replacement, 1, jobTestNow.Add(UploadReservationTimeout-time.Second)); !errors.Is(err, ErrJobFull) {
		t.Fatalf("reservation expired early: %v", err)
	}
	if _, _, err := s.ReserveJob(replacement, 1, jobTestNow.Add(UploadReservationTimeout)); err != nil {
		t.Fatalf("expired reservation blocked admission: %v", err)
	}
	old, err := s.GetJob("abandoned")
	if err != nil || old.Status != JobUploading {
		t.Fatalf("abandoned reservation=%+v error=%v", old, err)
	}
}

func TestGlobalJobAdmissionStillBoundsDistinctRepositories(t *testing.T) {
	s := newTestStore(t)
	results := make(chan error, 4)
	var wg sync.WaitGroup
	for i := range 4 {
		name := "repo-" + strconv.Itoa(i)
		repo, err := s.CreateRepo(name, "/src", "/bare", RepoReady)
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			_, _, err := s.ReserveJob(jobTestSpec(repo.ID, name, JobUpload), 3, jobTestNow)
			results <- err
		})
	}
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
	if admitted != 3 {
		t.Fatalf("global admission=%d, want 3", admitted)
	}
}

func TestLateUploadSubmissionBeforeHardDeadline(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	spec := jobTestSpec(repo.ID, "upload", JobUpload)
	spec.ExpiresAt = jobTestNow.Add(25 * time.Minute).Unix()
	if _, _, err := s.ReserveJob(spec, 1, jobTestNow); err != nil {
		t.Fatal(err)
	}
	job, err := s.QueueJob(spec.ID, 1, jobTestNow.Add(11*time.Minute))
	if err != nil || job.Status != JobQueued {
		t.Fatalf("valid late upload rejected: %+v error=%v", job, err)
	}
}

func TestLateUploadMustReacquireRepositoryCapacity(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "late-upload", JobUpload), 1, jobTestNow); err != nil {
		t.Fatal(err)
	}
	later := jobTestNow.Add(11 * time.Minute)
	if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "occupant", JobBuild), 1, later); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueJob("late-upload", 1, later); !errors.Is(err, ErrJobFull) {
		t.Fatalf("late upload bypassed full quota: %v", err)
	}
	retained, err := s.GetJob("late-upload")
	if err != nil || retained.Status != JobUploading {
		t.Fatalf("denied late upload lost retry state: %+v error=%v", retained, err)
	}
	if err := s.CancelJob("occupant", "drained", later); err != nil {
		t.Fatal(err)
	}
	queued, err := s.QueueJob("late-upload", 1, later)
	if err != nil || queued.Status != JobQueued {
		t.Fatalf("late upload did not retry after drain: %+v error=%v", queued, err)
	}
}

func TestConcurrentLateSubmissionAndReservationShareOneBound(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	if _, _, err := s.ReserveJob(jobTestSpec(repo.ID, "late-upload", JobUpload), 1, jobTestNow); err != nil {
		t.Fatal(err)
	}
	later := jobTestNow.Add(11 * time.Minute)
	start := make(chan struct{})
	results := make(chan error, 31)
	var wg sync.WaitGroup
	wg.Go(func() { <-start; _, err := s.QueueJob("late-upload", 1, later); results <- err })
	for i := range 30 {
		wg.Go(func() {
			<-start
			_, _, err := s.ReserveJob(jobTestSpec(repo.ID, "new-"+strconv.Itoa(i), JobBuild), 1, later)
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
	if admitted != 1 {
		t.Fatalf("concurrent late submit and reserve admitted %d jobs, want 1", admitted)
	}
	stats, err := s.JobStats(later)
	if err != nil || stats.Queued != 1 || stats.Uploading != 0 {
		t.Fatalf("admission state: %+v error=%v", stats, err)
	}
}
