package main

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Templating, applied to the YAML before it is decoded and validated (so every field can use it, and the result is
// checked like hand-written configuration). `delivery-board -print-config` shows the result.
//
//	services: [web, api]     one pipeline per service: id <id>-<service>, title "<title> · <service>";
//	                         $(service) anywhere in the pipeline is the service
//	defaults:
//	  environment: {…}       merged under every environment (the environment's own keys win)
//	  promotion: {…}         merged under every environment's promotion
//	$(env)                   inside an environment: its name
//	$(from)                  inside an environment: its promotion's from
var placeholder = regexp.MustCompile(`\$\((service|env|from)\)`)

func expandConfig(src []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the configuration must be a mapping")
	}
	pipes := get(doc.Content[0], "pipelines")
	if pipes != nil && pipes.Kind == yaml.SequenceNode {
		var out []*yaml.Node
		for _, p := range pipes.Content {
			expanded, err := expandPipeline(p)
			if err != nil {
				return nil, err
			}
			out = append(out, expanded...)
		}
		pipes.Content = out
	}
	var left []string
	walk(doc.Content[0], func(n *yaml.Node) {
		if placeholder.MatchString(n.Value) {
			left = append(left, fmt.Sprintf("line %d: %q", n.Line, n.Value))
		}
	})
	if len(left) > 0 {
		return nil, fmt.Errorf("placeholders outside their scope ($(service) needs services, $(env) / $(from) belong in an environment): %s",
			strings.Join(left, ", "))
	}
	return yaml.Marshal(doc.Content[0])
}

func expandPipeline(p *yaml.Node) ([]*yaml.Node, error) {
	if p.Kind != yaml.MappingNode {
		return []*yaml.Node{p}, nil
	}
	id := scalar(p, "id")
	services := []string{""}
	if s := take(p, "services"); s != nil {
		if s.Kind != yaml.SequenceNode || len(s.Content) == 0 {
			return nil, fmt.Errorf("pipeline %q: services must be a non-empty list", id)
		}
		services = nil
		for _, n := range s.Content {
			if !idRe.MatchString(n.Value) {
				return nil, fmt.Errorf("pipeline %q: service %q must match %s", id, n.Value, idRe)
			}
			services = append(services, n.Value)
		}
	}
	var defEnv, defPromo *yaml.Node
	if d := take(p, "defaults"); d != nil {
		if d.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("pipeline %q: defaults must be a mapping", id)
		}
		for i := 0; i < len(d.Content); i += 2 {
			switch k, v := d.Content[i].Value, d.Content[i+1]; k {
			case "environment":
				defEnv = v
			case "promotion":
				defPromo = v
			default:
				return nil, fmt.Errorf("pipeline %q: defaults.%s: only environment and promotion", id, k)
			}
		}
	}
	var out []*yaml.Node
	for _, svc := range services {
		q := clone(p)
		if envs := get(q, "environments"); envs != nil && envs.Kind == yaml.SequenceNode {
			for i, e := range envs.Content {
				if defEnv != nil {
					e = merge(clone(defEnv), e)
				}
				if pr := get(e, "promotion"); pr != nil && defPromo != nil {
					set(e, "promotion", merge(clone(defPromo), pr))
				}
				name, from := scalar(e, "name"), ""
				if pr := get(e, "promotion"); pr != nil {
					from = scalar(pr, "from")
				}
				subst(e, map[string]string{"$(env)": name, "$(from)": from})
				envs.Content[i] = e
			}
		}
		if svc != "" {
			subst(q, map[string]string{"$(service)": svc})
			title := or(scalar(q, "title"), id)
			set(q, "id", str(id+"-"+svc))
			set(q, "title", str(title+" · "+svc))
		}
		out = append(out, q)
	}
	return out, nil
}

// ── yaml.Node helpers ──────────────────────────────────────────────────────────────────────────────────────────────

func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func take(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return v
		}
	}
	return nil
}

func set(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, str(key), v)
}

func scalar(m *yaml.Node, key string) string {
	if v := get(m, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

func str(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

func clone(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, x := range n.Content {
		c.Content[i] = clone(x)
	}
	return &c
}

// merge src into dst (mappings recursively; anything else: src wins) and return dst.
func merge(dst, src *yaml.Node) *yaml.Node {
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		return src
	}
	for i := 0; i+1 < len(src.Content); i += 2 {
		k, v := src.Content[i].Value, src.Content[i+1]
		if d := get(dst, k); d != nil {
			set(dst, k, merge(d, v))
		} else {
			set(dst, k, v)
		}
	}
	return dst
}

// subst replaces placeholders in every scalar value (not in keys).
func subst(n *yaml.Node, vars map[string]string) {
	switch n.Kind {
	case yaml.ScalarNode:
		for k, v := range vars {
			n.Value = strings.ReplaceAll(n.Value, k, v)
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			subst(n.Content[i], vars)
		}
	default:
		for _, c := range n.Content {
			subst(c, vars)
		}
	}
}

func walk(n *yaml.Node, f func(*yaml.Node)) {
	if n.Kind == yaml.ScalarNode {
		f(n)
	}
	for _, c := range n.Content {
		walk(c, f)
	}
}
