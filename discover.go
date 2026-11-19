package main

import (
	"encoding/base64"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Discover: pipelines from application descriptors (<app>/app.yaml) in a manifests repository, so registering an
// application there is enough (format: gitops-manifests storefront/app.yaml). One pipeline per service.
type Discover struct {
	Repo      string            `yaml:"repo"`   // manifests repository, owner/name
	Branch    string            `yaml:"branch"` // default main
	Push      *Push             `yaml:"push"`
	Jobs      map[string]string `yaml:"jobs"` // release workflow: job name → step name
	Promotion struct {
		Workflow  string `yaml:"workflow"` // in Repo; inputs app, image, from, to, version (+ soakInput)
		SoakInput string `yaml:"soakInput"`
	} `yaml:"promotion"`
}

type appDesc struct {
	Name         string `yaml:"name"`
	Repo         string `yaml:"repo"`
	Environments []struct {
		Name      string `yaml:"name"`
		Promotion *struct {
			From string   `yaml:"from"`
			Soak Duration `yaml:"soak"`
		} `yaml:"promotion"`
	} `yaml:"environments"`
	Components []struct {
		Name      string `yaml:"name"`
		Kustomize string `yaml:"kustomize"`
		Helm      string `yaml:"helm"`
	} `yaml:"components"`
	Services []struct {
		Name      string `yaml:"name"`
		Image     string `yaml:"image"`
		Component string `yaml:"component"`
		Release   string `yaml:"release"` // default release-<name>
		Workload  struct {
			Kind      string `yaml:"kind"`
			Name      string `yaml:"name"`
			Container string `yaml:"container"`
		} `yaml:"workload"`
	} `yaml:"services"`
}

var descPath = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*)/app\.yaml$`)

// discover lists */app.yaml at the branch head and turns each service into a pipeline (validated like configured
// ones). A descriptor that does not parse is reported and skipped; the others still load.
func discover(gh GitHub, d *Discover) (pipes []*Pipeline, errs []error, ok bool) {
	branch := or(d.Branch, "main")
	var tree struct {
		Tree []struct{ Path, Type, SHA string } `json:"tree"`
	}
	if err := gh.Get(fmt.Sprintf("repos/%s/git/trees/%s?recursive=1", d.Repo, branch), false, &tree); err != nil {
		return nil, []error{err}, false
	}
	var out []*Pipeline
	for _, t := range tree.Tree {
		m := descPath.FindStringSubmatch(t.Path)
		if m == nil || t.Type != "blob" {
			continue
		}
		var blob struct {
			Content string `json:"content"`
		}
		if err := gh.Get(fmt.Sprintf("repos/%s/git/blobs/%s", d.Repo, t.SHA), true, &blob); err != nil {
			errs = append(errs, err)
			continue
		}
		raw, _ := base64.StdEncoding.DecodeString(stripNL(blob.Content))
		ps, err := pipelinesOf(raw, m[1], d)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Path, err))
			continue
		}
		out = append(out, ps...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, errs, true
}

func pipelinesOf(raw []byte, dir string, d *Discover) ([]*Pipeline, error) {
	var a appDesc
	if err := yaml.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.Name == "" || a.Repo == "" || len(a.Environments) == 0 || len(a.Components) == 0 {
		return nil, fmt.Errorf("name, repo, environments and components required")
	}
	var out []*Pipeline
	for _, s := range a.Services {
		var comp *struct {
			Name      string `yaml:"name"`
			Kustomize string `yaml:"kustomize"`
			Helm      string `yaml:"helm"`
		}
		for i := range a.Components {
			if a.Components[i].Name == s.Component {
				comp = &a.Components[i]
			}
		}
		if comp == nil {
			return nil, fmt.Errorf("service %s: no component %q", s.Name, s.Component)
		}
		p := &Pipeline{ID: a.Name + "-" + s.Name, Title: a.Name + " · " + s.Name, Repo: a.Repo, Push: d.Push, discoveredFrom: d.Repo,
			Release: &Workflow{Name: or(s.Release, "release-"+s.Name), Jobs: d.Jobs}}
		if p.Push == nil {
			p.Push = &Push{Label: "push"}
		}
		for _, env := range a.Environments {
			e := &Environment{Name: env.Name}
			e.Manifests.Repo, e.Manifests.Image = d.Repo, s.Image
			if comp.Helm != "" {
				e.Manifests.Path = path.Join(dir, comp.Helm, "values-"+env.Name+".yaml")
			} else {
				e.Manifests.Path = path.Join(dir, comp.Kustomize, env.Name, "kustomization.yaml")
			}
			e.ArgoCD.App = comp.Name + "-" + env.Name
			e.Workload.Namespace = a.Name + "-" + env.Name
			e.Workload.Kind = s.Workload.Kind
			e.Workload.Name = or(s.Workload.Name, s.Name)
			e.Workload.Container = or(s.Workload.Container, s.Name)
			if pr := env.Promotion; pr != nil && d.Promotion.Workflow != "" {
				e.Promotion = &Promotion{From: pr.From, Soak: pr.Soak,
					// promote.yml titles its PR "promote <app> <image name> → <env>"
					PRTitle: "^" + regexp.QuoteMeta("promote "+a.Name+" "+path.Base(s.Image)+" → "+env.Name) + "$"}
				e.Promotion.Workflow.File = d.Promotion.Workflow
				e.Promotion.Workflow.Repo = d.Repo
				e.Promotion.Workflow.SoakInput = d.Promotion.SoakInput
				e.Promotion.Workflow.Inputs = map[string]string{"app": a.Name, "image": s.Image, "from": pr.From, "to": env.Name}
			}
			p.Environments = append(p.Environments, e)
		}
		if err := p.validate(); err != nil {
			return nil, fmt.Errorf("service %s: %w", s.Name, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Pipelines: the configured ones, then the discovered ones (refreshed at most every discoverEvery).
func (b *Board) Pipelines() []*Pipeline {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append(append([]*Pipeline{}, b.cfg.Pipelines...), b.discovered...)
}

const discoverEvery = 2 * time.Minute

func (b *Board) rediscover() {
	if len(b.cfg.Discover) == 0 {
		return
	}
	b.mu.RLock()
	fresh := !b.discoveredAt.IsZero() && b.now().Sub(b.discoveredAt) < discoverEvery
	b.mu.RUnlock()
	if fresh {
		return
	}
	var all []*Pipeline
	var msgs []string
	seen := map[string]bool{}
	for _, p := range b.cfg.Pipelines {
		seen[p.ID] = true
	}
	b.mu.RLock()
	prev := b.discovered
	b.mu.RUnlock()
	for _, d := range b.cfg.Discover {
		ps, errs, ok := discover(b.gh, d)
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		if !ok { // the repository could not be read: keep what this source had
			for _, p := range prev {
				if p.discoveredFrom == d.Repo {
					ps = append(ps, p)
				}
			}
		}
		for _, p := range ps {
			if seen[p.ID] {
				msgs = append(msgs, "discovered pipeline "+p.ID+" duplicates a configured one: skipped")
				continue
			}
			seen[p.ID] = true
			all = append(all, p)
		}
	}
	b.mu.Lock()
	b.discovered, b.discoveredAt, b.discoverErr = all, b.now(), strings.Join(msgs, "; ")
	b.mu.Unlock()
}
