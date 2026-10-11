package main

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/limesh/core/api"
)

// newTopoCmd 构造 topo 子命令。
func newTopoCmd(configPaths *[]string) *cobra.Command {
	var (
		dot    bool
		format string
	)

	cmd := &cobra.Command{
		Use:   "topo",
		Short: "显示网络拓扑",
		Long: `以文本或 Graphviz DOT 格式显示当前节点视角的网络拓扑。

DOT 输出可交给 graphviz 渲染:
  mesh topo --format dot | dot -Tpng -o topo.png`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newAdminClient(cmd, *configPaths)
			if err != nil {
				return err
			}
			ctx, cancel := withTimeout()
			defer cancel()

			topo, err := cl.Topology(ctx)
			if err != nil {
				return err
			}

			// --format 为主;--dot 是等价的便捷别名;--json 覆盖一切。
			mode := format
			if dot {
				mode = "dot"
			}
			if outputJSON {
				mode = "json"
			}

			switch mode {
			case "json":
				return printJSON(cmd.OutOrStdout(), topo)
			case "dot":
				writeDOT(cmd, topo)
			case "", "text":
				writeASCII(cmd, topo)
			default:
				return fmt.Errorf("unknown format %q (want text/dot/json)", mode)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&dot, "dot", false, "输出 Graphviz DOT 格式(等价于 --format dot)")
	cmd.Flags().StringVar(&format, "format", "", "输出格式:text / dot / json")
	addAdminFlags(cmd)
	return cmd
}

// writeDOT 输出 Graphviz DOT。
func writeDOT(cmd *cobra.Command, topo *api.TopologySnapshot) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "digraph mesh {")
	fmt.Fprintln(out, "  rankdir=LR;")
	fmt.Fprintln(out, "  node [shape=box];")

	// 节点:标注 self 与虚拟 IP。
	for _, n := range topo.Nodes {
		label := shortID(n.NodeID)
		if n.VirtualIP != "" {
			label += "\\n" + n.VirtualIP
		}
		attrs := ""
		if n.IsSelf {
			attrs = `, style=filled, fillcolor=lightblue`
		}
		fmt.Fprintf(out, "  \"%s\" [label=\"%s\"%s];\n", n.NodeID, label, attrs)
	}
	for _, e := range topo.Edges {
		fmt.Fprintf(out, "  \"%s\" -> \"%s\" [label=\"%d\"];\n", e.From, e.To, e.Cost)
	}
	fmt.Fprintln(out, "}")
}

// writeASCII 输出便于人读的邻接列表式拓扑。
//
// 完整的分层图形渲染需要布局算法;这里采用"节点 + 邻居树"的稳定文本形式,
// 既能表达拓扑,又不依赖终端宽度与图形字符支持。
func writeASCII(cmd *cobra.Command, topo *api.TopologySnapshot) {
	out := cmd.OutOrStdout()

	nodes := append([]api.NodeSnapshot(nil), topo.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		// self 排在最前。
		if nodes[i].IsSelf != nodes[j].IsSelf {
			return nodes[i].IsSelf
		}
		return nodes[i].NodeID < nodes[j].NodeID
	})

	// 邻接表:from -> []edge。
	adj := map[string][]api.EdgeSnapshot{}
	for _, e := range topo.Edges {
		adj[e.From] = append(adj[e.From], e)
	}
	for k := range adj {
		es := adj[k]
		sort.Slice(es, func(i, j int) bool { return es[i].To < es[j].To })
		adj[k] = es
	}

	if len(nodes) == 0 {
		fmt.Fprintln(out, "(empty topology)")
		return
	}

	for _, n := range nodes {
		self := ""
		if n.IsSelf {
			self = " [self]"
		}
		fmt.Fprintf(out, "%s  %s%s\n", shortID(n.NodeID), n.VirtualIP, self)
		for _, e := range adj[n.NodeID] {
			fmt.Fprintf(out, "  ├─ cost %-4d ▼ %s\n", e.Cost, shortID(e.To))
		}
	}
}
