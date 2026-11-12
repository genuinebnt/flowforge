package csr

// Partition groups the nodes of a graph, for example into connected or
// strongly connected components.
type Partition struct {
	// Of[v] is the group of node v.
	Of []uint32
	// Starts has Count()+1 entries; group i is Members[Starts[i]:Starts[i+1]].
	Starts []int
	// Members lists every node once, grouped, ascending within a group.
	Members []uint32
}

// Count returns the number of groups.
func (p Partition) Count() int { return max(len(p.Starts)-1, 0) }

// Group returns the members of group i, ascending. The slice aliases the
// partition.
func (p Partition) Group(i int) []uint32 {
	return p.Members[p.Starts[i]:p.Starts[i+1]:p.Starts[i+1]]
}

// Groups returns every group as its own slice (aliasing the partition).
func (p Partition) Groups() [][]uint32 {
	out := make([][]uint32, p.Count())
	for i := range out {
		out[i] = p.Group(i)
	}
	return out
}

// partitionFrom builds a Partition from a group id per node, with group
// ids already in the desired output order.
func partitionFrom(of []uint32, count int) Partition {
	p := Partition{Of: of, Starts: make([]int, count+1), Members: make([]uint32, len(of))}
	for _, c := range of {
		p.Starts[c+1]++
	}
	for i := 1; i <= count; i++ {
		p.Starts[i] += p.Starts[i-1]
	}
	cursor := make([]int, count)
	copy(cursor, p.Starts[:count])
	for v, c := range of {
		p.Members[cursor[c]] = uint32(v)
		cursor[c]++
	}
	return p
}
