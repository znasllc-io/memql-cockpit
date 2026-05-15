package explorer

import (
	"fmt"
	"strings"
)

// AgentPathPrefix marks a tree-node Path as referring to an agent row
// rather than an on-disk .memql file. The app layer's OnFileOpen
// switches on this prefix to route the open into the agent-rendering
// path (which reads from the cached AgentEntry map) instead of trying
// to load source from a file.
const AgentPathPrefix = "agent://"

// AgentIDFromPath strips the AgentPathPrefix and returns the bare row
// id, ok=false when the path is not an agent path.
func AgentIDFromPath(path string) (string, bool) {
	if !strings.HasPrefix(path, AgentPathPrefix) {
		return "", false
	}
	return strings.TrimPrefix(path, AgentPathPrefix), true
}

// AgentPathForID is the inverse of AgentIDFromPath -- builds the
// prefixed path the tree node carries.
func AgentPathForID(id string) string {
	return AgentPathPrefix + id
}

// CategoryDef defines a file category for the explorer tree.
type CategoryDef struct {
	Name string
	Icon rune
}

// DefaultCategories returns the standard memQL file categories.
func DefaultCategories() []CategoryDef {
	return []CategoryDef{
		{Name: "Agents", Icon: '☻'},
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
			icon := '▪'
			if strings.HasPrefix(f.Path, AgentPathPrefix) {
				icon = '☻'
			}
			catNode.Children = append(catNode.Children, &TreeNode{
				Name: f.Name,
				Path: f.Path,
				Type: "file",
				Icon: icon,
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

// AgentEntry is a materialized v1:agents:agent row projected into the
// shape the explorer renders in its detail pane. Lives here (not in
// app.go) so the explorer view package can render it without taking a
// dependency on the cli package.
type AgentEntry struct {
	ID          string
	Name        string
	Description string
	Role        string
	RoleSlug    string
	Gender      string
	Active      bool
	Deleted     bool
	Personality string
	OwnerUserId string

	LLMPolicyName string
	LLMProvider   string
	LLMModel      string

	Tools     []string
	Domains   []string
	Keywords  []string
	GroupIds  []string

	CapAvatar       bool
	CapLipSync      bool
	CapVision       bool
	CapVoiceToVoice bool
	CapClaw         bool

	AutoJoin    bool
	GreetOnJoin bool
	SpeakWhen   string

	AudioControl string
	VideoControl string
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

// RenderAgent formats an AgentEntry into the human-readable block the
// detail pane shows when the user opens an agent row. Shape is meant
// to read like a condensed agent.memql declaration so users familiar
// with the DSL recognize the layout, but rendered fields reflect the
// MATERIALIZED row (post-user-personalization), not the static DSL
// template.
func RenderAgent(a AgentEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agent %s {\n", displayNameOrFallback(a))
	fmt.Fprintf(&b, "  id:           %s\n", a.ID)
	if a.Description != "" {
		fmt.Fprintf(&b, "  description:  %q\n", a.Description)
	}
	fmt.Fprintf(&b, "  role:         %s\n", quoteOrEmpty(a.Role))
	if a.RoleSlug != "" {
		fmt.Fprintf(&b, "  roleSlug:     %q\n", a.RoleSlug)
	}
	if a.Gender != "" {
		fmt.Fprintf(&b, "  gender:       %q\n", a.Gender)
	}
	fmt.Fprintf(&b, "  active:       %v\n", a.Active)
	if a.Personality != "" {
		fmt.Fprintf(&b, "  personality:  %q\n", a.Personality)
	}
	if a.OwnerUserId != "" {
		fmt.Fprintf(&b, "  ownerUserId:  %s\n", a.OwnerUserId)
	}

	if a.LLMPolicyName != "" || a.LLMProvider != "" || a.LLMModel != "" {
		b.WriteString("\n  providerConfig {\n")
		b.WriteString("    llm {\n")
		if a.LLMProvider != "" {
			fmt.Fprintf(&b, "      provider:    %q\n", a.LLMProvider)
		}
		if a.LLMModel != "" {
			fmt.Fprintf(&b, "      model:       %q\n", a.LLMModel)
		}
		if a.LLMPolicyName != "" {
			fmt.Fprintf(&b, "      policyName:  %q\n", a.LLMPolicyName)
		}
		b.WriteString("    }\n")
		b.WriteString("  }\n")
	}

	b.WriteString("\n  capabilities {\n")
	fmt.Fprintf(&b, "    avatar:       %v\n", a.CapAvatar)
	fmt.Fprintf(&b, "    lipSync:      %v\n", a.CapLipSync)
	fmt.Fprintf(&b, "    vision:       %v\n", a.CapVision)
	fmt.Fprintf(&b, "    voiceToVoice: %v\n", a.CapVoiceToVoice)
	fmt.Fprintf(&b, "    claw:         %v\n", a.CapClaw)
	if len(a.Tools) > 0 {
		fmt.Fprintf(&b, "    tools:        %s\n", quotedList(a.Tools))
	}
	if len(a.Domains) > 0 {
		fmt.Fprintf(&b, "    domains:      %s\n", quotedList(a.Domains))
	}
	if len(a.Keywords) > 0 {
		fmt.Fprintf(&b, "    keywords:     %s\n", quotedList(a.Keywords))
	}
	b.WriteString("  }\n")

	b.WriteString("\n  triggerBehavior {\n")
	fmt.Fprintf(&b, "    autoJoin:     %v\n", a.AutoJoin)
	fmt.Fprintf(&b, "    greetOnJoin:  %v\n", a.GreetOnJoin)
	if a.SpeakWhen != "" {
		fmt.Fprintf(&b, "    speakWhen:    %q\n", a.SpeakWhen)
	}
	b.WriteString("  }\n")

	if a.AudioControl != "" {
		fmt.Fprintf(&b, "\n  audioControl: %q\n", a.AudioControl)
	}
	if a.VideoControl != "" {
		fmt.Fprintf(&b, "  videoControl: %q\n", a.VideoControl)
	}

	if len(a.GroupIds) > 0 {
		fmt.Fprintf(&b, "\n  groupIds:     %s\n", quotedList(a.GroupIds))
	}

	b.WriteString("}\n")
	return b.String()
}

func displayNameOrFallback(a AgentEntry) string {
	if a.Name != "" {
		return a.Name
	}
	if a.RoleSlug != "" {
		return a.RoleSlug
	}
	return a.ID
}

func quoteOrEmpty(s string) string {
	if s == "" {
		return `""`
	}
	return fmt.Sprintf("%q", s)
}

func quotedList(items []string) string {
	var b strings.Builder
	b.WriteString("[")
	for i, item := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", item)
	}
	b.WriteString("]")
	return b.String()
}

func kindToCategory(kind string) string {
	switch kind {
	case "agent":
		return "Agents"
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
