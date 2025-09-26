package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	includeSelector = `{{- include "auki.selectorLabelsFor" (dict "ctx" $ctx "base" $base) | nindent %d }}`
	includeMeta     = `{{- include "auki.metaLabelsFor"      (dict "ctx" $ctx "base" $base) | nindent %d }}`
	headerBlockTmpl = `{{- $base := "%s" -}}
{{- $ctx := . -}}
{{- $name := include "auki.nameFor" (dict "ctx" $ctx "base" $base) -}}

`
)

// keyCtx tracks YAML key path + indent while scanning.
type keyCtx struct {
	indent int
	key    string
}

func main() {
	root := "."
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Base(path) == "deployment.yaml" &&
			strings.Contains(path, string(filepath.Separator)+"templates"+string(filepath.Separator)) {
			files = append(files, path)
		}
		return nil
	})
	must(err)

	if len(files) == 0 {
		fmt.Println("no templates/**/deployment.yaml files found")
		return
	}

	for _, f := range files {
		if err := processFile(f); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR %s: %v\n", f, err)
		} else {
			fmt.Printf("updated %s\n", f)
		}
	}
}

func processFile(path string) error {
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(orig)

	// If already migrated (header present), still run replacements safely.
	hasHeader := strings.Contains(content, `include "auki.nameFor"`)

	base, err := detectBaseFromTopMetadataName(content)
	if err != nil {
		return fmt.Errorf("detect base: %w", err)
	}
	if strings.HasSuffix(base, "-green") {
		base = strings.TrimSuffix(base, "-green")
	}

	lines := splitKeepNL(content)

	kind := "" // current K8s kind
	var stack []keyCtx

	var buf bytes.Buffer
	sc := newScanner(lines)

	// Write header once at the very top if missing
	if !hasHeader {
		buf.WriteString(fmt.Sprintf(headerBlockTmpl, base))
	}

	// Regex helpers
	reKey := regexp.MustCompile(`^(\s*)([A-Za-z0-9_.-]+):(?:\s*(.*))?$`)
	reKind := regexp.MustCompile(`^\s*kind:\s*([A-Za-z0-9]+)\s*$`)
	reDashName := regexp.MustCompile(`^(\s*)-\s*name:\s*(.+?)\s*$`)

	// Skip state: when we rewrite labels/matchLabels, ignore old map entries
	skipActive := false
	skipIndent := -1

	for sc.Scan() {
		line := sc.Text()

		// If we're skipping the old labels/matchLabels block, ignore lines indented under it
		if skipActive {
			if countIndent(line) > skipIndent {
				continue // still inside the old block
			}
			// block ended
			skipActive = false
			// fall through to process this line normally
		}

		// Track kind across docs
		if m := reKind.FindStringSubmatch(line); m != nil {
			kind = m[1]
		}

		// Update path stack on key lines
		if m := reKey.FindStringSubmatch(line); m != nil {
			indent := len(m[1])
			key := m[2]
			val := m[3]

			// shrink to parent
			for len(stack) > 0 && indent <= stack[len(stack)-1].indent {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, keyCtx{indent: indent, key: key})

			// Current dot path "a.b.c"
			p := pathOf(stack)

			// ---- Rewrites that MUST happen on the key line itself ----

			// 1) Top-level metadata.name -> {{ $name }}
			if kind == "Deployment" && p == "metadata.name" {
				buf.WriteString(spaces(indent) + "name: {{ $name }}\n")
				continue
			}

			// 2) metadata.labels (scalar or map) -> normalize and inject META labels
			if kind == "Deployment" && p == "metadata.labels" {
				buf.WriteString(spaces(indent) + "labels:\n")
				buf.WriteString(fmt.Sprintf(includeMeta, indent+2))
				buf.WriteString("\n")
				// Skip any original block contents indented under this key
				skipActive = true
				skipIndent = indent
				continue
			}

			// 3) spec.template.metadata.labels -> normalize and inject SELECTOR labels
			if kind == "Deployment" && p == "spec.template.metadata.labels" {
				buf.WriteString(spaces(indent) + "labels:\n")
				buf.WriteString(fmt.Sprintf(includeSelector, indent+2))
				buf.WriteString("\n")
				skipActive = true
				skipIndent = indent
				continue
			}

			// 4) spec.selector.matchLabels -> normalize and inject SELECTOR labels
			if kind == "Deployment" && p == "spec.selector.matchLabels" {
				buf.WriteString(spaces(indent) + "matchLabels:\n")
				buf.WriteString(fmt.Sprintf(includeSelector, indent+2))
				buf.WriteString("\n")
				skipActive = true
				skipIndent = indent
				continue
			}

			// 5) containers: first item name -> {{ $base }}
			if kind == "Deployment" && p == "spec.template.spec.containers" && val == "" {
				// rewrite happens on list item lines below
			}
		}

		// Replace container list item " - name: ..." anywhere under containers
		if kind == "Deployment" {
			if m := reDashName.FindStringSubmatch(line); m != nil {
				indent := m[1]
				// Only rewrite when we're under ...spec.template.spec.containers
				if strings.Contains(pathOf(stack), "spec.template.spec.containers") {
					buf.WriteString(fmt.Sprintf("%s- name: {{ $base }}\n", indent))
					continue
				}
			}
		}

		// Default: copy original line
		buf.WriteString(line)
	}

	if err := sc.Err(); err != nil {
		return err
	}

	out := buf.String()

	// Avoid double header if somehow present twice
	if !hasHeader && strings.Count(out, `include "auki.nameFor"`) > 1 {
		parts := strings.SplitN(out, `{{- $name := include "auki.nameFor" (dict "ctx" $ctx "base" $base) -}}`, 2)
		if len(parts) == 2 {
			after := strings.SplitN(parts[1], "\n", 2)
			if len(after) == 2 {
				out = parts[0] + `{{- $name := include "auki.nameFor" (dict "ctx" $ctx "base" $base) -}}` + "\n" + after[1]
			}
		}
	}

	// Write back (with .bak)
	backup := path + ".bak"
	if err := os.WriteFile(backup, orig, 0644); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	if err := os.WriteFile(path, []byte(out), 0644); err != nil {
		return fmt.Errorf("write updated: %w", err)
	}
	return nil
}

func detectBaseFromTopMetadataName(content string) (string, error) {
	lines := strings.Split(content, "\n")
	kind := ""
	inMeta := false
	metaIndent := -1
	reKind := regexp.MustCompile(`^\s*kind:\s*([A-Za-z0-9]+)\s*$`)
	reMeta := regexp.MustCompile(`^(\s*)metadata:\s*$`)
	reName := regexp.MustCompile(`^\s*name:\s*(.+?)\s*$`)

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := reKind.FindStringSubmatch(line); m != nil {
			kind = m[1]
			inMeta = false
			metaIndent = -1
			continue
		}
		if kind != "Deployment" {
			continue
		}
		if !inMeta {
			if m := reMeta.FindStringSubmatch(line); m != nil {
				inMeta = true
				metaIndent = len(m[1])
				continue
			}
		} else {
			if len(strings.TrimSpace(line)) == 0 {
				continue
			}
			if leadingSpaces(line) <= metaIndent {
				inMeta = false
				continue
			}
			if leadingSpaces(line) == metaIndent+2 && reName.MatchString(strings.TrimLeft(line, " ")) {
				m := reName.FindStringSubmatch(strings.TrimLeft(line, " "))
				val := strings.TrimSpace(m[1])
				val = strings.Trim(val, `"'`)
				if strings.Contains(val, "{{") {
					return "", errors.New("metadata.name already templated; abort base detection")
				}
				return val, nil
			}
		}
	}
	return "", errors.New("top-level metadata.name not found for Deployment")
}

func ensureSelectorInclude(buf bytes.Buffer, nindent int, reInc *regexp.Regexp) bytes.Buffer {
	s := buf.String()
	// check tail to avoid duplicate injection
	tail := s
	if len(tail) > 800 {
		tail = tail[len(tail)-800:]
	}
	if reInc.MatchString(tail) {
		return buf
	}
	buf.WriteString(fmt.Sprintf(includeSelector, nindent))
	buf.WriteString("\n")
	return buf
}

func isLabelsKeyLine(line string) bool {
	trim := strings.TrimSpace(line)
	return trim == "labels:" || strings.HasSuffix(strings.TrimRight(line, " "), "labels:")
}

func isAppLabelLine(line string, expectedIndent int) bool {
	trim := strings.TrimSpace(line)
	if !strings.HasPrefix(trim, "app:") {
		return false
	}
	return leadingSpaces(line) >= expectedIndent
}

func leadingSpaces(s string) int { return countIndent(s) }

func countIndent(s string) int {
	i := 0
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return i
}

type scanner struct {
	lines []string
	i     int
	err   error
}

func newScanner(lines []string) *scanner { return &scanner{lines: lines} }
func (s *scanner) Scan() bool {
	if s.i >= len(s.lines) {
		return false
	}
	s.i++
	return true
}
func (s *scanner) Text() string { return s.lines[s.i-1] }
func (s *scanner) Err() error   { return s.err }

func splitKeepNL(s string) []string {
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Split(bufio.ScanLines)
	var out []string
	for sc.Scan() {
		out = append(out, sc.Text()+"\n")
	}
	// preserve no-trailing-newline case
	if len(s) > 0 && s[len(s)-1] != '\n' && len(out) > 0 {
		out[len(out)-1] = strings.TrimSuffix(out[len(out)-1], "\n")
	}
	return out
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(" ", n)
}

func pathOf(stack []keyCtx) string {
	if len(stack) == 0 {
		return ""
	}
	var parts []string
	for _, c := range stack {
		parts = append(parts, c.key)
	}
	return strings.Join(parts, ".")
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
