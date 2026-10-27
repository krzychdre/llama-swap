package config

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	macroNameRegex    = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	macroPatternRegex = regexp.MustCompile(`\$\{([a-zA-Z0-9_-]+)\}`)
	envMacroRegex     = regexp.MustCompile(`\$\{env\.([a-zA-Z_][a-zA-Z0-9_]*)\}`)
)

// macroExpansionLimit bounds the resolved macro values in one scope, so a
// chain such as m1: ${m0}${m0}, m2: ${m1}${m1} fails to load instead of
// materialising 2^n copies of a value.
const macroExpansionLimit = 4 << 20

// resolvedMacro carries a macro's value twice: value for a whole-placeholder
// replacement, which keeps the declared scalar type, and text for expanding the
// placeholder inside a longer string.
type resolvedMacro struct {
	value any
	text  string
}

type modelMacroConfig struct {
	Macros MacroList `yaml:"macros"`
}

type configMacroConfig struct {
	Macros MacroList                   `yaml:"macros"`
	Models map[string]modelMacroConfig `yaml:"models"`
}

// validateMacro validates macro name and value constraints
func validateMacro(name string, value any) error {
	if len(name) >= 64 {
		return fmt.Errorf("macro name '%s' exceeds maximum length of 63 characters", name)
	}
	if !macroNameRegex.MatchString(name) {
		return fmt.Errorf("macro name '%s' contains invalid characters, must match pattern ^[a-zA-Z0-9_-]+$", name)
	}

	// Validate that value is a scalar type
	switch v := value.(type) {
	case string:
		// Check for self-reference
		macroSlug := fmt.Sprintf("${%s}", name)
		if strings.Contains(v, macroSlug) {
			return fmt.Errorf("macro '%s' contains self-reference", name)
		}
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, bool:
		// These types are allowed
	default:
		return fmt.Errorf("macro '%s' has invalid type %T, must be a scalar type (string, int, float, or bool)", name, value)
	}

	switch name {
	case "PID", "PORT", "MODEL_ID":
		return fmt.Errorf("macro name '%s' is reserved", name)
	}

	return nil
}

// resolveConfigMacros expands all non-environment macros before the typed
// configuration is decoded. Decoding into untyped values first materializes
// YAML aliases and preserves scalar macro values.
func resolveConfigMacros(yamlStr string) (map[string]any, configMacroConfig, error) {
	// Environment macros have already been expanded by LoadConfigFromReader.
	// From here the flow is:
	//
	//  1. Read and validate the ordered macro declarations.
	//  2. Decode a second, untyped view of the YAML so direct replacements can
	//     retain their scalar types and YAML aliases are fully materialized.
	//  3. Expand global values, then each model with its effective global and
	//     model-local scope.
	//  4. Allocate the runtime-derived PORT value in stable model-ID order.
	//  5. Reject anything unresolved before the caller decodes the result into
	//     Config.
	//
	// The declarations and untyped values intentionally remain separate:
	// MacroList preserves declaration order, while map[string]any is the value
	// tree that can be transformed without first satisfying Config's concrete
	// field types.
	var declarations configMacroConfig
	if err := yaml.Unmarshal([]byte(yamlStr), &declarations); err != nil {
		return nil, configMacroConfig{}, err
	}

	// Validate declarations before touching the value tree. This produces
	// declaration-focused errors and prevents reserved runtime macros from
	// entering the ordinary replacement list.
	for _, macro := range declarations.Macros {
		if err := validateMacro(macro.Name, macro.Value); err != nil {
			return nil, configMacroConfig{}, err
		}
	}
	for modelID, model := range declarations.Models {
		for _, macro := range model.Macros {
			if err := validateMacro(macro.Name, macro.Value); err != nil {
				return nil, configMacroConfig{}, fmt.Errorf("model %s: %s", modelID, err)
			}
		}
	}

	var raw map[string]any
	if err := yaml.Unmarshal([]byte(yamlStr), &raw); err != nil {
		return nil, configMacroConfig{}, err
	}
	if raw == nil {
		raw = make(map[string]any)
	}

	// Macro declarations are definitions, not substitution targets. They are
	// restored on the typed Config after expansion so their original order and
	// values remain observable.
	delete(raw, "macros")

	// Prepare models before expansion. Macro blocks are detached for the same
	// reason as the global block. Command comments must be removed before a
	// placeholder in a comment can expand into executable text, and the proxy
	// default must exist now so its PORT placeholder participates in the same
	// generic model pass as explicitly configured values.
	models, _ := raw["models"].(map[string]any)
	for modelID, value := range models {
		model, ok := value.(map[string]any)
		if !ok {
			continue
		}
		delete(model, "macros")
		stripRawCommandComments(model)
		if proxy, found := model["proxy"]; !found || proxy == nil {
			model["proxy"] = MODEL_CONFIG_DEFAULT_PROXY
		}
		models[modelID] = model
	}

	resolvedGlobals, err := resolveMacros(declarations.Macros)
	if err != nil {
		return nil, configMacroConfig{}, err
	}

	// Global macros apply to every top-level configuration value except models,
	// which also need their model-local macro scope.
	for key, value := range raw {
		if key == "models" {
			continue
		}
		raw[key] = substituteMacros(value, resolvedGlobals)
	}

	// startPort may itself contain a global macro, so read it only after the
	// global pass. Sorting models preserves the existing deterministic port
	// allocation contract.
	startPort, err := rawStartPort(raw)
	if err != nil {
		return nil, configMacroConfig{}, err
	}

	modelIDs := make([]string, 0, len(models))
	for modelID := range models {
		modelIDs = append(modelIDs, modelID)
	}
	sort.Strings(modelIDs)

	nextPort := startPort
	for _, modelID := range modelIDs {
		model, ok := models[modelID].(map[string]any)
		if !ok {
			continue
		}

		// mergeModelMacros adds MODEL_ID, overlays model definitions on globals,
		// and preserves declaration order. The substitution then covers every
		// model value; setParamsByID keys are handled separately because
		// ordinary mapping keys are intentionally not macro targets.
		macros := mergeModelMacros(modelID, declarations.Macros, declarations.Models[modelID].Macros)
		resolved, err := resolveMacros(macros)
		if err != nil {
			return nil, configMacroConfig{}, fmt.Errorf("model %s: %w", modelID, err)
		}
		model = substituteMacros(model, resolved).(map[string]any)
		if err := substituteSetParamsByIDKeys(model, resolved); err != nil {
			return nil, configMacroConfig{}, fmt.Errorf("model %s filters.setParamsByID: %w", modelID, err)
		}

		cmd, _ := model["cmd"].(string)
		proxy, _ := model["proxy"].(string)
		cmdHasPort := strings.Contains(cmd, "${PORT}")
		proxyHasPort := strings.Contains(proxy, "${PORT}")
		if cmdHasPort || proxyHasPort {
			// PORT is available only to models whose command requests an
			// automatically allocated port. Once allocated, apply it throughout
			// that model just like a typed macro value.
			if !cmdHasPort && proxyHasPort {
				return nil, configMacroConfig{}, fmt.Errorf("model %s: proxy uses ${PORT} but cmd does not - ${PORT} is only available when used in cmd", modelID)
			}

			portResolved, err := resolveMacros(MacroList{{Name: "PORT", Value: nextPort}})
			if err != nil {
				return nil, configMacroConfig{}, fmt.Errorf("model %s: %w", modelID, err)
			}
			model = substituteMacros(model, portResolved).(map[string]any)
			if err := substituteSetParamsByIDKeys(model, portResolved); err != nil {
				return nil, configMacroConfig{}, fmt.Errorf("model %s filters.setParamsByID: %w", modelID, err)
			}
			nextPort++
		}

		if err := validateSetParamsByIDKeys(model, modelID); err != nil {
			return nil, configMacroConfig{}, err
		}
		models[modelID] = model
	}
	raw["models"] = models

	// This is the final boundary before concrete YAML decoding. PID is allowed
	// to survive only in cmdStop for process-time substitution; every other
	// recognized placeholder must have been resolved by this point.
	if err := validateConfigMacroUses(raw); err != nil {
		return nil, configMacroConfig{}, err
	}
	return raw, declarations, nil
}

func stripRawCommandComments(model map[string]any) {
	for _, field := range []string{"cmd", "cmdStop"} {
		if value, ok := model[field].(string); ok {
			model[field] = StripComments(value)
		}
	}
}

func rawStartPort(raw map[string]any) (int, error) {
	var node yaml.Node
	if err := node.Encode(raw); err != nil {
		return 0, err
	}
	value := struct {
		StartPort int `yaml:"startPort"`
	}{StartPort: 5800}
	if err := node.Decode(&value); err != nil {
		return 0, err
	}
	if value.StartPort < 1 {
		return 0, fmt.Errorf("startPort must be greater than 1")
	}
	return value.StartPort, nil
}

func mergeModelMacros(modelID string, global, model MacroList) MacroList {
	merged := make(MacroList, 0, len(global)+len(model)+1)
	merged = append(merged, MacroEntry{Name: "MODEL_ID", Value: modelID})
	merged = append(merged, global...)

	for _, entry := range model {
		found := false
		for i, existing := range merged {
			if existing.Name == entry.Name {
				merged[i] = entry
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, entry)
		}
	}
	return merged
}

// resolveMacros expands each macro value once, in declaration order, so a value
// may reference macros declared before it. A reference to a later macro stays
// literal and is reported by validateConfigMacroUses. Expansion is therefore
// linear in the declaration count, with macroExpansionLimit as the hard stop.
func resolveMacros(macros MacroList) (map[string]resolvedMacro, error) {
	resolved := make(map[string]resolvedMacro, len(macros))
	total := 0
	for _, entry := range macros {
		macro := resolveMacro(entry.Value, resolved)
		total += len(macro.text)
		if total > macroExpansionLimit {
			return nil, fmt.Errorf("macro expansion exceeded %d bytes", macroExpansionLimit)
		}
		resolved[entry.Name] = macro
	}
	return resolved, nil
}

// resolveMacro resolves one declaration against the macros declared before it.
// A value that is exactly one placeholder into an already resolved macro takes
// that macro's value, type included, so a chain of such declarations keeps the
// scalar type of the macro it ends at.
func resolveMacro(value any, resolved map[string]resolvedMacro) resolvedMacro {
	text, isString := value.(string)
	if !isString {
		return resolvedMacro{value: value, text: fmt.Sprintf("%v", value)}
	}
	if name, ok := macroPlaceholder(text); ok {
		if inner, found := resolved[name]; found {
			return inner
		}
	}
	expanded := expandMacroText(text, resolved)
	return resolvedMacro{value: expanded, text: expanded}
}

// substituteMacros replaces every resolved placeholder in a value structure. A
// string that is exactly one placeholder takes the macro's typed value, so an
// integer or boolean macro reaches the typed Config as that type.
func substituteMacros(value any, resolved map[string]resolvedMacro) any {
	switch v := value.(type) {
	case string:
		if name, ok := macroPlaceholder(v); ok {
			if macro, found := resolved[name]; found {
				return macro.value
			}
			return v
		}
		return expandMacroText(v, resolved)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, val := range v {
			out[key] = substituteMacros(val, resolved)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, val := range v {
			out[i] = substituteMacros(val, resolved)
		}
		return out
	default:
		return value
	}
}

func macroPlaceholder(s string) (string, bool) {
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		return "", false
	}
	name := s[2 : len(s)-1]
	if !macroNameRegex.MatchString(name) {
		return "", false
	}
	return name, true
}

func expandMacroText(s string, resolved map[string]resolvedMacro) string {
	return macroPatternRegex.ReplaceAllStringFunc(s, func(match string) string {
		if macro, found := resolved[match[2:len(match)-1]]; found {
			return macro.text
		}
		return match
	})
}

func substituteSetParamsByIDKeys(model map[string]any, macros map[string]resolvedMacro) error {
	filters, ok := model["filters"].(map[string]any)
	if !ok {
		return nil
	}
	params, ok := filters["setParamsByID"].(map[string]any)
	if !ok {
		return nil
	}

	resolved := make(map[string]any, len(params))
	for key, value := range params {
		newKey := expandMacroText(key, macros)
		if _, exists := resolved[newKey]; exists {
			return fmt.Errorf("keys %q and %q resolve to the same value", key, newKey)
		}
		resolved[newKey] = value
	}
	filters["setParamsByID"] = resolved
	return nil
}

func validateSetParamsByIDKeys(model map[string]any, modelID string) error {
	filters, ok := model["filters"].(map[string]any)
	if !ok {
		return nil
	}
	params, ok := filters["setParamsByID"].(map[string]any)
	if !ok {
		return nil
	}
	for key := range params {
		if matches := macroPatternRegex.FindAllStringSubmatch(key, -1); len(matches) > 0 {
			return fmt.Errorf("unknown macro '${%s}' found in model %s filters.setParamsByID key", matches[0][1], modelID)
		}
	}
	return nil
}

func validateConfigMacroUses(raw map[string]any) error {
	knownFields := make(map[string]struct{})
	configType := reflect.TypeOf(Config{})
	for i := 0; i < configType.NumField(); i++ {
		field := configType.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name != "" && name != "-" {
			knownFields[name] = struct{}{}
		}
	}

	for key, value := range raw {
		if _, known := knownFields[key]; !known {
			continue
		}
		if key != "models" {
			if err := validateMacroUses(value, key, "", "", false); err != nil {
				return err
			}
			continue
		}

		models, ok := value.(map[string]any)
		if !ok {
			continue
		}
		for modelID, modelValue := range models {
			model, ok := modelValue.(map[string]any)
			if !ok {
				continue
			}
			for field, fieldValue := range model {
				if err := validateMacroUses(fieldValue, "models."+modelID+"."+field, modelID, field, field == "cmdStop"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateMacroUses(value any, path, modelID, fieldPath string, allowPID bool) error {
	switch v := value.(type) {
	case string:
		matches := macroPatternRegex.FindAllStringSubmatch(v, -1)
		for _, match := range matches {
			macroName := match[1]
			if macroName == "PID" && allowPID {
				continue
			}
			if modelID != "" {
				switch fieldPath {
				case "cmd", "cmdStop", "proxy", "checkEndpoint", "filters.stripParams", "name", "description",
					"sleepWake.sleepEndpoint", "sleepWake.wakeEndpoint", "sleepWake.isSleepingEndpoint":
					if macroName == "PORT" || macroName == "MODEL_ID" {
						return fmt.Errorf("macro '${%s}' should have been substituted in %s.%s", macroName, modelID, fieldPath)
					}
					return fmt.Errorf("unknown macro '${%s}' found in %s.%s", macroName, modelID, fieldPath)
				}

				context := "model " + modelID + " " + fieldPath
				if strings.HasPrefix(fieldPath, "capabilities.") {
					context = "model " + modelID + " capabilities"
				} else if strings.HasPrefix(fieldPath, "metadata.") {
					context = "model " + modelID + " metadata"
				}
				return fmt.Errorf("%s: unknown macro '${%s}'", context, macroName)
			}
			return fmt.Errorf("%s: unknown macro '${%s}'", path, macroName)
		}
		envMatches := envMacroRegex.FindAllStringSubmatch(v, -1)
		for _, match := range envMatches {
			varName := match[1]
			return fmt.Errorf("%s: environment variable '%s' not set", path, varName)
		}
		return nil

	case map[string]any:
		for key, val := range v {
			childPath := path + "." + key
			childFieldPath := fieldPath
			if modelID != "" {
				childFieldPath = fieldPath + "." + key
			}
			if err := validateMacroUses(val, childPath, modelID, childFieldPath, allowPID); err != nil {
				return err
			}
		}
		return nil

	case []any:
		for i, val := range v {
			if err := validateMacroUses(val, fmt.Sprintf("%s[%d]", path, i), modelID, fieldPath, allowPID); err != nil {
				return err
			}
		}
		return nil

	default:
		// Scalar types don't contain macros
		return nil
	}
}

// substituteEnvMacros replaces ${env.VAR_NAME} with environment variable values.
// Returns error if any referenced env var is not set or contains invalid characters.
// Env macros inside YAML comments are ignored by unmarshalling the YAML first
// (which strips comments) and only checking the comment-free version for macros.
func substituteEnvMacros(s string) (string, error) {
	// Unmarshal and remarshal to strip YAML comments
	var raw any
	if err := yaml.Unmarshal([]byte(s), &raw); err != nil {
		// If YAML is invalid, fall back to scanning the original string
		// so the user gets the env var error rather than a confusing YAML parse error
		return substituteEnvMacrosInString(s, s)
	}
	clean, err := yaml.Marshal(raw)
	if err != nil {
		return substituteEnvMacrosInString(s, s)
	}

	return substituteEnvMacrosInString(s, string(clean))
}

// substituteEnvMacrosInString finds ${env.VAR} macros in scanStr and substitutes
// them in target. This separation allows scanning comment-free YAML while
// substituting in the original string.
func substituteEnvMacrosInString(target, scanStr string) (string, error) {
	result := target
	matches := envMacroRegex.FindAllStringSubmatch(scanStr, -1)
	for _, match := range matches {
		fullMatch := match[0] // ${env.VAR_NAME}
		varName := match[1]   // VAR_NAME

		value, exists := os.LookupEnv(varName)
		if !exists {
			return "", fmt.Errorf("environment variable '%s' is not set", varName)
		}

		// Sanitize the value for safe YAML substitution
		value, err := sanitizeEnvValueForYAML(value, varName)
		if err != nil {
			return "", err
		}

		result = strings.ReplaceAll(result, fullMatch, value)
	}
	return result, nil
}

// sanitizeEnvValueForYAML ensures an environment variable value is safe for YAML substitution.
// It rejects values with characters that break YAML structure and escapes quotes/backslashes
// for compatibility with double-quoted YAML strings.
func sanitizeEnvValueForYAML(value, varName string) (string, error) {
	// Reject values that would break YAML structure regardless of quoting context
	if strings.ContainsAny(value, "\n\r\x00") {
		return "", fmt.Errorf("environment variable '%s' contains newlines or null bytes which are not allowed in YAML substitution", varName)
	}

	// Escape backslashes and double quotes for safe use in double-quoted YAML strings.
	// In unquoted contexts, these escapes appear literally (harmless for most use cases).
	// In double-quoted contexts, they are interpreted correctly.
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)

	return value, nil
}
