// Package statuscmd reports the observed state of a Flywheel local cluster.
// It never changes the selected branch or reconciles resources.
package statuscmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/k3d"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/cobr-io/flywheel/internal/naming"
)

type Options struct {
	RepoDir string
	Stdout  io.Writer
	Verbose bool
	// run is overridden by tests. Arguments are always an argv, never a shell.
	run func(context.Context, string, ...string) ([]byte, error)
}

type query struct {
	name, resource string
	namespace      string
}

var queries = []query{
	{"nodes", "nodes", ""},
	{"pods", "pods", "*"},
	{"replicasets", "replicasets", "*"},
	{"deployments", "deployments", "*"},
	{"statefulsets", "statefulsets", "*"},
	{"daemonsets", "daemonsets", "*"},
	{"gitrepositories", "gitrepositories.source.toolkit.fluxcd.io", "*"},
	{"kustomizations", "kustomizations.kustomize.toolkit.fluxcd.io", "*"},
	{"helmreleases", "helmreleases.helm.toolkit.fluxcd.io", "*"},
	{"helmrepositories", "helmrepositories.source.toolkit.fluxcd.io", "*"},
	{"helmcharts", "helmcharts.source.toolkit.fluxcd.io", "*"},
	{"ocirepositories", "ocirepositories.source.toolkit.fluxcd.io", "*"},
	{"buckets", "buckets.source.toolkit.fluxcd.io", "*"},
	{"imagerepositories", "imagerepositories.image.toolkit.fluxcd.io", "*"},
	{"imagepolicies", "imagepolicies.image.toolkit.fluxcd.io", "*"},
	{"imageupdateautomations", "imageupdateautomations.image.toolkit.fluxcd.io", "*"},
	{"jobs", "jobs", naming.FlywheelNamespace},
}

type snapshot struct {
	items  map[string][]unstructured.Unstructured
	errors map[string]error
}

func Run(ctx context.Context, opts Options) error {
	if opts.RepoDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts.RepoDir = wd
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.run == nil {
		opts.run = command
	}
	cfg, err := config.Load(opts.RepoDir, config.LoadOptions{RequireCluster: true})
	if err != nil {
		return err
	}
	contextName := k3d.KubeContext(cfg.Cluster.Name)
	s := collect(ctx, opts.run, contextName)
	return report(ctx, opts, contextName, cfg.IntegrationBranch(), s)
}

func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...)
	out, err := c.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func collect(ctx context.Context, run func(context.Context, string, ...string) ([]byte, error), contextName string) snapshot {
	s := snapshot{items: make(map[string][]unstructured.Unstructured), errors: make(map[string]error)}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, q := range queries {
		wg.Add(1)
		go func(q query) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			args := []string{"get", q.resource, "--context", contextName}
			if q.namespace == "*" {
				args = append(args, "-A")
			}
			if q.namespace != "" && q.namespace != "*" {
				args = append(args, "-n", q.namespace)
			}
			args = append(args, "-o", "json", "--request-timeout=7s")
			out, err := run(qctx, "kubectl", args...)
			var list unstructured.UnstructuredList
			if err == nil {
				err = json.Unmarshal(out, &list)
			}
			mu.Lock()
			if err != nil {
				s.errors[q.name] = err
			} else {
				s.items[q.name] = list.Items
			}
			mu.Unlock()
		}(q)
	}
	wg.Wait()
	return s
}

func report(ctx context.Context, opts Options, contextName, defaultBranch string, s snapshot) error {
	out := opts.Stdout
	failures := 0
	add := func(text string, fatal bool) {
		if fatal {
			failures++
			style.Err(out, "%s", text)
		} else {
			style.Warn(out, "%s", text)
		}
	}
	style.Summary(out, "Flywheel status — %s", contextName)
	if len(s.errors) > 0 {
		keys := make([]string, 0, len(s.errors))
		for k := range s.errors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			add(fmt.Sprintf("cannot read %s: %v", k, s.errors[k]), true)
		}
	}

	style.Step(out, "Cluster")
	nodes := s.items["nodes"]
	ready := 0
	for _, n := range nodes {
		if condition(n, "Ready") == "True" {
			ready++
		} else {
			add(fmt.Sprintf("node %s is not Ready", n.GetName()), true)
		}
	}
	style.Detail(out, "nodes: %d/%d Ready", ready, len(nodes))
	if len(nodes) == 0 && s.errors["nodes"] == nil {
		add("no nodes found", true)
	}

	style.Step(out, "Source and mirrors")
	var self *unstructured.Unstructured
	gitRepos := append([]unstructured.Unstructured(nil), s.items["gitrepositories"]...)
	sort.Slice(gitRepos, func(i, j int) bool { return objectKey(gitRepos[i]) < objectKey(gitRepos[j]) })
	for i := range gitRepos {
		g := &gitRepos[i]
		if g.GetNamespace() == naming.FluxNamespace && g.GetName() == "flux-system" {
			self = g
			continue
		}
		rev, _, _ := unstructured.NestedString(g.Object, "status", "artifact", "revision")
		branch, _, _ := unstructured.NestedString(g.Object, "spec", "ref", "branch")
		if rev == "" {
			rev = "unknown"
		}
		if branch == "" {
			branch = "pinned"
		}
		if !opts.Verbose {
			rev = shortRevision(rev)
		}
		style.Detail(out, "%s: %s @ %s", objectKey(*g), branch, rev)
	}
	if self == nil && s.errors["gitrepositories"] == nil {
		add("self GitRepository flux-system/flux-system is missing", true)
	}
	if self != nil {
		selected := self.GetAnnotations()[naming.DeployBranchAnnotation]
		if selected == "" {
			selected = defaultBranch
			style.Detail(out, "branch selection: default (%s)", defaultBranch)
		}
		rev, _, _ := unstructured.NestedString(self.Object, "status", "artifact", "revision")
		if rev == "" {
			rev = "unknown"
			add("deploy branch has no mirrored revision", true)
		}
		if !opts.Verbose {
			rev = shortRevision(rev)
		}
		style.Detail(out, "selected: %s; deployed: %s", selected, rev)
		local, err := opts.run(ctx, "git", "-C", opts.RepoDir, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err == nil {
			branch := strings.TrimSpace(string(local))
			style.Detail(out, "checkout: %s", branch)
			if selected != "unknown" && branch != selected {
				style.Warn(out, "checkout is on %s while %s is selected", branch, selected)
			}
		}
	}

	owned := inventory(s.items["kustomizations"])
	for _, group := range []string{"deployments", "statefulsets", "daemonsets"} {
		for _, u := range s.items[group] {
			if u.GetLabels()[naming.ManagedByLabelKey] == naming.ManagedByLabelValue ||
				(u.GetNamespace() == naming.FluxNamespace && u.GetLabels()["app.kubernetes.io/part-of"] == "flux") {
				owned[objectKey(u)+"/"+u.GetKind()] = true
			}
		}
	}
	style.Step(out, "Flux")
	fluxKinds := []string{"gitrepositories", "kustomizations", "helmreleases", "helmrepositories", "helmcharts", "ocirepositories", "buckets", "imagerepositories", "imagepolicies", "imageupdateautomations"}
	fluxOK, fluxTotal := 0, 0
	for _, kind := range fluxKinds {
		for _, obj := range s.items[kind] {
			fluxTotal++
			if condition(obj, "Ready") == "True" {
				fluxOK++
				if opts.Verbose {
					style.Detail(out, "Flux %s %s: Ready", obj.GetKind(), objectKey(obj))
				}
				continue
			}
			fatal := fluxOwned(obj, owned)
			add(fmt.Sprintf("Flux %s %s: Ready=%s %s", obj.GetKind(), objectKey(obj), statusWord(condition(obj, "Ready")), conditionMessage(obj, "Ready")), fatal)
		}
	}
	style.Detail(out, "resources: %d/%d Ready", fluxOK, fluxTotal)
	if fluxTotal == 0 && s.errors["gitrepositories"] == nil {
		add("no Flux resources found", true)
	}

	style.Step(out, "Image builds")
	builds := buildStates(gitRepos, s.items["jobs"], s.items["imagerepositories"], s.items["imagepolicies"])
	if len(builds) == 0 {
		style.Detail(out, "no image builds found")
	}
	for _, b := range builds {
		style.Detail(out, "%s: %s; image policy: %s", b.name, b.state, b.policy)
		if b.failed {
			add("current image build failed: "+b.name, true)
		} else if strings.Contains(b.state, "waiting for build") || strings.Contains(b.policy, "behind source") {
			style.Warn(out, "image %s has not reached the current source revision", b.name)
		}
	}

	style.Step(out, "Workloads")
	controllers := unhealthyControllers(s, owned)
	unhealthyOwned := map[string]bool{}
	for _, c := range controllers {
		msg := fmt.Sprintf("%s %s rollout incomplete (%d/%d Ready)", c.kind, c.key, c.ready, c.desired)
		add(msg, c.owned)
		if c.owned {
			unhealthyOwned[c.key+"/"+c.kind] = true
		}
	}
	badPods := unhealthyPods(s.items["pods"])
	if len(badPods) == 0 {
		style.Detail(out, "pods: no unhealthy active pods")
	}
	for _, p := range badPods {
		fatal := p.GetLabels()[naming.ManagedByLabelKey] == naming.ManagedByLabelValue || podOwned(p, s.items["replicasets"], unhealthyOwned)
		add(fmt.Sprintf("pod %s: %s", objectKey(p), podState(p)), fatal)
	}
	rolloutDetails(ctx, opts.run, contextName, controllers, out, opts.Verbose)
	if opts.Verbose {
		style.Detail(out, "checks: %d nodes, %d mirrors, %d build jobs, %d pods", len(nodes), len(gitRepos), len(s.items["jobs"]), len(s.items["pods"]))
	}
	style.Detail(out, "details: flux get all --all-namespaces --context %s", contextName)
	style.Detail(out, "         kubectl --context %s get pods -A", contextName)
	if failures > 0 {
		return fmt.Errorf("%d Flywheel status check(s) failed", failures)
	}
	return nil
}

func objectKey(u unstructured.Unstructured) string { return u.GetNamespace() + "/" + u.GetName() }
func statusWord(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}

func shortRevision(rev string) string {
	before, sha, ok := strings.Cut(rev, "sha1:")
	if ok && len(sha) > 8 {
		return before + "sha1:" + sha[:8]
	}
	return rev
}

func condition(u unstructured.Unstructured, name string) string {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if ok && c["type"] == name {
			v, _ := c["status"].(string)
			return v
		}
	}
	return ""
}

func conditionMessage(u unstructured.Unstructured, name string) string {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if ok && c["type"] == name {
			v, _ := c["message"].(string)
			return v
		}
	}
	return ""
}

// inventory is the exact set of objects Flux directly applies. An operator's
// children are deliberately not inferred from namespace membership.
func inventory(kustomizations []unstructured.Unstructured) map[string]bool {
	owned := map[string]bool{}
	for _, k := range kustomizations {
		if k.GetNamespace() != naming.FluxNamespace {
			continue
		}
		if !strings.HasPrefix(k.GetName(), "client-") && !strings.HasPrefix(k.GetName(), "flywheel-") {
			continue
		}
		entries, _, _ := unstructured.NestedSlice(k.Object, "status", "inventory", "entries")
		for _, raw := range entries {
			e, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, _ := e["id"].(string)
			parts := strings.Split(id, "_")
			if len(parts) < 4 {
				continue
			}
			owned[parts[0]+"/"+parts[1]+"/"+parts[len(parts)-1]] = true
		}
	}
	return owned
}

func fluxOwned(u unstructured.Unstructured, owned map[string]bool) bool {
	if u.GetNamespace() == naming.FluxNamespace && (u.GetName() == "flux-system" || strings.HasPrefix(u.GetName(), "client-") || strings.HasPrefix(u.GetName(), "flywheel-")) {
		return true
	}
	if u.GetLabels()[naming.ManagedByLabelKey] == naming.ManagedByLabelValue {
		return true
	}
	return owned[u.GetNamespace()+"/"+u.GetName()+"/"+u.GetKind()]
}

type buildState struct {
	name, state, policy string
	failed              bool
}

func buildStates(repos, jobs, imageRepos, policies []unstructured.Unstructured) []buildState {
	revision := map[string]string{}
	for _, r := range repos {
		if r.GetNamespace() != naming.FlywheelNamespace {
			continue
		}
		v, _, _ := unstructured.NestedString(r.Object, "status", "artifact", "revision")
		if _, sha, ok := strings.Cut(v, "sha1:"); ok {
			revision[r.GetName()] = sha
		}
	}
	policyByKey := map[string]string{}
	repoImages := map[string]string{}
	for _, ir := range imageRepos {
		image, _, _ := unstructured.NestedString(ir.Object, "spec", "image")
		if image != "" {
			repoImages[ir.GetNamespace()+"/"+ir.GetName()] = image[strings.LastIndex(image, "/")+1:]
		}
	}
	for _, p := range policies {
		ref, _, _ := unstructured.NestedString(p.Object, "spec", "imageRepositoryRef", "name")
		image := repoImages[p.GetNamespace()+"/"+ref]
		if image == "" {
			continue
		}
		tag, _, _ := unstructured.NestedString(p.Object, "status", "latestRef", "tag")
		if tag == "" {
			tag = "unknown"
		}
		policyByKey[p.GetName()+"/"+image] = tag
	}
	latest := map[string]unstructured.Unstructured{}
	for _, j := range jobs {
		if j.GetLabels()["app"] != "image-builder" {
			continue
		}
		key := j.GetLabels()["repo"] + "/" + j.GetLabels()["image"]
		if key == "/" {
			continue
		}
		old, ok := latest[key]
		if !ok || j.GetCreationTimestamp().After(old.GetCreationTimestamp().Time) {
			latest[key] = j
		}
	}
	keys := map[string]bool{}
	for k := range latest {
		keys[k] = true
	}
	// A policy can exist after the Job TTL has removed the build evidence.
	for k := range policyByKey {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	result := make([]buildState, 0, len(names))
	for _, k := range names {
		repo, image, _ := strings.Cut(k, "/")
		b := buildState{name: k, state: "unknown (no retained Job)", policy: policyByKey[k]}
		if b.policy == "" {
			for policyKey, tag := range policyByKey {
				if strings.HasSuffix(policyKey, "/"+image) {
					b.policy = tag
					break
				}
			}
		}
		if b.policy == "" {
			b.policy = "unknown"
		}
		if j, ok := latest[k]; ok {
			commit := j.GetLabels()["commit"]
			if sha := revision[repo]; sha != "" && (commit == "" || !strings.HasPrefix(sha, commit)) {
				b.state = "waiting for build of current source revision"
			} else {
				switch {
				case condition(j, "Complete") == "True":
					b.state = "succeeded"
				case condition(j, "Failed") == "True":
					b.state = "failed"
					b.failed = true
				default:
					b.state = "in progress"
				}
			}
		}
		if sha := revision[repo]; sha != "" && len(sha) >= 7 && b.policy != "unknown" && !strings.HasSuffix(b.policy, sha[:7]) {
			b.policy += " (behind source)"
		}
		result = append(result, b)
	}
	return result
}

type controller struct {
	kind, key      string
	ready, desired int64
	owned          bool
}

func unhealthyControllers(s snapshot, owned map[string]bool) []controller {
	var result []controller
	for _, spec := range []struct{ name, kind string }{
		{"deployments", "Deployment"}, {"statefulsets", "StatefulSet"}, {"daemonsets", "DaemonSet"},
	} {
		for _, u := range s.items[spec.name] {
			var desired, ready, updated int64
			if spec.kind == "DaemonSet" {
				desired, _, _ = unstructured.NestedInt64(u.Object, "status", "desiredNumberScheduled")
				ready, _, _ = unstructured.NestedInt64(u.Object, "status", "numberReady")
				updated, _, _ = unstructured.NestedInt64(u.Object, "status", "updatedNumberScheduled")
			} else {
				desired, _, _ = unstructured.NestedInt64(u.Object, "spec", "replicas")
				if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "replicas"); !found {
					desired = 1
				}
				ready, _, _ = unstructured.NestedInt64(u.Object, "status", "readyReplicas")
				updated, _, _ = unstructured.NestedInt64(u.Object, "status", "updatedReplicas")
			}
			observed, _, _ := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
			if ready >= desired && updated >= desired && observed >= u.GetGeneration() && condition(u, "Progressing") != "False" {
				continue
			}
			key := objectKey(u)
			isOwned := owned[key+"/"+spec.kind]
			result = append(result, controller{spec.kind, key, ready, desired, isOwned})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].key+result[i].kind < result[j].key+result[j].kind })
	return result
}

func unhealthyPods(pods []unstructured.Unstructured) []unstructured.Unstructured {
	var bad []unstructured.Unstructured
	for _, p := range pods {
		if p.GetLabels()["app"] == "image-builder" {
			continue
		} // build Jobs are evaluated by source revision above
		if p.GetDeletionTimestamp() != nil {
			continue
		}
		phase, _, _ := unstructured.NestedString(p.Object, "status", "phase")
		if phase == "Succeeded" {
			continue
		}
		if phase == "Running" && condition(p, "Ready") == "True" {
			continue
		}
		bad = append(bad, p)
	}
	sort.Slice(bad, func(i, j int) bool { return objectKey(bad[i]) < objectKey(bad[j]) })
	return bad
}

func podState(p unstructured.Unstructured) string {
	phase, _, _ := unstructured.NestedString(p.Object, "status", "phase")
	if phase == "" {
		phase = "Unknown"
	}
	if phase == "Running" {
		return "Running, not Ready"
	}
	return phase
}

func podOwned(p unstructured.Unstructured, replicaSets []unstructured.Unstructured, owned map[string]bool) bool {
	for _, o := range p.GetOwnerReferences() {
		if owned[p.GetNamespace()+"/"+o.Name+"/"+o.Kind] {
			return true
		}
		if o.Kind != "ReplicaSet" {
			continue
		}
		for _, rs := range replicaSets {
			if rs.GetNamespace() != p.GetNamespace() || rs.GetName() != o.Name {
				continue
			}
			for _, parent := range rs.GetOwnerReferences() {
				if owned[p.GetNamespace()+"/"+parent.Name+"/"+parent.Kind] {
					return true
				}
			}
		}
	}
	return false
}

func rolloutDetails(ctx context.Context, run func(context.Context, string, ...string) ([]byte, error), contextName string, controllers []controller, out io.Writer, verbose bool) {
	type result struct {
		detail string
		failed bool
	}
	results := make([]result, len(controllers))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, c := range controllers {
		wg.Add(1)
		go func(i int, c controller) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ns, name, _ := strings.Cut(c.key, "/")
			qctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			args := []string{"rollout", "status", strings.ToLower(c.kind) + "/" + name, "--context", contextName, "-n", ns, "--watch=false", "--timeout=5s"}
			body, err := run(qctx, "kubectl", args...)
			if err != nil {
				results[i].detail = fmt.Sprintf("%s: %v", c.key, err)
				results[i].failed = true
			} else {
				results[i].detail = fmt.Sprintf("%s: %s", c.key, strings.TrimSpace(string(body)))
			}
		}(i, c)
	}
	wg.Wait()
	for _, r := range results {
		if r.detail != "" && (verbose || r.failed) {
			style.Detail(out, "rollout %s", r.detail)
		}
	}
}
