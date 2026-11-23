package chatcompletions

import (
	"encoding/json"

	"github.com/staysup/agentbus/body"
)

// Split cuts a Chat Completions request into pieces: each message, each tool
// definition, and each legacy functions entry. Everything else stays in Top.
// tools or messages that are not arrays stay in Top unchanged.
func (Adapter) Split(data []byte) (body.Request, error) {
	fields, err := body.Fields(data)
	if err != nil {
		return body.Request{}, err
	}
	var req body.Request
	var top []body.Field
	for _, f := range fields {
		switch f.Key {
		case "messages":
			if els, ok := body.Elements(f.Value); ok {
				for _, e := range els {
					req.Pieces = append(req.Pieces, body.Piece{Kind: body.Message, Label: body.Label(e, "role"), JSON: e})
				}
				continue
			}
		case "tools":
			if els, ok := body.Elements(f.Value); ok {
				for _, e := range els {
					req.Pieces = append(req.Pieces, body.Piece{Kind: body.Tool, Label: toolLabel(e), JSON: e})
				}
				continue
			}
		case "functions":
			if els, ok := body.Elements(f.Value); ok {
				for _, e := range els {
					req.Pieces = append(req.Pieces, body.Piece{Kind: body.Tool, Label: body.Label(e, "name"), JSON: e})
				}
				continue
			}
		}
		top = append(top, f)
	}
	req.Top = body.Object(top)
	return req, nil
}

// toolLabel names a Chat Completions tool: function.name, then custom.name,
// then type. The name sits inside the nested object, not at the top.
func toolLabel(raw []byte) string {
	var t struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Custom struct {
			Name string `json:"name"`
		} `json:"custom"`
	}
	if json.Unmarshal(raw, &t) != nil {
		return body.Label(raw, "name", "type")
	}
	switch {
	case t.Function.Name != "":
		return t.Function.Name
	case t.Custom.Name != "":
		return t.Custom.Name
	case t.Type != "":
		return t.Type
	}
	return ""
}
