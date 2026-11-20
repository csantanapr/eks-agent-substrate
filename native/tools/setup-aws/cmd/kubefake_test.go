package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// kubeFake is an in-memory Kubernetes API behind a TLS test server, for the
// object API of kubeapi.go: server-side apply (PATCH
// application/apply-patch+yaml), get, list with a label selector, and
// delete, of the kinds of kubeKinds. It serves the core kinds always, and a
// custom resource's kind only once its CRD is applied, as an API server
// does. It records every request, can refuse one, and gives the objects the
// status their controllers would: Established CRDs, an available
// Deployment, Ready NodePools, EC2NodeClasses and NodeOverlays.
type kubeFake struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	objs  map[string]kubeObject // by object path
	rv    int
	calls []string // "METHOD path", "PATCH path dryRun" for a dry run
	// deny answers "METHOD path" (an object or a collection) with a status;
	// only that many times when denyLeft has the key.
	deny     map[string]int
	denyLeft map[string]int
	// notReady keeps the object at a path not Ready, with this message;
	// notAvailable keeps the Deployment unavailable.
	notReady       map[string]string
	notAvailable   bool
	notEstablished bool
	// finalizing keeps a deleted object for this many reads after its
	// delete (finalizers).
	finalizing map[string]int
	// onDelete runs after an object is deleted (or marked deleted), with
	// the fake locked: a test's stand-in for what a controller does then.
	onDelete func(k kubeKind, name string)
	// onGet runs before a GET of an object is answered, with the fake
	// locked: a controller that changes an object while setup-aws works.
	onGet func(path string)
	// statusChurn: a controller writes an object's status between any two
	// requests, as Karpenter does a NodePool's node count.
	statusChurn bool
}

func newKubeFake(t *testing.T) *kubeFake {
	f := &kubeFake{t: t, objs: map[string]kubeObject{}, deny: map[string]int{}, denyLeft: map[string]int{}, notReady: map[string]string{}, finalizing: map[string]int{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// client is a kubeClient of the fake, as newKubeClient makes one.
func (f *kubeFake) client() kubeObjects {
	k, err := newKubeClient(kubeTestCluster(f.srv), fixedPresigner(presignedURL))
	if err != nil {
		f.t.Fatal(err)
	}
	return k
}

// served reports a kind the API serves now.
func (f *kubeFake) served(k kubeKind) bool {
	group, _, ok := strings.Cut(k.APIVersion, "/")
	if !ok || strings.HasSuffix(group, ".k8s.io") || group == "apps" || group == "policy" {
		return true
	}
	_, ok = f.objs[kindCRD.path("", k.Resource+"."+group)]
	return ok
}

// route finds the kind, namespace and name of a request path.
func route(path string) (kubeKind, string, string, bool) {
	for _, k := range kubeKinds {
		base := k.path("", "")
		if k.Namespaced {
			pre, post, _ := strings.Cut(k.path("NS", ""), "NS")
			if rest, ok := strings.CutPrefix(path, pre); ok {
				ns, tail, ok := strings.Cut(rest, "/")
				if !ok {
					continue
				}
				if tail == strings.TrimPrefix(post, "/") {
					return k, ns, "", true
				}
				if name, ok := strings.CutPrefix(tail, strings.TrimPrefix(post, "/")+"/"); ok && !strings.Contains(name, "/") {
					return k, ns, name, true
				}
			}
			continue
		}
		if path == base {
			return k, "", "", true
		}
		if name, ok := strings.CutPrefix(path, base+"/"); ok && !strings.Contains(name, "/") {
			return k, "", name, true
		}
	}
	return kubeKind{}, "", "", false
}

func (f *kubeFake) status(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "status": "Failure", "message": msg, "code": code})
}

func (f *kubeFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer k8s-aws-v1.") {
		f.status(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	call := r.Method + " " + r.URL.Path
	if r.URL.Query().Get("dryRun") == "All" {
		call += " dryRun"
	}
	f.calls = append(f.calls, call)
	if code, ok := f.deny[r.Method+" "+r.URL.Path]; ok {
		n, limited := f.denyLeft[r.Method+" "+r.URL.Path]
		if !limited || n > 0 {
			if limited {
				f.denyLeft[r.Method+" "+r.URL.Path] = n - 1
			}
			// As the API names the caller, account id and all.
			f.status(w, code, "denied by the test: arn:aws:sts::111122223333:assumed-role/admin/me may not")
			return
		}
	}
	k, ns, name, ok := route(r.URL.Path)
	if !ok {
		f.t.Errorf("kube fake: no route for %s", call)
		f.status(w, http.StatusNotFound, "no route")
		return
	}
	if !f.served(k) {
		f.status(w, http.StatusNotFound, "the server could not find the requested resource")
		return
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && name == "":
		f.list(w, k, ns, r.URL.Query().Get("labelSelector"))
	case r.Method == http.MethodGet:
		if f.onGet != nil {
			f.onGet(path)
		}
		o := f.read(path)
		if o == nil {
			f.status(w, http.StatusNotFound, k.Resource+" "+name+" not found")
			return
		}
		json.NewEncoder(w).Encode(o)
	case r.Method == http.MethodPatch:
		f.apply(w, r, k, ns, name)
	case r.Method == http.MethodDelete:
		o := f.objs[path]
		if o == nil {
			f.status(w, http.StatusNotFound, k.Resource+" "+name+" not found")
			return
		}
		if f.finalizing[path] > 0 {
			md := o["metadata"].(map[string]any)
			md["deletionTimestamp"] = "2026-11-19T00:00:00Z"
		} else {
			delete(f.objs, path)
		}
		if f.onDelete != nil {
			f.onDelete(k, name)
		}
		json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "status": "Success"})
	default:
		f.status(w, http.StatusMethodNotAllowed, "no")
	}
}

// read is the object at path, counting down a deleted object's finalizers.
func (f *kubeFake) read(path string) kubeObject {
	o := f.objs[path]
	if o == nil {
		return nil
	}
	if o.field("metadata", "deletionTimestamp") != nil {
		if f.finalizing[path]--; f.finalizing[path] <= 0 {
			delete(f.objs, path)
			delete(f.finalizing, path)
			return nil
		}
	}
	return o
}

// selects reports labels that match a selector of "k=v" and "k in (a,b)"
// terms, comma-separated ("" matches all).
func selects(sel string, labels map[string]string) bool {
	for sel = strings.TrimSpace(sel); sel != ""; sel = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sel), ",")) {
		if k, rest, ok := strings.Cut(sel, " in ("); ok && !strings.ContainsAny(k, "=,") {
			vals, after, _ := strings.Cut(rest, ")")
			if !slices.Contains(strings.Split(vals, ","), labels[strings.TrimSpace(k)]) {
				return false
			}
			sel = after
			continue
		}
		term, after, _ := strings.Cut(sel, ",")
		k, v, _ := strings.Cut(term, "=")
		if labels[strings.TrimSpace(k)] != strings.TrimSpace(v) {
			return false
		}
		sel = after
	}
	return true
}

func objLabels(o kubeObject) map[string]string {
	m := map[string]string{}
	ls, _ := o.field("metadata", "labels").(map[string]any)
	for k, v := range ls {
		m[k], _ = v.(string)
	}
	return m
}

func (f *kubeFake) list(w http.ResponseWriter, k kubeKind, ns, sel string) {
	var paths []string
	for p := range f.objs {
		if kk, pns, name, ok := route(p); ok && kk == k && name != "" && pns == ns {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	items := []kubeObject{}
	for _, p := range paths {
		if o := f.read(p); o != nil && selects(sel, objLabels(o)) {
			items = append(items, o)
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"kind": k.Kind + "List", "items": items})
}

// apply stores the applied object, or with dryRun only answers what it
// would store. An object whose applied fields have not changed keeps its
// resourceVersion, as the API server keeps it on a no-op apply.
func (f *kubeFake) apply(w http.ResponseWriter, r *http.Request, k kubeKind, ns, name string) {
	q := r.URL.Query()
	if r.Header.Get("Content-Type") != "application/apply-patch+yaml" || q.Get("fieldManager") != fieldManager || q.Get("force") != "true" {
		f.t.Errorf("kube fake: apply %s with Content-Type %q and %s", r.URL.Path, r.Header.Get("Content-Type"), q.Encode())
		f.status(w, http.StatusBadRequest, "not a server-side apply")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var in kubeObject
	if err := json.Unmarshal(body, &in); err != nil {
		f.status(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.name() != name || in.namespace() != ns || in.str("kind") != k.Kind || in.str("apiVersion") != k.APIVersion {
		f.t.Errorf("kube fake: apply %s with %s %s/%s", r.URL.Path, in.str("kind"), in.namespace(), in.name())
		f.status(w, http.StatusBadRequest, "the object does not match the path")
		return
	}
	path := r.URL.Path
	have := f.objs[path]
	spec := func(o kubeObject) kubeObject {
		c := kubeObject{}
		for key, v := range o {
			if key != "status" {
				c[key] = v
			}
		}
		b, _ := json.Marshal(c)
		var out kubeObject
		json.Unmarshal(b, &out)
		if md, ok := out["metadata"].(map[string]any); ok {
			for _, x := range []string{"uid", "resourceVersion", "generation", "creationTimestamp"} {
				delete(md, x)
			}
		}
		return out
	}
	// Annotations another manager set stay, as server-side apply keeps
	// the fields it does not own.
	merged := spec(in)
	md := merged["metadata"].(map[string]any)
	if have != nil {
		if ha, ok := spec(have)["metadata"].(map[string]any)["annotations"].(map[string]any); ok {
			ann, _ := md["annotations"].(map[string]any)
			for k, v := range ann {
				ha[k] = v
			}
			md["annotations"] = ha
		}
	}
	if have != nil && reflect.DeepEqual(spec(have), merged) {
		if st, ok := have["status"].(map[string]any); ok && f.statusChurn && (k == kindNodePool || k == kindEC2NodeClass) {
			f.rv++
			st["nodes"] = float64(f.rv)
		}
		json.NewEncoder(w).Encode(have)
		return
	}
	out := merged
	gen := 1.0
	if have != nil {
		hm := have["metadata"].(map[string]any)
		md["uid"], md["creationTimestamp"] = hm["uid"], hm["creationTimestamp"]
		gen = hm["generation"].(float64) + 1
	} else {
		md["uid"], md["creationTimestamp"] = fmt.Sprintf("uid-%s", name), "2026-11-19T00:00:00Z"
	}
	f.rv++
	md["resourceVersion"], md["generation"] = fmt.Sprint(f.rv), gen
	out["status"] = f.statusOf(k, path, out, gen)
	if q.Get("dryRun") != "All" {
		f.objs[path] = out
	}
	if have == nil {
		w.WriteHeader(http.StatusCreated)
	}
	json.NewEncoder(w).Encode(out)
}

// statusOf is the status a controller would give the object.
func (f *kubeFake) statusOf(k kubeKind, path string, o kubeObject, gen float64) map[string]any {
	cond := func(t, status, msg string) map[string]any {
		return map[string]any{"conditions": []any{map[string]any{"type": t, "status": status, "reason": t, "message": msg}}}
	}
	switch k {
	case kindCRD:
		if f.notEstablished {
			return cond("Established", "False", "the names are not accepted")
		}
		return cond("Established", "True", "")
	case kindDeployment:
		replicas, _ := o.field("spec", "replicas").(float64)
		st := cond("Available", "True", "")
		if f.notAvailable {
			st, replicas = cond("Available", "False", "Deployment does not have minimum availability."), 0
		}
		st["observedGeneration"], st["availableReplicas"] = gen, replicas
		return st
	case kindNodePool, kindEC2NodeClass, kindNodeOverlay:
		if msg, ok := f.notReady[path]; ok {
			return cond("Ready", "False", msg)
		}
		return cond("Ready", "True", "")
	}
	return map[string]any{}
}

// put stores an object as it is, as a controller (or a person) made it.
func (f *kubeFake) put(o kubeObject) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, err := o.kind()
	if err != nil {
		f.t.Fatal(err)
	}
	f.objs[k.path(o.namespace(), o.name())] = o
}

// get is the stored object of kind k, or nil.
func (f *kubeFake) get(k kubeKind, ns, name string) kubeObject {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objs[k.path(ns, name)]
}

// writes are the requests that change something: PATCH without dryRun, and
// DELETE.
func (f *kubeFake) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if (strings.HasPrefix(c, "PATCH ") && !strings.HasSuffix(c, " dryRun")) || strings.HasPrefix(c, "DELETE ") {
			out = append(out, c)
		}
	}
	return out
}

// installCRDs makes the cluster serve Karpenter's kinds, as an install
// would have.
func (f *kubeFake) installCRDs() {
	for _, n := range []string{"nodepools.karpenter.sh", "nodeclaims.karpenter.sh", "ec2nodeclasses.karpenter.k8s.aws", "nodeoverlays.karpenter.sh"} {
		f.put(kubeObject{"apiVersion": kindCRD.APIVersion, "kind": kindCRD.Kind,
			"metadata": map[string]any{"name": n, "labels": map[string]any{managedByKey: managedByValue}}})
	}
}

// unreachableKube is a Kubernetes API that never answers: a closed server.
func unreachableKube(t *testing.T) kubeObjects {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	k, err := newKubeClient(kubeTestCluster(srv), fixedPresigner(presignedURL))
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	return k
}
