package jvm

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// xnode is a minimal XML element tree (local names only: namespaces are ignored), enough to read POMs.
type xnode struct {
	name string
	text string // trimmed character data directly inside the element
	kids []*xnode
}

// parseXML parses b and returns the root element. Unknown encodings are passed through unchanged (coordinates are ASCII).
func parseXML(b []byte) (*xnode, error) {
	// A BOM and bytes of a legacy encoding (ISO-8859-1 comments, names) must not make the whole POM unreadable.
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(b) {
		b = []byte(strings.ToValidUTF8(string(b), "?"))
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	d.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	var root *xnode
	var stack []*xnode
	var text []*strings.Builder
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xnode{name: t.Name.Local}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.kids = append(p.kids, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
			text = append(text, &strings.Builder{})
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errors.New("unbalanced end element")
			}
			n := stack[len(stack)-1]
			n.text = strings.TrimSpace(text[len(text)-1].String())
			stack = stack[:len(stack)-1]
			text = text[:len(text)-1]
		case xml.CharData:
			if len(stack) > 0 {
				text[len(text)-1].Write(t)
			}
		}
	}
	if root == nil {
		return nil, errors.New("no root element")
	}
	return root, nil
}

// child returns the first direct child with the given name.
func (n *xnode) child(name string) *xnode {
	if n == nil {
		return nil
	}
	for _, k := range n.kids {
		if k.name == name {
			return k
		}
	}
	return nil
}

// childText is the text of the first direct child with the given name ("" when absent).
func (n *xnode) childText(name string) string {
	if c := n.child(name); c != nil {
		return c.text
	}
	return ""
}

// children returns the direct children with the given name.
func (n *xnode) children(name string) []*xnode {
	if n == nil {
		return nil
	}
	var out []*xnode
	for _, k := range n.kids {
		if k.name == name {
			out = append(out, k)
		}
	}
	return out
}

// walk calls fn for every descendant (not n itself) with the chain of its ancestors, outermost first, ending with its parent.
func (n *xnode) walk(fn func(x *xnode, ancestors []*xnode)) {
	var rec func(x *xnode, anc []*xnode)
	rec = func(x *xnode, anc []*xnode) {
		for _, k := range x.kids {
			fn(k, anc)
			rec(k, append(anc, k))
		}
	}
	rec(n, []*xnode{n})
}

// canon serializes the tree with a stable layout; skip names the element paths ("project/version") left out.
func (n *xnode) canon(skip map[string]bool) string {
	var sb strings.Builder
	var rec func(x *xnode, p string)
	rec = func(x *xnode, p string) {
		sb.WriteString("<" + x.name + ">")
		sb.WriteString(x.text)
		for _, k := range x.kids {
			kp := p + "/" + k.name
			if skip[kp] {
				continue
			}
			rec(k, kp)
		}
		sb.WriteString("</>")
	}
	rec(n, n.name)
	return sb.String()
}
