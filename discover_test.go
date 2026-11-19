package main

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

var testDiscover = &Discover{Repo: "MIERAI/gitops-manifests", Push: &Push{Label: "提交"}, Jobs: map[string]string{"test": "test"}}

func init() {
	testDiscover.Promotion.Workflow, testDiscover.Promotion.SoakInput = "promote.yml", "soak"
}

// storefront/app.yaml (a copy from gitops-manifests) → the pipelines that used to be configured by hand.
func TestPipelinesFromDescriptor(t *testing.T) {
	raw, err := os.ReadFile("testdata/storefront-app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := pipelinesOf(raw, "storefront", testDiscover)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range ps {
		ids = append(ids, p.ID)
	}
	if strings.Join(ids, ",") != "storefront-web,storefront-api,storefront-worker" {
		t.Fatalf("pipelines %v", ids)
	}
	web, worker := ps[0], ps[2]
	if web.Title != "storefront · web" || web.Repo != "MIERAI/storefront" || web.Release.Name != "release-web" || web.Push.Label != "提交" {
		t.Errorf("web %+v %+v", web, web.Release)
	}
	dev, staging, prod := web.Environments[0], web.Environments[1], web.Environments[2]
	if dev.Manifests.Path != "storefront/overlays/dev/kustomization.yaml" || dev.Manifests.Format != "kustomize" ||
		dev.ArgoCD.App != "storefront-dev" || dev.Workload.Namespace != "storefront-dev" || dev.Workload.Name != "web" ||
		dev.Workload.Kind != "Deployment" || dev.Promotion != nil {
		t.Errorf("web dev %+v %+v %+v", dev.Manifests, dev.ArgoCD, dev.Workload)
	}
	if pr := staging.Promotion; pr == nil || pr.From != "dev" || pr.Soak.Minutes() != 5 || !pr.prTitle.MatchString("promote storefront storefront-web → staging") ||
		pr.prTitle.MatchString("promote storefront storefront-api → staging") || pr.Workflow.Inputs["app"] != "storefront" ||
		pr.Workflow.Inputs["image"] != "ghcr.io/mierai/storefront-web" || pr.Workflow.Inputs["to"] != "staging" || pr.Workflow.SoakInput != "soak" {
		t.Errorf("web staging promotion %+v", staging.Promotion)
	}
	if prod.Promotion.From != "staging" || prod.Promotion.Soak.Minutes() != 10 {
		t.Errorf("web prod promotion %+v", prod.Promotion)
	}
	w := worker.Environments[1]
	if w.Manifests.Path != "storefront/worker/values-staging.yaml" || w.Manifests.Format != "helm" || w.ArgoCD.App != "storefront-worker-staging" ||
		w.Workload.Namespace != "storefront-staging" || w.Workload.Name != "worker" {
		t.Errorf("worker staging %+v %+v %+v", w.Manifests, w.ArgoCD, w.Workload)
	}
}

func TestDiscoverFromTree(t *testing.T) {
	raw, _ := os.ReadFile("testdata/storefront-app.yaml")
	gh := fakeGitHub{
		"repos/MIERAI/gitops-manifests/git/trees/main?recursive=1": map[string]any{"tree": []any{
			map[string]any{"path": "storefront", "type": "tree", "sha": "t1"},
			map[string]any{"path": "storefront/app.yaml", "type": "blob", "sha": "b1"},
			map[string]any{"path": "storefront/worker/app.yaml", "type": "blob", "sha": "b2"}, // not top level: ignored
			map[string]any{"path": "broken/app.yaml", "type": "blob", "sha": "b3"},
		}},
		"repos/MIERAI/gitops-manifests/git/blobs/b1": map[string]any{"content": base64.StdEncoding.EncodeToString(raw)},
		"repos/MIERAI/gitops-manifests/git/blobs/b3": map[string]any{"content": base64.StdEncoding.EncodeToString([]byte("name: broken\n"))},
	}
	ps, errs, ok := discover(gh, testDiscover)
	if !ok || len(ps) != 3 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken/app.yaml") {
		t.Fatalf("ok %v, %d pipelines, errs %v", ok, len(ps), errs)
	}
	// the repository cannot be read: not ok (the board keeps what it had)
	if _, _, ok := discover(fakeGitHub{}, testDiscover); ok {
		t.Fatal("discover ok without the tree")
	}
}
