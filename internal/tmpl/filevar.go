package tmpl

// filevar.go — `json_file:` and `yaml_file:` variables: a config file read into
// a tree that templates can walk.
//
// A machine-wide chore file is code, the same on every machine; what differs
// per machine is config. These two forms let a task read that config as data
// instead of shelling out to jq and checking its shape by hand:
//
//	vars:
//	  PROFILES: {json_file: '{{.TASKFILE_DIR}}/../agent-profiles.json'}
//	tasks:
//	  show: {cmd: 'echo {{.PROFILES.work.directory}}'}
//
// The file is parsed here, in Go, and both formats give the same tree: objects
// keep the order they were written in, numbers keep the text they were written
// as, and true, false and null are themselves. Templates get the tree; the
// environment ($PROFILES) gets it as compact JSON; `for:` walks its top level.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Object is a config file's object as a template sees it. Field access and
// `index` read it like any map; printing it gives the object as compact JSON
// rather than Go's `map[...]`, so `{{.PROFILES.work}}` is something a shell
// command can take. A printed object lists its keys sorted, as `range` walks
// them; the variable's own `$NAME` and `for:` keep the written order.
type Object map[string]any

// String is the object as compact JSON, keys sorted.
func (o Object) String() string { return o.node().json() }

// Array is a config file's array as a template sees it. `range` and `index`
// read it; printing it gives compact JSON.
type Array []any

// String is the array as compact JSON.
func (a Array) String() string { return a.node().json() }

// onode is the parsed file with its written order intact. Object and Array are
// the views templates read; onode is what the JSON and `for:` forms are made
// from, because a Go map forgets the order its keys were written in.
type onode struct {
	kind  nodeKind
	text  string   // a string's value, or a number's text as written
	keys  []string // an object's keys, in written order
	items []*onode // an object's values (parallel to keys), or an array's items
}

type nodeKind int

const (
	nodeString nodeKind = iota
	nodeNumber
	nodeBool
	nodeNull
	nodeObject
	nodeArray
)

// Number is a number as written in the file. It prints as that text, so
// `port: 8080` renders 8080 and `ratio: 0.50` renders 0.50.
type Number string

// String returns the number's text.
func (n Number) String() string { return string(n) }

// view turns a node into what templates read.
func (n *onode) view() any {
	switch n.kind {
	case nodeString:
		return n.text
	case nodeNumber:
		return Number(n.text)
	case nodeBool:
		return n.text == "true"
	case nodeNull:
		return nil
	case nodeObject:
		o := make(Object, len(n.keys))
		for i, k := range n.keys {
			o[k] = n.items[i].view()
		}
		return o
	default:
		a := make(Array, len(n.items))
		for i, it := range n.items {
			a[i] = it.view()
		}
		return a
	}
}

// json writes the node as compact JSON, objects in written order.
func (n *onode) json() string {
	var b bytes.Buffer
	n.writeJSON(&b)
	return b.String()
}

func (n *onode) writeJSON(b *bytes.Buffer) {
	switch n.kind {
	case nodeString:
		s, _ := json.Marshal(n.text)
		b.Write(s)
	case nodeNumber, nodeBool:
		b.WriteString(n.text)
	case nodeNull:
		b.WriteString("null")
	case nodeObject:
		b.WriteByte('{')
		for i, k := range n.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			s, _ := json.Marshal(k)
			b.Write(s)
			b.WriteByte(':')
			n.items[i].writeJSON(b)
		}
		b.WriteByte('}')
	default:
		b.WriteByte('[')
		for i, it := range n.items {
			if i > 0 {
				b.WriteByte(',')
			}
			it.writeJSON(b)
		}
		b.WriteByte(']')
	}
}

// flat is a node's string form where a single string is wanted — a `for:` item:
// a string or number as its text, anything else as compact JSON.
func (n *onode) flat() string {
	switch n.kind {
	case nodeString, nodeNumber:
		return n.text
	case nodeNull:
		return ""
	}
	return n.json()
}

func (o Object) node() *onode {
	n := &onode{kind: nodeObject}
	for _, k := range slices.Sorted(maps.Keys(o)) {
		n.keys = append(n.keys, k)
		n.items = append(n.items, nodeOf(o[k]))
	}
	return n
}

func (a Array) node() *onode {
	n := &onode{kind: nodeArray}
	for _, it := range a {
		n.items = append(n.items, nodeOf(it))
	}
	return n
}

// nodeOf turns a template view back into a node.
func nodeOf(v any) *onode {
	switch v := v.(type) {
	case nil:
		return &onode{kind: nodeNull}
	case string:
		return &onode{kind: nodeString, text: v}
	case Number:
		return &onode{kind: nodeNumber, text: string(v)}
	case bool:
		return &onode{kind: nodeBool, text: fmt.Sprint(v)}
	case Object:
		return v.node()
	case Array:
		return v.node()
	}
	return &onode{kind: nodeString, text: fmt.Sprint(v)}
}

// FileValue reads a `json_file:` or `yaml_file:` variable; the caller names the
// variable in any error. path is already
// rendered; a leading "?" makes a missing file an empty object rather than an
// error. A relative path is taken from dir, the directory of the chorefile the
// variable is written in. format is "json" or "yaml".
func FileValue(path, format, dir string) (Value, error) {
	optional := strings.HasPrefix(path, "?")
	path = strings.TrimPrefix(path, "?")
	if path == "" {
		return Value{}, fmt.Errorf("%s_file: the path rendered empty", format)
	}
	if !filepath.IsAbs(path) && dir != "" {
		path = filepath.Join(dir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if optional {
				return treeValue(&onode{kind: nodeObject}), nil
			}
			return Value{}, fmt.Errorf("%s does not exist — write it, or put \"?\" before the path if it is optional", path)
		}
		return Value{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var root *onode
	switch format {
	case "json":
		root, err = parseJSON(data)
	case "yaml":
		root, err = parseYAML(data)
	default:
		return Value{}, fmt.Errorf("unknown file format %q", format)
	}
	if err != nil {
		return Value{}, fmt.Errorf("%s is not valid %s: %w", path, strings.ToUpper(format), err)
	}
	if root.kind != nodeObject && root.kind != nodeArray {
		return Value{}, fmt.Errorf("%s holds a single value; a %s_file: variable needs an object or an array at the top", path, format)
	}
	return treeValue(root), nil
}

// treeValue wraps a parsed file as a scope Value: its tree for templates, its
// compact JSON as the string form, and its top level for `for:`.
func treeValue(root *onode) Value {
	v := Value{tree: root.view(), str: root.json()}
	switch root.kind {
	case nodeObject:
		v.Kind = Map
		v.Keys = append([]string{}, root.keys...)
		for _, it := range root.items {
			v.Items = append(v.Items, it.flat())
		}
		if v.Keys == nil {
			v.Keys, v.Items = []string{}, []string{}
		}
	case nodeArray:
		v.Kind = List
		for _, it := range root.items {
			v.Items = append(v.Items, it.flat())
		}
		if v.Items == nil {
			v.Items = []string{}
		}
	}
	return v
}

// parseJSON decodes JSON keeping object order and number text. Anything after
// the first value, and a repeated key, is an error rather than a silent choice.
func parseJSON(data []byte) (*onode, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := jsonValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after the top-level value")
	}
	return n, nil
}

func jsonValue(dec *json.Decoder) (*onode, error) {
	tok, err := dec.Token()
	if err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("the file is empty")
		}
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &onode{kind: nodeObject}
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				if seen[k] {
					return nil, fmt.Errorf("key %q appears twice", k)
				}
				seen[k] = true
				v, err := jsonValue(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k)
				n.items = append(n.items, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		case '[':
			n := &onode{kind: nodeArray}
			for dec.More() {
				v, err := jsonValue(dec)
				if err != nil {
					return nil, err
				}
				n.items = append(n.items, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
	case string:
		return &onode{kind: nodeString, text: t}, nil
	case json.Number:
		return &onode{kind: nodeNumber, text: t.String()}, nil
	case bool:
		return &onode{kind: nodeBool, text: fmt.Sprint(t)}, nil
	case nil:
		return &onode{kind: nodeNull}, nil
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

// parseYAML decodes one YAML document into the same tree JSON gives. Anchors
// and aliases are followed; merge keys and tags other than the core schema's
// are refused rather than half-understood.
func parseYAML(data []byte) (*onode, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("the file is empty")
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("the file holds more than one YAML document")
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("the file is empty")
	}
	return yamlValue(doc.Content[0])
}

func yamlValue(y *yaml.Node) (*onode, error) {
	switch y.Kind {
	case yaml.AliasNode:
		return yamlValue(y.Alias)
	case yaml.MappingNode:
		n := &onode{kind: nodeObject}
		seen := map[string]bool{}
		for i := 0; i+1 < len(y.Content); i += 2 {
			k, v := y.Content[i], y.Content[i+1]
			if k.Tag == "!!merge" {
				return nil, fmt.Errorf("line %d: merge keys (<<) are not supported", k.Line)
			}
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: a key must be a plain value", k.Line)
			}
			if seen[k.Value] {
				return nil, fmt.Errorf("line %d: key %q appears twice", k.Line, k.Value)
			}
			seen[k.Value] = true
			val, err := yamlValue(v)
			if err != nil {
				return nil, err
			}
			n.keys = append(n.keys, k.Value)
			n.items = append(n.items, val)
		}
		return n, nil
	case yaml.SequenceNode:
		n := &onode{kind: nodeArray}
		for _, c := range y.Content {
			val, err := yamlValue(c)
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, val)
		}
		return n, nil
	case yaml.ScalarNode:
		switch y.ShortTag() {
		case "!!str", "!!binary", "!!timestamp":
			return &onode{kind: nodeString, text: y.Value}, nil
		case "!!int", "!!float":
			return &onode{kind: nodeNumber, text: y.Value}, nil
		case "!!bool":
			var b bool
			if err := y.Decode(&b); err != nil {
				return nil, fmt.Errorf("line %d: %w", y.Line, err)
			}
			return &onode{kind: nodeBool, text: fmt.Sprint(b)}, nil
		case "!!null":
			return &onode{kind: nodeNull}, nil
		}
		return nil, fmt.Errorf("line %d: tag %s is not supported", y.Line, y.Tag)
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", y.Line)
}
