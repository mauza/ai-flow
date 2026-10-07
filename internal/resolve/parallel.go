package resolve

import "github.com/mauza/ai-flow/internal/flow"

// Lane places a node inside one branch of a parallel node.
type Lane struct {
	Fork   string // the parallel node
	Branch string // the branch's first node
	Join   string // where every branch of Fork ends
}

// Lanes maps every node inside a parallel branch to its branch: the nodes
// reachable from the branch's first node without passing its join. Nodes that
// more than one branch reaches are returned separately; validation rejects them.
func (r *Resolved) Lanes() (lanes map[string]Lane, shared []string) {
	lanes = map[string]Lane{}
	seenShared := map[string]bool{}
	for _, id := range r.Order {
		fork := r.Nodes[id]
		if fork == nil || fork.Type != flow.TypeParallel {
			continue
		}
		for _, b := range fork.Branches {
			lane := Lane{Fork: id, Branch: b, Join: fork.Join}
			seen := map[string]bool{}
			queue := []string{b}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				n := r.Nodes[cur]
				if seen[cur] || n == nil || cur == fork.Join || cur == id || flow.Terminal(cur) {
					continue
				}
				seen[cur] = true
				if prev, ok := lanes[cur]; ok && prev != lane {
					if !seenShared[cur] {
						seenShared[cur] = true
						shared = append(shared, cur)
					}
					continue
				}
				lanes[cur] = lane
				queue = append(queue, n.Targets()...)
			}
		}
	}
	return lanes, shared
}
