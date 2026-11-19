package main

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config describes the pipelines the board follows. Everything the board shows is derived from these sources at
// every refresh (GitHub + the cluster); the board keeps no state of its own.
type Config struct {
	Title     string      `yaml:"title"`
	Timezone  string      `yaml:"timezone"`
	Interval  Duration    `yaml:"interval"`
	MaxRuns   int         `yaml:"maxRuns"` // per pipeline
	Pipelines []*Pipeline `yaml:"pipelines"`
	Discover  []*Discover `yaml:"discover"` // pipelines from app.yaml descriptors in manifests repositories

	loc *time.Location
}

// Pipeline: one application, from its changes (pull requests, pushes) to the last environment.
type Pipeline struct {
	ID           string         `yaml:"id"`
	Title        string         `yaml:"title"`
	Repo         string         `yaml:"repo"`   // application repository, owner/name
	Branch       string         `yaml:"branch"` // default main
	Changes      []*Change      `yaml:"changes"`
	Push         *Push          `yaml:"push"`
	Release      *Workflow      `yaml:"release"`
	Environments []*Environment `yaml:"environments"`
	Try          *Try           `yaml:"try"`

	discoveredFrom string // the manifests repository of its app.yaml ("" = configured)
}

// Change: a kind of pull request that starts a run (e.g. a design PR). Its steps are followed in order; a run
// reaches the release when the steps yield a commit of the branch (the PR's merge commit, or a follow-up PR's).
type Change struct {
	Label string     `yaml:"label"`
	Head  string     `yaml:"head"`  // regexp on the head branch
	Title string     `yaml:"title"` // regexp on the title (a PR matching either is a change of this kind)
	Items string     `yaml:"items"` // regexp on the changed files: group 1 = an item (e.g. an API) named by the run
	Steps []*StepDef `yaml:"steps"`

	head, title, items *regexp.Regexp
}

// StepDef: exactly one of Workflow, Merge, FollowPR.
//
//	workflow  before the merge step: the runs of that workflow on the PR's branch (each run = one round)
//	          after it: the run of that workflow on the current commit (jobs: job name → step name)
//	merge     the PR is merged; the current commit becomes its merge commit
//	followPR  a PR opened by automation (head branch regexp, $(sha7) = current commit); its merge commit becomes current
type StepDef struct {
	Stage              string            `yaml:"stage"`
	Name               string            `yaml:"name"`
	Workflow           string            `yaml:"workflow"`
	Jobs               map[string]string `yaml:"jobs"`
	FailureNeedsPerson bool              `yaml:"failureNeedsPerson"` // a failed run on an open PR waits for a person
	PersonNote         string            `yaml:"personNote"`
	Merge              bool              `yaml:"merge"`
	FollowPR           string            `yaml:"followPR"`
}

// Push: commits pushed to the branch that no change covers are runs too (from their release).
type Push struct {
	Label string `yaml:"label"`
}

// Workflow: the release workflow on a commit of the branch (builds, then deploys to the first environment).
type Workflow struct {
	Name  string            `yaml:"name"`
	Stage string            `yaml:"stage"`
	Jobs  map[string]string `yaml:"jobs"`
}

type Environment struct {
	Name  string `yaml:"name"`
	Stage string `yaml:"stage"`
	URL   string `yaml:"url"`
	// the desired version: the image tag in a file of the manifests repository (its history = when a version arrived)
	Manifests struct {
		Repo  string `yaml:"repo"`
		Path  string `yaml:"path"`
		Image string `yaml:"image"`
		// kustomize: images[name=image].newTag. helm: a values file; the tag of the mapping {repository: image, tag}
		// (the Helm chart convention), or Key (dot path, e.g. image.tag) for other charts. Default: by the file name.
		Format string `yaml:"format"`
		Key    string `yaml:"key"`
	} `yaml:"manifests"`
	ArgoCD struct {
		Namespace string `yaml:"namespace"`
		App       string `yaml:"app"`
	} `yaml:"argocd"`
	// the running version
	Workload struct {
		Namespace string `yaml:"namespace"`
		Kind      string `yaml:"kind"` // Deployment, StatefulSet, Rollout
		Name      string `yaml:"name"`
		Container string `yaml:"container"`
	} `yaml:"workload"`
	Promotion *Promotion `yaml:"promotion"`
}

// Promotion into this environment: a person starts it (workflow_dispatch → PR → merge), behind a gate.
type Promotion struct {
	From     string   `yaml:"from"`
	Soak     Duration `yaml:"soak"` // the version must have run in From at least this long
	PRTitle  string   `yaml:"prTitle"`
	Workflow struct {
		Repo         string            `yaml:"repo"`
		File         string            `yaml:"file"`
		Ref          string            `yaml:"ref"`
		Inputs       map[string]string `yaml:"inputs"`
		VersionInput string            `yaml:"versionInput"`
		SoakInput    string            `yaml:"soakInput"` // optional: the workflow's input for the soak (minutes), so it re-checks it
	} `yaml:"workflow"`

	prTitle *regexp.Regexp
}

// Try: the delivered items' examples (a JSON catalog in the app repo at the version From runs) + live calls.
type Try struct {
	Catalog     string `yaml:"catalog"`
	Env         string `yaml:"env"`
	InternalURL string `yaml:"internalURL"`
	TokenEnv    string `yaml:"tokenEnv"`
	CurlToken   string `yaml:"curlToken"` // shown in the copied curl command
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}

func ParseConfig(src []byte) (*Config, error) {
	b, err := expandConfig(src)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	if c.Title == "" {
		c.Title = "Delivery"
	}
	if c.Interval.Duration == 0 {
		c.Interval.Duration = 30 * time.Second
	}
	if c.MaxRuns == 0 {
		c.MaxRuns = 12
	}
	loc, err := time.LoadLocation(or(c.Timezone, "UTC"))
	if err != nil {
		return fmt.Errorf("config: timezone: %w", err)
	}
	c.loc = loc
	if len(c.Pipelines) == 0 && len(c.Discover) == 0 {
		return fmt.Errorf("config: no pipelines and nothing to discover")
	}
	for i, d := range c.Discover {
		if strings.Count(d.Repo, "/") != 1 {
			return fmt.Errorf("config: discover[%d].repo must be owner/name", i)
		}
	}
	ids := map[string]bool{}
	for _, p := range c.Pipelines {
		if err := p.validate(); err != nil {
			return fmt.Errorf("config: pipeline %q: %w", p.ID, err)
		}
		if ids[p.ID] {
			return fmt.Errorf("config: pipeline %q twice", p.ID)
		}
		ids[p.ID] = true
	}
	return nil
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (p *Pipeline) validate() error {
	if !idRe.MatchString(p.ID) {
		return fmt.Errorf("id must match %s", idRe)
	}
	if p.Title == "" {
		p.Title = p.ID
	}
	if strings.Count(p.Repo, "/") != 1 {
		return fmt.Errorf("repo must be owner/name")
	}
	p.Branch = or(p.Branch, "main")
	for i, ch := range p.Changes {
		var err error
		if ch.Head == "" && ch.Title == "" {
			return fmt.Errorf("changes[%d]: head or title required", i)
		}
		if ch.head, err = optRe(ch.Head); err != nil {
			return fmt.Errorf("changes[%d].head: %w", i, err)
		}
		if ch.title, err = optRe(ch.Title); err != nil {
			return fmt.Errorf("changes[%d].title: %w", i, err)
		}
		if ch.items, err = optRe(ch.Items); err != nil {
			return fmt.Errorf("changes[%d].items: %w", i, err)
		}
		for j, s := range ch.Steps {
			n := 0
			for _, set := range []bool{s.Workflow != "", s.Merge, s.FollowPR != ""} {
				if set {
					n++
				}
			}
			if n != 1 {
				return fmt.Errorf("changes[%d].steps[%d]: exactly one of workflow, merge, followPR", i, j)
			}
			if s.Stage == "" {
				return fmt.Errorf("changes[%d].steps[%d]: stage required", i, j)
			}
			if s.FollowPR != "" {
				if _, err := regexp.Compile(sha7(s.FollowPR, "0000000")); err != nil {
					return fmt.Errorf("changes[%d].steps[%d].followPR: %w", i, j, err)
				}
			}
			s.Name = or(s.Name, s.Workflow, map[bool]string{true: "merge"}[s.Merge], "follow-up PR")
		}
	}
	if p.Release != nil {
		if p.Release.Name == "" {
			return fmt.Errorf("release.name required")
		}
		p.Release.Stage = or(p.Release.Stage, "Release")
	}
	if p.Push != nil && p.Release == nil {
		return fmt.Errorf("push runs need release")
	}
	if len(p.Environments) == 0 {
		return fmt.Errorf("no environments")
	}
	seen := map[string]bool{}
	for i, e := range p.Environments {
		if e.Name == "" || e.Manifests.Repo == "" || e.Manifests.Path == "" || e.Manifests.Image == "" {
			return fmt.Errorf("environments[%d]: name and manifests.{repo,path,image} required", i)
		}
		e.Stage = or(e.Stage, strings.ToUpper(e.Name[:1])+e.Name[1:])
		if e.Manifests.Format == "" {
			e.Manifests.Format = "helm"
			if base := path.Base(e.Manifests.Path); base == "kustomization.yaml" || base == "kustomization.yml" || base == "Kustomization" {
				e.Manifests.Format = "kustomize"
			}
		}
		if e.Manifests.Format != "kustomize" && e.Manifests.Format != "helm" {
			return fmt.Errorf("environments[%d].manifests.format: kustomize or helm", i)
		}
		if e.Manifests.Key != "" && e.Manifests.Format != "helm" {
			return fmt.Errorf("environments[%d].manifests.key: only for helm values", i)
		}
		e.ArgoCD.Namespace = or(e.ArgoCD.Namespace, "argocd")
		e.Workload.Kind = or(e.Workload.Kind, "Deployment")
		if _, ok := workloadAPI[e.Workload.Kind]; !ok {
			return fmt.Errorf("environments[%d].workload.kind: one of Deployment, StatefulSet, Rollout", i)
		}
		if pr := e.Promotion; pr != nil {
			if !seen[pr.From] {
				return fmt.Errorf("environments[%d].promotion.from: must name an earlier environment", i)
			}
			var err error
			if pr.prTitle, err = optRe(pr.PRTitle); err != nil {
				return fmt.Errorf("environments[%d].promotion.prTitle: %w", i, err)
			}
			pr.Workflow.Repo = or(pr.Workflow.Repo, e.Manifests.Repo)
			pr.Workflow.Ref = or(pr.Workflow.Ref, "main")
			pr.Workflow.VersionInput = or(pr.Workflow.VersionInput, "version")
			if pr.Workflow.File == "" {
				return fmt.Errorf("environments[%d].promotion.workflow.file required", i)
			}
		}
		seen[e.Name] = true
	}
	if t := p.Try; t != nil {
		if t.Catalog == "" || !seen[t.Env] {
			return fmt.Errorf("try: catalog and env (an environment) required")
		}
	}
	return nil
}

func (p *Pipeline) env(name string) *Environment {
	for _, e := range p.Environments {
		if e.Name == name {
			return e
		}
	}
	return nil
}

func optRe(s string) (*regexp.Regexp, error) {
	if s == "" {
		return nil, nil
	}
	return regexp.Compile(s)
}

func or(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// sha7 fills the followPR placeholder ($(sha7); {sha7} is accepted too).
func sha7(pattern, sha string) string {
	return strings.ReplaceAll(strings.ReplaceAll(pattern, "$(sha7)", sha), "{sha7}", sha)
}
