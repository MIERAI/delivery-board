package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The templated example (services + defaults) is exactly the hand-written configuration it replaced.
func TestTemplateEqualsHandWritten(t *testing.T) {
	a, err := LoadConfig("examples/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadConfig("testdata/handwritten.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ya, _ := yaml.Marshal(a)
	yb, _ := yaml.Marshal(b)
	la, lb := strings.Split(string(ya), "\n"), strings.Split(string(yb), "\n")
	for i := 0; i < min(len(la), len(lb)); i++ {
		if la[i] != lb[i] {
			t.Fatalf("templated config differs from the hand-written one at line %d:\n  templated:    %s\n  hand-written: %s\n(context: %s)", i+1, la[i], lb[i], strings.Join(la[max(0, i-6):i], " | "))
		}
	}
	if len(la) != len(lb) {
		t.Fatalf("templated %d lines, hand-written %d", len(la), len(lb))
	}
}

const tplBase = `
pipelines:
  - id: shop
    repo: o/shop
    services: [web, api]
    release: { name: release-$(service) }
    defaults:
      environment:
        manifests: { repo: o/m, path: shop/$(env).yaml, image: img-$(service) }
        workload: { namespace: shop-$(env), name: $(service) }
      promotion:
        workflow: { file: promote.yml, inputs: { from: $(from), to: $(env), image: img-$(service) } }
    environments:
      - name: dev
      - { name: prod, workload: { name: special }, promotion: { from: dev } }
`

func TestServicesAndDefaults(t *testing.T) {
	c, err := ParseConfig([]byte(tplBase))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Pipelines) != 2 {
		t.Fatalf("pipelines: %d", len(c.Pipelines))
	}
	api := c.Pipelines[1]
	if api.ID != "shop-api" || api.Title != "shop · api" || api.Release.Name != "release-api" {
		t.Errorf("pipeline %s %q %s", api.ID, api.Title, api.Release.Name)
	}
	dev, prod := api.Environments[0], api.Environments[1]
	if dev.Manifests.Path != "shop/dev.yaml" || dev.Manifests.Image != "img-api" || dev.Workload.Namespace != "shop-dev" || dev.Workload.Name != "api" {
		t.Errorf("dev %+v %+v", dev.Manifests, dev.Workload)
	}
	// the environment's own keys win, the rest of the mapping is still merged
	if prod.Workload.Name != "special" || prod.Workload.Namespace != "shop-prod" {
		t.Errorf("prod workload %+v", prod.Workload)
	}
	if dev.Promotion != nil {
		t.Error("promotion defaults must not create a promotion")
	}
	if in := prod.Promotion.Workflow.Inputs; in["from"] != "dev" || in["to"] != "prod" || in["image"] != "img-api" {
		t.Errorf("promotion inputs %v", in)
	}
}

func TestTemplateErrors(t *testing.T) {
	for name, c := range map[string]struct{ edit, with, want string }{
		"service placeholder without services":   {"    services: [web, api]\n", "", "placeholders outside their scope"},
		"env placeholder outside an environment": {"release-$(service)", "release-$(env)", "placeholders outside their scope"},
		"bad service name":                       {"[web, api]", "[Web]", "must match"},
		"unknown defaults key":                   {"      promotion:\n", "      stage:\n", "only environment and promotion"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(strings.Replace(tplBase, c.edit, c.with, 1)))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// The shipped example and test data carry no deployment specifics (private or tailnet addresses, home-directory paths).
func TestExamplesAreNeutral(t *testing.T) {
	specific := regexp.MustCompile(`\b(10|100|172|192)\.\d+\.\d+\.\d+\b|~/|/home/|/Users/|\.secrets/`)
	for _, f := range []string{"examples/config.yaml", "testdata/handwritten.yaml", "testdata/storefront-app.yaml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if m := specific.FindString(line); m != "" {
				t.Errorf("%s:%d: environment-specific %q", f, i+1, m)
			}
		}
	}
}
