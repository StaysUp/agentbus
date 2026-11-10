package blobs

import (
	"encoding/json"
	"io"
	"net/http"
)

func jsonDecode(r io.Reader, v any) error    { return json.NewDecoder(r).Decode(v) }
func jsonWrite(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
