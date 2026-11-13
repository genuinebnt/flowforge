package htmlparse

// selectedcontent mirroring (HTML "The select element" and "The
// selectedcontent element" in §4.10). In a browser the option and selectedcontent
// insertion / "children parsed" steps copy the selected option's children
// into every enabled selectedcontent descendant of its select while the
// parser runs. The html5lib expectations include that effect, so the tree
// builder reproduces its end state once parsing is complete: for each
// select without the multiple attribute, the selected option is the last
// option with a selected attribute, else the first enabled option when
// the display size is 1; each enabled selectedcontent descendant then
// holds a clone of that option's children (without script, SVG script,
// frame, iframe, embed and object elements).
//
// This is a DOM side effect, not part of the §13.2 parsing algorithm; it
// runs only when the input contained a selectedcontent element.

// mirrorSelectedContent applies the end state described above to every
// select in the arena (template contents included).
func (b *treeBuilder) mirrorSelectedContent() {
	d := b.doc
	n := len(d.Nodes) // nodes cloned below are not revisited
	for id := NodeID(0); int(id) < n; id++ {
		nd := &d.Nodes[id]
		if nd.Kind != ElementNode || nd.Namespace != HTMLNamespace || nd.Name != "select" {
			continue
		}
		if _, multiple := d.Attr(id, "multiple"); multiple {
			continue
		}
		targets := d.selectedContentsOf(id)
		if len(targets) == 0 {
			continue
		}
		opt := d.selectedOption(id)
		for _, sc := range targets {
			for c := d.Nodes[sc].FirstChild; c != None; c = d.Nodes[sc].FirstChild {
				d.detach(c)
			}
			if opt == None {
				continue
			}
			for c := range d.Children(opt) {
				if clone := d.cloneForSelectedContent(c); clone != None {
					d.insertBefore(sc, clone, None)
				}
			}
		}
	}
}

// isHTML reports an HTML element with local name name.
func (d *Document) isHTML(id NodeID, name string) bool {
	n := &d.Nodes[id]
	return n.Kind == ElementNode && n.Namespace == HTMLNamespace && n.Name == name
}

// optionsOf is the select element's "list of options" (2025 rules): option
// descendants in tree order, not looking inside nested select, hr,
// option or datalist elements, nor inside an optgroup that is itself
// inside an optgroup of this select.
func (d *Document) optionsOf(sel NodeID) []NodeID {
	var out []NodeID
	node := d.Nodes[sel].FirstChild
	for node != None {
		if d.isHTML(node, "option") {
			out = append(out, node)
		}
		skip := d.isHTML(node, "select") || d.isHTML(node, "hr") || d.isHTML(node, "option") || d.isHTML(node, "datalist")
		if !skip && d.isHTML(node, "optgroup") {
			for p := d.Nodes[node].Parent; p != None && p != sel; p = d.Nodes[p].Parent {
				if d.isHTML(p, "optgroup") {
					skip = true
					break
				}
			}
		}
		node = d.nextInSubtree(node, sel, skip)
	}
	return out
}

// nextInSubtree is the next node in tree order within root, optionally
// skipping node's descendants.
func (d *Document) nextInSubtree(node, root NodeID, skipChildren bool) NodeID {
	if !skipChildren && d.Nodes[node].FirstChild != None {
		return d.Nodes[node].FirstChild
	}
	for node != None && node != root {
		if s := d.Nodes[node].NextSibling; s != None {
			return s
		}
		node = d.Nodes[node].Parent
	}
	return None
}

// optionDisabled is the option's "disabled" state: its own disabled
// attribute or that of a parent optgroup.
func (d *Document) optionDisabled(opt NodeID) bool {
	if _, ok := d.Attr(opt, "disabled"); ok {
		return true
	}
	if p := d.Nodes[opt].Parent; p != None && d.isHTML(p, "optgroup") {
		_, ok := d.Attr(p, "disabled")
		return ok
	}
	return false
}

// selectedOption is the option whose children are mirrored: the last
// option with a selected attribute, else (display size 1) the first
// enabled option, else None. A size above 1 means a list box with no
// default selection.
func (d *Document) selectedOption(sel NodeID) NodeID {
	opts := d.optionsOf(sel)
	last := None
	for _, o := range opts {
		if _, ok := d.Attr(o, "selected"); ok {
			last = o
		}
	}
	if last != None {
		return last
	}
	if v, ok := d.Attr(sel, "size"); ok {
		if n, ok := parseNonNegativeInt(v); ok && n > 1 {
			return None
		}
	}
	for _, o := range opts {
		if !d.optionDisabled(o) {
			return o
		}
	}
	return None
}

// parseNonNegativeInt is the HTML "rules for parsing non-negative
// integers" (leading whitespace, optional '+', digits). A leading '-' is
// rejected (the spec would accept "-0"; it only matters for size="-0",
// which means display size 1 either way). The value saturates near 2^30
// so long digit runs cannot overflow.
func parseNonNegativeInt(s string) (int, bool) {
	i := 0
	for i < len(s) && isHTMLSpaceB(s[i]) {
		i++
	}
	if i < len(s) && s[i] == '+' {
		i++
	}
	if i >= len(s) || s[i] < '0' || s[i] > '9' {
		return 0, false
	}
	n := 0
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		if n < 1<<30 {
			n = n*10 + int(s[i]-'0')
		}
	}
	return n, true
}

// selectedContentsOf returns the enabled selectedcontent descendants of
// sel whose nearest select ancestor is sel, in tree order.
func (d *Document) selectedContentsOf(sel NodeID) []NodeID {
	var out []NodeID
	for n := range d.Descendants(sel) {
		if !d.isHTML(n, "selectedcontent") {
			continue
		}
		nearest, disabled := None, false
		for a := d.Nodes[n].Parent; a != None; a = d.Nodes[a].Parent {
			if d.isHTML(a, "select") {
				if nearest == None {
					nearest = a
					continue
				}
				disabled = true
				break
			}
			if d.isHTML(a, "option") || d.isHTML(a, "selectedcontent") {
				disabled = true
				break
			}
		}
		if !disabled && nearest == sel {
			out = append(out, n)
		}
	}
	return out
}

// cloneForSelectedContent deep-clones n, dropping the element types
// "clone an option into a selectedcontent" removes. It recurses over the
// option's subtree depth.
//
// PORT: recursive like dumpNode; make it iterative in Rust.
func (d *Document) cloneForSelectedContent(n NodeID) NodeID {
	src := d.Nodes[n]
	if src.Kind == ElementNode {
		switch {
		case src.Namespace == HTMLNamespace && (src.Name == "script" || src.Name == "frame" ||
			src.Name == "iframe" || src.Name == "embed" || src.Name == "object"):
			return None
		case src.Namespace == SVGNamespace && src.Name == "script":
			return None
		}
	}
	id := d.NewNode(src.Kind)
	c := &d.Nodes[id]
	c.Namespace, c.Name, c.Data, c.PublicID, c.SystemID, c.Span = src.Namespace, src.Name, src.Data, src.PublicID, src.SystemID, src.Span
	if src.Attrs != nil {
		c.Attrs = append([]Attr(nil), src.Attrs...)
	}
	if src.Content != None {
		frag := d.NewNode(DocumentFragmentNode)
		d.Nodes[id].Content = frag
		for k := range d.Children(src.Content) {
			if cc := d.cloneForSelectedContent(k); cc != None {
				d.insertBefore(frag, cc, None)
			}
		}
	}
	for k := range d.Children(n) {
		if cc := d.cloneForSelectedContent(k); cc != None {
			d.insertBefore(id, cc, None)
		}
	}
	return id
}
