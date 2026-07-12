// Package yamledit is the single comment-preserving YAML-editing dialect for
// flywheel's client-owned files (flywheel.yaml, kustomization.yaml, the managed
// namespaces stream). It replaces three hand-rolled mechanisms that had each
// grown their own bugs:
//
//   - byte-splicing keyed on a buggy line-span walk (foot comments and multi-line
//     block scalars extended past the computed span, corrupting the file);
//   - a whole-document re-encode that reformatted the user's file on every edit
//     (blank lines stripped, comment spacing collapsed, CRLF flattened to LF);
//   - a raw line-scanner that missed `resources:  # comment`, mangled 4-space
//     indentation, and refused CRLF files.
//
// Every operation here is SURGICAL: it parses the file into a yaml.Node tree to
// locate the edit precisely, then rewrites only the bytes that actually change —
// a single scalar token, one inserted line, or one re-serialized block — leaving
// every other byte (comments, blank lines, unrelated sections, line endings)
// exactly as the user wrote them.
package yamledit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNoSequenceKey reports that the addressed key is absent or is not a sequence
// (nor an empty/`[]`/null placeholder that can become one). Callers wrap it with
// a file-specific message (e.g. add-app's "missing a `resources:` key").
var ErrNoSequenceKey = errors.New("yamledit: key is not a sequence")

// SetScalar sets the scalar addressed by path (a chain of mapping keys) to value,
// rendered with the given yaml tag (e.g. "!!int", "!!str"). When the key exists,
// ONLY its value token on its own line is rewritten — indentation, the key, the
// spacing before any line comment, the comment itself, and the file's line
// endings are preserved byte-for-byte. When the leaf key is absent it is appended
// into its parent block; when the whole parent chain is absent it is created at
// end of file. Intended for flywheel.yaml's cluster ports and flywheel.version.
func SetScalar(data []byte, path []string, tag, value string) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("yamledit: empty path")
	}
	root, err := rootMapping(data)
	if err != nil {
		return nil, err
	}
	token, err := renderToken(tag, value)
	if err != nil {
		return nil, err
	}

	m := root
	for i := 0; i < len(path)-1; i++ {
		ki := findKey(m, path[i])
		if ki < 0 {
			// The parent chain is missing from here down; only a top-level attach
			// (m is the root) is supported — the only "somehow absent" case the
			// callers need.
			if m != root {
				return nil, fmt.Errorf("yamledit: %q missing under a nested mapping", path[i])
			}
			return appendChain(data, path[i:], token), nil
		}
		v := m.Content[ki+1]
		if v.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("yamledit: %q is not a mapping", path[i])
		}
		m = v
	}

	leaf := path[len(path)-1]
	if li := findKey(m, leaf); li >= 0 {
		return replaceLeafToken(data, m.Content[li+1], token)
	}
	return appendLeaf(data, m, leaf, token), nil
}

// AppendListItem appends the block-sequence scalar item under path (which must
// address a `resources:`-style key). An empty inline `[]` or a bare (null) key is
// rewritten to block form carrying the first item; an existing block sequence is
// appended to AT ITS OWN INDENT (2-space or 4-space alike). It is idempotent: if
// item is already present the input is returned unchanged. CRLF files are edited
// with CRLF line endings. Returns ErrNoSequenceKey if the key is absent or holds
// a non-sequence value.
func AppendListItem(data []byte, path []string, item string) ([]byte, error) {
	root, err := rootMapping(data)
	if err != nil {
		return nil, err
	}
	m := root
	for i := 0; i < len(path)-1; i++ {
		ki := findKey(m, path[i])
		if ki < 0 {
			return nil, ErrNoSequenceKey
		}
		v := m.Content[ki+1]
		if v.Kind != yaml.MappingNode {
			return nil, ErrNoSequenceKey
		}
		m = v
	}
	key := path[len(path)-1]
	ki := findKey(m, key)
	if ki < 0 {
		return nil, ErrNoSequenceKey
	}
	keyNode, valNode := m.Content[ki], m.Content[ki+1]

	switch {
	case valNode.Kind == yaml.SequenceNode && len(valNode.Content) > 0:
		// Existing sequence: dedup, then insert after the last item at the items'
		// own indent (valNode.Column is the dash column).
		for _, it := range valNode.Content {
			if it.Value == item {
				return data, nil // already present — no-op
			}
		}
		if valNode.Style&yaml.FlowStyle != 0 {
			// Non-empty inline `[a, b]`: re-serialize this one key as a block
			// sequence and splice its single line. Rare in kustomizations.
			return spliceSeqAsBlock(data, keyNode, valNode, item)
		}
		indent := valNode.Column - 1
		anchor := lastLineOfNode(valNode)
		return insertLineAfter(data, anchor, strings.Repeat(" ", indent)+"- "+item), nil

	case valNode.Kind == yaml.SequenceNode && len(valNode.Content) == 0,
		valNode.Kind == yaml.ScalarNode && valNode.Tag == "!!null":
		// Empty inline `[]` or a bare `resources:` (null): the key line may carry
		// the `[]` and/or a line comment; rewrite it to a plain `key:` (preserving
		// the comment) and drop the first block item beneath it.
		keyIndent := keyNode.Column - 1
		lineComment := firstNonEmpty(keyNode.LineComment, valNode.LineComment)
		newKeyLine := strings.Repeat(" ", keyIndent) + key + ":"
		if lineComment != "" {
			newKeyLine += "  " + lineComment
		}
		out := replaceLine(data, keyNode.Line, newKeyLine)
		return insertLineAfter(out, keyNode.Line, strings.Repeat(" ", keyIndent+2)+"- "+item), nil

	default:
		return nil, ErrNoSequenceKey
	}
}

// HasSequenceKey reports whether path addresses a sequence-shaped key: an
// existing block/flow sequence (possibly empty) or a bare (null) key that
// AppendListItem would turn into one. Used by add-app's read-only preflight so it
// accepts exactly the files AppendListItem can edit — including
// `resources:  # comment`, which the old line-scanner rejected.
func HasSequenceKey(data []byte, path []string) (bool, error) {
	root, err := rootMapping(data)
	if err != nil {
		return false, err
	}
	m := root
	for i := 0; i < len(path)-1; i++ {
		ki := findKey(m, path[i])
		if ki < 0 {
			return false, nil
		}
		v := m.Content[ki+1]
		if v.Kind != yaml.MappingNode {
			return false, nil
		}
		m = v
	}
	ki := findKey(m, path[len(path)-1])
	if ki < 0 {
		return false, nil
	}
	v := m.Content[ki+1]
	if v.Kind == yaml.SequenceNode {
		return true, nil
	}
	if v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
		return true, nil
	}
	return false, nil
}

// HasNamespace reports whether the multi-document YAML stream in data declares a
// `kind: Namespace` object named ns. It parses each document (rather than
// line-scanning `name:`), so a same-indent `name:` under some other kind cannot
// produce a false positive. Empty/absent input (nil data) yields false.
func HasNamespace(data []byte, ns string) (bool, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if doc.Kind == "Namespace" && doc.Metadata.Name == ns {
			return true, nil
		}
	}
}

// EditBlock locates the top-level key `key` (its value must be a mapping; a fresh
// one is created when the key is absent), hands the value node to fn for
// mutation, then writes back a surgical edit: only the `key:` block's own line
// span is re-serialized from the node tree; every other byte is preserved. When
// the key is absent the freshly rendered block is appended after one blank line.
//
// This is the workspace-block editor behind flywheel add app / publish-app. The
// block's true end line is computed from the NEXT top-level sibling (not by
// walking the block's own nodes), so foot comments and multi-line block scalars
// inside the block no longer fall outside the span (the old maxNodeLine bug).
func EditBlock(data []byte, key string, fn func(val *yaml.Node) error) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, errors.New("yamledit: not a YAML document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("yamledit: top level is not a mapping")
	}

	ki := findKey(root, key)
	var (
		val                *yaml.Node
		startLine, endLine int
		exists             = ki >= 0
	)
	if exists {
		val = root.Content[ki+1]
		if val.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("yamledit: %q is not a mapping", key)
		}
		// Capture the span NOW, from the freshly parsed nodes: replaced/appended
		// nodes carry no source position (Line == 0).
		starts := lineStarts(data)
		startLine = root.Content[ki].Line
		endLine = topLevelBlockLastLine(data, starts, root, ki)
	} else {
		val = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}

	if err := fn(val); err != nil {
		return nil, err
	}
	block, err := marshalBlock(key, val)
	if err != nil {
		return nil, err
	}
	if exists {
		return spliceLines(data, startLine, endLine, block), nil
	}
	return appendBlock(data, block), nil
}

// ---- node navigation ----

func rootMapping(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, errors.New("yamledit: not a YAML document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("yamledit: top level is not a mapping")
	}
	return root, nil
}

// findKey returns the index of key's scalar node in mapping m, or -1.
func findKey(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// renderToken renders a scalar value to its inline token text (no trailing
// newline), using yaml's own quoting rules for the tag.
func renderToken(tag, value string) (string, error) {
	out, err := yaml.Marshal(&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// ---- scalar edits ----

// replaceLeafToken rewrites just the value token on the value node's source line,
// preserving indentation, key, comment spacing, the comment, and the line ending.
func replaceLeafToken(data []byte, valNode *yaml.Node, token string) ([]byte, error) {
	if valNode.Line <= 0 {
		return nil, errors.New("yamledit: value node has no source position")
	}
	starts := lineStarts(data)
	raw := string(lineAt(data, starts, valNode.Line))
	newLine := replaceScalarOnLine(raw, token, valNode.LineComment, valNode.Column)
	return replaceLine(data, valNode.Line, newLine), nil
}

// replaceScalarOnLine rebuilds a `<prefix><value>[  <comment>]` line with value
// swapped for newToken. prefix is everything up to the value's column; the
// comment (with its original leading whitespace) is carried over verbatim. A
// trailing CR is dropped here and re-added by the caller's newline.
func replaceScalarOnLine(rawLine, newToken, lineComment string, valueCol int) string {
	rawLine = strings.TrimRight(rawLine, "\r")
	if valueCol < 1 || valueCol-1 > len(rawLine) {
		return rawLine // defensive: leave untouched
	}
	prefix := rawLine[:valueCol-1]
	rest := rawLine[valueCol-1:]
	suffix := ""
	if lineComment != "" {
		if idx := strings.Index(rest, lineComment); idx >= 0 {
			j := idx
			for j > 0 && (rest[j-1] == ' ' || rest[j-1] == '\t') {
				j--
			}
			suffix = rest[j:] // leading whitespace + "# comment"
		}
	}
	return prefix + newToken + suffix
}

// appendLeaf inserts `<indent>key: token` as a new child at the end of mapping m.
func appendLeaf(data []byte, m *yaml.Node, key, token string) []byte {
	indent := childIndent(m)
	anchor := lastLineOfNode(m)
	return insertLineAfter(data, anchor, strings.Repeat(" ", indent)+key+": "+token)
}

// appendChain creates a missing top-level mapping chain at end of file:
//
//	path[0]:
//	  path[1]:
//	    ...: token
func appendChain(data []byte, path []string, token string) []byte {
	nl := newline(data)
	var b strings.Builder
	for depth, k := range path {
		b.WriteString(strings.Repeat(" ", depth*2) + k)
		if depth == len(path)-1 {
			b.WriteString(": " + token + nl)
		} else {
			b.WriteString(":" + nl)
		}
	}
	out := append([]byte(nil), data...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, []byte(nl)...)
	}
	return append(out, []byte(b.String())...)
}

// childIndent returns the space-count children of mapping m are indented to.
func childIndent(m *yaml.Node) int {
	if len(m.Content) > 0 && m.Content[0].Column > 1 {
		return m.Content[0].Column - 1
	}
	if m.Column > 1 {
		return m.Column - 1
	}
	return 2
}

// ---- sequence-as-block re-serialization (rare flow case) ----

func spliceSeqAsBlock(data []byte, keyNode, valNode *yaml.Node, item string) ([]byte, error) {
	valNode.Content = append(valNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: item})
	valNode.Style = 0 // block
	block, err := marshalBlock(keyNode.Value, valNode)
	if err != nil {
		return nil, err
	}
	return spliceLines(data, keyNode.Line, keyNode.Line, block), nil
}

// ---- block marshalling & line splicing ----

// marshalBlock renders a single `key:` mapping (key + val) to YAML text at
// 2-space indent, ending in a newline — the bytes a surgical splice writes for
// one top-level block.
func marshalBlock(key string, val *yaml.Node) ([]byte, error) {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// topLevelBlockLastLine returns the last source line belonging to the top-level
// block whose key is at root.Content[keyIdx]. The block ends just before the next
// top-level sibling (accounting for that sibling's head comment), walked back
// over any blank separator lines — a definition that is correct regardless of
// foot comments or multi-line scalars inside the block.
func topLevelBlockLastLine(data []byte, starts []int, root *yaml.Node, keyIdx int) int {
	startLine := root.Content[keyIdx].Line
	upper := len(starts) + 1 // one past EOF when this is the last block
	if keyIdx+2 < len(root.Content) {
		upper = firstLineOfKey(root.Content[keyIdx+2])
	}
	end := upper - 1
	for end > startLine && isBlank(lineAt(data, starts, end)) {
		end--
	}
	return end
}

// firstLineOfKey returns the first source line the key occupies, including any
// head-comment block immediately above the key line.
func firstLineOfKey(key *yaml.Node) int {
	line := key.Line
	if hc := key.HeadComment; hc != "" {
		line -= strings.Count(hc, "\n") + 1
	}
	if line < 1 {
		line = key.Line
	}
	return line
}

// lastLineOfNode returns the largest source line reached by n or any descendant.
// Adequate for anchoring an append after a mapping's last single-line child.
func lastLineOfNode(n *yaml.Node) int {
	max := 0
	var walk func(*yaml.Node)
	walk = func(x *yaml.Node) {
		if x == nil {
			return
		}
		if x.Line > max {
			max = x.Line
		}
		for _, c := range x.Content {
			walk(c)
		}
	}
	walk(n)
	return max
}

// ---- raw line helpers ----

// lineStarts returns the byte offset each line begins at; the start of 1-based
// line L is lineStarts(data)[L-1].
func lineStarts(data []byte) []int {
	starts := []int{0}
	for i, b := range data {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lineAt returns the bytes of 1-based line n WITHOUT the trailing newline (a
// trailing CR, if any, is retained).
func lineAt(data []byte, starts []int, n int) []byte {
	if n < 1 || n > len(starts) {
		return nil
	}
	start := starts[n-1]
	end := len(data)
	if n < len(starts) {
		end = starts[n] - 1 // the '\n'
	}
	if start > end {
		return nil
	}
	return data[start:end]
}

func isBlank(line []byte) bool {
	return len(bytes.TrimSpace(line)) == 0
}

// newline returns the file's dominant line ending.
func newline(data []byte) string {
	if bytes.Contains(data, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// replaceLine replaces the content of 1-based line n with text (a bare line, no
// newline), keeping the file's line ending.
func replaceLine(data []byte, n int, text string) []byte {
	return spliceLines(data, n, n, []byte(text+newline(data)))
}

// spliceLines replaces the inclusive 1-based line range [startLine, endLine] of
// data with block (which must end in a newline), leaving all other bytes intact.
func spliceLines(data []byte, startLine, endLine int, block []byte) []byte {
	starts := lineStarts(data)
	if startLine < 1 {
		startLine = 1
	}
	startOff := starts[startLine-1]
	endOff := len(data)
	if endLine < len(starts) {
		endOff = starts[endLine] // first byte of the line after endLine
	}
	out := make([]byte, 0, startOff+len(block)+(len(data)-endOff))
	out = append(out, data[:startOff]...)
	out = append(out, block...)
	out = append(out, data[endOff:]...)
	return out
}

// insertLineAfter inserts text as a fresh line immediately after 1-based
// afterLine (0 = at the top), using the file's line ending.
func insertLineAfter(data []byte, afterLine int, text string) []byte {
	nl := newline(data)
	starts := lineStarts(data)
	insertOff := len(data)
	if afterLine >= 0 && afterLine < len(starts) {
		insertOff = starts[afterLine]
	}
	var out []byte
	out = append(out, data[:insertOff]...)
	if insertOff == len(data) && len(data) > 0 && data[len(data)-1] != '\n' {
		out = append(out, []byte(nl)...)
	}
	out = append(out, []byte(text+nl)...)
	out = append(out, data[insertOff:]...)
	return out
}

// appendBlock adds a fresh block to the end of data, separated from existing
// content by exactly one blank line (the skeleton's inter-section spacing).
func appendBlock(data, block []byte) []byte {
	out := append([]byte(nil), data...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	if len(out) > 0 && !(len(out) >= 2 && out[len(out)-2] == '\n') {
		out = append(out, '\n')
	}
	return append(out, block...)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
