package openai

import "github.com/staysup/agentbus/body"

// Split cuts a Responses request into pieces: the instructions, each tool,
// each input item (an input given as one string is one item). Everything else
// stays in Top. Values of another shape stay in Top unchanged.
func (Adapter) Split(data []byte) (body.Request, error) {
	fields, err := body.Fields(data)
	if err != nil {
		return body.Request{}, err
	}
	var req body.Request
	var top []body.Field
	for _, f := range fields {
		switch f.Key {
		case "instructions":
			if body.IsString(f.Value) {
				req.Pieces = append(req.Pieces, body.Piece{Kind: body.Instructions, JSON: f.Value})
				continue
			}
		case "tools":
			if els, ok := body.Elements(f.Value); ok {
				for _, e := range els {
					req.Pieces = append(req.Pieces, body.Piece{Kind: body.Tool, Label: body.Label(e, "name", "type"), JSON: e})
				}
				continue
			}
		case "input":
			if body.IsString(f.Value) {
				req.Pieces = append(req.Pieces, body.Piece{Kind: body.InputItem, Label: "text", JSON: f.Value})
				continue
			}
			if els, ok := body.Elements(f.Value); ok {
				for _, e := range els {
					req.Pieces = append(req.Pieces, body.Piece{Kind: body.InputItem, Label: inputLabel(e), JSON: e})
				}
				continue
			}
		}
		top = append(top, f)
	}
	req.Top = body.Object(top)
	return req, nil
}

// inputLabel names an input item: a message by its role, anything else by its
// type. codex-cli 0.164.0 sends its tools as an input item of type
// additional_tools with role developer (2026-11-05), which the role alone
// would hide.
func inputLabel(raw []byte) string {
	if t := body.Label(raw, "type"); t != "" && t != "message" {
		return t
	}
	return body.Label(raw, "role")
}
