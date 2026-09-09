package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/lifecycle/cleanup"
)

func TestCleanupInvalidRequestPersistsBeforeAuthorityReads(t *testing.T) {
	for _, previous := range []string{"", "2"} {
		for _, token := range []string{"0", "01", "-1", "1"} {
			if previous == "" && token == "1" {
				continue
			}
			t.Run(previous+"/"+token, func(t *testing.T) {
				ctx := context.Background()
				r, repo, client, i := cleanupReviewFixture(t)
				if previous != "" {
					requestCleanupCheck(t, client, i, previous)
					if err := r.cleanupJournal().StartCheck(ctx, i, previous, r.HelperImage); err != nil {
						t.Fatal(err)
					}
					if err := r.cleanupJournal().BindCheck(ctx, i, previous, "failed-check"); err != nil {
						t.Fatal(err)
					}
					if err := r.cleanupJournal().FinishCheck(ctx, i, previous, "failed-check", false); err != nil {
						t.Fatal(err)
					}
				}
				requestCleanupCheck(t, client, i, token)
				r = &Reconciler{Client: client, Repository: repo, Namespace: r.Namespace, HelperImage: r.HelperImage}
				r.Repository = &completionReadRepository{memoryRepository: repo, getError: fmt.Errorf("authority must not be read")}
				client.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
					if a.GetResource().Resource != "configmaps" {
						t.Errorf("unexpected authority/Job action: %s %s", a.GetVerb(), a.GetResource().Resource)
					}
					return false, nil, nil
				})
				if err := r.sweepCleanup(ctx); err == nil {
					t.Fatal("invalid request accepted")
				}
				record, err := r.cleanupJournal().Get(ctx, i.MoveUID)
				want := cleanup.ValidateCheckRequest(token, previous).Error()
				if err != nil || record.Reason != want || record.State != cleanup.NeedsReview || record.Completed || record.CheckID != previous {
					t.Fatalf("diagnostic or attempt changed: %+v %v", record, err)
				}
			})
		}
	}
}

func TestCleanupActiveCheckContinuesAfterInvalidRequest(t *testing.T) {
	ctx := context.Background()
	r, _, client, i := cleanupReviewFixture(t)
	for range 2 {
		if err := r.sweepCleanup(ctx); err != nil {
			t.Fatal(err)
		}
	}
	record, err := r.cleanupJournal().Get(ctx, i.MoveUID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := client.BatchV1().Jobs("system").Get(ctx, cleanupCheckName(record), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if _, err := client.BatchV1().Jobs("system").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	requestCleanupCheck(t, client, i, "0")
	if err := r.sweepCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := r.cleanupJournal().Get(ctx, i.MoveUID)
	if err != nil || !after.Completed || after.CheckID != "1" || after.CheckJobUID != record.CheckJobUID {
		t.Fatalf("active proof interrupted: %+v %v", after, err)
	}
}

// Exercise client-go's real HTTP transport and token bucket; fake clients skip both.
func TestCleanupValidCheckConvergesUnderClientThrottling(t *testing.T) {
	ctx := context.Background()
	r, _, backing, i := cleanupReviewFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var obj any
		var err error
		path := req.URL.Path
		switch {
		case strings.Contains(path, "/configmaps"):
			cms := backing.CoreV1().ConfigMaps("system")
			if req.Method == http.MethodPut {
				var cm corev1.ConfigMap
				err = json.NewDecoder(req.Body).Decode(&cm)
				if err == nil {
					obj, err = cms.Update(req.Context(), &cm, metav1.UpdateOptions{})
				}
			} else if strings.HasSuffix(path, "/configmaps") {
				obj, err = cms.List(req.Context(), metav1.ListOptions{})
			} else {
				obj, err = cms.Get(req.Context(), cleanup.Name(i.MoveUID), metav1.GetOptions{})
			}
		case strings.Contains(path, "/nodes/"):
			obj, err = backing.CoreV1().Nodes().Get(req.Context(), i.SourceNode, metav1.GetOptions{})
		case strings.HasSuffix(path, "/pods"):
			obj = &corev1.PodList{}
		case strings.Contains(path, "/jobs"):
			jobs := backing.BatchV1().Jobs("system")
			name := path[strings.LastIndex(path, "/")+1:]
			if req.Method == http.MethodPost {
				var job batchv1.Job
				err = json.NewDecoder(req.Body).Decode(&job)
				if err == nil {
					obj, err = jobs.Create(req.Context(), &job, metav1.CreateOptions{})
				}
			} else if name == "jobs" {
				obj, err = jobs.List(req.Context(), metav1.ListOptions{LabelSelector: req.URL.Query().Get("labelSelector")})
			} else {
				obj, err = jobs.Get(req.Context(), name, metav1.GetOptions{})
			}
		default:
			t.Errorf("unexpected HTTP request: %s %s", req.Method, path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			if status, ok := err.(apierrors.APIStatus); ok {
				s := status.Status()
				w.WriteHeader(int(s.Code))
				obj = &s
			} else {
				t.Errorf("fixture API: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if err := json.NewEncoder(w).Encode(obj); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()
	// A small burst and shared 5-QPS limiter exercise throttled authority reads and writes.
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, QPS: 5, Burst: 1, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	r.Client = client
	for range 2 {
		passCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := r.sweepCleanup(passCtx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	record, err := (cleanup.Journal{Client: backing, Namespace: "system"}).Get(ctx, i.MoveUID)
	if err != nil || record.CheckID != "1" || record.CheckJobUID == "" || record.Completed {
		t.Fatalf("valid attempt failed to converge: %+v %v", record, err)
	}
	jobs, err := backing.BatchV1().Jobs("system").List(ctx, metav1.ListOptions{})
	if err != nil || len(jobs.Items) != 1 {
		t.Fatalf("attempt count: %+v %v", jobs, err)
	}
}
