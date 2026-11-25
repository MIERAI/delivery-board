package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fleet of 12 pipelines like the example configuration: one configured (design PRs, the test world's o/app) and
// eleven discovered from app.yaml descriptors in the manifests repository o/m (monorepos with kustomize and Helm,
// single services; two and three environments). Served over HTTP by a fake GitHub that answers conditional requests
// like the real one: an ETag per response, 304 for a matching If-None-Match, X-RateLimit-* headers.

type fleetApp struct {
	name     string
	envs     []string // the first has no promotion; each later one promotes from the one before
	services []string // "worker" is a Helm component, the others share a kustomize component
}

var fleetApps = []fleetApp{
	{"shop", []string{"dev", "staging", "prod"}, []string{"web", "api", "worker"}},
	{"hello", []string{"dev", "prod"}, []string{"hello"}},
	{"orders", []string{"dev", "staging", "prod"}, []string{"web", "api", "worker"}},
	{"billing", []string{"dev", "prod"}, []string{"api", "worker"}},
	{"search", []string{"dev", "prod"}, []string{"search"}},
	{"notify", []string{"dev", "prod"}, []string{"notify"}},
}

const fleetConfig = testConfig + `
discover:
  - repo: o/m
    push: { label: push }
    jobs: { test: test, "image / build": image }
    promotion: { workflow: promote.yml, soakInput: soak }
`

func hexOf(s string) string { return fmt.Sprintf("%x", sha1.Sum([]byte(s))) }

func b64(s string) map[string]string {
	return map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(s))}
}

// fakeServer: GitHub over HTTP. Data maps "path?query" (no leading slash) to a JSON-marshalable value.
type fakeServer struct {
	*httptest.Server
	mu      sync.Mutex
	data    map[string]any
	status  map[int]int    // responses by status code
	paths   map[string]int // requests by "path?query" (since reset)
	limit   int            // X-RateLimit-Limit
	used    int            // requests counted against the limit (200s)
	reset   time.Time      // X-RateLimit-Reset
	lastMod bool           // also send Last-Modified (and honor If-Modified-Since)
	noETag  bool           // send no ETag
	// override answers instead of the data when it returns a status
	override func(w http.ResponseWriter, r *http.Request) int
}

func newFakeServer(data map[string]any) *fakeServer {
	f := &fakeServer{data: data, status: map[int]int{}, paths: map[string]int{}, limit: 5000, reset: t0.Add(time.Hour)}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/")
	if r.URL.RawQuery != "" {
		key += "?" + r.URL.RawQuery
	}
	f.paths[key]++
	if f.override != nil {
		if st := f.override(w, r); st != 0 {
			f.status[st]++
			return
		}
	}
	x, ok := f.data[key]
	if !ok {
		f.status[404]++
		http.Error(w, `{"message":"Not Found"}`, 404)
		return
	}
	body, _ := json.Marshal(x)
	etag := `W/"` + hexOf(string(body)) + `"`
	h := w.Header()
	if !f.noETag {
		h.Set("ETag", etag)
	}
	lm := t0.Add(-time.Duration(len(body)) * time.Second).UTC().Format(http.TimeFormat) // changes with the body
	if f.lastMod {
		h.Set("Last-Modified", lm)
	}
	notModified := (!f.noETag && r.Header.Get("If-None-Match") == etag) || (f.lastMod && r.Header.Get("If-Modified-Since") == lm)
	if !notModified {
		f.used++
	}
	h.Set("X-RateLimit-Limit", strconv.Itoa(f.limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(max(f.limit-f.used, 0)))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(f.reset.Unix(), 10))
	if notModified {
		f.status[304]++
		w.WriteHeader(304)
		return
	}
	f.status[200]++
	w.Write(body)
}

// take: the responses by status since the last take, and the paths requested more than once.
func (f *fakeServer) take() (status map[int]int, total int, repeated []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status, f.status = f.status, map[int]int{}
	for p, n := range f.paths {
		total += n
		if n > 1 {
			repeated = append(repeated, fmt.Sprintf("%s ×%d", p, n))
		}
	}
	f.paths = map[string]int{}
	sort.Strings(repeated)
	return
}

func (f *fakeServer) set(key string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] = v
}

// fleetData: the test world's o/app plus the discovered applications and their manifests history in o/m.
func fleetData() map[string]any {
	data := map[string]any{}
	for k, v := range newWorld(time.Hour).gh {
		data[k] = v
	}
	var tree []any
	for _, a := range fleetApps {
		repo := "o/" + a.name
		c1, c2 := hexOf(a.name+"1"), hexOf(a.name+"2")
		var desc strings.Builder
		fmt.Fprintf(&desc, "name: %s\nrepo: %s\nenvironments:\n", a.name, repo)
		for i, e := range a.envs {
			fmt.Fprintf(&desc, "  - name: %s\n", e)
			if i > 0 {
				fmt.Fprintf(&desc, "    promotion: { from: %s, soak: 5m }\n", a.envs[i-1])
			}
		}
		fmt.Fprintf(&desc, "components:\n  - { name: %s, kustomize: overlays }\n  - { name: %s-worker, helm: worker }\nservices:\n", a.name, a.name)
		var images []string
		for _, s := range a.services {
			comp := a.name
			if s == "worker" {
				comp += "-worker"
			}
			img := "ghcr.io/o/" + a.name + "-" + s
			images = append(images, img)
			fmt.Fprintf(&desc, "  - { name: %s, image: %s, component: %s }\n", s, img, comp)
		}
		blob := hexOf("blob " + a.name)
		tree = append(tree, map[string]any{"path": a.name, "type": "tree", "sha": hexOf("tree " + a.name)},
			map[string]any{"path": a.name + "/app.yaml", "type": "blob", "sha": blob})
		data["repos/o/m/git/blobs/"+blob] = b64(desc.String())

		at := t0.Add(-2 * time.Hour)
		data["repos/"+repo+"/commits?sha=main&per_page=100"] = []any{
			map[string]any{"sha": c2, "commit": map[string]any{"committer": map[string]any{"date": at}}},
			map[string]any{"sha": c1, "commit": map[string]any{"committer": map[string]any{"date": at.Add(-48 * time.Hour)}}},
		}
		var runs []any
		id := 1000 * (len(data) + 1)
		for _, s := range a.services {
			for _, c := range []string{c2, c1} {
				id++
				runs = append(runs, map[string]any{"id": id, "name": "release-" + s, "head_branch": "main", "head_sha": c, "event": "push",
					"status": "completed", "conclusion": "success", "html_url": "r", "created_at": at, "updated_at": at, "run_started_at": at})
				data[fmt.Sprintf("repos/%s/actions/runs/%d/jobs?per_page=50", repo, id)] = map[string]any{"jobs": []any{
					map[string]any{"name": "test", "status": "completed", "conclusion": "success", "html_url": "j", "started_at": at, "completed_at": at},
					map[string]any{"name": "image / build", "status": "completed", "conclusion": "success", "html_url": "j", "started_at": at, "completed_at": at},
				}}
			}
		}
		data["repos/"+repo+"/actions/runs?per_page=100"] = map[string]any{"workflow_runs": runs}

		for i, e := range a.envs {
			tags := []string{short(c1)}
			if i == 0 {
				tags = append(tags, short(c2)) // dev runs c2, the others still c1
			}
			for _, comp := range []string{"overlays/" + e + "/kustomization.yaml", "worker/values-" + e + ".yaml"} {
				p := a.name + "/" + comp
				var commits []any
				for j, tag := range tags {
					mc := hexOf(p + tag)
					commits = append([]any{map[string]any{"sha": mc, "html_url": "mc",
						"commit": map[string]any{"committer": map[string]any{"date": t0.Add(-time.Duration(48-j*47) * time.Hour)}}}}, commits...)
					if strings.HasPrefix(comp, "worker") {
						data["repos/o/m/contents/"+p+"?ref="+mc] = b64("image:\n  repository: ghcr.io/o/" + a.name + "-worker\n  tag: \"" + tag + "\"\n")
					} else {
						var y strings.Builder
						y.WriteString("images:\n")
						for _, img := range images {
							fmt.Fprintf(&y, "  - name: %s\n    newTag: \"%s\"\n", img, tag)
						}
						data["repos/o/m/contents/"+p+"?ref="+mc] = b64(y.String())
					}
				}
				data["repos/o/m/commits?path="+strings.ReplaceAll(p, "/", "%2F")+"&per_page=40"] = commits
			}
		}
	}
	data["repos/o/m/git/trees/main?recursive=1"] = map[string]any{"tree": tree}
	return data
}

type noKube struct{}

func (noKube) Get(path string, v any) error { return errors.New("no cluster in this test") }

// fleetBoard: a board on the fleet, its GitHub the fake server, its clock *clock.
func fleetBoard(t *testing.T, f *fakeServer, clock *time.Time) *Board {
	t.Helper()
	cfg, err := ParseConfig([]byte(fleetConfig))
	if err != nil {
		t.Fatal(err)
	}
	b := NewBoard(cfg)
	b.gh.api = f.URL + "/"
	b.kube = noKube{}
	b.now = func() time.Time { return *clock }
	return b
}

// TestRequestsPerCycle measures the requests of one refresh of the fleet (go test -run RequestsPerCycle -v):
// cold, warm without discovery (it runs every 2 minutes), warm with discovery, and with activity in one repository.
func TestRequestsPerCycle(t *testing.T) {
	f := newFakeServer(fleetData())
	defer f.Close()
	clock := t0
	b := fleetBoard(t, f, &clock)
	cycle := func(name string) (counted int) {
		b.refresh()
		st, total, repeated := f.take()
		if st[404] > 0 {
			t.Fatalf("%s: %d paths missing from the fixture", name, st[404])
		}
		t.Logf("%-28s %3d requests: %3d × 200 (counted), %3d × 304 (free); same URL more than once: %d", name, total, st[200], st[304], len(repeated))
		if len(repeated) > 0 { // one cycle reads a path once, whatever number of pipelines read it
			t.Errorf("%s: read more than once: %v", name, repeated)
		}
		return st[200]
	}
	cycle("cold (with discovery)")
	if n := len(b.Pipelines()); n != 12 {
		t.Fatalf("%d pipelines, want 12", n)
	}
	for _, s := range b.All() {
		if s.Error != "" {
			t.Fatalf("%s: %s", s.P.ID, s.Error)
		}
	}
	clock = clock.Add(30 * time.Second)
	idle := cycle("warm, idle")
	clock = clock.Add(2 * time.Minute)
	idleDiscover := cycle("warm, idle, with discovery")
	// activity: a new commit on o/shop and its release running
	c3 := hexOf("shop3")
	f.set("repos/o/shop/commits?sha=main&per_page=100", []any{map[string]any{"sha": c3, "commit": map[string]any{"committer": map[string]any{"date": t0}}},
		map[string]any{"sha": hexOf("shop2")}, map[string]any{"sha": hexOf("shop1")}})
	runs := f.data["repos/o/shop/actions/runs?per_page=100"].(map[string]any)["workflow_runs"].([]any)
	f.set("repos/o/shop/actions/runs?per_page=100", map[string]any{"workflow_runs": append([]any{map[string]any{"id": 99, "name": "release-web",
		"head_branch": "main", "head_sha": c3, "event": "push", "status": "in_progress", "html_url": "r", "created_at": t0, "updated_at": t0, "run_started_at": t0}}, runs...)})
	f.set("repos/o/shop/actions/runs/99/jobs?per_page=50", map[string]any{"jobs": []any{map[string]any{"name": "test", "status": "in_progress", "html_url": "j", "started_at": t0}}})
	clock = clock.Add(30 * time.Second)
	active := cycle("warm, one repository active")
	clock = clock.Add(30 * time.Second)
	if again := cycle("warm, idle again"); again != 0 {
		t.Errorf("unchanged data: %d requests counted, want only 304s", again)
	}
	if idle != 0 || idleDiscover != 0 || active != 3 { // the commits and runs of o/shop, the jobs of its running release
		t.Errorf("counted: idle %d, idle with discovery %d, active %d", idle, idleDiscover, active)
	}
	t.Logf("counted per hour at 30s (a discovery every 4th cycle): idle %d, one repository active every cycle %d",
		90*idle+30*idleDiscover, 90*active+30*(active+idleDiscover-idle))
	t.Logf("counted per hour at 2m (a discovery every cycle): idle %d, active %d", 30*idleDiscover, 30*(active+idleDiscover-idle))
}
