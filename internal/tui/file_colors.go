package tui

import (
	"path/filepath"
	"strings"
)

// Color reinforces the textual type badge; selection keeps its own background.
func fileColor(name, kind string) string {
	switch kind {
	case "LINK":
		return "110;210;245"
	case "DIR", "BACK":
		return accent
	case "IMAGE":
		return "186;158;247"
	case "GIF":
		return "241;151;209"
	case "VIDEO":
		return "244;192;112"
	case "AUDIO":
		return "153;214;151"
	case "MD", "MMD":
		return "128;188;245"
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".zip", ".gz", ".xz", ".tar", ".bz2", ".7z", ".rar":
		return "232;153;119"
	case ".pdf":
		return "243;143;153"
	case ".json", ".yaml", ".yml", ".toml", ".ini", ".conf", ".xml", ".lock":
		return "244;192;112"
	case ".go", ".rs", ".zig", ".py", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".sh", ".bash", ".zsh", ".c", ".h", ".cpp", ".java", ".kt", ".rb", ".lua", ".sql", ".html", ".css", ".scss", ".swift":
		return "128;188;245"
	}
	switch strings.ToLower(filepath.Base(name)) {
	case "makefile", "dockerfile", "justfile", "go.mod", ".env":
		return "244;192;112"
	}
	return fg
}
