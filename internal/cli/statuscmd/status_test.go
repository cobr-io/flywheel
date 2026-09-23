package statuscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/cobr-io/flywheel/internal/naming"
)

func obj(kind, ns, name string, fields map[string]any) unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns},
	}}
	for k, v := range fields {
		u.Object[k] = v
	}
	return u
}

func readyCondition() map[string]any { return map[string]any{"type": "Ready", "status": "True"} }

func baseSnapshot() snapshot {
	node := obj("Node", "", "node-0", map[string]any{"status": map[string]any{"conditions": []any{readyCondition()}}})
	self := obj("GitRepository", naming.FluxNamespace, "flux-system", map[string]any{
		"metadata": map[string]any{"name": "flux-system", "namespace": naming.FluxNamespace,
			"annotations": map[string]any{naming.DeployBranchAnnotation: "feature/x"}},
		"status": map[string]any{"conditions": []any{readyCondition()}, "artifact": map[string]any{"revision": "flywheel/local-deploy@sha1:abcd"}},
	})
	k := obj("Kustomization", naming.FluxNamespace, "client-apps", map[string]any{
		"status": map[string]any{"conditions": []any{readyCondition()}, "inventory": map[string]any{"entries": []any{
			map[string]any{"id": "apps_my-app_apps_Deployment"},
		}}},
	})
	return snapshot{items: map[string][]unstructured.Unstructured{
		"nodes": {node}, "gitrepositories": {self}, "kustomizations": {k},
	}, errors: map[string]error{}}
}

func render(t *testing.T, s snapshot, run func(context.Context, string, ...string) ([]byte, error)) (string, error) {
	t.Helper()
	var out bytes.Buffer
	if run == nil {
		run = func(_ context.Context, name string, args ...string) ([]byte, error) {
			return []byte("feature/x\n"), nil
		}
	}
	err := report(context.Background(), Options{RepoDir: t.TempDir(), Stdout: &out, run: run}, "k3d-demo", "main", s)
	return out.String(), err
}

func TestUnrelatedPodIsAdvisory(t *testing.T) {
	s := baseSnapshot()
	s.items["pods"] = []unstructured.Unstructured{obj("Pod", "other", "unrelated", map[string]any{
		"status": map[string]any{"phase": "Pending"},
	})}
	out, err := render(t, s, nil)
	if err != nil {
		t.Fatalf("unrelated pod caused failure: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pod other/unrelated: Pending") {
		t.Fatalf("missing advisory pod: %s", out)
	}
}

func TestOldPodDoesNotFailHealthyDeployment(t *testing.T) {
	s := baseSnapshot()
	dep := obj("Deployment", "apps", "my-app", map[string]any{
		"spec":   map[string]any{"replicas": int64(1)},
		"status": map[string]any{"readyReplicas": int64(1), "updatedReplicas": int64(1), "observedGeneration": int64(1)},
	})
	dep.SetGeneration(1)
	s.items["deployments"] = []unstructured.Unstructured{dep}
	rs := obj("ReplicaSet", "apps", "old-replica", nil)
	rs.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Deployment", Name: "my-app"}})
	s.items["replicasets"] = []unstructured.Unstructured{rs}
	pod := obj("Pod", "apps", "old-pod", map[string]any{"status": map[string]any{"phase": "Failed"}})
	pod.SetOwnerReferences([]metav1.OwnerReference{{Kind: "ReplicaSet", Name: "old-replica"}})
	s.items["pods"] = []unstructured.Unstructured{pod}
	out, err := render(t, s, nil)
	if err != nil {
		t.Fatalf("old pod failed healthy deployment: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pod apps/old-pod: Failed") {
		t.Fatalf("pod not shown: %s", out)
	}
}

func TestManagedPodAndTargetedRolloutFail(t *testing.T) {
	s := baseSnapshot()
	bad := obj("Deployment", "apps", "my-app", map[string]any{
		"spec":   map[string]any{"replicas": int64(1)},
		"status": map[string]any{"readyReplicas": int64(0), "observedGeneration": int64(1)},
	})
	bad.SetGeneration(1)
	good := obj("Deployment", "other", "healthy", map[string]any{
		"spec":   map[string]any{"replicas": int64(1)},
		"status": map[string]any{"readyReplicas": int64(1), "updatedReplicas": int64(1), "observedGeneration": int64(1)},
	})
	good.SetGeneration(1)
	s.items["deployments"] = []unstructured.Unstructured{bad, good}
	var rollouts []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" {
			return []byte("feature/x\n"), nil
		}
		rollouts = append(rollouts, strings.Join(args, " "))
		return nil, errors.New("rollout incomplete")
	}
	out, err := render(t, s, run)
	if err == nil {
		t.Fatalf("managed rollout did not fail: %s", out)
	}
	if len(rollouts) != 1 || !strings.Contains(rollouts[0], "deployment/my-app") || strings.Contains(rollouts[0], "healthy") {
		t.Fatalf("rollout checks = %v", rollouts)
	}
}

func TestCompletedJobPodIsIgnored(t *testing.T) {
	s := baseSnapshot()
	s.items["pods"] = []unstructured.Unstructured{obj("Pod", naming.FlywheelNamespace, "old-job", map[string]any{
		"status": map[string]any{"phase": "Succeeded"},
	})}
	out, err := render(t, s, nil)
	if err != nil {
		t.Fatalf("completed Job pod caused failure: %v\n%s", err, out)
	}
	if strings.Contains(out, "pod flywheel-system/old-job:") {
		t.Fatalf("completed pod reported unhealthy: %s", out)
	}
}

func TestPodStateShowsKubernetesReason(t *testing.T) {
	cases := []struct {
		name   string
		status map[string]any
		want   string
	}{
		{"image pull", map[string]any{"phase": "Pending", "containerStatuses": []any{map[string]any{
			"name": "api", "state": map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff"}},
		}}}, "Pending: ImagePullBackOff (api)"},
		{"init container", map[string]any{"phase": "Pending", "initContainerStatuses": []any{map[string]any{
			"name": "init-db", "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
		}}}, "Pending: CrashLoopBackOff (init-db)"},
		{"completed init is skipped", map[string]any{
			"phase": "Pending",
			"initContainerStatuses": []any{map[string]any{
				"name": "init-db", "state": map[string]any{"terminated": map[string]any{"reason": "Completed"}},
			}},
			"containerStatuses": []any{map[string]any{
				"name": "api", "state": map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff"}},
			}},
		}, "Pending: ImagePullBackOff (api)"},
		{"evicted", map[string]any{"phase": "Failed", "reason": "Evicted"}, "Failed: Evicted"},
		{"unschedulable", map[string]any{"phase": "Pending", "conditions": []any{map[string]any{
			"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
		}}}, "Pending: Unschedulable"},
		{"no reason", map[string]any{"phase": "Running"}, "Running, not Ready"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := obj("Pod", "apps", "api", map[string]any{"status": tc.status})
			if got := podState(pod); got != tc.want {
				t.Errorf("podState = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildResultRequiresCurrentSourceRevision(t *testing.T) {
	repo := obj("GitRepository", naming.FlywheelNamespace, "local-app", map[string]any{
		"status": map[string]any{"artifact": map[string]any{"revision": "main@sha1:abcdef0123456789"}},
	})
	job := obj("Job", naming.FlywheelNamespace, "build-local-app", map[string]any{
		"metadata": map[string]any{"name": "build-local-app", "namespace": naming.FlywheelNamespace,
			"labels": map[string]any{"app": "image-builder", "repo": "local-app", "image": "app", "commit": "1234567"}},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "True"}}},
	})
	states := buildStates([]unstructured.Unstructured{repo}, []unstructured.Unstructured{job}, nil, nil)
	if len(states) != 1 || states[0].failed || !strings.Contains(states[0].state, "waiting") {
		t.Fatalf("stale failure = %+v", states)
	}
	job.SetLabels(map[string]string{"app": "image-builder", "repo": "local-app", "image": "app", "commit": "abcdef0"})
	states = buildStates([]unstructured.Unstructured{repo}, []unstructured.Unstructured{job}, nil, nil)
	if len(states) != 1 || !states[0].failed {
		t.Fatalf("current failure = %+v", states)
	}
}

func TestExpiredJobUnknownAndPolicyLag(t *testing.T) {
	repo := obj("GitRepository", naming.FlywheelNamespace, "local-app", map[string]any{
		"status": map[string]any{"artifact": map[string]any{"revision": "main@sha1:abcdef0123456789"}},
	})
	imageRepo := obj("ImageRepository", naming.FluxNamespace, "local-app", map[string]any{
		"spec": map[string]any{"image": "registry/app"},
	})
	policy := obj("ImagePolicy", naming.FluxNamespace, "local-app", map[string]any{
		"spec":   map[string]any{"imageRepositoryRef": map[string]any{"name": "local-app"}},
		"status": map[string]any{"latestRef": map[string]any{"tag": "1234567-deadbee"}},
	})
	states := buildStates([]unstructured.Unstructured{repo}, nil, []unstructured.Unstructured{imageRepo}, []unstructured.Unstructured{policy})
	if len(states) != 1 || states[0].failed || !strings.Contains(states[0].state, "unknown") || !strings.Contains(states[0].policy, "behind source") {
		t.Fatalf("expired build = %+v", states)
	}
}

func TestDefaultBranchWhenUseWasNeverCalled(t *testing.T) {
	s := baseSnapshot()
	s.items["gitrepositories"][0].SetAnnotations(nil)
	out, err := render(t, s, nil)
	if err != nil {
		t.Fatalf("default branch failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "branch selection: default (main)") {
		t.Fatalf("default missing: %s", out)
	}
}

func TestRunUsesConfiguredContextAndReadOnlyQueries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "flywheel.yaml"), []byte("cluster:\n  name: sample-local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := baseSnapshot()
	seen := map[string]bool{}
	var mu sync.Mutex
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" {
			return []byte("feature/x\n"), nil
		}
		if name != "kubectl" || !strings.Contains(strings.Join(args, " "), "--context k3d-sample-local") {
			return nil, errors.New("unexpected command or context")
		}
		if len(args) < 2 || args[0] != "get" {
			return nil, errors.New("status attempted a write")
		}
		for _, q := range queries {
			if args[1] != q.resource {
				continue
			}
			mu.Lock()
			seen[q.name] = true
			mu.Unlock()
			body, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": s.items[q.name]})
			return body, err
		}
		return nil, errors.New("unexpected resource")
	}
	var out bytes.Buffer
	if err := Run(context.Background(), Options{RepoDir: dir, Stdout: &out, run: run}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if len(seen) != len(queries) || !strings.Contains(out.String(), "k3d-sample-local") {
		t.Fatalf("queries/context mismatch: %v\n%s", seen, out.String())
	}
}

func TestQueryFailureReturnsNonzeroAfterSummary(t *testing.T) {
	s := baseSnapshot()
	s.errors["pods"] = errors.New("API unavailable")
	out, err := render(t, s, nil)
	if err == nil || !strings.Contains(out, "cannot read pods") || !strings.Contains(out, "Source and mirrors") {
		t.Fatalf("query failure: err=%v\n%s", err, out)
	}
}
