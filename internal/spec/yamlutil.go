package spec

import "gopkg.in/yaml.v3"

type entry struct{ key, val *yaml.Node }

func posOf(file string, n *yaml.Node) SourcePos {
	if n == nil {
		return SourcePos{File: file}
	}
	return SourcePos{File: file, Line: n.Line, Col: n.Column}
}

// fieldNode returns the value node of key in mapping n, or nil.
func fieldNode(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// fieldPos returns the position of key's value, falling back to n.
func fieldPos(file string, n *yaml.Node, key string) SourcePos {
	if f := fieldNode(n, key); f != nil {
		return posOf(file, f)
	}
	return posOf(file, n)
}

func mapEntries(n *yaml.Node) []entry {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]entry, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, entry{n.Content[i], n.Content[i+1]})
	}
	return out
}
