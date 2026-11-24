package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// ── GitHub ─────────────────────────────────────────────────────────────────────────────────────────────────────────

// GitHub reads the REST API. Objects that cannot change any more (a finished run's jobs, a file at a commit) are
// cached for the life of the process.
type GitHub interface {
	Get(path string, final bool, v any) error
}

type gitHub struct {
	api, token string
	client     *http.Client
	mu         sync.Mutex
	cache      map[string][]byte
}

func newGitHub(token string) *gitHub {
	return &gitHub{api: "https://api.github.com/", token: token, client: &http.Client{Timeout: 20 * time.Second},
		cache: map[string][]byte{}}
}

func (g *gitHub) Get(path string, final bool, v any) error {
	g.mu.Lock()
	b, ok := g.cache[path]
	g.mu.Unlock()
	if !ok {
		var err error
		if b, err = g.do("GET", path, g.token, nil); err != nil {
			return err
		}
		if final {
			g.mu.Lock()
			g.cache[path] = b
			g.mu.Unlock()
		}
	}
	return json.Unmarshal(b, v)
}

func (g *gitHub) do(method, path, token string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, g.api+path, rd)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "delivery-board")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub %s %s: %s %s", method, strings.SplitN(path, "?", 2)[0], resp.Status, trunc(string(b), 160))
	}
	return b, nil
}

type ghCommit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message   string `json:"message"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

type ghPull struct {
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	HTMLURL        string     `json:"html_url"`
	CreatedAt      time.Time  `json:"created_at"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	Head           struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

type ghRun struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	HeadBranch   string    `json:"head_branch"`
	HeadSHA      string    `json:"head_sha"`
	Event        string    `json:"event"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	DisplayTitle string    `json:"display_title"`
	HTMLURL      string    `json:"html_url"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	RunStartedAt time.Time `json:"run_started_at"`
}

type ghJob struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	HTMLURL     string    `json:"html_url"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

// ── Kubernetes ─────────────────────────────────────────────────────────────────────────────────────────────────────

// Kube reads objects from the API server: in the cluster with the service account, or through `kubectl proxy`
// (K8S_API=http://127.0.0.1:8001) for local development.
type Kube interface {
	Get(path string, v any) error
}

type kube struct {
	base, tokenFile string
	client          *http.Client
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

func newKube() *kube {
	if base := os.Getenv("K8S_API"); base != "" {
		return &kube{base: base, client: &http.Client{Timeout: 10 * time.Second}}
	}
	pool := x509.NewCertPool()
	if ca, err := os.ReadFile(saDir + "/ca.crt"); err == nil {
		pool.AppendCertsFromPEM(ca)
	}
	return &kube{base: "https://kubernetes.default.svc", tokenFile: saDir + "/token",
		client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}}
}

func (k *kube) Get(path string, v any) error {
	req, _ := http.NewRequest("GET", k.base+path, nil)
	if k.tokenFile != "" {
		tok, err := os.ReadFile(k.tokenFile) // re-read: projected tokens rotate
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("kube %s: %s", path, resp.Status)
	}
	return json.Unmarshal(b, v)
}

var workloadAPI = map[string]string{
	"Deployment":  "/apis/apps/v1/namespaces/%s/deployments/%s",
	"StatefulSet": "/apis/apps/v1/namespaces/%s/statefulsets/%s",
	"Rollout":     "/apis/argoproj.io/v1alpha1/namespaces/%s/rollouts/%s",
}

type workload struct {
	Spec struct {
		Replicas *int `json:"replicas"`
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas        int    `json:"replicas"`
		UpdatedReplicas int    `json:"updatedReplicas"`
		ReadyReplicas   int    `json:"readyReplicas"`
		Phase           string `json:"phase"` // Rollout
	} `json:"status"`
}

type argoApp struct {
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		OperationState struct {
			FinishedAt *time.Time `json:"finishedAt"`
		} `json:"operationState"`
	} `json:"status"`
}

// EnvState: what an environment runs now.
type EnvState struct {
	Name, Stage, URL string
	Tag              string // running image tag
	Sync, Health     string // Argo CD
	Ready            bool   // every replica updated and ready
	Error            string
	// Observed: a status source of the environment answered. False when it is declared unobserved (its cluster is not
	// the board's) or when neither its workload nor its Argo CD Application could be read: its state is unknown.
	Observed bool
}

func (s *EnvState) Healthy() bool {
	return s.Sync == "Synced" && s.Health == "Healthy" && s.Ready
}

func readEnv(k Kube, e *Environment) *EnvState {
	out := &EnvState{Name: e.Name, Stage: e.Stage, URL: e.URL, Sync: "?", Health: "?"}
	if e.Unobserved {
		out.Error = "无法观测(另一个集群)"
		return out
	}
	w := e.Workload
	if w.Name != "" {
		var d workload
		if err := k.Get(fmt.Sprintf(workloadAPI[w.Kind], w.Namespace, w.Name), &d); err != nil {
			out.Error = trunc(err.Error(), 100)
		} else {
			out.Observed = true
			for i, c := range d.Spec.Template.Spec.Containers {
				if c.Name == w.Container || (w.Container == "" && i == 0) {
					out.Tag = imageTag(c.Image)
				}
			}
			if w.Kind == "Rollout" {
				out.Ready = d.Status.Phase == "Healthy"
			} else {
				want := 1
				if d.Spec.Replicas != nil {
					want = *d.Spec.Replicas
				}
				out.Ready = d.Status.UpdatedReplicas == want && d.Status.ReadyReplicas == want
			}
		}
	}
	if e.ArgoCD.App != "" {
		var a argoApp
		if err := k.Get(fmt.Sprintf("/apis/argoproj.io/v1alpha1/namespaces/%s/applications/%s", e.ArgoCD.Namespace, e.ArgoCD.App), &a); err != nil {
			out.Error = trunc(err.Error(), 100)
		} else {
			out.Observed = true
			out.Sync, out.Health = or(a.Status.Sync.Status, "?"), or(a.Status.Health.Status, "?")
		}
	}
	return out
}

func imageTag(image string) string {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	return ""
}

// trunc shortens s to at most n characters (runes, never bytes: a cut inside a multi-byte character renders as "�")
// and marks the cut with "…" (dropping spaces and dashes before it). It cuts at a word boundary when one lies in the last third (a CJK text has none: it is
// cut at n), and never between a character and what joins it (a combining mark, a variation selector, a ZWJ emoji).
func trunc(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	cut := n
	for i := n; i > n-n/3 && i > 0; i-- {
		if unicode.IsSpace(r[i]) {
			cut = i
			break
		}
	}
	for cut > 0 && (joins(r[cut]) || r[cut-1] == zwj) {
		cut--
	}
	return strings.TrimRightFunc(string(r[:cut]), func(r rune) bool { return unicode.IsSpace(r) || unicode.Is(unicode.Pd, r) }) + "…"
}

const zwj = '\u200d'

// joins: r belongs to the character before it.
func joins(r rune) bool {
	return r == zwj || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Variation_Selector, r) ||
		(r >= 0x1F3FB && r <= 0x1F3FF) // skin tone modifiers
}
