package claude

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// ansiPattern matches ANSI escape sequences:
//   - CSI sequences: ESC [ ... final_byte
//   - OSC sequences: ESC ] ... ST
//   - Bare ESC characters (0x1B)
var ansiPattern = regexp.MustCompile(`\x1b(?:\[[0-9;]*[a-zA-Z]|\][^\x07\x1b]*(?:\x07|\x1b\\))|\x1b`)

// stripANSI removes ANSI escape sequences and bare ESC characters from s.
func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

func sse(event string, payload map[string]any) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\n")
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return buf.String(), nil
}
