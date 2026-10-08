package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Update sets scalar values in the config file at path, addressed by dotted
// keys (e.g. "tls.cert_file"), keeping comments and layout. Missing keys are
// appended. The result must pass Validate before it replaces the file.
func Update(path string, set map[string]any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: top level is not a mapping", path)
	}
	for key, val := range set {
		if err := setPath(root, strings.Split(key, "."), val); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	enc.Close()
	out := restoreSpacing(buf.Bytes())

	cfg := Default()
	if err := yaml.Unmarshal(out, cfg); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("refusing to write %s: %w", path, err)
	}
	return writeFileAtomic(path, out)
}

// restoreSpacing puts back the blank lines yaml.v3 drops: before a top-level
// comment block and after a nested block, which is how the generated config
// separates its sections.
func restoreSpacing(data []byte) []byte {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var b strings.Builder
	for i, l := range lines {
		if i > 0 && l != "" && l[0] != ' ' {
			prev := lines[i-1]
			startsComment := l[0] == '#' && !strings.HasPrefix(prev, "#")
			endsBlock := strings.HasPrefix(prev, " ") && l[0] != '#'
			if prev != "" && (startsComment || endsBlock) {
				b.WriteByte('\n')
			}
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func setPath(m *yaml.Node, keys []string, val any) error {
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if k.Value != keys[0] {
			continue
		}
		if len(keys) == 1 {
			return setScalar(v, val)
		}
		if v.Kind != yaml.MappingNode {
			// e.g. "tls:" with no value: turn it into a mapping.
			*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		return setPath(v, keys[1:], val)
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: keys[0]}
	v := &yaml.Node{}
	if len(keys) == 1 {
		if err := setScalar(v, val); err != nil {
			return err
		}
	} else {
		*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if err := setPath(v, keys[1:], val); err != nil {
			return err
		}
	}
	m.Content = append(m.Content, k, v)
	return nil
}

func setScalar(n *yaml.Node, val any) error {
	var repl yaml.Node
	if err := repl.Encode(val); err != nil {
		return err
	}
	if repl.Kind != yaml.ScalarNode {
		return errors.New("only scalar values can be set")
	}
	n.Kind, n.Tag, n.Value, n.Style, n.Content = repl.Kind, repl.Tag, repl.Value, repl.Style, nil
	if s, ok := val.(string); ok && s == "" {
		n.Style = yaml.DoubleQuotedStyle
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	perm := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
