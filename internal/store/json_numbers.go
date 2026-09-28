package store

import (
	"bytes"
	"encoding/json"
)

// decodeJSONNumbers preserves literal numbers in dynamic metadata loaded from
// SQL. A plain json.Unmarshal silently rounds integers larger than 2^53.
func decodeJSONNumbers(encoded string, destination any) error {
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	decoder.UseNumber()
	return decoder.Decode(destination)
}
