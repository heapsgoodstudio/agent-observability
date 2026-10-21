package installer

import (
	"encoding/json"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"sort"
)

func ProfileName(profileDir string) string { return profiles.LegacyName(profileDir) }

// TelemetryEnv keeps the historical Claude labels used by existing series.
func TelemetryEnv(profileName string) map[string]string { return profiles.ClaudeTelemetry(profileName) }

func parse(settings []byte) (map[string]any, error) {
	m := map[string]any{}
	if len(settings) == 0 {
		return m, nil
	}
	err := json.Unmarshal(settings, &m)
	return m, err
}

func marshal(m map[string]any) ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

func MergeTelemetry(settings []byte, desired map[string]string) ([]byte, bool, []string, error) {
	m, err := parse(settings)
	if err != nil {
		return nil, false, nil, err
	}
	cur, _ := m["env"].(map[string]any)
	if cur == nil {
		cur = map[string]any{}
	}
	identical := true
	var conflicts []string
	for k, v := range desired {
		got, present := cur[k]
		if !present {
			identical = false
			continue
		}
		if got != v {
			conflicts = append(conflicts, k)
		}
	}
	sort.Strings(conflicts)
	if len(conflicts) > 0 {
		return nil, false, conflicts, nil
	}
	if identical {
		return settings, false, nil, nil
	}
	for k, v := range desired {
		cur[k] = v
	}
	m["env"] = cur
	out, err := marshal(m)
	return out, true, nil, err
}

func RemoveTelemetry(settings []byte) ([]byte, error) {
	m, err := parse(settings)
	if err != nil {
		return nil, err
	}
	cur, _ := m["env"].(map[string]any)
	if cur != nil {
		for k := range TelemetryEnv("") {
			delete(cur, k)
		}
		if len(cur) == 0 {
			delete(m, "env")
		} else {
			m["env"] = cur
		}
	}
	return marshal(m)
}

func statuslineCommand(m map[string]any) string {
	sl, _ := m["statusLine"].(map[string]any)
	if sl == nil {
		return ""
	}
	cmd, _ := sl["command"].(string)
	return cmd
}

func SetStatusline(settings []byte, cmd string) ([]byte, string, error) {
	m, err := parse(settings)
	if err != nil {
		return nil, "", err
	}
	if _, present := m["statusLine"]; present {
		return settings, statuslineCommand(m), nil
	}
	m["statusLine"] = map[string]any{"type": "command", "command": cmd}
	out, err := marshal(m)
	return out, "", err
}

func RemoveStatusline(settings []byte, cmds []string) ([]byte, bool, error) {
	m, err := parse(settings)
	if err != nil {
		return nil, false, err
	}
	cur := statuslineCommand(m)
	for _, c := range cmds {
		if cur != "" && cur == c {
			delete(m, "statusLine")
			out, err := marshal(m)
			return out, true, err
		}
	}
	return settings, false, nil
}
