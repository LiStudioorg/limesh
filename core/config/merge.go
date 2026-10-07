package config

import (
	"fmt"
	"sort"
	"strings"
)

// ResetKey 是合并期元字段名,取值为需要清空的字段路径列表。
//
// reset 在合并阶段被消费,不进入运行时路径:它决定"哪些继承值被丢弃",
// 本身不产生变更事件,也没有响应级别。被它清空后重新定义的字段,
// 响应级别按字段自身的定义处理。
const ResetKey = "reset"

// deepMerge 将 src 递归合并进 dst,并返回合并结果。
//
// 合并规则:
//   - map(表)   → 递归合并,同键继续下钻
//   - 数组      → 追加(前者在前,后者在后),不去重
//   - 标量      → 覆盖(后者胜出)
//
// dst 与 src 均不会被修改;返回值是新的 map。若 src 为空,返回 dst 的浅拷贝。
func deepMerge(dst, src map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}

	for k, sv := range src {
		dv, exists := out[k]
		if !exists {
			out[k] = sv
			continue
		}

		dm, dIsMap := dv.(map[string]any)
		sm, sIsMap := sv.(map[string]any)
		if dIsMap && sIsMap {
			out[k] = deepMerge(dm, sm)
			continue
		}

		da, dIsSlice := dv.([]any)
		sa, sIsSlice := sv.([]any)
		if dIsSlice && sIsSlice {
			// 数组追加:后者接在前者之后,不做去重。
			merged := make([]any, 0, len(da)+len(sa))
			merged = append(merged, da...)
			merged = append(merged, sa...)
			out[k] = merged
			continue
		}

		// 类型不一致或标量:高优先级覆盖。
		out[k] = sv
	}

	return out
}

// applyReset 依据 src 中的 reset 字段,从 dst 中删除指定路径的值。
//
// 语义:
//   - 生效时机:在当前文件参与的合并开始前,先丢弃这些路径从低优先级来源继承的值
//   - 作用范围:只清空 dst 中的值;更高优先级来源(ENV / CLI)由调用序保证不受影响
//   - 字段路径:点号分隔,如 "routing.weight"
//   - 表与数组:对 map 清空整表,对数组清空整列表
//   - 不存在的路径:静默忽略,不报错
//
// applyReset 返回被清空的路径列表(已去重并排序),供日志与调试使用。
func applyReset(dst, src map[string]any) []string {
	paths := extractResetPaths(src)
	if len(paths) == 0 {
		return nil
	}
	for _, p := range paths {
		deletePath(dst, p)
	}
	return paths
}

// extractResetPaths 从配置 map 中提取 reset 声明的字段路径。
//
// 支持两种写法:
//
//	reset = ["listen", "peers"]
//	reset = "listen"            # 单值放宽处理
//
// 非字符串项被忽略,避免因配置笔误导致加载失败。
func extractResetPaths(m map[string]any) []string {
	raw, ok := m[ResetKey]
	if !ok {
		return nil
	}

	var paths []string
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				paths = append(paths, s)
			}
		}
	case []string:
		paths = append(paths, v...)
	case string:
		if v != "" {
			paths = append(paths, v)
		}
	}

	return dedupeSorted(paths)
}

// deletePath 从嵌套 map 中删除点号分隔的路径。
//
// 路径不存在时静默返回,不视为错误。
func deletePath(m map[string]any, path string) {
	parts := strings.Split(path, ".")
	cur := m

	for i, part := range parts {
		if part == "" {
			return
		}
		// 最后一段:直接删除。
		if i == len(parts)-1 {
			delete(cur, part)
			return
		}
		next, ok := cur[part].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
}

// subMap 取出路径对应的嵌套 map;不存在或类型不符时返回 nil。
func subMap(m map[string]any, path string) map[string]any {
	parts := strings.Split(path, ".")
	cur := m

	for _, part := range parts {
		if part == "" {
			return nil
		}
		next, ok := cur[part].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// setPath 在嵌套 map 中设置点号分隔路径的值,中间层不存在时自动创建。
func setPath(m map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	cur := m

	for i, part := range parts {
		if part == "" {
			return
		}
		if i == len(parts)-1 {
			cur[part] = value
			return
		}
		next, ok := cur[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[part] = next
		}
		cur = next
	}
}

// mergeWithReset 先执行 src 的 reset,再把 src 合并进 dst。
//
// 这是配置加载的核心步骤:每个文件在参与合并前,其 reset 声明先生效。
func mergeWithReset(dst, src map[string]any) (map[string]any, []string) {
	reset := applyReset(dst, src)
	out := deepMerge(dst, src)
	// reset 是合并期元字段,不进入运行时配置。
	delete(out, ResetKey)
	return out, reset
}

// dedupeSorted 去重并排序,保证不同机器上的结果一致。
func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))

	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// formatResetPaths 把路径列表渲染为日志友好的形式。
func formatResetPaths(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return fmt.Sprintf("[%s]", strings.Join(paths, ", "))
}
