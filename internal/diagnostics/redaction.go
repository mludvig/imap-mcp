package diagnostics

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

type redactor struct {
	passwords []string
	replacer  *strings.Replacer
}

func newRedactor(password string) redactor {
	if password == "" {
		return redactor{}
	}
	encoded, _ := json.Marshal(password)
	var plain bytes.Buffer
	encoder := json.NewEncoder(&plain)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(password)
	noHTML := bytes.TrimSpace(plain.Bytes())
	quoted := strconv.Quote(password)
	variants := []string{password, string(encoded[1 : len(encoded)-1]), string(noHTML[1 : len(noHTML)-1]), quoted[1 : len(quoted)-1]}
	sort.Slice(variants, func(i, j int) bool { return len(variants[i]) > len(variants[j]) })
	marker := "[REDACTED]"
	if strings.Contains(marker, password) {
		marker = ""
	}
	var replacements []string
	for _, variant := range variants {
		replacements = append(replacements, variant, marker)
	}
	return redactor{passwords: variants, replacer: strings.NewReplacer(replacements...)}
}

// Decode each JSON string before redacting it, so quotes, backslashes, control
// characters, errors, map keys, and JSON embedded in messages are all covered.
// Unchanged tokens retain their original encoding and attribute order.
func (r redactor) redactJSON(record []byte) []byte {
	if len(r.passwords) == 0 {
		return record
	}
	var out bytes.Buffer
	previous := 0
	for i := 0; i < len(record); i++ {
		if record[i] != '"' {
			continue
		}
		start := i
		i++
		for i < len(record) {
			if record[i] == '\\' {
				i += 2
				continue
			}
			if record[i] == '"' {
				break
			}
			i++
		}
		if i >= len(record) {
			break
		}
		var value string
		if json.Unmarshal(record[start:i+1], &value) != nil {
			continue
		}
		redacted := r.replacer.Replace(value)
		if strings.Contains(redacted, `"`) {
			// Embedded JSON can use equivalent escapes, such as \u0070 or \/.
			redacted = string(r.redactJSON([]byte(redacted)))
		}
		if redacted != value {
			out.Write(record[previous:start])
			encoded, _ := json.Marshal(redacted)
			out.Write(encoded)
			previous = i + 1
		}
	}
	if previous == 0 {
		return record
	}
	out.Write(record[previous:])
	return out.Bytes()
}
