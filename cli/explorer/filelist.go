package explorer

// CategoryDef defines a file category for the explorer tree.
type CategoryDef struct {
	Name string
	Icon rune
}

// DefaultCategories returns the standard memQL file categories.
func DefaultCategories() []CategoryDef {
	return []CategoryDef{
		{Name: "Queries", Icon: '?'},
		{Name: "Mutations", Icon: '!'},
		{Name: "Automations", Icon: '⚡'},
		{Name: "Specs", Icon: '✓'},
		{Name: "Shapes", Icon: '◇'},
		{Name: "Tools", Icon: '⚙'},
		{Name: "Prompts", Icon: '💬'},
		{Name: "Providers", Icon: '◈'},
		{Name: "Concepts", Icon: '◉'},
	}
}

// BuildTree creates the root tree nodes from category definitions and file entries.
// files maps category name -> list of file entries (name, path).
func BuildTree(categories []CategoryDef, files map[string][]FileEntry) []*TreeNode {
	var roots []*TreeNode
	for _, cat := range categories {
		catNode := &TreeNode{
			Name:     cat.Name,
			Type:     "category",
			Icon:     cat.Icon,
			Expanded: false,
		}

		entries := files[cat.Name]
		for _, f := range entries {
			catNode.Children = append(catNode.Children, &TreeNode{
				Name: f.Name,
				Path: f.Path,
				Type: "file",
				Icon: '▪',
			})
		}

		roots = append(roots, catNode)
	}
	return roots
}

// FileEntry represents a single .memql file in the explorer.
type FileEntry struct {
	Name string // display name (e.g., "spaceParticipants")
	Path string // identifier for loading content
}

// BuildTreeFromFlatList creates tree nodes from a flat list of file info maps.
// Each map should have "name", "path", and "kind" keys.
func BuildTreeFromFlatList(items []map[string]string) map[string][]FileEntry {
	result := make(map[string][]FileEntry)
	for _, item := range items {
		kind := item["kind"]
		category := kindToCategory(kind)
		result[category] = append(result[category], FileEntry{
			Name: item["name"],
			Path: item["path"],
		})
	}
	return result
}

func kindToCategory(kind string) string {
	switch kind {
	case "query":
		return "Queries"
	case "mutation":
		return "Mutations"
	case "automation":
		return "Automations"
	case "spec":
		return "Specs"
	case "shape":
		return "Shapes"
	case "tool":
		return "Tools"
	case "prompt":
		return "Prompts"
	case "provider":
		return "Providers"
	case "concept":
		return "Concepts"
	default:
		return "Other"
	}
}
