package profiles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

func ClaudeTelemetry(id string) map[string]string {
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_METRICS_EXPORTER":        "otlp", "OTEL_LOGS_EXPORTER": "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":                       "http/protobuf",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT":               "http://127.0.0.1:8428/opentelemetry/v1/metrics",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":                  "http://127.0.0.1:9428/insert/opentelemetry/v1/logs",
		"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "cumulative",
		"OTEL_METRIC_EXPORT_INTERVAL":                       "15000", "OTEL_LOG_TOOL_DETAILS": "1",
		"OTEL_RESOURCE_ATTRIBUTES": "profile=" + id,
	}
}
func (s *Store) wire(p *Profile) ([]fileEdit, error) {
	p.WiringError = ""
	if !p.Enabled {
		p.WiringStatus = "disabled"
		return nil, nil
	}
	if st, err := os.Stat(p.Directory); err != nil || !st.IsDir() {
		p.WiringStatus = "missing"
		p.WiringError = "Profile directory is unavailable"
		return nil, nil
	}
	if p.Provider == Claude {
		return s.wireClaude(p)
	}
	if p.Provider == Codex {
		return s.wireCodex(p)
	}
	if p.Provider == Omp {
		inspectOmp(p)
		return nil, nil
	}
	if p.Provider == Opencode {
		inspectOpencode(p)
		return nil, nil
	}
	return nil, fmt.Errorf("unknown provider %q", p.Provider)
}
func conflict(p *Profile, message string) ([]fileEdit, error) {
	p.WiringStatus = "conflict"
	p.WiringError = message
	return nil, nil
}
func settingsObject(data []byte) (map[string]any, error) {
	m := map[string]any{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		if m == nil {
			return nil, fmt.Errorf("settings must be a JSON object")
		}
	}
	return m, nil
}
func (s *Store) wireClaude(p *Profile) ([]fileEdit, error) {
	e, err := readEdit(filepath.Join(p.Directory, "settings.json"))
	if err != nil {
		return nil, err
	}
	m, err := settingsObject(e.Before)
	if err != nil {
		return conflict(p, "settings.json: "+err.Error())
	}
	env, ok := m["env"].(map[string]any)
	if m["env"] != nil && !ok {
		return conflict(p, "settings.json env must be an object")
	}
	if env == nil {
		env = map[string]any{}
	}
	desired := ClaudeTelemetry(p.ID)
	var conflicts []string
	for key, value := range desired {
		if got, ok := env[key]; ok && got != value {
			conflicts = append(conflicts, key)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return conflict(p, "Existing telemetry settings differ: "+strings.Join(conflicts, ", ")+". Existing destinations were preserved.")
	}
	owned := &Ownership{Path: e.Path, Env: map[string]string{}}
	if p.Owned != nil {
		for k, v := range p.Owned.Env {
			owned.Env[k] = v
		}
		owned.Statusline = p.Owned.Statusline
	}
	legacy := p.Owned != nil && p.Owned.Legacy
	for key, value := range desired {
		if _, present := env[key]; !present || legacy {
			owned.Env[key] = value
		}
		env[key] = value
	}
	m["env"] = env
	command := filepath.Join(s.Dir, "bin", "statusline.sh")
	if existing, present := m["statusLine"]; !present {
		m["statusLine"] = map[string]any{"type": "command", "command": command}
		owned.Statusline = command
	} else if sl, ok := existing.(map[string]any); ok && sl["command"] == command && legacy {
		owned.Statusline = command
	}
	e.After, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	e.After = append(e.After, '\n')
	p.Owned = owned
	p.WiringStatus = "connected"
	return []fileEdit{e}, nil
}
func (s *Store) wireCodex(p *Profile) ([]fileEdit, error) {
	e, err := readEdit(filepath.Join(p.Directory, "config.toml"))
	if err != nil {
		return nil, err
	}
	original := string(e.Before)
	without := original
	if p.Owned != nil && p.Owned.CodexBlock != "" {
		block, ok := ownedCodexBlock(original, p.Owned.CodexBlock)
		if !ok {
			return conflict(p, "The managed Codex telemetry block was edited; restore it or remove its otel settings before repair.")
		}
		without = strings.Replace(original, block, "", 1)
	}
	var parsed map[string]any
	if err := toml.Unmarshal([]byte(without), &parsed); err != nil {
		return conflict(p, "config.toml: "+err.Error())
	}
	insertAt := len(without)
	header := "[otel]\n"
	existing := map[string]any{}
	if otel, present := parsed["otel"]; present {
		var ok bool
		existing, ok = otel.(map[string]any)
		if !ok {
			return conflict(p, "Existing Codex otel configuration is not a table.")
		}
		// Insert inside an existing explicit table without reserializing any
		// unrelated keys, comments, trust declarations or credential config.
		offset, found := otelTableOffset([]byte(without))
		if !found {
			return conflict(p, "Codex otel uses inline or dotted settings; use an [otel] table before repair.")
		}
		insertAt = offset
		header = ""
	}
	fields := []struct{ name, value string }{
		{"log_user_prompt", "false"},
		{"exporter", "{ otlp-http = { endpoint = " + strconv.Quote("http://127.0.0.1:8427/ingest/codex/"+p.ID+"/v1/logs") + ", protocol = \"binary\" } }"},
		{"metrics_exporter", "{ otlp-http = { endpoint = " + strconv.Quote("http://127.0.0.1:8427/ingest/codex/"+p.ID+"/v1/metrics") + ", protocol = \"binary\" } }"},
	}
	block := "\n# Agent Observability managed telemetry (" + p.ID + ").\n" + header
	for _, field := range fields {
		if current, present := existing[field.name]; present {
			var desired map[string]any
			if err := toml.Unmarshal([]byte(field.name+" = "+field.value), &desired); err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(current, desired[field.name]) {
				return conflict(p, "Existing Codex otel."+field.name+" was preserved. Resolve that telemetry conflict before repair.")
			}
			continue
		}
		block += field.name + " = " + field.value + "\n"
	}
	block += "# End Agent Observability managed telemetry.\n"
	e.After = []byte(without[:insertAt] + block + without[insertAt:])

	var validate map[string]any
	if err := toml.Unmarshal(e.After, &validate); err != nil {
		return conflict(p, "Cannot safely merge Codex telemetry: "+err.Error())
	}
	p.Owned = &Ownership{Path: e.Path, CodexBlock: block}
	p.WiringStatus = "connected"
	return []fileEdit{e}, nil
}
func (s *Store) unwire(p Profile) ([]fileEdit, error) {
	owned := p.Owned
	if owned == nil {
		return nil, nil
	}
	if owned.Legacy && p.Provider == Claude {
		owned = &Ownership{Path: filepath.Join(p.Directory, "settings.json"), Env: ClaudeTelemetry(p.ID), Statusline: filepath.Join(s.Dir, "bin", "statusline.sh")}
	}
	if owned.Path == "" {
		return nil, nil
	}
	e, err := readEdit(owned.Path)
	if err != nil {
		return nil, err
	}
	if !e.Existed {
		return nil, nil
	}
	if p.Provider == Codex {
		if owned.CodexBlock == "" {
			return nil, nil
		}
		block, ok := ownedCodexBlock(string(e.Before), owned.CodexBlock)
		if !ok {
			return nil, fmt.Errorf("%s: managed telemetry was edited; disconnect refused to discard those edits", owned.Path)
		}
		e.After = []byte(strings.Replace(string(e.Before), block, "", 1))
		return []fileEdit{e}, nil
	}
	m, err := settingsObject(e.Before)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", owned.Path, err)
	}
	env, _ := m["env"].(map[string]any)
	for key, value := range owned.Env {
		if env[key] == value {
			delete(env, key)
		}
	}
	if len(env) == 0 {
		delete(m, "env")
	}
	if sl, ok := m["statusLine"].(map[string]any); ok && owned.Statusline != "" && sl["command"] == owned.Statusline {
		delete(m, "statusLine")
	}
	e.After, err = json.MarshalIndent(m, "", "  ")
	e.After = append(e.After, '\n')
	return []fileEdit{e}, err
}

// Some config writers omit the file's final newline. That leaves telemetry
// unchanged and must not prevent inspection, repair or disconnect. Accept only
// that exact EOF difference; edits to managed settings still remain conflicts.
func ownedCodexBlock(contents, owned string) (string, bool) {
	if owned == "" {
		return "", false
	}
	if strings.Contains(contents, owned) {
		return owned, true
	}
	withoutNewline := strings.TrimSuffix(owned, "\n")
	if withoutNewline != owned && strings.HasSuffix(contents, withoutNewline) {
		return withoutNewline, true
	}
	return "", false
}

// otelTableOffset locates a real table header through the TOML parser, so text
// resembling [otel] inside multiline strings or comments cannot misdirect edits.
func otelTableOffset(data []byte) (int, bool) {
	var parser unstable.Parser
	parser.Reset(data)
	for parser.NextExpression() {
		expr := parser.Expression()
		if expr.Kind != unstable.Table {
			continue
		}
		key := expr.Key()
		if !key.Next() {
			continue
		}
		node := key.Node()
		offset := int(node.Raw.Offset + node.Raw.Length)
		if string(node.Data) != "otel" || key.Next() {
			continue
		}
		if end := strings.IndexByte(string(data[offset:]), '\n'); end >= 0 {
			return offset + end + 1, true
		}
		return len(data), true
	}
	return 0, false
}

// inspectWiring reads the current files rather than treating the last successful
// write as proof that wiring is still present.
func (s *Store) inspectWiring(p *Profile) {
	if !p.Enabled {
		p.WiringStatus = "disabled"
		p.WiringError = ""
		return
	}
	if st, err := os.Stat(p.Directory); err != nil || !st.IsDir() {
		p.WiringStatus = "missing"
		p.WiringError = "Profile directory is unavailable"
		return
	}
	if p.Provider == Claude {
		data, err := os.ReadFile(filepath.Join(p.Directory, "settings.json"))
		m, parseErr := settingsObject(data)
		if err != nil || parseErr != nil {
			p.WiringStatus = "disconnected"
			p.WiringError = "settings.json is missing or invalid; use Repair"
			return
		}
		env, _ := m["env"].(map[string]any)
		for key, value := range ClaudeTelemetry(p.ID) {
			if env[key] != value {
				p.WiringStatus = "conflict"
				p.WiringError = "Telemetry settings differ or are missing; use Repair"
				return
			}
		}
		p.WiringStatus = "connected"
		p.WiringError = ""
		return
	}
	if p.Provider == Omp {
		inspectOmp(p)
		return
	}
	if p.Provider == Opencode {
		inspectOpencode(p)
		return
	}
	data, err := os.ReadFile(filepath.Join(p.Directory, "config.toml"))
	if err != nil {
		p.WiringStatus = "disconnected"
		p.WiringError = "config.toml is missing; use Repair"
		return
	}
	var blockPresent bool
	if p.Owned != nil {
		_, blockPresent = ownedCodexBlock(string(data), p.Owned.CodexBlock)
	}
	if !blockPresent {
		p.WiringStatus = "conflict"
		if p.WiringError == "" {
			p.WiringError = "Codex telemetry is missing or changed; use Repair"
		}
		return
	}
	var parsed map[string]any
	if err := toml.Unmarshal(data, &parsed); err != nil {
		p.WiringStatus = "conflict"
		p.WiringError = "config.toml: " + err.Error()
		return
	}
	probe := *p
	if _, err := s.wireCodex(&probe); err != nil {
		p.WiringStatus = "conflict"
		p.WiringError = err.Error()
		return
	}
	p.WiringStatus = probe.WiringStatus
	p.WiringError = probe.WiringError
}

// inspectOmp has only the sessions directory to check. A profile pointed at
// ~/.omp rather than ~/.omp/agent is the likely mistake, so the error names
// the directory omp actually writes to.
func inspectOmp(p *Profile) {
	if st, err := os.Stat(filepath.Join(p.Directory, "sessions")); err != nil || !st.IsDir() {
		p.WiringStatus = "missing"
		p.WiringError = "No sessions directory here; choose the Oh My Pi agent directory, usually ~/.omp/agent"
		return
	}
	p.WiringStatus = "connected"
	p.WiringError = ""
}

// inspectOpencode has only opencode's database to check. The directory is
// opencode's data directory, not its config directory, which is the likely
// mistake, so the error names the one that holds opencode.db.
func inspectOpencode(p *Profile) {
	if st, err := os.Stat(filepath.Join(p.Directory, "opencode.db")); err != nil || !st.Mode().IsRegular() {
		p.WiringStatus = "missing"
		p.WiringError = "No opencode.db here; choose opencode's data directory, usually ~/.local/share/opencode"
		return
	}
	p.WiringStatus = "connected"
	p.WiringError = ""
}
