package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeepMergeMapRecursive(t *testing.T) {
	dst := map[string]any{
		"network": map[string]any{"name": "prod", "mtu": 1380},
	}
	src := map[string]any{
		"network": map[string]any{"mtu": 1400},
	}

	got := deepMerge(dst, src)

	net, ok := got["network"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "prod", net["name"], "未提及的字段应保留")
	assert.Equal(t, 1400, net["mtu"], "高优先级应覆盖")
}

func TestDeepMergeDoesNotMutateInputs(t *testing.T) {
	dst := map[string]any{"a": map[string]any{"x": 1}}
	src := map[string]any{"a": map[string]any{"y": 2}}

	_ = deepMerge(dst, src)

	dstA := dst["a"].(map[string]any)
	assert.NotContains(t, dstA, "y", "dst 不应被修改")
	assert.Equal(t, 1, dstA["x"])

	srcA := src["a"].(map[string]any)
	assert.NotContains(t, srcA, "x", "src 不应被修改")
}

func TestDeepMergeArrayAppends(t *testing.T) {
	dst := map[string]any{"peers": []any{"1.2.3.4:11010"}}
	src := map[string]any{"peers": []any{"5.6.7.8:11010"}}

	got := deepMerge(dst, src)

	assert.Equal(t, []any{"1.2.3.4:11010", "5.6.7.8:11010"}, got["peers"],
		"数组应追加而非替换")
}

func TestDeepMergeArrayNoDedup(t *testing.T) {
	// 追加规则明确不去重,重复项应原样保留。
	dst := map[string]any{"peers": []any{"a:1"}}
	src := map[string]any{"peers": []any{"a:1"}}

	got := deepMerge(dst, src)
	assert.Equal(t, []any{"a:1", "a:1"}, got["peers"])
}

func TestDeepMergeEmptyArrayAppendsNothing(t *testing.T) {
	// 裸写 [] 只是"追加零个元素",继承值仍在。这是设计使然。
	dst := map[string]any{"peers": []any{"a:1"}}
	src := map[string]any{"peers": []any{}}

	got := deepMerge(dst, src)
	assert.Equal(t, []any{"a:1"}, got["peers"], "[] 不应清空继承值")
}

func TestDeepMergeScalarOverrides(t *testing.T) {
	dst := map[string]any{"log": map[string]any{"level": "info"}}
	src := map[string]any{"log": map[string]any{"level": "debug"}}

	got := deepMerge(dst, src)
	assert.Equal(t, "debug", got["log"].(map[string]any)["level"])
}

func TestDeepMergeTypeMismatchOverrides(t *testing.T) {
	// 类型不一致时高优先级整体覆盖,避免产生畸形结构。
	dst := map[string]any{"x": map[string]any{"a": 1}}
	src := map[string]any{"x": "scalar"}

	got := deepMerge(dst, src)
	assert.Equal(t, "scalar", got["x"])
}

func TestDeepMergeEmptySrc(t *testing.T) {
	dst := map[string]any{"a": 1}
	got := deepMerge(dst, map[string]any{})
	assert.Equal(t, map[string]any{"a": 1}, got)
}

func TestApplyResetDeletesPath(t *testing.T) {
	dst := map[string]any{
		"peers":  []any{"1.2.3.4:11010"},
		"listen": []any{map[string]any{"addr": "udp://0.0.0.0:11010"}},
	}
	src := map[string]any{"reset": []any{"peers", "listen"}}

	paths := applyReset(dst, src)

	assert.Equal(t, []string{"listen", "peers"}, paths, "返回的路径应去重并排序")
	assert.NotContains(t, dst, "peers")
	assert.NotContains(t, dst, "listen")
}

func TestApplyResetNestedPath(t *testing.T) {
	dst := map[string]any{
		"routing": map[string]any{"weight": 10, "strategy": "latency"},
	}
	src := map[string]any{"reset": []any{"routing.weight"}}

	applyReset(dst, src)

	routing := dst["routing"].(map[string]any)
	assert.NotContains(t, routing, "weight", "被 reset 的嵌套字段应删除")
	assert.Equal(t, "latency", routing["strategy"], "同级其它字段应保留")
}

func TestApplyResetMissingPathIsSilent(t *testing.T) {
	dst := map[string]any{"a": 1}
	src := map[string]any{"reset": []any{"does.not.exist"}}

	assert.NotPanics(t, func() {
		paths := applyReset(dst, src)
		assert.Equal(t, []string{"does.not.exist"}, paths)
	})
	assert.Equal(t, map[string]any{"a": 1}, dst, "不存在的路径应静默忽略")
}

func TestApplyResetNoResetKey(t *testing.T) {
	dst := map[string]any{"a": 1}
	assert.Nil(t, applyReset(dst, map[string]any{}))
	assert.Equal(t, map[string]any{"a": 1}, dst)
}

func TestExtractResetPathsVariants(t *testing.T) {
	assert.Equal(t, []string{"a", "b"},
		extractResetPaths(map[string]any{"reset": []any{"b", "a", "b"}}),
		"应去重并排序")
	assert.Equal(t, []string{"solo"},
		extractResetPaths(map[string]any{"reset": "solo"}), "单值写法应支持")
	assert.Nil(t, extractResetPaths(map[string]any{"reset": []any{1, true, ""}}),
		"非字符串与空串应被忽略")
	assert.Nil(t, extractResetPaths(map[string]any{}))
}

func TestMergeWithReset(t *testing.T) {
	dst := map[string]any{
		"peers": []any{"1.2.3.4:11010"},
		"log":   map[string]any{"level": "info"},
	}
	src := map[string]any{
		"reset": []any{"peers"},
		"peers": []any{"9.9.9.9:11010"},
	}

	got, paths := mergeWithReset(dst, src)

	assert.Equal(t, []string{"peers"}, paths)
	assert.Equal(t, []any{"9.9.9.9:11010"}, got["peers"], "reset 后重新定义的值应为最终值")
	assert.NotContains(t, got, ResetKey, "reset 是合并期元字段,不得进入运行时配置")
}

func TestMergeWithResetClearsWithoutRedefine(t *testing.T) {
	dst := map[string]any{"peers": []any{"1.2.3.4:11010"}}
	src := map[string]any{"reset": []any{"peers"}}

	got, _ := mergeWithReset(dst, src)

	_, exists := got["peers"]
	assert.False(t, exists, "只 reset 不重新定义时该字段应消失")
}

func TestResetSequenceMatchesDocumentation(t *testing.T) {
	// 文档示例:base 定义 relay,override 用 [] 不清空、用 reset 才清空。
	base := map[string]any{"peers": map[string]any{"relay": []any{"1.2.3.4:11010"}}}

	withEmpty := deepMerge(base, map[string]any{
		"peers": map[string]any{"relay": []any{}},
	})
	assert.Equal(t, []any{"1.2.3.4:11010"},
		withEmpty["peers"].(map[string]any)["relay"],
		"裸写 [] 不清空(与 CONFIG.md 4.3 一致)")

	withReset, _ := mergeWithReset(base, map[string]any{
		"reset": []any{"peers"},
		"peers": map[string]any{"relay": []any{"5.6.7.8:11010"}},
	})
	assert.Equal(t, []any{"5.6.7.8:11010"},
		withReset["peers"].(map[string]any)["relay"],
		"reset 后重新定义才是最终值")
}

func TestDeletePathHandlesEmptySegments(t *testing.T) {
	m := map[string]any{"a": map[string]any{"b": 1}}
	assert.NotPanics(t, func() { deletePath(m, "") })
	assert.NotPanics(t, func() { deletePath(m, "a..b") })
	assert.Equal(t, 1, m["a"].(map[string]any)["b"])
}

func TestSetPathCreatesIntermediate(t *testing.T) {
	m := map[string]any{}
	setPath(m, "a.b.c", 7)
	assert.Equal(t, 7, m["a"].(map[string]any)["b"].(map[string]any)["c"])
}

func TestSubMap(t *testing.T) {
	m := map[string]any{"a": map[string]any{"b": map[string]any{"c": 1}}}
	assert.Equal(t, map[string]any{"c": 1}, subMap(m, "a.b"))
	assert.Nil(t, subMap(m, "a.z"))
	assert.Nil(t, subMap(m, ""))
}

func TestDedupeSorted(t *testing.T) {
	assert.Equal(t, []string{"a", "b", "c"}, dedupeSorted([]string{"c", "a", "b", "a"}))
	assert.Nil(t, dedupeSorted(nil))
}

func TestFormatResetPaths(t *testing.T) {
	assert.Equal(t, "[a, b]", formatResetPaths([]string{"a", "b"}))
	assert.Equal(t, "", formatResetPaths(nil))
}
