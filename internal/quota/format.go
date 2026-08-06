package quota

import (
	"strings"

	"github.com/Rentheria/llm-agent-spend-manager/internal/humanize"
)

// millions and count are the two number shapes this package's prose uses. They
// are thin aliases so a lever's evidence line reads the same as the table the
// reader is comparing it against.
func millions(v float64) string { return humanize.Millions(v) }

func count(n int) string { return humanize.Int(n) }

// workspaceNameSegments is how much of a path is enough to tell two workspaces
// apart. The last segment alone collides (every repo has a "src"); the full
// absolute path buries the distinguishing part at the end of a long line.
const workspaceNameSegments = 2

// WorkspaceName shortens a working directory to something readable in a table
// while staying unambiguous: "/home/user/.openclaw/workspace" becomes
// ".openclaw/workspace". Non-paths and short paths pass through untouched.
func WorkspaceName(path string) string {
	if path == "" {
		return "(sin directorio)"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) <= workspaceNameSegments {
		return path
	}
	return strings.Join(parts[len(parts)-workspaceNameSegments:], "/")
}
